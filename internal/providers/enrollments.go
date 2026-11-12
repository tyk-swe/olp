package providers

import (
	"context"
	"errors"
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

type EnrollmentAuthority struct {
	BeginMutation func(context.Context) (pgx.Tx, access.Principal, error)
	Reauthorize   func(context.Context, pgx.Tx) (access.Principal, error)
	Match         func(string) error
	Audit         func(context.Context, pgx.Tx, access.Actor, string, string, string, string) error
}

type Enrollments struct{ catalogue *Catalogue }

func NewEnrollments(catalogue *Catalogue) *Enrollments { return &Enrollments{catalogue: catalogue} }

type EnrollmentCompletion struct {
	ProviderID        string `json:"provider_id"`
	ETag              string `json:"etag"`
	CredentialID      string `json:"credential_id"`
	CredentialVersion int    `json:"credential_version"`
	Principal         string `json:"principal"`
}

type EnrollmentStatus struct {
	Status     grants.Status         `json:"status"`
	Interval   *int64                `json:"interval,omitempty"`
	Completion *EnrollmentCompletion `json:"completion,omitempty"`
}

func (s *Enrollments) Start(ctx context.Context, authority EnrollmentAuthority, principal access.Principal, providerID, requestedSlot string) (grants.Enrollment, error) {
	c := s.catalogue
	current, err := load(ctx, c.Access.Pool, providerID, false)
	if err != nil {
		return grants.Enrollment{}, err
	}
	if err := principal.Project(current.ProjectID, access.Change); err != nil {
		return grants.Enrollment{}, err
	}
	if err := authority.Match(current.ETag); err != nil {
		return grants.Enrollment{}, err
	}
	if !current.Connection.Document().Grant() {
		return grants.Enrollment{}, access.Fail(422, "grant_enrollment_unavailable", "This provider's profile authenticates without a grant.")
	}
	slots, err := loadSlots(ctx, c.Access.Pool, providerID)
	if err != nil {
		return grants.Enrollment{}, err
	}
	slot, err := enrollmentSlot(slots, requestedSlot)
	if err != nil {
		return grants.Enrollment{}, err
	}
	client, err := c.prober.Client(ctx, current.target(nil))
	if err != nil {
		return grants.Enrollment{}, err
	}
	enrollment, err := grants.Start(ctx, c.Plugins, grants.Enrollment{ProviderID: providerID, SlotID: slot.ID, ExpectedCredentialID: slot.CredentialID, PluginDigest: current.Connection.Document().ProfileRevision, ProfileID: current.Connection.Document().ProfileID, StartedBy: principal.ID}, current.Connection.Document().Options.PluginOptions, client)
	if err != nil {
		if audited := s.auditFailure(ctx, authority, principal.Actor(), providerID); audited != nil {
			return grants.Enrollment{}, audited
		}
		return grants.Enrollment{}, err
	}
	tx, authorized, err := authority.BeginMutation(ctx)
	if err != nil {
		return grants.Enrollment{}, err
	}
	defer tx.Rollback(ctx)
	locked, err := load(ctx, tx, providerID, true)
	if err != nil {
		return grants.Enrollment{}, err
	}
	if err := authorized.Project(locked.ProjectID, access.Change); err != nil {
		return grants.Enrollment{}, err
	}
	if locked.ETag != current.ETag {
		return grants.Enrollment{}, access.Fail(412, "etag_mismatch", "The connection changed while its plugin started grant enrollment; reload and retry.")
	}
	if err := enrollment.Save(ctx, tx, c.Access); err != nil {
		return grants.Enrollment{}, err
	}
	return enrollment, tx.Commit(ctx)
}

func enrollmentSlot(slots []slotRow, requested string) (slotRow, error) {
	for _, slot := range slots {
		if slot.ID == requested || requested == "" && slot.Default {
			return slot, nil
		}
	}
	return slotRow{}, access.Invalid("slot_id", "Unknown credential slot for this connection.")
}

func (s *Enrollments) Continue(ctx context.Context, authority EnrollmentAuthority, providerID, enrollmentID, input string) (EnrollmentCompletion, error) {
	if input == "" || len(input) > maxGrantInput || strings.ContainsRune(input, 0) {
		return EnrollmentCompletion{}, access.Invalid("input", "Paste the callback URL, or the code the upstream displayed, in at most 8192 bytes.")
	}
	tx, principal, err := authority.BeginMutation(ctx)
	if err != nil {
		return EnrollmentCompletion{}, err
	}
	defer tx.Rollback(ctx)
	current, err := load(ctx, tx, providerID, false)
	if err != nil {
		return EnrollmentCompletion{}, err
	}
	if err := principal.Project(current.ProjectID, access.Change); err != nil {
		return EnrollmentCompletion{}, err
	}
	enrollment, err := grants.Claim(ctx, tx, s.catalogue.Access, providerID, enrollmentID, principal.ID)
	if err != nil {
		return EnrollmentCompletion{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return EnrollmentCompletion{}, err
	}
	completion, err := s.exchange(ctx, authority, current, enrollment, input)
	if err != nil {
		if audited := s.auditFailure(ctx, authority, principal.Actor(), providerID); audited != nil {
			return EnrollmentCompletion{}, audited
		}
	}
	return completion, err
}

func (s *Enrollments) exchange(ctx context.Context, authority EnrollmentAuthority, current *record, enrollment grants.Enrollment, input string) (EnrollmentCompletion, error) {
	client, err := s.catalogue.prober.Client(ctx, current.target(nil))
	if err != nil {
		return EnrollmentCompletion{}, err
	}
	grant, err := grants.Exchange(ctx, s.catalogue.Plugins, enrollment, input, current.Connection.Document().Options.PluginOptions, client)
	if err != nil {
		return EnrollmentCompletion{}, err
	}
	return s.stage(ctx, authority, current, enrollment, grant)
}

func (s *Enrollments) stage(requestContext context.Context, authority EnrollmentAuthority, current *record, enrollment grants.Enrollment, grant abi.Grant) (EnrollmentCompletion, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(requestContext), stageGrantTimeout)
	defer cancel()
	tx, principal, err := authority.BeginMutation(ctx)
	if err != nil {
		return EnrollmentCompletion{}, err
	}
	defer tx.Rollback(ctx)
	locked, err := load(ctx, tx, current.ID, true)
	if err != nil {
		return EnrollmentCompletion{}, err
	}
	if err := principal.Project(locked.ProjectID, access.Change); err != nil {
		return EnrollmentCompletion{}, err
	}
	stale := access.Fail(409, "grant_enrollment_stale", "The connection's plugin profile, credential boundary or credential slot changed during grant enrollment. Start another.")
	change, err := connection.Diff(current.Connection.Document(), locked.Connection.Document())
	if err != nil {
		return EnrollmentCompletion{}, err
	}
	if cfg := locked.Connection.Document(); !cfg.Grant() || cfg.ProfileRevision != enrollment.PluginDigest || cfg.ProfileID != enrollment.ProfileID || change.Enrollment {
		return EnrollmentCompletion{}, stale
	}
	credentialID, version, err := grants.Store(ctx, tx, s.catalogue.Access, current.ID, enrollment.PluginDigest, enrollment.ProfileID, grant)
	if err != nil {
		return EnrollmentCompletion{}, err
	}
	if err := enrollment.Complete(ctx, tx, credentialID); err != nil {
		return EnrollmentCompletion{}, err
	}
	completion, err := s.catalogue.bindGrant(ctx, tx, authority, principal.Actor(), grantBinding{ProviderID: current.ID, SlotID: enrollment.SlotID, ExpectedCredentialID: enrollment.ExpectedCredentialID, Credential: grants.Credential{ID: credentialID, Version: version, Principal: grant.Principal}, Action: "provider.grant.enroll"}, stale)
	if err != nil {
		return EnrollmentCompletion{}, err
	}
	return completion, tx.Commit(ctx)
}

type grantBinding struct {
	ProviderID           string
	SlotID               string
	ExpectedCredentialID *string
	Credential           grants.Credential
	Action               string
}

func (s *Catalogue) bindGrant(ctx context.Context, tx pgx.Tx, authority EnrollmentAuthority, actor access.Actor, binding grantBinding, stale error) (EnrollmentCompletion, error) {
	bound, err := tx.Exec(ctx, `UPDATE olp.provider_slots SET credential_id=$3,validated_at=NULL,validated_fingerprint=NULL
  WHERE provider_id=$1 AND id=$2 AND credential_id IS NOT DISTINCT FROM $4::uuid`, binding.ProviderID, binding.SlotID, binding.Credential.ID, binding.ExpectedCredentialID)
	if err != nil {
		return EnrollmentCompletion{}, err
	}
	if bound.RowsAffected() != 1 {
		return EnrollmentCompletion{}, stale
	}
	etag, err := touch(ctx, tx, binding.ProviderID)
	if err != nil {
		return EnrollmentCompletion{}, err
	}
	if _, err := tx.Exec(ctx, "UPDATE olp.providers SET slots_etag=$2 WHERE id=$1", binding.ProviderID, access.NewID()); err != nil {
		return EnrollmentCompletion{}, err
	}
	if err := authority.Audit(ctx, tx, actor, binding.Action, "provider_credential", binding.Credential.ID, "success"); err != nil {
		return EnrollmentCompletion{}, err
	}
	return completion(binding.ProviderID, etag, binding.Credential), nil
}

func completion(providerID, etag string, credential grants.Credential) EnrollmentCompletion {
	return EnrollmentCompletion{ProviderID: providerID, ETag: etag, CredentialID: credential.ID, CredentialVersion: credential.Version, Principal: credential.Principal}
}

func (s *Enrollments) Poll(ctx context.Context, authority EnrollmentAuthority, providerID, enrollmentID string) (EnrollmentStatus, error) {
	tx, err := s.catalogue.Access.Pool.Begin(ctx)
	if err != nil {
		return EnrollmentStatus{}, err
	}
	defer tx.Rollback(ctx)
	principal, err := authority.Reauthorize(ctx, tx)
	if err != nil {
		return EnrollmentStatus{}, err
	}
	current, err := load(ctx, tx, providerID, false)
	if err != nil {
		return EnrollmentStatus{}, err
	}
	if err := principal.Project(current.ProjectID, access.Change); err != nil {
		return EnrollmentStatus{}, err
	}
	enrollment, standing, err := grants.Watch(ctx, tx, s.catalogue.Access, providerID, enrollmentID, principal.ID)
	if err != nil {
		return EnrollmentStatus{}, err
	}
	if standing.Ended {
		if err := authority.Audit(ctx, tx, principal.Actor(), "provider.grant.enroll", "provider", providerID, "failure"); err != nil {
			return EnrollmentStatus{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return EnrollmentStatus{}, err
	}
	if enrollment == nil {
		return grantStatus(current, standing), nil
	}
	staged, err := s.poll(ctx, authority, current, *enrollment)
	if err == nil {
		return EnrollmentStatus{Status: grants.Completed, Completion: &staged}, nil
	}
	settled, settleErr := enrollment.Settle(ctx, s.catalogue.Access.Pool, err)
	if (settleErr != nil || settled.Status != grants.Pending) && !errors.Is(settleErr, pgx.ErrNoRows) {
		if audited := s.auditFailure(ctx, authority, principal.Actor(), providerID); audited != nil {
			return EnrollmentStatus{}, audited
		}
	}
	if settleErr != nil {
		return EnrollmentStatus{}, settleErr
	}
	return grantStatus(current, settled), nil
}

func (s *Enrollments) poll(ctx context.Context, authority EnrollmentAuthority, current *record, enrollment grants.Enrollment) (EnrollmentCompletion, error) {
	client, err := s.catalogue.prober.Client(ctx, current.target(nil))
	if err != nil {
		return EnrollmentCompletion{}, err
	}
	grant, err := grants.Poll(ctx, s.catalogue.Plugins, enrollment, current.Connection.Document().Options.PluginOptions, client)
	if err != nil {
		return EnrollmentCompletion{}, err
	}
	return s.stage(ctx, authority, current, enrollment, grant)
}

func grantStatus(current *record, standing grants.Standing) EnrollmentStatus {
	if standing.Status == grants.Completed {
		completed := completion(current.ID, current.ETag, standing.Credential)
		return EnrollmentStatus{Status: standing.Status, Completion: &completed}
	}
	if standing.Status == grants.Pending || standing.Interval > 0 {
		return EnrollmentStatus{Status: standing.Status, Interval: new(int64(standing.Interval / time.Second))}
	}
	return EnrollmentStatus{Status: standing.Status}
}

func (s *Enrollments) auditFailure(requestContext context.Context, authority EnrollmentAuthority, actor access.Actor, providerID string) error {
	ctx := context.WithoutCancel(requestContext)
	tx, err := s.catalogue.Access.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := authority.Audit(ctx, tx, actor, "provider.grant.enroll", "provider", providerID, "failure"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Enrollments) Cancel(ctx context.Context, authority EnrollmentAuthority, providerID, enrollmentID string) error {
	tx, principal, err := authority.BeginMutation(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	current, err := load(ctx, tx, providerID, false)
	if err != nil {
		return err
	}
	if err := principal.Project(current.ProjectID, access.Change); err != nil {
		return err
	}
	if err := grants.Cancel(ctx, tx, providerID, enrollmentID, principal.ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
