package grants

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// SessionTTL bounds a grant enrollment: the operator signs in upstream and
// pastes back what the upstream returned within it. A device authorization
// lasts as long as its user code instead, within maxDeviceTTL.
const SessionTTL = 10 * time.Minute

// sessionPurpose is the secret purpose of an enrollment's session state.
const sessionPurpose = "grant_enrollment"

// maxSession bounds the state a plugin carries between enrollment steps.
const maxSession = 16 << 10

// An Enrollment is a grant enrollment in progress: an operator's sign-in to an
// upstream account, through the plugin profile a provider pins, for one of the
// provider's credential slots. It is persisted with its session state
// encrypted, so any control replica can continue it. The operator continues it
// once with what the upstream returned, or, for a device authorization,
// approves the device upstream while status requests poll it.
type Enrollment struct {
	ID, ProviderID, SlotID string
	// PluginDigest and ProfileID identify the plugin profile the grant is
	// enrolled for.
	PluginDigest, ProfileID string
	// StartedBy is the principal that started the enrollment. Only it may
	// continue or cancel it.
	StartedBy        string
	ExpiresAt        time.Time
	AuthorizationURL string
	// Device, instead of AuthorizationURL, is the device authorization the
	// operator approves upstream.
	Device *abi.DeviceAuthorization
	// session is the plugin's state for its next step.
	session string
	// interval is how long a device authorization waits between polls.
	interval time.Duration
}

// Start runs the plugin's first grant enrollment step for an enrollment that
// names its provider, slot, plugin profile and principal, on behalf of the
// provider with its option values, and returns it ready to Save, with the
// authorization URL the operator opens or the device authorization the
// operator approves. The step reaches the plugin's approved origins through
// client, the provider's network path.
func Start(ctx context.Context, host *plugins.Host, e Enrollment, options map[string]string, client *http.Client) (Enrollment, error) {
	var authorization abi.GrantAuthorization
	manifest, err := step(ctx, host, e, options, client, plugins.Call{Method: abi.MethodGrantStart, Params: abi.GrantStart{Profile: e.ProfileID}}, &authorization)
	if err != nil {
		return e, failure(err)
	}
	if len(authorization.Session) > maxSession {
		return e, refused("its session state exceeds 16 KiB")
	}
	e.ID, e.ExpiresAt, e.session = access.NewID(), time.Now().Add(SessionTTL), authorization.Session
	switch {
	case authorization.Device != nil && authorization.URL == "":
		return e, e.authorizeDevice(manifest, *authorization.Device)
	case authorization.Device != nil || !approved(manifest, authorization.URL):
		return e, refused("it returned neither an authorization request at one of the plugin's approved origins nor a device authorization")
	}
	e.AuthorizationURL = authorization.URL
	return e, nil
}

// approved reports whether address is a URL at one of the plugin's approved
// origins, where OLP may send the operator.
func approved(manifest abi.Manifest, address string) bool {
	target, err := url.Parse(address)
	return len(address) <= 8<<10 && err == nil && (target.Scheme == "https" || target.Scheme == "http") && slices.Contains(manifest.Origins, plugins.Origin(target))
}

// Save persists a started enrollment, its session state encrypted until the
// enrollment expires. A device authorization's first poll is due after its
// interval.
func (e Enrollment) Save(ctx context.Context, tx pgx.Tx, a *access.Server) error {
	if err := a.Keys.Store(ctx, tx, a.Installation, e.ID, sessionPurpose, []byte(e.session), &e.ExpiresAt); err != nil {
		return err
	}
	var interval *int64
	if e.Device != nil {
		interval = new(seconds(e.interval))
	}
	_, err := tx.Exec(ctx, `INSERT INTO olp.grant_enrollments(id,provider_id,slot_id,plugin_digest,profile_id,started_by,expires_at,poll_interval,poll_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8::integer,now()+$8::integer*interval '1 second')`,
		e.ID, e.ProviderID, e.SlotID, e.PluginDigest, e.ProfileID, e.StartedBy, e.ExpiresAt, interval)
	return err
}

// Claim takes a provider's enrollment for the principal that started it, to
// continue it with what the operator pasted back: it can be claimed once,
// before it expires, and its session state is deleted as it is read. The
// caller commits the claim before running the plugin, so no other replica can
// continue the enrollment meanwhile. A device authorization is polled, never
// continued: while pending, it is not found.
func Claim(ctx context.Context, tx pgx.Tx, a *access.Server, providerID, id, principal string) (Enrollment, error) {
	e := Enrollment{ID: id, ProviderID: providerID, StartedBy: principal}
	err := tx.QueryRow(ctx, `UPDATE olp.grant_enrollments SET continued_at=now()
		WHERE id=$1 AND provider_id=$2 AND started_by=$3 AND continued_at IS NULL AND expires_at>now() AND poll_interval IS NULL
		RETURNING slot_id::text,plugin_digest,profile_id,expires_at`, id, providerID, principal).Scan(&e.SlotID, &e.PluginDigest, &e.ProfileID, &e.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, unavailable(ctx, tx, providerID, id, principal)
	}
	if err != nil {
		return e, err
	}
	session, err := a.Keys.Read(ctx, tx, a.Installation, id, sessionPurpose)
	if err != nil {
		return e, err
	}
	e.session = string(session)
	_, err = tx.Exec(ctx, "DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2", id, sessionPurpose)
	return e, err
}

// Cancel ends a provider's enrollment that the principal started and has not
// continued.
func Cancel(ctx context.Context, tx pgx.Tx, providerID, id, principal string) error {
	tag, err := tx.Exec(ctx, "DELETE FROM olp.grant_enrollments WHERE id=$1 AND provider_id=$2 AND started_by=$3 AND continued_at IS NULL", id, providerID, principal)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return unavailable(ctx, tx, providerID, id, principal)
	}
	_, err = tx.Exec(ctx, "DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2", id, sessionPurpose)
	return err
}

// unavailable explains why an enrollment can no longer be continued or
// cancelled. Another principal's enrollment is not found, like one purged
// after it expired, and so is a device authorization still pending, which is
// polled rather than continued.
func unavailable(ctx context.Context, q access.Queryer, providerID, id, principal string) error {
	var continued, polled bool
	err := q.QueryRow(ctx, "SELECT continued_at IS NOT NULL, poll_interval IS NOT NULL FROM olp.grant_enrollments WHERE id=$1 AND provider_id=$2 AND started_by=$3", id, providerID, principal).Scan(&continued, &polled)
	switch {
	case err != nil:
		return err
	case continued:
		return access.Fail(409, "grant_enrollment_used", "This grant enrollment was already continued, and a grant enrollment is continued once. Start another.")
	case polled:
		return pgx.ErrNoRows
	}
	return access.Fail(410, "grant_enrollment_expired", "This grant enrollment expired. Start another, and paste back what the upstream returns within 10 minutes.")
}

// Complete records the credential version an enrollment's grant created, in
// the transaction that creates it, which ends the enrollment and deletes any
// session state it kept. It fails with pgx.ErrNoRows for an enrollment that
// ended or was cancelled meanwhile.
func (e Enrollment) Complete(ctx context.Context, tx pgx.Tx, credentialID string) error {
	// A claimed enrollment, or a device authorization still pending.
	tag, err := tx.Exec(ctx, `UPDATE olp.grant_enrollments SET continued_at=coalesce(continued_at,now()),outcome='completed',credential_id=$2
		WHERE id=$1 AND outcome IS NULL AND (continued_at IS NULL)=(poll_interval IS NOT NULL)`, e.ID, credentialID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	_, err = tx.Exec(ctx, "DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2", e.ID, sessionPurpose)
	return err
}

// Exchange runs the plugin's step that exchanges what the operator pasted
// back for a grant, on behalf of the provider with its option values, and
// checks that the grant is one the profile's hosting adaptation can place. The
// step reaches the plugin's approved origins through client.
func Exchange(ctx context.Context, host *plugins.Host, e Enrollment, input string, options map[string]string, client *http.Client) (abi.Grant, error) {
	var grant abi.Grant
	call := plugins.Call{
		Method:  abi.MethodGrantExchange,
		Params:  abi.GrantExchange{Profile: e.ProfileID, Session: e.session, Input: input},
		Secrets: []string{e.session, input},
	}
	manifest, err := step(ctx, host, e, options, client, call, &grant)
	if err != nil {
		return grant, failure(err)
	}
	return grant, fits(manifest, e.ProfileID, &grant)
}

// fits checks that a grant the plugin obtained is one the profile's hosting
// adaptation can place.
func fits(manifest abi.Manifest, profile string, grant *abi.Grant) error {
	i := slices.IndexFunc(manifest.Profiles, func(p abi.Profile) bool { return p.ID == profile })
	if i < 0 || manifest.Profiles[i].Grant == nil {
		return refused("the plugin declares no profile " + profile + " that authenticates with a grant")
	}
	if err := validate(manifest.Profiles[i].Grant, grant); err != nil {
		return refused(err.Error())
	}
	return nil
}

// step runs one grant step of an enrollment on its usable plugin, confined or
// unconfined, on behalf of the enrollment's provider with its option values,
// granting it HTTP to the plugin's approved origins through client, and
// returns the plugin's manifest.
func step(ctx context.Context, host *plugins.Host, e Enrollment, options map[string]string, client *http.Client, call plugins.Call, result any) (abi.Manifest, error) {
	manifest, err := host.Manifest(ctx, e.PluginDigest)
	if err != nil {
		return manifest, err
	}
	call.Provider = &abi.Provider{Profile: e.ProfileID, Options: options}
	call.HTTP = &plugins.HTTP{Origins: manifest.Origins, Client: client}
	return manifest, host.Call(ctx, e.PluginDigest, call, result)
}

// failure is the problem the operator sees for a grant step that failed.
func failure(err error) error {
	if reported, ok := errors.AsType[*abi.Error](err); ok {
		if reported.Code == abi.CodeStateMismatch {
			return access.Fail(422, "grant_state_mismatch", "What was pasted back answers another sign-in than this grant enrollment's: "+reported.Message)
		}
		return access.Fail(422, "grant_enrollment_failed", "The plugin could not enroll a grant ("+reported.Code+"): "+reported.Message)
	}
	if refusal, ok := errors.AsType[*plugins.Error](err); ok {
		return access.Fail(422, refusal.Code, refusal.Message)
	}
	return err
}

// refused is the problem for a plugin step whose result OLP can't use.
func refused(reason string) error {
	return access.Fail(422, "grant_enrollment_failed", "The plugin's grant enrollment returned something OLP can't use: "+reason+".")
}
