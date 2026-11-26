package providers

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/grants"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/plugin/abi"
)

const grantStepTimeout = time.Minute

const maxGrantInput = 8 << 10

const stageGrantTimeout = 15 * time.Second

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
	providerID, err := httpkit.IDParam(r, "provider_id")
	if err != nil {
		return "", "", err
	}
	enrollmentID, err := httpkit.IDParam(r, "enrollment_id")
	return providerID, enrollmentID, err
}

func (s *enrollmentService) startGrantEnrollment(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	providerID, err := httpkit.IDParam(r, "provider_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var input grantStart
	if r.ContentLength != 0 {
		if err := httpkit.DecodeUnique(r, &input, 1<<10); err != nil {
			return httpkit.Reply{}, err
		}
	}
	requestedSlot, err := optionalSlotID(input.SlotID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	ctx := r.Context()
	current, err := readProvider(ctx, s.access.Pool, providerID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err := p.Project(current.ProjectID, authz.Change); err != nil {
		return httpkit.Reply{}, err
	}
	if err := httpkit.Match(r, current.ETag); err != nil {
		return httpkit.Reply{}, err
	}
	document := current.Connection.Document()
	if !document.Grant() {
		return httpkit.Reply{}, httpkit.Fail(422, "grant_enrollment_unavailable", "This provider's profile authenticates without a grant.")
	}
	slots, err := loadSlots(ctx, s.access.Pool, providerID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	slot, err := enrollmentSlot(slots, requestedSlot)
	if err != nil {
		return httpkit.Reply{}, err
	}
	client, err := s.prober.Client(ctx, current.target(nil))
	if err != nil {
		return httpkit.Reply{}, err
	}
	enrollment, err := grants.Start(ctx, s.plugins, grants.Enrollment{ProviderID: providerID, SlotID: slot.ID, ExpectedCredentialID: slot.CredentialID, PluginDigest: document.ProfileRevision, ProfileID: document.ProfileID, StartedBy: p.ID}, document.Options.PluginOptions, client)
	if err != nil {
		return httpkit.Reply{}, s.failedEnrollment(r, providerID, err)
	}
	reply, err := access.Transact(s.access, r, unchangedSince(ctx, current, "grant enrollment"), func(tx pgx.Tx, _ authz.Principal, _ *record) (httpkit.Reply, access.Event, error) {
		if err := enrollment.Save(ctx, tx, s.access); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Reply{Status: 201, Body: started(enrollment)}, access.Event{}, nil
	})
	return reply, err
}

func optionalSlotID(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	parsed, err := httpkit.ParseUUID(raw)
	if err != nil {
		return "", httpkit.Invalid("slot_id", "Use a credential slot identifier.")
	}
	return parsed, nil
}

func enrollmentSlot(slots []slotRow, requested string) (slotRow, error) {
	for _, slot := range slots {
		if slot.ID == requested || requested == "" && slot.Default {
			return slot, nil
		}
	}
	return slotRow{}, httpkit.Invalid("slot_id", "Unknown credential slot for this connection.")
}

func (s *enrollmentService) continueGrantEnrollment(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	providerID, enrollmentID, err := enrollmentIDs(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	var input grantContinuation
	if err := httpkit.DecodeUnique(r, &input, 2*maxGrantInput); err != nil {
		return httpkit.Reply{}, err
	}
	if input.Input == "" || len(input.Input) > maxGrantInput || strings.ContainsRune(input.Input, 0) {
		return httpkit.Reply{}, httpkit.Invalid("input", "Paste the callback URL, or the code the upstream displayed, in at most 8192 bytes.")
	}
	ctx := r.Context()
	claim, err := access.Run(s.access, r, func(tx pgx.Tx, principal authz.Principal) (claimedEnrollment, access.Event, error) {
		current, err := readProvider(ctx, tx, providerID)
		if err != nil {
			return claimedEnrollment{}, access.Event{}, err
		}
		if err := principal.Project(current.ProjectID, authz.Change); err != nil {
			return claimedEnrollment{}, access.Event{}, err
		}
		enrollment, err := grants.Claim(ctx, tx, s.access, providerID, enrollmentID, principal.ID)
		return claimedEnrollment{provider: current, enrollment: enrollment}, access.Event{}, err
	})
	if err != nil {
		return httpkit.Reply{}, err
	}
	completed, err := s.exchange(r, claim.provider, claim.enrollment, input.Input)
	if err != nil {
		return httpkit.Reply{}, s.failedEnrollment(r, providerID, err)
	}
	return httpkit.Reply{Status: 201, ETag: completed.ETag, Body: completed}, nil
}

func (s *enrollmentService) exchange(r *http.Request, current *record, enrollment grants.Enrollment, input string) (enrollmentCompletion, error) {
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

func (s *enrollmentService) stage(r *http.Request, current *record, enrollment grants.Enrollment, grant abi.Grant) (enrollmentCompletion, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), stageGrantTimeout)
	defer cancel()
	return access.Run(s.access, r.WithContext(ctx), func(tx pgx.Tx, principal authz.Principal) (enrollmentCompletion, access.Event, error) {
		completed, err := s.stageGrant(ctx, tx, principal, current, enrollment, grant)
		if err != nil {
			return enrollmentCompletion{}, access.Event{}, err
		}
		return completed, access.Event{Action: grantEnrollAction, Resource: "provider_credential", ID: completed.CredentialID}, nil
	})
}

func (s *enrollmentService) stageGrant(ctx context.Context, tx pgx.Tx, principal authz.Principal, current *record, enrollment grants.Enrollment, grant abi.Grant) (enrollmentCompletion, error) {
	locked, err := lockProvider(ctx, tx, current.ID)
	if err != nil {
		return enrollmentCompletion{}, err
	}
	if err := principal.Project(locked.ProjectID, authz.Change); err != nil {
		return enrollmentCompletion{}, err
	}
	stale := httpkit.Fail(409, "grant_enrollment_stale", "The connection's plugin profile, credential boundary or credential slot changed during grant enrollment. Start another.")
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
	return bindGrant(ctx, tx, grantBinding{ProviderID: current.ID, SlotID: enrollment.SlotID, ExpectedCredentialID: enrollment.ExpectedCredentialID, Credential: grants.Credential{ID: credentialID, Version: version, Principal: grant.Principal}}, stale)
}

type claimedEnrollment struct {
	provider   *record
	enrollment grants.Enrollment
}

const grantEnrollAction = "provider.grant.enroll"

type grantBinding struct {
	ProviderID           string
	SlotID               string
	ExpectedCredentialID *string
	Credential           grants.Credential
}

func bindGrant(ctx context.Context, tx pgx.Tx, binding grantBinding, stale error) (enrollmentCompletion, error) {
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
	return completion(binding.ProviderID, etag, binding.Credential), nil
}

func completion(providerID, etag string, credential grants.Credential) enrollmentCompletion {
	return enrollmentCompletion{ProviderID: providerID, ETag: etag, CredentialID: credential.ID, CredentialVersion: credential.Version, Principal: credential.Principal}
}

type watchedEnrollment struct {
	provider   *record
	enrollment *grants.Enrollment
	standing   grants.Standing
}

func (s *enrollmentService) pollGrantEnrollment(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	providerID, enrollmentID, err := enrollmentIDs(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	ctx := r.Context()
	watched, err := access.Run(s.access, r, func(tx pgx.Tx, principal authz.Principal) (watchedEnrollment, access.Event, error) {
		current, err := readProvider(ctx, tx, providerID)
		if err != nil {
			return watchedEnrollment{}, access.Event{}, err
		}
		if err := principal.Project(current.ProjectID, authz.Change); err != nil {
			return watchedEnrollment{}, access.Event{}, err
		}
		enrollment, standing, err := grants.Watch(ctx, tx, s.access, providerID, enrollmentID, principal.ID)
		if err != nil {
			return watchedEnrollment{}, access.Event{}, err
		}
		return watchedEnrollment{provider: current, enrollment: enrollment, standing: standing}, enrollmentAuditEvent(providerID, standing), nil
	})
	if err != nil {
		return httpkit.Reply{}, err
	}
	if watched.enrollment == nil {
		return enrollmentStandingReply(watched.provider, watched.standing)
	}
	staged, pollErr := s.poll(r, watched.provider, *watched.enrollment)
	if pollErr == nil {
		return httpkit.OK(enrollmentStatus{Status: grants.Completed, Completion: &staged}), nil
	}
	settled, err := s.settlePollFailure(r, providerID, *watched.enrollment, pollErr)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return enrollmentStandingReply(watched.provider, settled)
}

func (s *enrollmentService) settlePollFailure(r *http.Request, providerID string, enrollment grants.Enrollment, pollErr error) (grants.Standing, error) {
	interrupted := r.Context().Err() != nil
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), stageGrantTimeout)
	defer cancel()
	return access.Run(s.access, r.WithContext(settleCtx), func(tx pgx.Tx, _ authz.Principal) (grants.Standing, access.Event, error) {
		settled, err := enrollment.Settle(settleCtx, tx, pollErr, interrupted)
		if err != nil {
			return grants.Standing{}, access.Event{}, err
		}
		return settled, enrollmentAuditEvent(providerID, settled), nil
	})
}

func enrollmentAuditEvent(providerID string, standing grants.Standing) access.Event {
	if !standing.Ended {
		return access.Event{}
	}
	return access.Event{Action: grantEnrollAction, Resource: "provider", ID: providerID, Failed: true}
}

func enrollmentStandingReply(current *record, standing grants.Standing) (httpkit.Reply, error) {
	if standing.Failure != nil {
		return httpkit.Reply{}, standing.Failure
	}
	return httpkit.OK(grantStatus(current, standing)), nil
}

func (s *enrollmentService) poll(r *http.Request, current *record, enrollment grants.Enrollment) (enrollmentCompletion, error) {
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

func (s *enrollmentService) failedEnrollment(r *http.Request, providerID string, failure error) error {
	if audited := s.auditFailedEnrollment(r, providerID); audited != nil {
		return audited
	}
	return failure
}

func (s *enrollmentService) auditFailedEnrollment(r *http.Request, providerID string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), stageGrantTimeout)
	defer cancel()
	_, err := access.Run(s.access, r.WithContext(ctx), func(_ pgx.Tx, _ authz.Principal) (struct{}, access.Event, error) {
		return struct{}{}, access.Event{Action: grantEnrollAction, Resource: "provider", ID: providerID, Failed: true}, nil
	})
	return err
}

func (s *enrollmentService) cancelGrantEnrollment(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	providerID, enrollmentID, err := enrollmentIDs(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	reply, err := access.Transact(s.access, r, func(tx pgx.Tx, principal authz.Principal) (*record, error) {
		current, err := readProvider(r.Context(), tx, providerID)
		if err != nil {
			return nil, err
		}
		return current, principal.Project(current.ProjectID, authz.Change)
	}, func(tx pgx.Tx, principal authz.Principal, _ *record) (httpkit.Reply, access.Event, error) {
		if err := grants.Cancel(r.Context(), tx, providerID, enrollmentID, principal.ID); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Reply{Status: 204}, access.Event{}, nil
	})
	return reply, err
}
