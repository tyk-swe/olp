//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/grants"
	"github.com/tyk-swe/olp/internal/grants/personal"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/platform/secretstore"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/plugin/abi"
	"github.com/tyk-swe/olp/tests/harness"
	"github.com/tyk-swe/olp/tests/harness/vendor"
)

type stubExternalSecrets struct {
	mu      sync.Mutex
	resolve func(context.Context, secretstore.Reference) ([]byte, error)
}

func (s *stubExternalSecrets) set(fn func(context.Context, secretstore.Reference) ([]byte, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolve = fn
}

func (s *stubExternalSecrets) Resolve(ctx context.Context, ref secretstore.Reference) ([]byte, error) {
	s.mu.Lock()
	fn := s.resolve
	s.mu.Unlock()
	return fn(ctx, ref)
}

type providerAPITestEnv struct {
	pool    *pgxpool.Pool
	client  *managementClient
	server  *providers.Server
	ownerID string
}

func newProviderAPITestEnv(t *testing.T, external *stubExternalSecrets) providerAPITestEnv {
	t.Helper()
	ctx := t.Context()
	pool, _ := harness.Database(t)
	ownerID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO olp.users(id,email,display_name,role,etag) VALUES($1::uuid,$1::text||'@example.com','Owner','owner',$1::uuid)`, ownerID); err != nil {
		t.Fatal(err)
	}
	policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	var server *providers.Server
	client := managementHTTPAs(t, pool, ownerID, func(mux *http.ServeMux, accessServer *access.Server) {
		deps := providers.Deps{
			Access:    accessServer,
			Egress:    &policy,
			Getenv:    func(string) string { return "" },
			Activated: func(context.Context, pgx.Tx, string, string) error { return nil },
			Log:       slog.New(slog.DiscardHandler),
		}
		if external != nil {
			deps.ExternalSecrets = external
		}
		server = providers.New(deps)
		server.Register(mux)
	})
	return providerAPITestEnv{pool: pool, client: client, server: server, ownerID: ownerID}
}

func (e providerAPITestEnv) call(t *testing.T, method, path, body, ifMatch, idempotencyKey string) apiResult {
	t.Helper()
	quoted := ""
	if ifMatch != "" {
		quoted = `"` + ifMatch + `"`
	}
	if idempotencyKey != "" {
		return e.client.call(t, method, path, quoted, body, idempotencyKey)
	}
	return e.client.call(t, method, path, quoted, body)
}

func assertInstallationLockFree(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin lock check: %w", err)
	}
	defer tx.Rollback(ctx)
	var id string
	if err := tx.QueryRow(ctx, "SELECT id::text FROM olp.installation WHERE singleton FOR UPDATE NOWAIT").Scan(&id); err != nil {
		return fmt.Errorf("installation lock was held during external resolution: %w", err)
	}
	return nil
}

func TestProviderExternalSecretActivationRunsWithoutInstallationLock(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	upstream := httptest.NewServer(vendor.Handler())
	t.Cleanup(upstream.Close)

	external := &stubExternalSecrets{}
	env := newProviderAPITestEnv(t, external)
	providerID, slotID, modelID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	initialETag := uuid.NewString()
	cfgJSON := `{"kind":"openai_compatible","auth_mode":"api_key","endpoint":` + strconv.Quote(upstream.URL+"/v1") + `}`
	if _, err := env.pool.Exec(ctx, `INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by) VALUES($1,'external-fixture','openai_compatible','draft',$2,$3,$3,$4)`,
		providerID, cfgJSON, initialETag, env.ownerID); err != nil {
		t.Fatal(err)
	}
	ref := secretstore.Reference{Store: "gcp", SecretID: "projects/test-project/secrets/provider-key", Version: "1"}
	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	credID, _, err := env.server.StoreExternalCredential(ctx, tx, providerID, ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(ctx, `INSERT INTO olp.provider_slots(id,provider_id,is_default,position,name,enabled,priority,weight,credential_id,restrictions,limits) VALUES($1,$2,true,0,'default',true,0,1,$3,'{}','{}')`,
		slotID, providerID, credID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(ctx, `INSERT INTO olp.provider_models(id,provider_id,upstream_model,display_name,enabled,capabilities) VALUES($1,$2,$3,'Fixture Model',true,'[{"operation":"generation","surface":"openai","mode":"unary","source":"declared"}]')`,
		modelID, providerID, vendor.Model); err != nil {
		t.Fatal(err)
	}

	var probeCalls atomic.Int32
	external.set(func(resolveCtx context.Context, got secretstore.Reference) ([]byte, error) {
		probeCalls.Add(1)
		if got != ref {
			return nil, fmt.Errorf("unexpected reference %+v", got)
		}
		if err := assertInstallationLockFree(resolveCtx, env.pool); err != nil {
			return nil, err
		}
		return []byte(vendor.Secret), nil
	})
	probeRes := env.call(t, http.MethodPost, "/api/v1/providers/"+providerID+"/probe", "", initialETag, "")
	if probeRes.status != http.StatusOK {
		t.Fatalf("probe HTTP %d: %s", probeRes.status, probeRes.body)
	}
	if probeCalls.Load() != 1 {
		t.Fatalf("probe external resolve calls = %d, want 1", probeCalls.Load())
	}

	certRes := env.call(t, http.MethodPost, "/api/v1/providers/"+providerID+"/models/"+modelID+"/certify", "", initialETag, "")
	if certRes.status != http.StatusOK {
		t.Fatalf("certify HTTP %d: %s", certRes.status, certRes.body)
	}
	var certified struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(certRes.body, &certified); err != nil || certified.Status != "certified" {
		t.Fatalf("certify body = %s, want status certified", certRes.body)
	}
	currentETag := func() string {
		t.Helper()
		var etag string
		if err := env.pool.QueryRow(ctx, `SELECT etag::text FROM olp.providers WHERE id=$1`, providerID).Scan(&etag); err != nil {
			t.Fatal(err)
		}
		return etag
	}

	etagBeforeConflict := currentETag()
	external.set(func(resolveCtx context.Context, got secretstore.Reference) ([]byte, error) {
		if err := assertInstallationLockFree(resolveCtx, env.pool); err != nil {
			return nil, err
		}
		if _, err := env.pool.Exec(resolveCtx, `UPDATE olp.providers SET etag=$2 WHERE id=$1`, providerID, uuid.NewString()); err != nil {
			return nil, err
		}
		return []byte(vendor.Secret), nil
	})
	conflictRes := env.call(t, http.MethodPost, "/api/v1/providers/"+providerID+"/activate", "", etagBeforeConflict, uuid.NewString())
	if conflictRes.status != http.StatusPreconditionFailed || !strings.Contains(string(conflictRes.body), "/etag_mismatch") {
		t.Fatalf("concurrent edit during activate returned HTTP %d: %s, want 412 etag_mismatch", conflictRes.status, conflictRes.body)
	}
	var state string
	if err := env.pool.QueryRow(ctx, `SELECT state FROM olp.providers WHERE id=$1`, providerID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "draft" {
		t.Fatalf("provider state after conflicted activate = %q, want draft", state)
	}

	var activateCalls atomic.Int32
	external.set(func(resolveCtx context.Context, got secretstore.Reference) ([]byte, error) {
		activateCalls.Add(1)
		if err := assertInstallationLockFree(resolveCtx, env.pool); err != nil {
			return nil, err
		}
		return []byte(vendor.Secret), nil
	})
	activateKey := uuid.NewString()
	etagBeforeActivate := currentETag()
	first := env.call(t, http.MethodPost, "/api/v1/providers/"+providerID+"/activate", "", etagBeforeActivate, activateKey)
	if first.status != http.StatusOK {
		t.Fatalf("activate HTTP %d: %s", first.status, first.body)
	}
	if activateCalls.Load() != 1 {
		t.Fatalf("activate external resolve calls = %d, want 1", activateCalls.Load())
	}
	var activated struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(first.body, &activated); err != nil || activated.State != "active" {
		t.Fatalf("activate body = %s, want active", first.body)
	}
	replay := env.call(t, http.MethodPost, "/api/v1/providers/"+providerID+"/activate", "", etagBeforeActivate, activateKey)
	if replay.status != http.StatusOK || string(replay.body) != string(first.body) {
		t.Fatalf("activate replay HTTP %d %s, want 200 %s", replay.status, replay.body, first.body)
	}
	if activateCalls.Load() != 1 {
		t.Fatalf("activate replay invoked external Resolve again: calls = %d, want 1", activateCalls.Load())
	}
}

func TestProviderExpiredEnrollmentPollSettlesAtomicallyAndRejectsRevokedSession(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	env := newProviderAPITestEnv(t, nil)
	providerID, slotID := uuid.NewString(), uuid.NewString()
	etag := uuid.NewString()
	if _, err := env.pool.Exec(ctx, `INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by) VALUES($1,'enrollment-fixture','openai_compatible','draft',$2,$3,$3,$4)`,
		providerID, providerEndpoint, etag, env.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(ctx, `INSERT INTO olp.provider_slots(id,provider_id,is_default,position,name,enabled,priority,weight,restrictions,limits) VALUES($1,$2,true,0,'default',true,0,1,'{}','{}')`,
		slotID, providerID); err != nil {
		t.Fatal(err)
	}
	insertExpired := func() string {
		t.Helper()
		id := uuid.NewString()
		if _, err := env.pool.Exec(ctx, `INSERT INTO olp.secrets(id,purpose,key_version,ciphertext,expires_at) VALUES($1,'grant_enrollment',1,'\x00',now()+interval '1 hour')`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := env.pool.Exec(ctx, `INSERT INTO olp.grant_enrollments(id,provider_id,slot_id,plugin_digest,profile_id,started_by,expires_at,poll_interval,poll_at)
			VALUES($1,$2,$3,repeat('0',64),'fixture-profile',$4,now()-interval '1 minute',5,now()-interval '1 minute')`, id, providerID, slotID, env.ownerID); err != nil {
			t.Fatal(err)
		}
		return id
	}
	expiredID := insertExpired()
	untouchedID := insertExpired()

	res := env.call(t, http.MethodPost, "/api/v1/providers/"+providerID+"/grant-enrollments/"+expiredID+"/poll", "", "", "")
	if res.status != http.StatusOK {
		t.Fatalf("expired poll HTTP %d: %s", res.status, res.body)
	}
	var status struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(res.body, &status); err != nil || status.Status != "expired" {
		t.Fatalf("expired poll body = %s, want status expired", res.body)
	}
	var outcome *string
	var continued bool
	var secretCount, auditCount int
	if err := env.pool.QueryRow(ctx, `SELECT outcome,continued_at IS NOT NULL,(SELECT count(*) FROM olp.secrets WHERE id=$1 AND purpose='grant_enrollment'),
		(SELECT count(*) FROM olp.audit WHERE action='provider.grant.enroll' AND resource_type='provider' AND resource_id=$2 AND outcome='failure')
		FROM olp.grant_enrollments WHERE id=$1`, expiredID, providerID).Scan(&outcome, &continued, &secretCount, &auditCount); err != nil {
		t.Fatal(err)
	}
	if outcome == nil || *outcome != "expired" || !continued || secretCount != 0 || auditCount != 1 {
		t.Fatalf("settled state outcome=%v continued=%v secrets=%d audits=%d, want expired/true/0/1", outcome, continued, secretCount, auditCount)
	}

	if _, err := env.pool.Exec(ctx, `DELETE FROM olp.sessions WHERE id=$1`, env.client.sessionID); err != nil {
		t.Fatal(err)
	}
	revokedRes := env.call(t, http.MethodPost, "/api/v1/providers/"+providerID+"/grant-enrollments/"+untouchedID+"/poll", "", "", "")
	if revokedRes.status != http.StatusUnauthorized {
		t.Fatalf("revoked session poll HTTP %d: %s, want 401", revokedRes.status, revokedRes.body)
	}
	if err := env.pool.QueryRow(ctx, `SELECT outcome,continued_at IS NOT NULL,(SELECT count(*) FROM olp.secrets WHERE id=$1 AND purpose='grant_enrollment'),
		(SELECT count(*) FROM olp.audit WHERE action='provider.grant.enroll' AND resource_type='provider' AND resource_id=$2 AND outcome='failure')
		FROM olp.grant_enrollments WHERE id=$1`, untouchedID, providerID).Scan(&outcome, &continued, &secretCount, &auditCount); err != nil {
		t.Fatal(err)
	}
	if outcome != nil || continued || secretCount != 1 || auditCount != 1 {
		t.Fatalf("revoked session mutated enrollment outcome=%v continued=%v secrets=%d audits=%d, want nil/false/1/1", outcome, continued, secretCount, auditCount)
	}
}

func TestGrantEnrollmentSettlePersistsStandingAndSecretState(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	env := newProviderAPITestEnv(t, nil)
	providerID, slotID := uuid.NewString(), uuid.NewString()
	etag := uuid.NewString()
	if _, err := env.pool.Exec(ctx, `INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by) VALUES($1,'settle-fixture','openai_compatible','draft',$2,$3,$3,$4)`,
		providerID, providerEndpoint, etag, env.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(ctx, `INSERT INTO olp.provider_slots(id,provider_id,is_default,position,name,enabled,priority,weight,restrictions,limits) VALUES($1,$2,true,0,'default',true,0,1,'{}','{}')`,
		slotID, providerID); err != nil {
		t.Fatal(err)
	}

	claimDevice := func(t *testing.T, intervalSeconds int64) grants.Enrollment {
		t.Helper()
		id := uuid.NewString()
		expires := time.Now().Add(10 * time.Minute)
		tx, err := env.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err := env.client.access.Keys.Store(ctx, tx, env.client.access.Installation, id, secrets.GrantEnrollment, []byte("device-session"), &expires); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO olp.grant_enrollments(id,provider_id,slot_id,plugin_digest,profile_id,started_by,expires_at,poll_interval,poll_at)
			VALUES($1,$2,$3,repeat('0',64),'fixture-profile',$4,$5,$6,now()-interval '1 second')`,
			id, providerID, slotID, env.ownerID, expires, intervalSeconds); err != nil {
			t.Fatal(err)
		}
		claimed, standing, err := grants.Watch(ctx, tx, env.client.access, providerID, id, env.ownerID)
		if err != nil || claimed == nil {
			t.Fatalf("Watch claimed=%v standing=%+v err=%v", claimed, standing, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return *claimed
	}

	settle := func(t *testing.T, enrollment grants.Enrollment, failed error, interrupted bool) (grants.Standing, error) {
		t.Helper()
		tx, err := env.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		standing, err := enrollment.Settle(ctx, tx, failed, interrupted)
		if err != nil {
			return standing, err
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return standing, nil
	}

	type persistedState struct {
		pollInterval int64
		continued    bool
		outcome      *string
		secretCount  int
	}
	readState := func(t *testing.T, id string) persistedState {
		t.Helper()
		var s persistedState
		if err := env.pool.QueryRow(ctx, `SELECT poll_interval,continued_at IS NOT NULL,outcome,
			(SELECT count(*) FROM olp.secrets WHERE id=$1 AND purpose=$2)
			FROM olp.grant_enrollments WHERE id=$1`, id, secrets.GrantEnrollment).Scan(&s.pollInterval, &s.continued, &s.outcome, &s.secretCount); err != nil {
			t.Fatal(err)
		}
		return s
	}

	t.Run("authorization_pending keeps enrollment and secret", func(t *testing.T) {
		enrollment := claimDevice(t, 10)
		standing, err := settle(t, enrollment, &abi.Error{Code: abi.CodeAuthorizationPending, Message: "wait"}, false)
		if err != nil {
			t.Fatalf("Settle error: %v", err)
		}
		if standing.Status != grants.Pending || standing.Interval != 10*time.Second || standing.Ended || standing.Failure != nil {
			t.Fatalf("standing = %+v, want pending 10s", standing)
		}
		state := readState(t, enrollment.ID)
		if state.continued || state.outcome != nil || state.pollInterval != 10 || state.secretCount != 1 {
			t.Fatalf("persisted state = %+v, want pending with interval 10 and secret kept", state)
		}
	})

	t.Run("slow_down increases persisted interval and keeps secret", func(t *testing.T) {
		enrollment := claimDevice(t, 10)
		standing, err := settle(t, enrollment, &abi.Error{Code: abi.CodeSlowDown, Message: "slower"}, false)
		if err != nil {
			t.Fatalf("Settle error: %v", err)
		}
		if standing.Status != grants.Pending || standing.Interval != 15*time.Second || standing.Ended || standing.Failure != nil {
			t.Fatalf("standing = %+v, want pending 15s", standing)
		}
		state := readState(t, enrollment.ID)
		if state.continued || state.outcome != nil || state.pollInterval != 15 || state.secretCount != 1 {
			t.Fatalf("persisted state = %+v, want pending with interval 15 and secret kept", state)
		}
	})

	t.Run("interrupted poll leaves terminal failure pending and keeps secret", func(t *testing.T) {
		enrollment := claimDevice(t, 10)
		standing, err := settle(t, enrollment, &abi.Error{Code: "server_error", Message: "aborted"}, true)
		if err != nil {
			t.Fatalf("Settle error: %v", err)
		}
		if standing.Status != grants.Pending || standing.Interval != 10*time.Second || standing.Ended || standing.Failure != nil {
			t.Fatalf("standing = %+v, want pending 10s", standing)
		}
		state := readState(t, enrollment.ID)
		if state.continued || state.outcome != nil || state.pollInterval != 10 || state.secretCount != 1 {
			t.Fatalf("persisted state = %+v, want pending with secret kept", state)
		}
	})

	t.Run("access_denied ends enrollment and deletes secret", func(t *testing.T) {
		enrollment := claimDevice(t, 5)
		standing, err := settle(t, enrollment, &abi.Error{Code: abi.CodeAccessDenied, Message: "denied"}, false)
		if err != nil {
			t.Fatalf("Settle error: %v", err)
		}
		if standing.Status != grants.Denied || !standing.Ended || standing.Failure != nil {
			t.Fatalf("standing = %+v, want denied ended", standing)
		}
		state := readState(t, enrollment.ID)
		if !state.continued || state.outcome == nil || *state.outcome != "denied" || state.secretCount != 0 {
			t.Fatalf("persisted state = %+v, want denied with secret deleted", state)
		}
	})

	t.Run("expired_token ends enrollment and deletes secret", func(t *testing.T) {
		enrollment := claimDevice(t, 5)
		standing, err := settle(t, enrollment, &abi.Error{Code: abi.CodeExpiredToken, Message: "expired"}, false)
		if err != nil {
			t.Fatalf("Settle error: %v", err)
		}
		if standing.Status != grants.Expired || !standing.Ended || standing.Failure != nil {
			t.Fatalf("standing = %+v, want expired ended", standing)
		}
		state := readState(t, enrollment.ID)
		if !state.continued || state.outcome == nil || *state.outcome != "expired" || state.secretCount != 0 {
			t.Fatalf("persisted state = %+v, want expired with secret deleted", state)
		}
	})

	t.Run("terminal abi error commits settlement and returns problem on Standing.Failure", func(t *testing.T) {
		enrollment := claimDevice(t, 5)
		standing, err := settle(t, enrollment, &abi.Error{Code: "server_error", Message: "upstream refused"}, false)
		if err != nil {
			t.Fatalf("expected nil tx error so caller commits, got: %v", err)
		}
		if !standing.Ended || standing.Status != "" {
			t.Fatalf("standing = %+v, want ended terminal failure", standing)
		}
		problem, ok := errors.AsType[*httpkit.Problem](standing.Failure)
		if !ok || problem.Status != 422 || problem.Code != "grant_enrollment_failed" {
			t.Fatalf("Standing.Failure = %#v, want 422 grant_enrollment_failed", standing.Failure)
		}
		state := readState(t, enrollment.ID)
		if !state.continued || state.outcome != nil || state.secretCount != 0 {
			t.Fatalf("persisted state = %+v, want ended without outcome and secret deleted", state)
		}
	})

	t.Run("terminal non-abi error preserves problem and deletes secret", func(t *testing.T) {
		enrollment := claimDevice(t, 5)
		stale := httpkit.Fail(409, "grant_enrollment_stale", "slot changed")
		standing, err := settle(t, enrollment, stale, false)
		if err != nil {
			t.Fatalf("Settle error: %v", err)
		}
		if !standing.Ended || !errors.Is(standing.Failure, stale) {
			t.Fatalf("standing = %+v, want ended with stale problem", standing)
		}
		state := readState(t, enrollment.ID)
		if !state.continued || state.outcome != nil || state.secretCount != 0 {
			t.Fatalf("persisted state = %+v, want ended with secret deleted", state)
		}
	})

	t.Run("rolled back transaction leaves enrollment pending and secret intact", func(t *testing.T) {
		enrollment := claimDevice(t, 5)
		tx, err := env.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		standing, err := enrollment.Settle(ctx, tx, &abi.Error{Code: abi.CodeAccessDenied}, false)
		if err != nil || standing.Status != grants.Denied {
			_ = tx.Rollback(ctx)
			t.Fatalf("Settle = (%+v, %v), want denied", standing, err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		state := readState(t, enrollment.ID)
		if state.continued || state.outcome != nil || state.secretCount != 1 {
			t.Fatalf("persisted state after rollback = %+v, want pending with secret intact", state)
		}
	})

	t.Run("cancelled enrollment returns pgx.ErrNoRows", func(t *testing.T) {
		enrollment := claimDevice(t, 5)
		cancelTx, err := env.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := grants.Cancel(ctx, cancelTx, providerID, enrollment.ID, env.ownerID); err != nil {
			_ = cancelTx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := cancelTx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		standing, err := settle(t, enrollment, &abi.Error{Code: abi.CodeAccessDenied}, false)
		if !errors.Is(err, pgx.ErrNoRows) || standing != (grants.Standing{}) {
			t.Fatalf("Settle after cancel = (%+v, %v), want (zero, pgx.ErrNoRows)", standing, err)
		}
	})
}

func TestPersonalDeviceEnrollmentRetriesClientSetup(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	var exchanges atomic.Int32
	mux := http.NewServeMux()
	provider := httptest.NewServer(mux)
	t.Cleanup(provider.Close)
	mux.HandleFunc("POST /device", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"device_code":"test-device","user_code":"test-user","verification_uri":%q,"expires_in":600,"interval":5}`, provider.URL+"/verify")
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"test-access-token","token_type":"Bearer","expires_in":600}`)
	})
	binding := personal.Binding{
		ServerID: uuid.NewString(), ProjectID: fixture.projectID, Endpoint: provider.URL + "/mcp",
		Identity: personal.Identity{Kind: personal.KeyIdentity, ID: fixture.keyID},
		OAuth: routing.MCPOAuth{
			ClientID: "test-client", Audience: provider.URL + "/mcp", Scopes: []string{"tools"},
			AuthorizationEndpoint: provider.URL + "/authorize", TokenEndpoint: provider.URL + "/token", DeviceAuthorizationEndpoint: provider.URL + "/device",
		},
	}
	revision := uuid.NewString()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO olp.mcp_servers(id,project_id,name,transport,endpoint,enabled,latest_revision_id,etag,created_by) VALUES($1,$2,'device-enrollment','streamable_http',$3,true,$4,$4,$5)`, binding.ServerID, binding.ProjectID, binding.Endpoint, revision, fixture.ownerID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO olp.mcp_server_revisions(id,server_id,revision,endpoint,protocol_version,tools,digest,oauth,created_by) VALUES($1,$2,1,$3,'2025-03-26','[]',repeat('0',64),$4,$5)`, revision, binding.ServerID, binding.Endpoint, binding.OAuth, fixture.ownerID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	keys, installation := notificationKeys(t, pool)
	auth := secrets.NewAuthKey(make([]byte, 32), installation)
	if err := access.PrepareInstallation(ctx, pool, installation, auth, keys); err != nil {
		t.Fatal(err)
	}
	policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	store := personal.NewStore(pool, installation, auth, keys, policy)
	handoff, err := store.Start(ctx, binding, &fixture.ownerID)
	if err != nil {
		t.Fatal(err)
	}
	_, token, _ := strings.Cut(handoff.EnrollmentURL, ".")
	if _, err := store.Claim(ctx, handoff.EnrollmentID, token, fixture.ownerID); err != nil {
		t.Fatal(err)
	}
	if result, err := store.Authorize(ctx, handoff.EnrollmentID, fixture.ownerID, personal.DeviceCode, ""); err != nil || result.Device == nil {
		t.Fatalf("device authorization = %+v, error = %v", result, err)
	}
	blocked := personal.NewStore(pool, installation, auth, keys, &egress.Policy{})
	_, setupErr := blocked.Poll(ctx, handoff.EnrollmentID, fixture.ownerID)
	problem, known := httpkit.ProblemFor(setupErr)
	if !known || problem.Status != http.StatusServiceUnavailable || problem.Code != "grant_authorization_unavailable" || exchanges.Load() != 0 {
		t.Fatalf("failed setup error = %v, token exchanges = %d", setupErr, exchanges.Load())
	}
	if result, err := store.Poll(ctx, handoff.EnrollmentID, fixture.ownerID); err != nil || result.State != personal.Active || exchanges.Load() != 1 {
		t.Fatalf("retry = %+v, error = %v, token exchanges = %d, want an active grant after one exchange", result, err, exchanges.Load())
	}
}

func TestPluginUninstallRetiresOnlyItsUnrevokedGrants(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool, _ := harness.Database(t)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	ownerID, providerID := uuid.NewString(), uuid.NewString()
	exec(`INSERT INTO olp.users(id,email,display_name,role,etag) VALUES($1::uuid,$1::text||'@example.com','Owner','owner',$1::uuid)`, ownerID)
	exec(`INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by) VALUES($1,'uninstall-fixture','openai_compatible','draft',$2,$3,$3,$4)`,
		providerID, providerEndpoint, uuid.NewString(), ownerID)
	client := managementHTTPAs(t, pool, ownerID, func(mux *http.ServeMux, accessServer *access.Server) {
		(&plugins.Management{Access: accessServer, Host: plugins.NewHost(nil, nil, pool)}).Register(mux)
	})
	removed, kept, unused := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	etag := uuid.NewString()
	for _, digest := range []string{removed, kept, unused} {
		exec(`INSERT INTO olp.plugins(digest,abi_version,manifest,module,size_bytes,etag,installed_by)
			VALUES($1,1,'{"name":"grant-fixture","version":"0.1.0","origins":[],"profiles":[]}','\x00',1,$2,$3)`, digest, etag, ownerID)
	}
	grants := []struct {
		name, digest                   string
		revoked, refreshable, retiring bool
	}{
		{name: "refreshable", digest: removed, refreshable: true, retiring: true},
		{name: "without a refresh token", digest: removed, retiring: true},
		{name: "revoked", digest: removed, revoked: true, refreshable: true},
		{name: "of another plugin", digest: kept, refreshable: true},
	}
	credentials, tokens := make([]string, len(grants)), make([]*string, len(grants))
	for i, grant := range grants {
		credentials[i] = uuid.NewString()
		exec(`INSERT INTO olp.provider_credentials(id,provider_id,version,principal,plugin_digest,grant_facts,revoked_at)
			VALUES($1,$2,$3,$4,$5,'{}',CASE WHEN $6 THEN now() END)`, credentials[i], providerID, i+1, grant.name, grant.digest, grant.revoked)
		if grant.refreshable {
			token := uuid.NewString()
			tokens[i] = &token
			exec(`INSERT INTO olp.secrets(id,purpose,key_version,ciphertext) VALUES($1,'provider_grant_refresh',1,'\x00')`, token)
		}
		exec(`INSERT INTO olp.provider_grants(credential_id,refresh_token_id) VALUES($1,$2)`, credentials[i], tokens[i])
	}

	for _, digest := range []string{removed, unused} {
		res := client.call(t, http.MethodDelete, "/api/v1/plugins/"+digest, `"`+etag+`"`, "", uuid.NewString())
		if res.status != http.StatusNoContent {
			t.Fatalf("uninstall %s HTTP %d: %s", digest[:1], res.status, res.body)
		}
	}

	for i, grant := range grants {
		var lapsed, holdsToken, secretStored bool
		var audits int
		if err := pool.QueryRow(ctx, `SELECT lapsed_at IS NOT NULL,refresh_token_id IS NOT NULL,EXISTS(SELECT 1 FROM olp.secrets WHERE id=$2),
			(SELECT count(*) FROM olp.audit WHERE action='provider.grant.retire' AND resource_id::text=$3)
			FROM olp.provider_grants WHERE credential_id=$1`, credentials[i], tokens[i], credentials[i]).Scan(&lapsed, &holdsToken, &secretStored, &audits); err != nil {
			t.Fatal(err)
		}
		keepsToken := grant.refreshable && !grant.retiring
		wantAudits := 0
		if grant.retiring {
			wantAudits = 1
		}
		if lapsed != grant.retiring || holdsToken != keepsToken || secretStored != keepsToken || audits != wantAudits {
			t.Errorf("grant %s: lapsed=%t holds token=%t secret stored=%t audits=%d, want %t/%t/%t/%d",
				grant.name, lapsed, holdsToken, secretStored, audits, grant.retiring, keepsToken, keepsToken, wantAudits)
		}
	}
}
