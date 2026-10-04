package grants

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

const (
	// refreshEvery is how often a worker looks for grants due a refresh, so a
	// grant whose access token the upstream refused is refreshed within it.
	refreshEvery = 5 * time.Second
	// maxRefreshLead bounds how long before its access token expires a grant
	// is refreshed: a quarter of the token's lifetime, at most this.
	maxRefreshLead = 10 * time.Minute
	// A failed refresh is retried after refreshBackoff, doubled for each
	// failure since the last refresh that succeeded, up to maxRefreshBackoff.
	refreshBackoff    = 30 * time.Second
	maxRefreshBackoff = 10 * time.Minute
	// refreshTimeout bounds one grant's refresh, loading its plugin included.
	refreshTimeout = time.Minute
	// refreshesPerPass bounds the grants one pass refreshes.
	refreshesPerPass = 100
	// maxRefreshFailure bounds the reason a failed refresh records.
	maxRefreshFailure = 1024
	// Recording a refreshed grant is attempted up to storeAttempts times,
	// each within storeTimeout, storeBackoff apart and doubling.
	storeAttempts = 5
	storeTimeout  = 10 * time.Second
	storeBackoff  = 250 * time.Millisecond
	// An abandoned attempt is recoverable only by lapsing the grant, never
	// by reusing its token. Allow the refresh and every store retry to finish,
	// with ten seconds for the retries' backoffs and scheduling.
	refreshAttemptLifetime = refreshTimeout + storeAttempts*storeTimeout + 10*time.Second
)

// refreshLockSeed keys the advisory lock that one grant's refresh holds.
const refreshLockSeed = int64(0x4f4c505f47524e54)

// errAnotherAccount marks a refresh that renewed another upstream account's
// authorization than the grant's.
var errAnotherAccount = errors.New("the refresh authorizes another account than the grant's")

var errRefreshInterrupted = errors.New("the previous refresh's outcome was lost; its refresh token cannot be reused")

var errRefreshNotClaimed = errors.New("the grant no longer accepts a refresh attempt")

// A Refresher refreshes grants ahead of their access tokens' expiry through
// their plugins, as a worker task that every worker replica runs. A grant's
// refresh holds a Postgres advisory lock and commits an attempt before
// dispatch, so even losing that session cannot authorize another use of the
// same rotating token. The new access token rewrites the secret of the grant's
// credential version and advances the grant's generation, which gateways poll;
// the refresh token stays under secrets.ProviderGrantRefresh. A refresh that
// fails permanently lapses the grant, and a grant that no configuration uses
// any more is retired rather than refreshed.
type Refresher struct {
	Pool         *pgxpool.Pool
	Keys         *secrets.KeyRing
	Installation string
	// Plugins runs each grant's plugin.
	Plugins *plugins.Host
	// Egress is the policy of every provider's network path.
	Egress *egress.Policy
	Log    *slog.Logger
}

// Run refreshes the grants due a refresh every few seconds until ctx ends,
// checkpointing each pass as the grant_refresh worker task.
func (r *Refresher) Run(ctx context.Context) {
	ticker := time.NewTicker(refreshEvery)
	defer ticker.Stop()
	for ctx.Err() == nil {
		outcome, progress := r.Pass(ctx)
		if ctx.Err() != nil {
			return
		}
		if err := usage.CheckpointTask(ctx, r.Pool, usage.TaskGrantRefresh, outcome, progress); err != nil {
			r.Log.Warn("grant refresh checkpoint failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Pass refreshes the grants due a refresh now, or retires those no
// configuration uses. A grant another worker is refreshing is left to it. A
// refresh that fails is recorded on its grant and retried with backoff only
// when its token is safe to reuse. An ambiguous attempt stays fenced until
// its recovery deadline, and a permanent failure lapses the grant. The pass
// fails only when it can't read or record grants. It reports whether it
// refreshed or retired a grant or recorded a failure.
func (r *Refresher) Pass(ctx context.Context) (usage.Outcome, bool) {
	due, err := r.due(ctx)
	if err != nil {
		r.Log.Warn("grants due a refresh could not be read", "error", err)
		return usage.OutcomeFailure, false
	}
	if len(due) == 0 {
		return usage.OutcomeSuccess, false
	}
	// A grant's lock is a session lock, so no transaction stays open while
	// its plugin reaches the upstream. Closing the connection releases any
	// lock it still holds.
	resource, err := r.Pool.Acquire(ctx)
	if err != nil {
		r.Log.Warn("grant refresh connection unavailable", "error", err)
		return usage.OutcomeFailure, false
	}
	conn := resource.Hijack()
	defer conn.Close(context.WithoutCancel(ctx))
	progress := false
	for _, credentialID := range due {
		refreshed, err := r.refresh(ctx, conn, credentialID)
		progress = progress || refreshed
		if err != nil {
			if ctx.Err() == nil {
				r.Log.Warn("grant refresh could not be recorded", "credential_id", credentialID, "error", err)
			}
			return usage.OutcomeFailure, progress
		}
	}
	return usage.OutcomeSuccess, progress
}

// due returns the credential versions whose grants are due a refresh, the
// longest due first, and those no configuration uses, which are retired
// independently of when their next refresh would have been: a grant whose
// access token carries no expiry is never due at all. In-flight attempts are
// left alone until their recovery deadline, even if their lock session ends.
func (r *Refresher) due(ctx context.Context) ([]string, error) {
	rows, err := r.Pool.Query(ctx, `SELECT g.credential_id::text FROM olp.provider_grants g
		JOIN olp.provider_credentials c ON c.id=g.credential_id
		WHERE `+dueCondition+`
		ORDER BY g.refresh_at LIMIT $1`, refreshesPerPass)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// refresh refreshes one grant under its advisory lock, unless another worker
// holds the lock. It reports whether it refreshed the grant or recorded a
// failure.
func (r *Refresher) refresh(ctx context.Context, conn *pgx.Conn, credentialID string) (bool, error) {
	var held bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtextextended($1, $2))", credentialID, refreshLockSeed).Scan(&held); err != nil || !held {
		return false, err
	}
	refreshed, err := r.refreshLocked(ctx, conn, credentialID)
	if _, unlock := conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock(hashtextextended($1, $2))", credentialID, refreshLockSeed); unlock != nil {
		err = errors.Join(err, unlock)
	}
	return refreshed, err
}

// using is the configuration a grant refreshes on behalf of, as a column of a
// query over the grant's credential version c: one that pins the plugin build
// that enrolled the grant and selects the version in one of its credential
// slots, the provider's active revision before its draft. It is NULL when no
// configuration or retained resource uses the grant, which is then retired.
const using = `coalesce(
	(SELECT r.configuration FROM olp.providers p JOIN olp.provider_revisions r ON r.id=p.active_revision_id
		WHERE p.id=c.provider_id AND r.configuration->>'profile_revision'=c.plugin_digest AND r.configuration->>'profile_id'=c.profile_id
		AND r.slots @> jsonb_build_array(jsonb_build_object('credential_id',c.id))),
	(SELECT p.configuration FROM olp.providers p WHERE p.id=c.provider_id AND p.configuration->>'profile_revision'=c.plugin_digest AND p.configuration->>'profile_id'=c.profile_id
		AND EXISTS (SELECT 1 FROM olp.provider_slots s WHERE s.provider_id=p.id AND s.credential_id=c.id)),
	(SELECT r.configuration FROM olp.provider_resources x
		JOIN olp.provider_revisions r ON r.id=x.provider_revision_id AND r.provider_id=x.provider_id
		WHERE x.provider_id=c.provider_id AND x.credential_id=c.id
		AND (x.expires_at IS NULL OR x.expires_at>now())
		AND r.configuration->>'profile_revision'=c.plugin_digest AND r.configuration->>'profile_id'=c.profile_id
		AND r.slots @> jsonb_build_array(jsonb_build_object('id',x.slot_id,'credential_id',c.id))
		ORDER BY x.created_at DESC,x.id DESC LIMIT 1))`

// dueCondition is what makes a grant g, over its credential version c, due: it
// holds a refresh token, its credential version is not revoked, and it is
// either due a refresh or unused by any configuration (using) and not
// mid-attempt.
const dueCondition = `g.refresh_token_id IS NOT NULL AND c.revoked_at IS NULL
	AND (g.refresh_at<=now() OR g.refresh_attempt_id IS NULL AND ` + using + ` IS NULL)`

// dueGrant is what refreshing a grant needs: its credential version's
// provider, plugin, principal and facts, its refresh token's secret, and the
// configuration it refreshes on behalf of.
type dueGrant struct {
	credentialID, providerID, digest, principal string
	facts                                       map[string]string
	refreshTokenID                              string
	refreshAttemptID                            *string
	failures                                    int
	configuration                               refreshConfiguration
}

// refreshConfiguration is what a grant's refresh reads of its provider's
// configuration: the profile, the option values and the network path.
type refreshConfiguration struct {
	ProfileID string `json:"profile_id"`
	Options   struct {
		Network       *egress.ConnectionOptions `json:"network"`
		PluginOptions map[string]string         `json:"plugin_options"`
	} `json:"options"`
}

// refreshLocked refreshes a grant this connection holds the lock of, if it
// is still due or still unused: a worker that refreshed or retired it
// meanwhile scheduled its next refresh or ended it. It records the outcome
// on the grant. A grant no configuration uses is retired instead.
func (r *Refresher) refreshLocked(ctx context.Context, conn *pgx.Conn, credentialID string) (bool, error) {
	g := dueGrant{credentialID: credentialID}
	var configuration *refreshConfiguration
	err := conn.QueryRow(ctx, `SELECT c.provider_id::text,c.plugin_digest,c.principal,c.grant_facts,g.refresh_token_id::text,g.refresh_failures,g.refresh_attempt_id::text,`+using+`
		FROM olp.provider_grants g JOIN olp.provider_credentials c ON c.id=g.credential_id
		WHERE g.credential_id=$1 AND `+dueCondition, credentialID).
		Scan(&g.providerID, &g.digest, &g.principal, &g.facts, &g.refreshTokenID, &g.failures, &g.refreshAttemptID, &configuration)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if configuration == nil {
		retired, err := endGrant(ctx, conn, &g, retire)
		if retired {
			r.Log.Info("grant retired: no configuration uses it", "provider_id", g.providerID, "credential_id", g.credentialID)
		}
		return retired, err
	}
	if g.refreshAttemptID != nil {
		return true, r.fail(ctx, conn, &g, errRefreshInterrupted)
	}
	g.configuration = *configuration
	step, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	grant, err := r.run(step, conn, &g)
	if err != nil {
		if errors.Is(err, errRefreshNotClaimed) {
			return false, nil
		}
		// Keep known-unsent evidence even when shutdown cancelled dispatch,
		// so the token stays retryable. Ambiguous outcomes remain fenced.
		record, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
		defer cancel()
		return true, r.fail(record, conn, &g, err)
	}
	return true, r.store(ctx, &g, grant)
}

// run runs the grant's refresh through its plugin, on behalf of the provider
// and over its network path, and checks what the refresh returns.
func (r *Refresher) run(ctx context.Context, conn *pgx.Conn, g *dueGrant) (abi.Grant, error) {
	token, err := r.Keys.Read(ctx, conn, r.Installation, g.refreshTokenID, secrets.ProviderGrantRefresh)
	if err != nil {
		return abi.Grant{}, fmt.Errorf("the refresh token is unreadable: %w", err)
	}
	client, err := r.client(ctx, conn, g)
	if err != nil {
		return abi.Grant{}, err
	}
	defer client.CloseIdleConnections()
	// The advisory lock only serializes live database sessions. This claim
	// survives session loss and worker exit, and fences every completion.
	g.refreshAttemptID = new(access.NewID())
	tag, err := conn.Exec(ctx, `UPDATE olp.provider_grants SET refresh_attempt_id=$3,
		refresh_at=now()+$4*interval '1 second',updated_at=now()
		WHERE credential_id=$1 AND refresh_token_id=$2 AND refresh_attempt_id IS NULL`,
		g.credentialID, g.refreshTokenID, g.refreshAttemptID, int(refreshAttemptLifetime/time.Second))
	if err != nil {
		return abi.Grant{}, err
	}
	if tag.RowsAffected() == 0 {
		return abi.Grant{}, errRefreshNotClaimed
	}
	profile := g.configuration.ProfileID
	provider := abi.Provider{Profile: profile, Options: g.configuration.Options.PluginOptions}
	grant, err := r.Plugins.RefreshGrant(ctx, g.digest, provider, abi.GrantRefresh{Profile: profile, RefreshToken: string(token), Facts: g.facts}, client, []string{string(token)})
	if err != nil {
		return grant, err
	}
	return grant, checkRefreshed(g, grant)
}

// client returns an HTTP client over the provider's network path, under the
// egress policy, with the network credential gateways would use.
func (r *Refresher) client(ctx context.Context, conn *pgx.Conn, g *dueGrant) (*http.Client, error) {
	network := g.configuration.Options.Network
	var secret []byte
	if network != nil && network.CredentialID != "" {
		var err error
		if secret, err = runtime.ReadNetworkSecret(ctx, conn, r.Keys, r.Installation, g.providerID, network.CredentialID); err != nil {
			return nil, fmt.Errorf("the provider's network credential is unavailable: %w", err)
		}
	}
	return r.Egress.ConnectionClient(network, secret, refreshTimeout)
}

// checkRefreshed checks what a plugin's refresh returned: an access token OLP
// can hold and place, for the grant's own account. A refresh may leave out the
// principal and facts, but any it reports are the grant's.
func checkRefreshed(g *dueGrant, grant abi.Grant) error {
	if err := validateTokens(grant); err != nil {
		return fmt.Errorf("the plugin's refresh returned something OLP can't use: %w", err)
	}
	switch {
	case grant.Principal != "" && grant.Principal != g.principal:
		return fmt.Errorf("%w: it observed another principal", errAnotherAccount)
	case len(grant.Facts) > 0 && !maps.Equal(grant.Facts, g.facts):
		return fmt.Errorf("%w: it reported other grant facts", errAnotherAccount)
	}
	return nil
}

// store holds a refreshed grant: its credential version's secret serves the
// new access token with the grant's facts, a rotated refresh token replaces
// the spent one, and the grant's generation advances, so gateways reload it
// on their next poll. A grant that ended during its refresh, such as one an
// operator revoked, keeps nothing of it.
//
// The upstream may have spent the grant's refresh token already, so a refresh
// whose outcome is lost lapses the grant after its attempt deadline. Recording it
// therefore outlasts the pass's cancellation, such as a worker's shutdown, and
// is retried a few times over a new database connection each, should the
// database or the pass's connection fail.
func (r *Refresher) store(ctx context.Context, g *dueGrant, grant abi.Grant) error {
	ctx = context.WithoutCancel(ctx)
	served, err := json.Marshal(connectors.GrantCredential{AccessToken: grant.AccessToken, Facts: g.facts})
	if err != nil {
		return err
	}
	expires, refreshAt := schedule(time.Now(), grant.ExpiresIn, true)
	var generation int64
	for attempt, wait := 1, storeBackoff; ; attempt, wait = attempt+1, 2*wait {
		generation, err = r.write(ctx, g, served, grant.RefreshToken, expires, refreshAt)
		if err == nil || errors.Is(err, pgx.ErrNoRows) || attempt == storeAttempts {
			break
		}
		r.Log.Warn("refreshed grant not recorded yet", "provider_id", g.providerID, "credential_id", g.credentialID, "attempt", attempt, "error", err)
		time.Sleep(wait)
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		r.Log.Info("grant ended during its refresh", "provider_id", g.providerID, "credential_id", g.credentialID)
		return nil
	case err != nil:
		r.Log.Error("refreshed grant lost: the upstream's new tokens were not recorded, so the grant will lapse after its attempt deadline",
			"provider_id", g.providerID, "credential_id", g.credentialID, "error", err)
		return err
	}
	r.Log.Info("grant refreshed", "provider_id", g.providerID, "credential_id", g.credentialID, "generation", generation)
	return nil
}

// write records a refreshed grant in one transaction, and returns the grant's
// new generation. It fails with pgx.ErrNoRows when the grant no longer holds
// the attempt that dispatched its refresh token.
func (r *Refresher) write(ctx context.Context, g *dueGrant, served []byte, refreshToken string, expires, refreshAt *time.Time) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	var generation int64
	err := pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		// Writing a secret holds the installation row, which transactions
		// that end grants take before the grant's.
		if err := r.Keys.Store(ctx, tx, r.Installation, g.credentialID, secrets.ProviderCredential, served, nil); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `UPDATE olp.provider_grants SET generation=generation+1,expires_at=$3,refresh_at=$4,
			refresh_attempt_id=NULL,refresh_failures=0,refresh_failure=NULL,updated_at=now()
			WHERE credential_id=$1 AND refresh_token_id=$2 AND refresh_attempt_id=$5 RETURNING generation`,
			g.credentialID, g.refreshTokenID, expires, refreshAt, g.refreshAttemptID).Scan(&generation); err != nil {
			return err
		}
		if refreshToken == "" {
			return nil
		}
		return r.Keys.Store(ctx, tx, r.Installation, g.refreshTokenID, secrets.ProviderGrantRefresh, []byte(refreshToken), nil)
	})
	return generation, err
}

// fail records a failed refresh on its grant. A transient failure is retried
// after a backoff only when OLP knows the token was not spent. Otherwise the
// attempt stays fenced until its recovery deadline. A permanent failure
// lapses the grant.
func (r *Refresher) fail(ctx context.Context, conn *pgx.Conn, g *dueGrant, failure error) error {
	reason := clip(failure.Error(), maxRefreshFailure)
	if !permanent(failure) {
		if g.refreshAttemptID != nil && !errors.Is(failure, connectors.ErrNotSent) {
			r.Log.Warn("grant refresh outcome unknown; its token stays fenced until the attempt deadline", "provider_id", g.providerID, "credential_id", g.credentialID, "reason", reason)
			_, err := conn.Exec(ctx, `UPDATE olp.provider_grants SET refresh_failures=refresh_failures+1,refresh_failure=$3,updated_at=now()
				WHERE credential_id=$1 AND refresh_token_id=$2 AND refresh_attempt_id=$4`,
				g.credentialID, g.refreshTokenID, reason, g.refreshAttemptID)
			return err
		}
		retry := time.Now().Add(backoff(g.failures + 1))
		r.Log.Warn("grant refresh failed", "provider_id", g.providerID, "credential_id", g.credentialID, "retry_at", retry, "reason", reason)
		_, err := conn.Exec(ctx, `UPDATE olp.provider_grants SET refresh_at=$3,refresh_attempt_id=NULL,
			refresh_failures=refresh_failures+1,refresh_failure=$4,updated_at=now()
			WHERE credential_id=$1 AND refresh_token_id=$2 AND refresh_attempt_id IS NOT DISTINCT FROM $5::uuid`,
			g.credentialID, g.refreshTokenID, retry, reason, g.refreshAttemptID)
		return err
	}
	lapsed, err := endGrant(ctx, conn, g, func(ctx context.Context, tx pgx.Tx, g *dueGrant) (bool, error) { return lapse(ctx, tx, g, reason) })
	if lapsed {
		r.Log.Warn("grant lapsed", "provider_id", g.providerID, "credential_id", g.credentialID, "reason", reason)
	}
	return err
}

// endGrant runs end, which ends a due grant, lapsing or retiring it, in a
// transaction on conn, and reports whether the grant ended.
func endGrant(ctx context.Context, conn *pgx.Conn, g *dueGrant, end func(context.Context, pgx.Tx, *dueGrant) (bool, error)) (bool, error) {
	var done bool
	err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) (err error) {
		done, err = end(ctx, tx, g)
		return err
	})
	return done && err == nil, err
}

// backoff is how long a grant's refresh waits after its failures-th failure
// in a row.
func backoff(failures int) time.Duration {
	if failures > 10 {
		return maxRefreshBackoff
	}
	return min(refreshBackoff<<(failures-1), maxRefreshBackoff)
}

// schedule returns when an access token that lasts expiresIn seconds from now
// expires, and when a refreshable grant holding it is refreshed: a quarter of
// the token's lifetime before it expires, at most maxRefreshLead before. When
// the upstream did not say how long the token lasts, or the grant can't be
// refreshed, no refresh is scheduled; a gateway that sees the upstream refuse
// the token requests one.
func schedule(now time.Time, expiresIn int64, refreshable bool) (expires, refresh *time.Time) {
	if expiresIn <= 0 {
		return nil, nil
	}
	lifetime := time.Duration(expiresIn) * time.Second
	expires = new(now.Add(lifetime))
	if refreshable {
		refresh = new(expires.Add(-min(lifetime/4, maxRefreshLead)))
	}
	return expires, refresh
}

// clip cuts text to valid UTF-8 of at most limit bytes, on a character
// boundary.
func clip(text string, limit int) string {
	text = strings.ToValidUTF8(text, "")
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}
