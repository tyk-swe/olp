package providers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/grants"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/plugin/abi"
)

const grantStepTimeout = time.Minute
const maxGrantInput = 8 << 10
const stageGrantTimeout = 15 * time.Second
const grantEnrollmentOnly = "A grant authenticates this provider: its credential versions come from grant enrollment, not a pasted credential."

type grantStart struct {
	SlotID string `json:"slot_id"`
}

type grantContinuation struct {
	Input string `json:"input"`
}

type enrollmentCompletion struct {
	ProviderID        string `json:"provider_id"`
	ETag              string `json:"etag"`
	CredentialID      string `json:"credential_id"`
	CredentialVersion int    `json:"credential_version"`
	Principal         string `json:"principal"`
}

type enrollmentStatus struct {
	Status     grants.Status         `json:"status"`
	Interval   *int64                `json:"interval,omitempty"`
	Completion *enrollmentCompletion `json:"completion,omitempty"`
}

type enrollmentStarted struct {
	ID               string               `json:"id"`
	ProviderID       string               `json:"provider_id"`
	SlotID           string               `json:"slot_id"`
	ExpiresAt        time.Time            `json:"expires_at"`
	AuthorizationURL *string              `json:"authorization_url,omitempty"`
	Device           *deviceAuthorization `json:"device,omitempty"`
	Input            string               `json:"input,omitempty"`
}

type deviceAuthorization struct {
	VerificationURL string `json:"verification_url"`
	UserCode        string `json:"user_code"`
	Interval        int64  `json:"interval"`
}

func started(enrollment grants.Enrollment) enrollmentStarted {
	return enrollmentStarted{ID: enrollment.ID, ProviderID: enrollment.ProviderID, SlotID: enrollment.SlotID, ExpiresAt: enrollment.ExpiresAt, Input: enrollment.Input,
		AuthorizationURL: authorizationURL(enrollment), Device: device(enrollment.Device)}
}

func authorizationURL(enrollment grants.Enrollment) *string {
	if enrollment.Device != nil {
		return nil
	}
	return &enrollment.AuthorizationURL
}

func device(authorization *abi.DeviceAuthorization) *deviceAuthorization {
	if authorization == nil {
		return nil
	}
	return &deviceAuthorization{VerificationURL: authorization.VerificationURL, UserCode: authorization.UserCode, Interval: authorization.Interval}
}

func enrollmentIDs(r *http.Request) (string, string, error) {
	providerID, err := access.IDParam(r, "provider_id")
	if err != nil {
		return "", "", err
	}
	enrollmentID, err := access.IDParam(r, "enrollment_id")
	return providerID, enrollmentID, err
}

func (s *Server) startGrantEnrollment(r *http.Request, p access.Principal) (access.Reply, error) {
	providerID, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input grantStart
	if r.ContentLength != 0 {
		if err := access.DecodeUnique(r, &input, 1<<10); err != nil {
			return access.Reply{}, err
		}
	}
	requestedSlot, err := optionalSlotID(input.SlotID)
	if err != nil {
		return access.Reply{}, err
	}
	ctx := r.Context()
	current, err := readProvider(ctx, s.access.Pool, providerID)
	if err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(current.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	if err := access.Match(r, current.ETag); err != nil {
		return access.Reply{}, err
	}
	document := current.Connection.Document()
	if !document.Grant() {
		return access.Reply{}, access.Fail(422, "grant_enrollment_unavailable", "This provider's profile authenticates without a grant.")
	}
	slots, err := loadSlots(ctx, s.access.Pool, providerID)
	if err != nil {
		return access.Reply{}, err
	}
	slot, err := enrollmentSlot(slots, requestedSlot)
	if err != nil {
		return access.Reply{}, err
	}
	client, err := s.prober.Client(ctx, current.target(nil))
	if err != nil {
		return access.Reply{}, err
	}
	enrollment, err := grants.Start(ctx, s.plugins, grants.Enrollment{ProviderID: providerID, SlotID: slot.ID, ExpectedCredentialID: slot.CredentialID, PluginDigest: document.ProfileRevision, ProfileID: document.ProfileID, StartedBy: p.ID}, document.Options.PluginOptions, client)
	if err != nil {
		return access.Reply{}, s.failedEnrollment(r, p.Actor(), providerID, err)
	}
	reply, err := access.Transact(s.access, r, unchangedSince(ctx, current, "grant enrollment"), func(tx pgx.Tx, _ access.Principal, _ *record) (access.Reply, error) {
		if err := enrollment.Save(ctx, tx, s.access); err != nil {
			return access.Reply{}, err
		}
		return access.Reply{Status: 201, Body: started(enrollment)}, nil
	})
	return reply, err
}

func optionalSlotID(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	parsed, err := access.ParseUUID(raw)
	if err != nil {
		return "", access.Invalid("slot_id", "Use a credential slot identifier.")
	}
	return parsed, nil
}

func enrollmentSlot(slots []slotRow, requested string) (slotRow, error) {
	for _, slot := range slots {
		if slot.ID == requested || requested == "" && slot.Default {
			return slot, nil
		}
	}
	return slotRow{}, access.Invalid("slot_id", "Unknown credential slot for this connection.")
}

func (s *Server) continueGrantEnrollment(r *http.Request, _ access.Principal) (access.Reply, error) {
	providerID, enrollmentID, err := enrollmentIDs(r)
	if err != nil {
		return access.Reply{}, err
	}
	var input grantContinuation
	if err := access.DecodeUnique(r, &input, 2*maxGrantInput); err != nil {
		return access.Reply{}, err
	}
	if input.Input == "" || len(input.Input) > maxGrantInput || strings.ContainsRune(input.Input, 0) {
		return access.Reply{}, access.Invalid("input", "Paste the callback URL, or the code the upstream displayed, in at most 8192 bytes.")
	}
	ctx := r.Context()
	tx, principal, err := s.access.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(ctx)
	current, err := readProvider(ctx, tx, providerID)
	if err != nil {
		return access.Reply{}, err
	}
	if err := principal.Project(current.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	enrollment, err := grants.Claim(ctx, tx, s.access, providerID, enrollmentID, principal.ID)
	if err != nil {
		return access.Reply{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return access.Reply{}, err
	}
	completed, err := s.exchange(r, current, enrollment, input.Input)
	if err != nil {
		return access.Reply{}, s.failedEnrollment(r, principal.Actor(), providerID, err)
	}
	return access.Reply{Status: 201, ETag: completed.ETag, Body: completed}, nil
}

func (s *Server) exchange(r *http.Request, current *record, enrollment grants.Enrollment, input string) (enrollmentCompletion, error) {
	client, err := s.prober.Client(r.Context(), current.target(nil))
	if err != nil {
		return enrollmentCompletion{}, err
	}
	grant, err := grants.Exchange(r.Context(), s.plugins, enrollment, input, current.Connection.Document().Options.PluginOptions, client)
	if err != nil {
		return enrollmentCompletion{}, err
	}
	return s.stage(r, current, enrollment, grant)
}

func (s *Server) stage(r *http.Request, current *record, enrollment grants.Enrollment, grant abi.Grant) (enrollmentCompletion, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), stageGrantTimeout)
	defer cancel()
	tx, principal, err := s.access.BeginMutation(r.WithContext(ctx))
	if err != nil {
		return enrollmentCompletion{}, err
	}
	defer tx.Rollback(ctx)
	locked, err := lockProvider(ctx, tx, current.ID)
	if err != nil {
		return enrollmentCompletion{}, err
	}
	if err := principal.Project(locked.ProjectID, access.Change); err != nil {
		return enrollmentCompletion{}, err
	}
	stale := access.Fail(409, "grant_enrollment_stale", "The connection's plugin profile, credential boundary or credential slot changed during grant enrollment. Start another.")
	change, err := connection.Diff(current.Connection.Document(), locked.Connection.Document())
	if err != nil {
		return enrollmentCompletion{}, err
	}
	if cfg := locked.Connection.Document(); !cfg.Grant() || cfg.ProfileRevision != enrollment.PluginDigest || cfg.ProfileID != enrollment.ProfileID || change.Enrollment {
		return enrollmentCompletion{}, stale
	}
	credentialID, version, err := grants.Store(ctx, tx, s.access, current.ID, enrollment.PluginDigest, enrollment.ProfileID, grant)
	if err != nil {
		return enrollmentCompletion{}, err
	}
	if err := enrollment.Complete(ctx, tx, credentialID); err != nil {
		return enrollmentCompletion{}, err
	}
	completion, err := bindGrant(ctx, tx, r, principal.Actor(), grantBinding{ProviderID: current.ID, SlotID: enrollment.SlotID, ExpectedCredentialID: enrollment.ExpectedCredentialID, Credential: grants.Credential{ID: credentialID, Version: version, Principal: grant.Principal}, Action: grantEnrollAction}, stale)
	if err != nil {
		return enrollmentCompletion{}, err
	}
	return completion, tx.Commit(ctx)
}

const grantEnrollAction = "provider.grant.enroll"

type grantBinding struct {
	ProviderID           string
	SlotID               string
	ExpectedCredentialID *string
	Credential           grants.Credential
	Action               string
}

func bindGrant(ctx context.Context, tx pgx.Tx, r *http.Request, actor access.Actor, binding grantBinding, stale error) (enrollmentCompletion, error) {
	bound, err := tx.Exec(ctx, `UPDATE olp.provider_slots SET credential_id=$3,validated_at=NULL,validated_fingerprint=NULL
  WHERE provider_id=$1 AND id=$2 AND credential_id IS NOT DISTINCT FROM $4::uuid`, binding.ProviderID, binding.SlotID, binding.Credential.ID, binding.ExpectedCredentialID)
	if err != nil {
		return enrollmentCompletion{}, err
	}
	if bound.RowsAffected() != 1 {
		return enrollmentCompletion{}, stale
	}
	etag, err := touchSlots(ctx, tx, binding.ProviderID)
	if err != nil {
		return enrollmentCompletion{}, err
	}
	if err := access.Audit(ctx, tx, r, actor, binding.Action, "provider_credential", binding.Credential.ID, "success"); err != nil {
		return enrollmentCompletion{}, err
	}
	return completion(binding.ProviderID, etag, binding.Credential), nil
}

func completion(providerID, etag string, credential grants.Credential) enrollmentCompletion {
	return enrollmentCompletion{ProviderID: providerID, ETag: etag, CredentialID: credential.ID, CredentialVersion: credential.Version, Principal: credential.Principal}
}

func (s *Server) pollGrantEnrollment(r *http.Request, _ access.Principal) (access.Reply, error) {
	providerID, enrollmentID, err := enrollmentIDs(r)
	if err != nil {
		return access.Reply{}, err
	}
	ctx := r.Context()
	tx, err := s.access.Pool.Begin(ctx)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(ctx)
	principal, err := s.access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	current, err := readProvider(ctx, tx, providerID)
	if err != nil {
		return access.Reply{}, err
	}
	if err := principal.Project(current.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	enrollment, standing, err := grants.Watch(ctx, tx, s.access, providerID, enrollmentID, principal.ID)
	if err != nil {
		return access.Reply{}, err
	}
	if standing.Ended {
		if err := access.Audit(ctx, tx, r, principal.Actor(), grantEnrollAction, "provider", providerID, "failure"); err != nil {
			return access.Reply{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return access.Reply{}, err
	}
	if enrollment == nil {
		return access.OK(grantStatus(current, standing)), nil
	}
	staged, err := s.poll(r, current, *enrollment)
	if err == nil {
		return access.OK(enrollmentStatus{Status: grants.Completed, Completion: &staged}), nil
	}
	settled, settleErr := enrollment.Settle(ctx, s.access.Pool, err)
	if (settleErr != nil || settled.Status != grants.Pending) && !errors.Is(settleErr, pgx.ErrNoRows) {
		if audited := s.auditFailedEnrollment(r, principal.Actor(), providerID); audited != nil {
			return access.Reply{}, audited
		}
	}
	if settleErr != nil {
		return access.Reply{}, settleErr
	}
	return access.OK(grantStatus(current, settled)), nil
}

func (s *Server) poll(r *http.Request, current *record, enrollment grants.Enrollment) (enrollmentCompletion, error) {
	client, err := s.prober.Client(r.Context(), current.target(nil))
	if err != nil {
		return enrollmentCompletion{}, err
	}
	grant, err := grants.Poll(r.Context(), s.plugins, enrollment, current.Connection.Document().Options.PluginOptions, client)
	if err != nil {
		return enrollmentCompletion{}, err
	}
	return s.stage(r, current, enrollment, grant)
}

func grantStatus(current *record, standing grants.Standing) enrollmentStatus {
	if standing.Status == grants.Completed {
		completed := completion(current.ID, current.ETag, standing.Credential)
		return enrollmentStatus{Status: standing.Status, Completion: &completed}
	}
	if standing.Status == grants.Pending || standing.Interval > 0 {
		return enrollmentStatus{Status: standing.Status, Interval: new(int64(standing.Interval / time.Second))}
	}
	return enrollmentStatus{Status: standing.Status}
}

func (s *Server) failedEnrollment(r *http.Request, actor access.Actor, providerID string, failure error) error {
	if audited := s.auditFailedEnrollment(r, actor, providerID); audited != nil {
		return audited
	}
	return failure
}

func (s *Server) auditFailedEnrollment(r *http.Request, actor access.Actor, providerID string) error {
	ctx := context.WithoutCancel(r.Context())
	tx, err := s.access.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := access.Audit(ctx, tx, r, actor, grantEnrollAction, "provider", providerID, "failure"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Server) cancelGrantEnrollment(r *http.Request, _ access.Principal) (access.Reply, error) {
	providerID, enrollmentID, err := enrollmentIDs(r)
	if err != nil {
		return access.Reply{}, err
	}
	reply, err := access.Transact(s.access, r, func(tx pgx.Tx, principal access.Principal) (*record, error) {
		current, err := readProvider(r.Context(), tx, providerID)
		if err != nil {
			return nil, err
		}
		return current, principal.Project(current.ProjectID, access.Change)
	}, func(tx pgx.Tx, principal access.Principal, _ *record) (access.Reply, error) {
		if err := grants.Cancel(r.Context(), tx, providerID, enrollmentID, principal.ID); err != nil {
			return access.Reply{}, err
		}
		return access.Reply{Status: 204}, nil
	})
	return reply, err
}
