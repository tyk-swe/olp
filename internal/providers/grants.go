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
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// grantStepTimeout bounds a grant enrollment request, which loads the plugin
// and runs one of its steps within the plugin limits.
const grantStepTimeout = time.Minute

// maxGrantInput bounds what an operator pastes back to continue a grant
// enrollment.
const maxGrantInput = 8 << 10

// grantEnrollmentOnly refuses a pasted credential for a provider that
// authenticates with a grant.
const grantEnrollmentOnly = "A grant authenticates this provider: its credential versions come from grant enrollment, not a pasted credential."

type grantStart struct {
	SlotID string `json:"slot_id"`
}

// startGrantEnrollment runs the first step of grant enrollment for a draft
// whose plugin profile authenticates with a grant: the plugin builds the
// authorization request the operator opens to sign in upstream, or starts a
// device authorization the operator approves upstream. The grant will back the
// credential slot the request names, or the default slot; for a slot a grant
// already backs, this re-enrolls its grant.
func (s *Server) startGrantEnrollment(r *http.Request, p access.Principal) (access.Reply, error) {
	a := s.Access
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input grantStart
	if r.ContentLength != 0 {
		if err = access.DecodeUnique(r, &input, 1<<10); err != nil {
			return access.Reply{}, err
		}
	}
	if input.SlotID != "" {
		if input.SlotID, err = access.ParseUUID(input.SlotID); err != nil {
			return access.Reply{}, access.Invalid("slot_id", "Use a credential slot identifier.")
		}
	}
	current, err := load(r.Context(), a.Pool, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(current.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, current.ETag); err != nil {
		return access.Reply{}, err
	}
	cfg := &current.Configuration
	if !cfg.Grant() {
		return access.Reply{}, access.Fail(422, "grant_enrollment_unavailable", "This provider's profile authenticates without a grant.")
	}
	slots, err := loadSlots(r.Context(), a.Pool, id)
	if err != nil {
		return access.Reply{}, err
	}
	var slotID string
	var expectedCredentialID *string
	for _, slot := range slots {
		if slot.ID == input.SlotID || input.SlotID == "" && slot.Default {
			slotID = slot.ID
			expectedCredentialID = slot.CredentialID
		}
	}
	if slotID == "" {
		return access.Reply{}, access.Invalid("slot_id", "Unknown credential slot for this connection.")
	}
	client, err := s.connectionClient(r.Context(), cfg, nil)
	if err != nil {
		return access.Reply{}, err
	}
	// The plugin may reach the upstream, so the step runs outside the
	// installation mutation lock; saving rechecks the draft.
	enrollment, err := grants.Start(r.Context(), s.Plugins, grants.Enrollment{ProviderID: id, SlotID: slotID, ExpectedCredentialID: expectedCredentialID, PluginDigest: cfg.ProfileRevision, ProfileID: cfg.ProfileID, StartedBy: p.ID}, cfg.Options.PluginOptions, client)
	if err != nil {
		if audited := s.auditFailedGrantEnrollment(r, p.Actor(), id); audited != nil {
			return access.Reply{}, audited
		}
		return access.Reply{}, err
	}
	tx, err := a.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	if p, err = a.Reauthorize(r, tx); err != nil {
		return access.Reply{}, err
	}
	locked, err := load(r.Context(), tx, id, true)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(locked.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	if locked.ETag != current.ETag {
		return access.Reply{}, access.Fail(412, "etag_mismatch", "The connection changed while its plugin started grant enrollment; reload and retry.")
	}
	if err = enrollment.Save(r.Context(), tx, a); err != nil {
		return access.Reply{}, err
	}
	body := map[string]any{"id": enrollment.ID, "provider_id": id, "slot_id": slotID, "expires_at": enrollment.ExpiresAt}
	if device := enrollment.Device; device != nil {
		body["device"] = map[string]any{"verification_url": device.VerificationURL, "user_code": device.UserCode, "interval": device.Interval}
	} else {
		body["authorization_url"] = enrollment.AuthorizationURL
	}
	return access.Commit(r, tx, access.Reply{Status: 201, Body: body})
}

type grantContinuation struct {
	Input string `json:"input"`
}

// continueGrantEnrollment exchanges what the operator pasted back, the
// loopback callback URL or the code the upstream displayed, for a grant. The
// grant becomes a new credential version of the provider, staged on the
// enrollment's credential slot like a rotation.
func (s *Server) continueGrantEnrollment(r *http.Request, _ access.Principal) (access.Reply, error) {
	a := s.Access
	providerID, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	enrollmentID, err := access.IDParam(r, "enrollment_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input grantContinuation
	if err = access.DecodeUnique(r, &input, 2*maxGrantInput); err != nil {
		return access.Reply{}, err
	}
	if input.Input == "" || len(input.Input) > maxGrantInput || strings.ContainsRune(input.Input, 0) {
		return access.Reply{}, access.Invalid("input", "Paste the callback URL, or the code the upstream displayed, in at most 8192 bytes.")
	}
	// The claim commits before the plugin reaches the upstream, so neither a
	// repeated continuation nor another replica can exchange the same sign-in.
	tx, err := a.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := a.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	current, err := load(r.Context(), tx, providerID, false)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(current.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	enrollment, err := grants.Claim(r.Context(), tx, a, providerID, enrollmentID, p.ID)
	if err != nil {
		return access.Reply{}, err
	}
	if err = tx.Commit(r.Context()); err != nil {
		return access.Reply{}, err
	}
	reply, err := s.exchangeGrant(r, current, enrollment, input.Input)
	if err != nil {
		if audited := s.auditFailedGrantEnrollment(r, p.Actor(), providerID); audited != nil {
			return access.Reply{}, audited
		}
	}
	return reply, err
}

// exchangeGrant runs the plugin's exchange of what was pasted back, for the
// provider with its current options and over its network path, and stages the
// grant it obtains.
func (s *Server) exchangeGrant(r *http.Request, current *record, enrollment grants.Enrollment, input string) (access.Reply, error) {
	cfg := &current.Configuration
	client, err := s.connectionClient(r.Context(), cfg, nil)
	if err != nil {
		return access.Reply{}, err
	}
	grant, err := grants.Exchange(r.Context(), s.Plugins, enrollment, input, cfg.Options.PluginOptions, client)
	if err != nil {
		return access.Reply{}, err
	}
	return s.stageGrant(r, current, enrollment, grant)
}

// stageGrantTimeout bounds storing a grant the upstream already granted:
// the work must outlive the request that obtained it, or a caller that went
// away would lose a grant only its enrollment can use.
const stageGrantTimeout = 15 * time.Second

// stageGrant completes a grant enrollment under the mutation lock: it holds
// the grant beneath a new credential version of the provider and binds that
// version to the enrollment's credential slot, like a rotation.
func (s *Server) stageGrant(r *http.Request, current *record, enrollment grants.Enrollment, grant abi.Grant) (access.Reply, error) {
	a := s.Access
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), stageGrantTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	tx, err := a.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := a.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	locked, err := load(r.Context(), tx, current.ID, true)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(locked.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	stale := access.Fail(409, "grant_enrollment_stale", "The connection's plugin profile or credential slot changed during grant enrollment. Start another.")
	if cfg := locked.Configuration; !cfg.Grant() || cfg.ProfileRevision != enrollment.PluginDigest || cfg.ProfileID != enrollment.ProfileID {
		return access.Reply{}, stale
	}
	credentialID, version, err := grants.Store(r.Context(), tx, a, current.ID, enrollment.PluginDigest, grant)
	if err != nil {
		return access.Reply{}, err
	}
	if err = enrollment.Complete(r.Context(), tx, credentialID); err != nil {
		return access.Reply{}, err
	}
	bound, err := tx.Exec(r.Context(), `UPDATE olp.provider_slots SET credential_id=$3,validated_at=NULL,validated_fingerprint=NULL
		WHERE provider_id=$1 AND id=$2 AND credential_id IS NOT DISTINCT FROM $4::uuid`, current.ID, enrollment.SlotID, credentialID, enrollment.ExpectedCredentialID)
	if err != nil {
		return access.Reply{}, err
	}
	if bound.RowsAffected() == 0 {
		return access.Reply{}, stale
	}
	etag, err := touch(r.Context(), tx, current.ID)
	if err != nil {
		return access.Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "UPDATE olp.providers SET slots_etag=$2 WHERE id=$1", current.ID, access.NewID()); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "provider.grant.enroll", "provider_credential", credentialID, "success"); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, access.Reply{Status: 201, ETag: etag, Body: completion(current.ID, etag, grants.Credential{ID: credentialID, Version: version, Principal: grant.Principal})})
}

// completion is what a completed grant enrollment reports: the credential
// version its grant created, staged on the provider draft with the ETag.
func completion(providerID, etag string, credential grants.Credential) map[string]any {
	return map[string]any{"provider_id": providerID, "etag": etag, "credential_id": credential.ID, "credential_version": credential.Version, "principal": credential.Principal}
}

// A grantEnrollmentStatus is what a status request reports about a grant
// enrollment by device authorization.
type grantEnrollmentStatus struct {
	Status grants.Status `json:"status"`
	// Interval is how many seconds to wait before the next status request,
	// while the enrollment is pending.
	Interval int64 `json:"interval,omitempty"`
	// Completion is what a continuation returns, once the enrollment
	// completed.
	Completion any `json:"completion,omitempty"`
}

// pollGrantEnrollment serves a status request for a grant enrollment by
// device authorization. Control runs no background jobs, so status requests
// drive polling: one made once the plugin's interval has passed since the
// last poll runs one poll step, and since the enrollment's session state is
// persisted, any control replica serves the next. Once the operator approves
// the device, the grant is staged like a pasted-back one.
func (s *Server) pollGrantEnrollment(r *http.Request, _ access.Principal) (access.Reply, error) {
	a := s.Access
	providerID, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	enrollmentID, err := access.IDParam(r, "enrollment_id")
	if err != nil {
		return access.Reply{}, err
	}
	// The claim commits before the plugin reaches the upstream, so no other
	// status request polls meanwhile. It locks the enrollment alone, not the
	// installation, since status requests come every few seconds.
	tx, err := a.Pool.Begin(r.Context())
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := a.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	current, err := load(r.Context(), tx, providerID, false)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(current.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	enrollment, standing, err := grants.Watch(r.Context(), tx, a, providerID, enrollmentID, p.ID)
	if err != nil {
		return access.Reply{}, err
	}
	// A device authorization that expired before a poll ended it ends with
	// the status request that finds it expired, which audits it once.
	if standing.Ended {
		if err = access.Audit(r.Context(), tx, r, p.Actor(), "provider.grant.enroll", "provider", providerID, "failure"); err != nil {
			return access.Reply{}, err
		}
	}
	if err = tx.Commit(r.Context()); err != nil {
		return access.Reply{}, err
	}
	if enrollment == nil {
		return access.OK(grantStatus(current, standing)), nil
	}
	staged, err := s.pollGrant(r, current, *enrollment)
	if err == nil {
		return access.OK(grantEnrollmentStatus{Status: grants.Completed, Completion: staged.Body}), nil
	}
	standing, err = enrollment.Settle(r.Context(), a.Pool, err)
	// A poll that ended the enrollment without a grant is audited, unless
	// the enrollment was cancelled meanwhile.
	if (err != nil || standing.Status != grants.Pending) && !errors.Is(err, pgx.ErrNoRows) {
		if audited := s.auditFailedGrantEnrollment(r, p.Actor(), providerID); audited != nil {
			return access.Reply{}, audited
		}
	}
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(grantStatus(current, standing)), nil
}

// pollGrant runs a poll step of a device authorization, for the provider with
// its current options and over its network path, and stages the grant once
// the operator approved the device.
func (s *Server) pollGrant(r *http.Request, current *record, enrollment grants.Enrollment) (access.Reply, error) {
	cfg := &current.Configuration
	client, err := s.connectionClient(r.Context(), cfg, nil)
	if err != nil {
		return access.Reply{}, err
	}
	grant, err := grants.Poll(r.Context(), s.Plugins, enrollment, cfg.Options.PluginOptions, client)
	if err != nil {
		return access.Reply{}, err
	}
	return s.stageGrant(r, current, enrollment, grant)
}

// grantStatus reports where a device authorization stands.
func grantStatus(current *record, standing grants.Standing) grantEnrollmentStatus {
	status := grantEnrollmentStatus{Status: standing.Status, Interval: int64(standing.Interval / time.Second)}
	if standing.Status == grants.Completed {
		status.Completion = completion(current.ID, current.ETag, standing.Credential)
	}
	return status
}

// auditFailedGrantEnrollment records a start or a continuation that failed,
// or a device authorization's poll that ended it without a grant, in its own
// transaction once the step's rolled back. Like every audit record, it names
// the principal and the provider, never what was pasted back.
func (s *Server) auditFailedGrantEnrollment(r *http.Request, actor access.Actor, providerID string) error {
	ctx := context.WithoutCancel(r.Context())
	tx, err := s.Access.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = access.Audit(ctx, tx, r, actor, "provider.grant.enroll", "provider", providerID, "failure"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// cancelGrantEnrollment ends a grant enrollment its principal started and has
// not continued, deleting its session state.
func (s *Server) cancelGrantEnrollment(r *http.Request, _ access.Principal) (access.Reply, error) {
	a := s.Access
	providerID, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	enrollmentID, err := access.IDParam(r, "enrollment_id")
	if err != nil {
		return access.Reply{}, err
	}
	tx, err := a.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := a.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	current, err := load(r.Context(), tx, providerID, false)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(current.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	if err = grants.Cancel(r.Context(), tx, providerID, enrollmentID, p.ID); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, access.Reply{Status: 204})
}
