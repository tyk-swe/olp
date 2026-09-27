package grants

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// Device authorization bounds. The interval's default and its slow-down step
// are RFC 8628's.
const (
	maxDeviceTTL    = 30 * time.Minute
	maxUserCode     = 64
	maxInterval     = 5 * time.Minute
	defaultInterval = 5 * time.Second
	slowDown        = 5 * time.Second
	// pollLease outlasts a status request, which runs one poll step: another
	// status request polls the enrollment again only once the lease ends,
	// should the request that holds it never record what it found.
	pollLease = time.Minute
)

// Status is where a grant enrollment by device authorization stands.
type Status string

const (
	// Pending: the operator has not approved the device upstream yet.
	Pending Status = "pending"
	// Completed: the operator approved the device, and the grant created a
	// credential version.
	Completed Status = "completed"
	// Denied: the operator denied the device upstream.
	Denied Status = "denied"
	// Expired: the device authorization expired before the operator
	// approved it.
	Expired Status = "expired"
)

// A Standing is what a status request reports about a grant enrollment by
// device authorization.
type Standing struct {
	Status Status
	// Interval is how long to wait before the next status request, while the
	// enrollment is Pending.
	Interval time.Duration
	// Credential is the credential version the grant created, once the
	// enrollment Completed.
	Credential Credential
}

// A Credential is a credential version grant enrollment created.
type Credential struct {
	ID, Principal string
	Version       int
}

// authorizeDevice checks a device authorization the plugin started and makes
// it the enrollment's, which lasts as long as its user code, within
// maxDeviceTTL.
func (e *Enrollment) authorizeDevice(manifest abi.Manifest, device abi.DeviceAuthorization) error {
	switch {
	case !approved(manifest, device.VerificationURL):
		return refused("its verification URL is not at one of the plugin's approved origins")
	case !text(device.UserCode, 1, maxUserCode):
		return refused("its user code is not 1 to 64 bytes without control characters")
	case device.ExpiresIn <= 0:
		return refused("its user code's lifetime is not a positive number of seconds")
	case device.Interval < 0 || device.Interval > seconds(maxInterval):
		return refused("its polling interval is not a number of seconds up to 300")
	}
	e.interval = time.Duration(device.Interval) * time.Second
	if e.interval == 0 {
		e.interval = defaultInterval
	}
	device.Interval = seconds(e.interval)
	if device.ExpiresIn < seconds(maxDeviceTTL) {
		e.ExpiresAt = time.Now().Add(time.Duration(device.ExpiresIn) * time.Second)
	} else {
		e.ExpiresAt = time.Now().Add(maxDeviceTTL)
	}
	e.Device = &device
	return nil
}

// Watch serves a status request for a provider's grant enrollment by device
// authorization, by the principal that started it. When a poll is due, the
// interval having passed since the last one, it claims the poll, so no other
// status request, on any control replica, polls meanwhile, and returns the
// enrollment with its session state to Poll; the caller commits the claim
// before running the plugin. Otherwise it returns where the enrollment stands.
// An enrollment the operator continues with what the upstream returned is not
// found.
func Watch(ctx context.Context, tx pgx.Tx, a *access.Server, providerID, id, principal string) (*Enrollment, Standing, error) {
	e := Enrollment{ID: id, ProviderID: providerID, StartedBy: principal}
	var interval int64
	err := tx.QueryRow(ctx, `UPDATE olp.grant_enrollments SET poll_at=now()+$4::integer*interval '1 second'
		WHERE id=$1 AND provider_id=$2 AND started_by=$3 AND continued_at IS NULL AND expires_at>now() AND poll_at<=now()
		RETURNING slot_id::text,plugin_digest,profile_id,expires_at,poll_interval`, id, providerID, principal, seconds(pollLease)).
		Scan(&e.SlotID, &e.PluginDigest, &e.ProfileID, &e.ExpiresAt, &interval)
	if errors.Is(err, pgx.ErrNoRows) {
		standing, err := stands(ctx, tx, providerID, id, principal)
		return nil, standing, err
	}
	if err != nil {
		return nil, Standing{}, err
	}
	e.interval = time.Duration(interval) * time.Second
	session, err := a.Keys.Read(ctx, tx, a.Installation, id, sessionPurpose)
	if err != nil {
		return nil, Standing{}, err
	}
	e.session = string(session)
	return &e, Standing{}, nil
}

// stands reads where a device authorization stands when no poll is due. One
// whose poll failed was continued, and is refused as such.
func stands(ctx context.Context, q access.Queryer, providerID, id, principal string) (Standing, error) {
	var (
		s                  Standing
		interval           int64
		continued, expired bool
		outcome            string
	)
	err := q.QueryRow(ctx, `SELECT e.poll_interval,e.continued_at IS NOT NULL,e.expires_at<=now(),coalesce(e.outcome,''),
			coalesce(c.id::text,''),coalesce(c.version,0),coalesce(c.principal,'')
		FROM olp.grant_enrollments e LEFT JOIN olp.provider_credentials c ON c.id=e.credential_id
		WHERE e.id=$1 AND e.provider_id=$2 AND e.started_by=$3 AND e.poll_interval IS NOT NULL`, id, providerID, principal).
		Scan(&interval, &continued, &expired, &outcome, &s.Credential.ID, &s.Credential.Version, &s.Credential.Principal)
	switch {
	case err != nil:
		return s, err
	case outcome != "":
		s.Status = Status(outcome)
	case continued:
		return s, used()
	case expired:
		s.Status = Expired
	default:
		s.Status, s.Interval = Pending, time.Duration(interval)*time.Second
	}
	return s, nil
}

// Poll runs one poll step of a device authorization Watch claimed: the plugin
// asks the upstream whether the operator approved the device, on behalf of the
// provider with its option values, reaching the plugin's approved origins
// through client. It returns the grant once the operator approved, checked
// like an exchanged one; until then it fails with the error Settle records.
func Poll(ctx context.Context, rt *plugins.Runtime, q access.Queryer, e Enrollment, options map[string]string, client *http.Client) (abi.Grant, error) {
	var grant abi.Grant
	call := plugins.Call{
		Method:  abi.MethodGrantPoll,
		Params:  abi.GrantPoll{Profile: e.ProfileID, Session: e.session},
		Secrets: []string{e.session},
	}
	manifest, err := step(ctx, rt, q, e, options, client, call, &grant)
	if err != nil {
		return grant, err
	}
	return grant, fits(manifest, e.ProfileID, &grant)
}

// Settle records why a poll step Watch claimed obtained no grant, or why its
// grant could not be staged, and returns where the enrollment stands: Pending,
// polled again after the interval, which grows by 5 seconds when the upstream
// asked to slow down; Denied; or Expired. Any other failure ends the
// enrollment and is the problem Settle returns. An enrollment cancelled
// meanwhile is not found.
func (e Enrollment) Settle(ctx context.Context, db *pgxpool.Pool, failed error) (Standing, error) {
	var code string
	if reported, ok := errors.AsType[*abi.Error](failed); ok {
		code = reported.Code
	}
	switch code {
	case abi.CodeAuthorizationPending, abi.CodeSlowDown:
		if code == abi.CodeSlowDown {
			e.interval += slowDown
		}
		err := e.settle(ctx, db, "poll_interval=$2::integer,poll_at=now()+$2::integer*interval '1 second'", false, seconds(e.interval))
		return Standing{Status: Pending, Interval: e.interval}, err
	case abi.CodeAccessDenied:
		return Standing{Status: Denied}, e.settle(ctx, db, "continued_at=now(),outcome='denied'", true)
	case abi.CodeExpiredToken:
		return Standing{Status: Expired}, e.settle(ctx, db, "expires_at=least(expires_at,now())", true)
	}
	if err := e.settle(ctx, db, "continued_at=now()", true); err != nil {
		return Standing{}, err
	}
	return Standing{}, failure(failed)
}

// settle updates a pending device authorization, deleting its session state
// once it has ended.
func (e Enrollment) settle(ctx context.Context, db *pgxpool.Pool, set string, ended bool, args ...any) error {
	tag, err := db.Exec(ctx, "UPDATE olp.grant_enrollments SET "+set+" WHERE id=$1 AND continued_at IS NULL", append([]any{e.ID}, args...)...)
	switch {
	case err != nil:
		return err
	case tag.RowsAffected() == 0:
		return pgx.ErrNoRows
	case !ended:
		return nil
	}
	_, err = db.Exec(ctx, "DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2", e.ID, sessionPurpose)
	return err
}

func seconds(d time.Duration) int64 { return int64(d / time.Second) }
