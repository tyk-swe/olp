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
// pastes back what the upstream returned within it.
const SessionTTL = 10 * time.Minute

// sessionPurpose is the secret purpose of an enrollment's session state.
const sessionPurpose = "grant_enrollment"

// maxSession bounds the state a plugin carries between enrollment steps.
const maxSession = 16 << 10

// An Enrollment is a grant enrollment in progress: an operator's sign-in to an
// upstream account, through the plugin profile a provider pins, for one of the
// provider's credential slots. It is persisted with its session state
// encrypted, so any control replica can continue it, and it is continued once.
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
	// session is the plugin's state for its next step.
	session string
}

// Start runs the plugin's first grant enrollment step for an enrollment that
// names its provider, slot, plugin profile and principal, on behalf of the
// provider with its option values, and returns it ready to Save, with the
// authorization URL the operator opens. The step reaches the plugin's approved
// origins through client, the provider's network path.
func Start(ctx context.Context, rt *plugins.Runtime, q access.Queryer, e Enrollment, options map[string]string, client *http.Client) (Enrollment, error) {
	var authorization abi.GrantAuthorization
	manifest, err := step(ctx, rt, q, e, options, client, plugins.Call{Method: abi.MethodGrantStart, Params: abi.GrantStart{Profile: e.ProfileID}}, &authorization)
	if err != nil {
		return e, failure(err)
	}
	target, err := url.Parse(authorization.URL)
	if len(authorization.URL) > 8<<10 || err != nil || (target.Scheme != "https" && target.Scheme != "http") || !slices.Contains(manifest.Origins, plugins.Origin(target)) {
		return e, refused("its authorization request is not a URL at one of the plugin's approved origins")
	}
	if len(authorization.Session) > maxSession {
		return e, refused("its session state exceeds 16 KiB")
	}
	e.ID, e.ExpiresAt = access.NewID(), time.Now().Add(SessionTTL)
	e.AuthorizationURL, e.session = authorization.URL, authorization.Session
	return e, nil
}

// Save persists a started enrollment, its session state encrypted until the
// enrollment expires.
func (e Enrollment) Save(ctx context.Context, tx pgx.Tx, a *access.Server) error {
	if err := a.Keys.Store(ctx, tx, a.Installation, e.ID, sessionPurpose, []byte(e.session), &e.ExpiresAt); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "INSERT INTO olp.grant_enrollments(id,provider_id,slot_id,plugin_digest,profile_id,started_by,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7)",
		e.ID, e.ProviderID, e.SlotID, e.PluginDigest, e.ProfileID, e.StartedBy, e.ExpiresAt)
	return err
}

// Claim takes a provider's enrollment for the principal that started it, to
// continue it: it can be claimed once, before it expires, and its session
// state is deleted as it is read. The caller commits the claim before running
// the plugin, so no other replica can continue the enrollment meanwhile.
func Claim(ctx context.Context, tx pgx.Tx, a *access.Server, providerID, id, principal string) (Enrollment, error) {
	e := Enrollment{ID: id, ProviderID: providerID, StartedBy: principal}
	err := tx.QueryRow(ctx, `UPDATE olp.grant_enrollments SET continued_at=now()
		WHERE id=$1 AND provider_id=$2 AND started_by=$3 AND continued_at IS NULL AND expires_at>now()
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

// unavailable explains why an enrollment can no longer be continued. Another
// principal's enrollment is not found, like one purged after it expired.
func unavailable(ctx context.Context, q access.Queryer, providerID, id, principal string) error {
	var continued bool
	err := q.QueryRow(ctx, "SELECT continued_at IS NOT NULL FROM olp.grant_enrollments WHERE id=$1 AND provider_id=$2 AND started_by=$3", id, providerID, principal).Scan(&continued)
	switch {
	case err != nil:
		return err
	case continued:
		return access.Fail(409, "grant_enrollment_used", "This grant enrollment was already continued, and a grant enrollment is continued once. Start another.")
	}
	return access.Fail(410, "grant_enrollment_expired", "This grant enrollment expired. Start another, and paste back what the upstream returns within 10 minutes.")
}

// Exchange runs the plugin's step that exchanges what the operator pasted
// back for a grant, on behalf of the provider with its option values, and
// checks that the grant is one the profile's hosting adaptation can place. The
// step reaches the plugin's approved origins through client.
func Exchange(ctx context.Context, rt *plugins.Runtime, q access.Queryer, e Enrollment, input string, options map[string]string, client *http.Client) (abi.Grant, error) {
	var grant abi.Grant
	call := plugins.Call{
		Method:  abi.MethodGrantExchange,
		Params:  abi.GrantExchange{Profile: e.ProfileID, Session: e.session, Input: input},
		Secrets: []string{e.session, input},
	}
	manifest, err := step(ctx, rt, q, e, options, client, call, &grant)
	if err != nil {
		return grant, failure(err)
	}
	i := slices.IndexFunc(manifest.Profiles, func(p abi.Profile) bool { return p.ID == e.ProfileID })
	if i < 0 || manifest.Profiles[i].Grant == nil {
		return grant, refused("the plugin declares no profile " + e.ProfileID + " that authenticates with a grant")
	}
	if err = validate(manifest.Profiles[i].Grant, &grant); err != nil {
		return grant, refused(err.Error())
	}
	return grant, nil
}

// step runs one grant step of an enrollment on its usable plugin, on behalf of
// the enrollment's provider with its option values, granting it HTTP to the
// plugin's approved origins through client, and returns the plugin's manifest.
func step(ctx context.Context, rt *plugins.Runtime, q access.Queryer, e Enrollment, options map[string]string, client *http.Client, call plugins.Call, result any) (abi.Manifest, error) {
	manifest, module, err := plugins.Usable(ctx, q, e.PluginDigest)
	if err != nil {
		return manifest, err
	}
	loaded, err := rt.Load(ctx, module)
	if err != nil {
		return manifest, err
	}
	defer loaded.Close(context.WithoutCancel(ctx))
	call.Provider = &abi.Provider{Profile: e.ProfileID, Options: options}
	call.HTTP = &plugins.HTTP{Origins: manifest.Origins, Client: client}
	return manifest, loaded.Call(ctx, call, result)
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
