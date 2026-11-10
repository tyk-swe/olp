package jobs

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/media/transport"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/staging"
)

const LeaseSlack = 60 * time.Second

type Deps struct {
	Store       *Store
	Transport   *transport.Transport
	Credentials release.Credentials
	Gaps        *atomic.Uint64
	Log         *slog.Logger
	Now         func() time.Time
}

type Service struct {
	store       *Store
	transport   *transport.Transport
	credentials release.Credentials
	gaps        *atomic.Uint64
	log         *slog.Logger
	now         func() time.Time
}

func New(deps Deps) *Service {
	return &Service{store: deps.Store, transport: deps.Transport, credentials: deps.Credentials,
		gaps: deps.Gaps, log: deps.Log, now: deps.Now}
}

func (s *Service) RecordGap() { s.gaps.Add(1) }

type Resolved struct {
	transport.Target
	Provider routing.Provider
	Slot     routing.Slot
	Timeout  time.Duration
}

func (r Resolved) Caller() bool { return r.Provider.CredentialSource == "caller" }

type Caller struct {
	Secret    func(provider, model string) ([]byte, error)
	Sensitive *egress.Sensitive
}

func (s *Service) Target(ctx context.Context, record Record) (Resolved, error) {
	runtimeUnavailable := &Unavailable{Code: "media_job_runtime_unavailable"}
	var raw []byte
	if err := s.store.db.QueryRow(ctx, "SELECT snapshot FROM olp.runtime_releases WHERE id = $1", record.RuntimeGenerationID).Scan(&raw); err != nil {
		return Resolved{}, runtimeUnavailable
	}
	pin, err := routing.DecodeReleasePin(raw, routing.ReleasePinIdentity{ProviderID: record.ProviderID, ProviderRevisionID: record.ProviderRevisionID, RouteSlug: record.RouteSlug, Model: record.UpstreamModel, SlotID: record.SlotID, CredentialID: record.CredentialVersionID})
	if err != nil {
		return Resolved{}, runtimeUnavailable
	}
	provider, slot := pin.Provider, pin.Slot
	config := provider.Connector()
	owned := provider.CredentialSource != "caller"
	if owned && record.CredentialVersionID != nil {
		if eligibility := s.credentials.Eligibility(*record.CredentialVersionID); eligibility != routing.Eligible {
			return Resolved{}, &Unavailable{Code: "media_job_credential_" + string(eligibility)}
		}
	}
	var secret []byte
	if owned && connectors.SecretRequired(config.AuthMode) {
		if record.CredentialVersionID == nil {
			return Resolved{}, runtimeUnavailable
		}
		secret, _, err = s.credentials.Secret(ctx, nil, *record.CredentialVersionID)
		if err != nil || len(secret) == 0 {
			return Resolved{}, runtimeUnavailable
		}
	}
	var networkSecret []byte
	if config.Network != nil && config.Network.CredentialID != "" {
		networkSecret, err = s.credentials.NetworkSecret(ctx, nil, provider.ID, config.Network.CredentialID)
		if err != nil {
			return Resolved{}, &Unavailable{Code: "media_job_network_credential_unavailable"}
		}
	}
	credential := "none"
	if slot.CredentialID != nil {
		credential = *slot.CredentialID
	}
	return Resolved{
		Target: transport.Target{Config: config, Model: config.Model(record.UpstreamModel), Secret: secret,
			NetworkSecret: networkSecret, ConnectionScope: provider.ID + "/" + provider.RevisionID + "/" + slot.ID + "/" + credential},
		Provider: provider,
		Slot:     slot,
		Timeout:  time.Duration(pin.Route.OverallTimeout) * time.Millisecond,
	}, nil
}

func (s *Service) bind(resolved Resolved, record Record, caller *Caller) (Resolved, error) {
	if !resolved.Caller() {
		return resolved, nil
	}
	unavailable := &Unavailable{Code: "media_job_caller_credential_unavailable"}
	if caller == nil {
		return Resolved{}, unavailable
	}
	secret, err := caller.Secret(resolved.Provider.ID, record.UpstreamModel)
	if err != nil {
		return Resolved{}, unavailable
	}
	return Resolved{Target: resolved.WithCaller(secret, caller.Sensitive), Provider: resolved.Provider,
		Slot: resolved.Slot, Timeout: resolved.Timeout}, nil
}

type upstreamFailure struct{ *transport.Failure }

func (f upstreamFailure) Error() string { return "media job upstream call failed: " + string(f.Class) }

func (s *Service) send(ctx context.Context, resolved Resolved, record Record, job media.JobCall) (*transport.Result, error) {
	call, encodeFailure := media.EncodeJob(job, resolved.Config)
	if encodeFailure != nil {
		return nil, &Unavailable{Code: "media_job_operation_invalid"}
	}
	ctx, cancel := context.WithTimeout(ctx, resolved.Timeout+LeaseSlack)
	defer cancel()
	result, failure := s.transport.Do(ctx, resolved.Target, call, transport.Options{Strict: record.StrictContract})
	if failure != nil {
		return nil, upstreamFailure{failure}
	}
	return result, nil
}

func (s *Service) dispatch(ctx context.Context, record Record, job media.JobCall, caller *Caller) (*transport.Result, error) {
	resolved, err := s.Target(ctx, record)
	if err != nil {
		return nil, err
	}
	resolved, err = s.bind(resolved, record, caller)
	if err != nil {
		return nil, err
	}
	return s.send(ctx, resolved, record, job)
}

func deleteConfirmed(result *transport.Result, err error) bool {
	if failure, ok := errors.AsType[upstreamFailure](err); ok {
		return failure.Status == 404
	}
	if err != nil {
		return false
	}
	deleted, ok := result.Payload.(transport.Deleted)
	return ok && deleted.VideoDeleteResult != nil && deleted.Deleted
}

func (s *Service) gap(message string, args ...any) {
	s.RecordGap()
	s.log.Error(message, args...)
}

func (s *Service) Abandon(ctx context.Context, id string, ambiguous bool) {
	if ambiguous {
		if _, err := s.store.MarkAmbiguous(ctx, id, Unfenced, "upstream_create_result_ambiguous"); err != nil {
			s.log.Error("failed to mark ambiguous video creation", "job_id", id, "error", err)
		}
		return
	}
	s.Finalize(ctx, id)
}

func (s *Service) Finalize(ctx context.Context, id string) bool {
	record, err := s.store.Finalize(ctx, id, Unfenced)
	if err == nil && record.Lifecycle == Deleted {
		return true
	}
	s.gap("video job tombstone was not finalized", "job_id", id, "error", err)
	return false
}

type Rejected struct{ Message string }

func (r *Rejected) Error() string { return r.Message }

var ErrReconciling = errors.New("video creation is being reconciled")

func (s *Service) Attach(ctx context.Context, reserved Record, video *media.VideoJobResult, caller *Caller) error {
	if video == nil {
		return s.reject(ctx, reserved.ID, nil, "upstream_create_response_missing_job_identity", "The provider returned an incompatible video creation response.")
	}
	if !media.ValidUpstreamJobID(video.ID) {
		return s.reject(ctx, reserved.ID, nil, "upstream_create_response_invalid_job_identity", "The provider returned an invalid video job identity.")
	}
	update, ok := Observe(video, s.now())
	if !ok {
		return s.reject(ctx, reserved.ID, &video.ID, "upstream_create_response_invalid_status", "The provider returned an unsupported video status.")
	}
	err := s.attach(ctx, reserved.ID, video.ID, update)
	if err == nil {
		return nil
	}
	if kind, _ := KindOf(err); kind != KindConflict && s.compensate(ctx, reserved, video.ID, caller) {
		return ErrReconciling
	}
	s.gap("video create reconciliation gap requires operator attention",
		"job_id", reserved.ID, "upstream_job_id", video.ID, "error", err)
	return ErrReconciling
}

func (s *Service) reject(ctx context.Context, id string, upstream *string, reason, message string) error {
	var err error
	if upstream == nil {
		_, err = s.store.MarkAmbiguous(ctx, id, Unfenced, reason)
	} else {
		_, err = s.store.MarkCleanup(ctx, id, Unfenced, *upstream, reason)
	}
	if err != nil {
		s.gap("failed to retire rejected video reservation", "job_id", id, "reason", reason, "error", err)
	}
	return &Rejected{Message: message}
}

func (s *Service) attach(ctx context.Context, id, upstream string, update Update) error {
	var err error
	for attempt := range 3 {
		if _, err = s.store.Attach(ctx, id, upstream, update); err == nil {
			return nil
		}
		if kind, _ := KindOf(err); kind != KindDatabase {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(attempt+1) * 25 * time.Millisecond):
		}
	}
	return err
}

func (s *Service) compensate(ctx context.Context, reserved Record, upstream string, caller *Caller) bool {
	marked, err := s.store.MarkCleanup(ctx, reserved.ID, Unfenced, upstream, "upstream_created_local_attach_failed")
	if err != nil {
		s.log.Error("failed to persist video cleanup metadata", "job_id", reserved.ID, "error", err)
		return false
	}
	result, err := s.dispatch(context.WithoutCancel(ctx), marked, media.VideoDelete{ID: upstream}, caller)
	if !deleteConfirmed(result, err) {
		return false
	}
	finalize, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	s.Finalize(finalize, reserved.ID)
	return true
}

func (s *Service) Refresh(ctx context.Context, id string) (Record, error) {
	record, err := s.store.Get(ctx, id)
	if err != nil || !refreshable(record, s.now()) {
		return record, err
	}
	if record.CallerCredential {
		return Record{}, errCallerCredential
	}
	claimed, ok, err := s.store.ClaimOne(ctx, id, s.now())
	if err != nil {
		return Record{}, err
	}
	if !ok {
		return Record{}, busy("media job is being reconciled by another worker")
	}
	s.reconcile(ctx, claimed)
	return s.store.Get(ctx, id)
}

func (s *Service) Delete(ctx context.Context, id string) (Record, error) {
	record, err := s.store.Get(ctx, id)
	if err != nil || record.Lifecycle == Deleted {
		return record, err
	}
	if record.Lifecycle == Creating {
		return Record{}, busy("media job create is still in flight")
	}
	if record.CallerCredential {
		return Record{}, errCallerCredential
	}
	claimed, ok, err := s.store.ClaimOne(ctx, id, s.now())
	if err != nil {
		return Record{}, err
	}
	if !ok {
		return Record{}, busy("media job is being reconciled by another worker")
	}
	if claimed.Lifecycle != Active {
		s.reconcile(ctx, claimed)
		return s.store.Get(ctx, id)
	}
	deleting, err := s.store.BeginDeletion(ctx, id, claimed.Fence())
	if errors.Is(err, errClaimLost) {
		return Record{}, busy("media job is being reconciled by another worker")
	}
	if err != nil {
		return Record{}, err
	}
	s.reconcile(ctx, Claimed{Record: deleting, claim: claimed.claim})
	return s.store.Get(ctx, id)
}

func (s *Service) Content(ctx context.Context, record Record, variant string) (*staging.Artifact, error) {
	upstream, ok := record.Upstream()
	if !ok || record.Lifecycle != Active || !record.ContentAvailable() {
		return nil, ErrNoContent
	}
	result, err := s.dispatch(ctx, record, media.VideoContent{ID: upstream, Variant: variant}, nil)
	if err != nil {
		return nil, err
	}
	content, ok := result.Payload.(transport.Content)
	if !ok || content.Artifact == nil {
		return nil, ErrNoContent
	}
	return content.Artifact, nil
}
