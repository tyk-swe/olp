//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/runtime"
)

type codeFixture struct {
	h                                                       *accessHarness
	owner                                                   *browser
	project, user, key, provider, credential, account, pool string
	accountRecord                                           map[string]any
	route                                                   codemode.Route
	store                                                   *resources.CodeStore
}

func newCodeFixture(t *testing.T, bodyLimit ...int64) *codeFixture {
	return newCodeFixtureWithProfile(t, "", bodyLimit...)
}

func newCodeFixtureWithProfile(t *testing.T, profile string, bodyLimit ...int64) *codeFixture {
	var maxBody *int64
	if len(bodyLimit) > 0 {
		maxBody = &bodyLimit[0]
	}
	t.Helper()
	h := newAccessHarness(t)
	owner := h.owner()
	f := &codeFixture{h: h, owner: owner, project: createProject(h, owner, "Coding"), key: access.NewID(), provider: access.NewID(), credential: access.NewID()}
	f.user = h.want(owner, "GET", "/api/v1/profile", nil, nil, 200)["id"].(string)
	f.exec(t, `INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by,project_id) VALUES($1,'Fixture coding subscription','plugin','draft','{"kind":"plugin","auth_mode":"grant","endpoint":"https://fixture.invalid"}',$2,$3,$4,$5)`, f.provider, access.NewID(), access.NewID(), f.user, f.project)
	f.addCredential(t, f.credential, "fixture-principal", 1)
	if profile != "" {
		f.exec(t, `UPDATE olp.providers SET configuration=configuration || jsonb_build_object('profile_id',$2::text,'profile_revision','fixture-digest') WHERE id=$1`, f.provider, profile)
		f.exec(t, `UPDATE olp.provider_credentials SET profile_id=$2 WHERE id=$1`, f.credential, profile)
	}
	f.addKey(t, f.key)
	f.accountRecord = h.want(owner, "POST", "/api/v1/code/accounts", map[string]any{"project_id": f.project, "provider_id": f.provider, "credential_id": f.credential, "name": "Fixture subscription", "enabled": true, "models": []string{"native-model"}}, idem("account"), 201)
	f.account = f.accountRecord["id"].(string)
	pool := h.want(owner, "POST", "/api/v1/code/pools", map[string]any{"project_id": f.project, "name": "Shared coding", "kind": "shared", "owner_user_id": nil, "account_ids": []string{f.account}, "api_key_ids": []string{f.key}}, idem("pool"), 201)
	f.pool = pool["id"].(string)
	draft := h.want(owner, "POST", "/api/v1/code/routes", map[string]any{"project_id": f.project, "slug": "coding", "max_body_bytes": maxBody, "pool_id": f.pool, "models": []string{"native-model"}, "enabled": true}, idem("route"), 201)
	headers := etagHeader(draft)
	headers["Idempotency-Key"] = "publish"
	published := h.want(owner, "POST", "/api/v1/code/routes/"+draft["id"].(string)+"/publish", nil, headers, 200)
	encoded, _ := json.Marshal(published)
	if err := json.Unmarshal(encoded, &f.route); err != nil {
		t.Fatal(err)
	}
	f.store = &resources.CodeStore{Pool: h.Pool}
	return f
}

func (f *codeFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.h.Pool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatal(err)
	}
}
func (f *codeFixture) addCredential(t *testing.T, id, principal string, version int) {
	t.Helper()
	f.exec(t, `INSERT INTO olp.provider_credentials(id,provider_id,version,plugin_digest,principal,grant_facts) VALUES($1,$2,$3,'fixture-digest',$4,'{}')`, id, f.provider, version, principal)
	f.exec(t, `INSERT INTO olp.provider_grants(credential_id) VALUES($1)`, id)
}
func (f *codeFixture) addKey(t *testing.T, id string) {
	t.Helper()
	f.exec(t, `INSERT INTO olp.api_keys(id,lookup_id,digest,name,created_by,policy,etag,project_id) VALUES($1::uuid,$1::text,'\x00','Fixture key',$2,'{"scopes":["inference"],"allowed_routes":["coding"]}',$3,$4)`, id, f.user, access.NewID(), f.project)
}
func (f *codeFixture) input(conversation, parent string, bound *codemode.TokenBound) resources.CodeAdmission {
	return resources.CodeAdmission{Route: f.route, APIKeyID: f.key, Providers: []string{f.provider}, Operation: codemode.Operation{Name: "responses.create", Model: "native-model", Identity: codemode.Identity{Conversation: conversation, Parent: parent}}, Bound: bound}
}
func codeRefusal(t *testing.T, err error, code string) {
	t.Helper()
	var refusal *codemode.Refusal
	if !errors.As(err, &refusal) || refusal.Code != code {
		t.Fatalf("refusal=%v; want %s", err, code)
	}
}

func TestCodeFoundationAtomicTreeAndRetirement(t *testing.T) {
	f := newCodeFixture(t)
	other, err := pgxpool.New(t.Context(), f.h.DBURL)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	stores := []*resources.CodeStore{f.store, {Pool: other}}
	const count = 24
	permits := make(chan resources.CodePermit, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Go(func() {
			p, err := stores[i%2].Admit(t.Context(), f.input("root", "", nil))
			if err != nil {
				errs <- err
			} else {
				permits <- p
			}
		})
	}
	wg.Wait()
	close(errs)
	close(permits)
	for err := range errs {
		t.Fatal(err)
	}
	var root resources.CodePermit
	attempts := map[string]bool{}
	for p := range permits {
		if root.Binding.ID != "" && (p.Binding.ID != root.Binding.ID || p.Account.ID != root.Account.ID) {
			t.Fatal("concurrent first requests chose different bindings")
		}
		if attempts[p.Attempt.ID] {
			t.Fatal("inference attempt replayed")
		}
		attempts[p.Attempt.ID] = true
		root = p
	}
	if len(attempts) != count {
		t.Fatal("attempts missing")
	}
	if root.Authority.ID != f.key || root.Authority.Policy.AllowedRoutes[0] != "coding" {
		t.Fatal("permit has no current key policy")
	}
	child, err := stores[1].Admit(t.Context(), f.input("child", "root", nil))
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := f.store.Admit(t.Context(), f.input("grandchild", "child", nil))
	if err != nil {
		t.Fatal(err)
	}
	if child.Binding.RootID != root.Binding.ID || grandchild.Binding.RootID != root.Binding.ID || child.Account.ID != root.Account.ID {
		t.Fatal("tree lost root pin")
	}
	_, err = f.store.Admit(t.Context(), f.input("orphan", "missing", nil))
	codeRefusal(t, err, "code_parent_unresolved")
	_, err = f.store.Admit(t.Context(), f.input("child", "different-parent", nil))
	codeRefusal(t, err, "code_parent_conflict")
	otherKey := access.NewID()
	f.addKey(t, otherKey)
	f.exec(t, `INSERT INTO olp.code_pool_keys(pool_id,api_key_id) VALUES($1,$2)`, f.pool, otherKey)
	in := f.input("foreign-child", "root", nil)
	in.APIKeyID = otherKey
	_, err = f.store.Admit(t.Context(), in)
	codeRefusal(t, err, "code_parent_unresolved")
	f.h.want(f.owner, "POST", "/api/v1/code/bindings/"+child.Binding.ID+"/retire", nil, idem("retire"), 200)
	for _, identity := range []codemode.Identity{{Conversation: "root"}, {Conversation: "child"}, {Conversation: "grandchild"}, {Conversation: "new-child", Parent: "root"}} {
		in := f.input(identity.Conversation, identity.Parent, nil)
		_, err = stores[1].Admit(t.Context(), in)
		codeRefusal(t, err, "code_binding_retired")
	}
	if _, err = f.h.Pool.Exec(t.Context(), `DELETE FROM olp.code_bindings WHERE id=$1`, root.Binding.ID); err == nil {
		t.Fatal("retirement could be deleted")
	}
	if _, err = f.h.Pool.Exec(t.Context(), `UPDATE olp.code_bindings SET retired_at=NULL WHERE id=$1`, root.Binding.ID); err == nil {
		t.Fatal("retirement could be undone")
	}
}

// Two providers of the Z.ai family through different profiles, and one
// OpenCode Go provider, frozen into the mixed route's connections.

// The OpenCode provider's draft claims the Z.ai family: the tree must
// still admit its account, the only frozen family it does not pin yet.

// The BigModel provider's draft claims OpenCode Go: the tree must still
// refuse it as the Z.ai family it already pins, not admit a second one.

// A pin whose transaction started first can be inserted last, so its
// creation time precedes the pin made before it.

// The Codex provider becomes OpenCode Go, so the republished route freezes
// two of the tree's accounts into one family. The one the tree pinned first
// keeps it.

// Accounts after the tree's first serve it only through pins.

func TestCodeFoundationTokenReservationsAcrossReplicas(t *testing.T) {
	f := newCodeFixture(t)
	budget := f.h.want(f.owner, "POST", "/api/v1/code/budgets", map[string]any{"project_id": f.project, "route_id": f.route.ID, "api_key_id": nil, "daily_tokens": 100, "monthly_tokens": 1000, "enabled": true}, idem("budget"), 201)
	other, err := pgxpool.New(t.Context(), f.h.DBURL)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	stores := []*resources.CodeStore{f.store, {Pool: other}}
	const count = 20
	keys := make([]string, count)
	for i := range count {
		keys[i] = access.NewID()
		f.addKey(t, keys[i])
		f.exec(t, `INSERT INTO olp.code_pool_keys(pool_id,api_key_id) VALUES($1,$2)`, f.pool, keys[i])
	}
	permits := make(chan resources.CodePermit, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Go(func() {
			in := f.input(fmt.Sprintf("budget-%d", i), "", &codemode.TokenBound{Tokens: 10, Evidence: "controlled-test-bound"})
			in.APIKeyID = keys[i]
			p, err := stores[i%2].Admit(t.Context(), in)
			if err != nil {
				errs <- err
			} else {
				permits <- p
			}
		})
	}
	wg.Wait()
	close(errs)
	close(permits)
	refused := 0
	for err := range errs {
		codeRefusal(t, err, "code_token_budget_exhausted")
		refused++
	}
	var accepted []resources.CodePermit
	for p := range permits {
		accepted = append(accepted, p)
	}
	if len(accepted) != 10 || refused != 10 {
		t.Fatalf("admitted=%d refused=%d", len(accepted), refused)
	}
	first := accepted[0]
	if err = f.store.MarkDispatched(t.Context(), first.Attempt.ID); err != nil {
		t.Fatal(err)
	}
	codeRefusal(t, f.store.MarkDispatched(t.Context(), first.Attempt.ID), "code_attempt_already_dispatched")
	codeRefusal(t, f.store.Abort(t.Context(), first.Attempt.ID), "code_dispatch_uncertain")
	if err = stores[1].Settle(t.Context(), first.Attempt.ID, codemode.Usage{}); err != nil {
		t.Fatal(err)
	}
	var reserved, measured int64
	if err = f.h.Pool.QueryRow(t.Context(), `SELECT reserved,measured FROM olp.code_token_windows WHERE budget_id=$1 AND period='day'`, budget["id"]).Scan(&reserved, &measured); err != nil || reserved != 100 || measured != 0 {
		t.Fatal("uncertainty was dropped", reserved, measured, err)
	}
	actual := int64(3)
	errs2 := make(chan error, count)
	for i := range count {
		wg.Go(func() { errs2 <- stores[i%2].Settle(t.Context(), first.Attempt.ID, codemode.Usage{Total: &actual}) })
	}
	wg.Wait()
	close(errs2)
	for err := range errs2 {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = f.h.Pool.QueryRow(t.Context(), `SELECT reserved,measured FROM olp.code_token_windows WHERE budget_id=$1 AND period='day'`, budget["id"]).Scan(&reserved, &measured); err != nil || reserved != 90 || measured != 3 {
		t.Fatal("settlement was not exactly once", reserved, measured, err)
	}
	conflict := int64(4)
	codeRefusal(t, f.store.Settle(t.Context(), first.Attempt.ID, codemode.Usage{Total: &conflict}), "code_settlement_conflict")
	_, err = f.store.Admit(t.Context(), f.input("unknown-bound", "", nil))
	codeRefusal(t, err, "code_token_bound_unavailable")
	for _, p := range accepted[1:] {
		if err = f.store.Abort(t.Context(), p.Attempt.ID); err != nil {
			t.Fatal(err)
		}
	}
	f.exec(t, `UPDATE olp.code_token_budgets SET enabled=false WHERE id=$1`, budget["id"])
	unbudgeted, err := f.store.Admit(t.Context(), f.input("unbudgeted", "", nil))
	if err != nil {
		t.Fatal("unbudgeted unknown bound refused", err)
	}
	if err = f.store.MarkDispatched(t.Context(), unbudgeted.Attempt.ID); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE olp.code_token_budgets SET enabled=true WHERE id=$1`, budget["id"])
	_, err = f.store.Admit(t.Context(), f.input("after-enable", "", &codemode.TokenBound{Tokens: 10, Evidence: "controlled-test-bound"}))
	codeRefusal(t, err, "code_token_budget_exhausted")
}

func TestCodeFoundationManagementIsolationPublicationAndPrivacy(t *testing.T) {
	f := newCodeFixture(t, 1048576)
	remaining := int64(42)
	if err := f.store.ObserveAllowance(t.Context(), f.account, codemode.Allowance{RemainingTokens: &remaining, ObservedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	stale := int64(99)
	if err := f.store.ObserveAllowance(t.Context(), f.account, codemode.Allowance{RemainingTokens: &stale, ObservedAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	accounts := f.h.want(f.owner, "GET", "/api/v1/code/accounts", nil, nil, 200)
	allowance := accounts["items"].([]any)[0].(map[string]any)["allowance"].(map[string]any)
	if allowance["remaining_tokens"].(float64) != 42 || allowance["remaining_requests"] != nil {
		t.Fatal("allowance observation lost unknown/stale semantics", allowance)
	}
	var document []byte
	if err := f.h.Pool.QueryRow(t.Context(), `SELECT snapshot FROM olp.runtime_releases ORDER BY sequence DESC LIMIT 1`).Scan(&document); err != nil {
		t.Fatal(err)
	}
	var snapshot routing.Snapshot
	if err := json.Unmarshal(document, &snapshot); err != nil {
		t.Fatal(err)
	}
	connection, ok := snapshot.CodeConnection(f.route, f.provider)
	if limit := snapshot.CodeRoutes["coding"].MaxBodyBytes; limit == nil || *limit != 1048576 {
		t.Fatalf("published code body limit lost: %v", limit)
	}
	if !ok || snapshot.CodeRoutes["coding"].RevisionID != f.route.RevisionID || connection.Endpoint != "https://fixture.invalid" {
		t.Fatal("code publication missing")
	}
	before, err := snapshot.Digest()
	if err != nil {
		t.Fatal(err)
	}
	route := snapshot.CodeRoutes["coding"]
	route.Enabled = false
	snapshot.CodeRoutes["coding"] = route
	after, err := snapshot.Digest()
	if err != nil || before == after {
		t.Fatal("code policy missing from digest")
	}
	f.exec(t, `UPDATE olp.providers SET configuration=jsonb_set(configuration::jsonb,'{endpoint}','"https://unpublished.invalid"') WHERE id=$1`, f.provider)
	tx, err := f.h.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := runtime.ReadSnapshot(t.Context(), tx)
	_ = tx.Rollback(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	connection, ok = compiled.CodeConnection(f.route, f.provider)
	if !ok || connection.Endpoint != "https://fixture.invalid" {
		t.Fatal("provider draft leaked into code runtime")
	}
	draft := f.h.want(f.owner, "PUT", "/api/v1/code/routes/"+f.route.ID, map[string]any{"project_id": f.project, "slug": "coding", "pool_id": f.pool, "models": []string{"native-model"}, "enabled": false}, map[string]string{"If-Match": `"` + f.route.ETag + `"`}, 200)
	if _, err = f.store.Admit(t.Context(), f.input("still-published", "", nil)); err != nil {
		t.Fatal("draft changed serving policy", err)
	}
	headers := etagHeader(draft)
	headers["Idempotency-Key"] = "disable"
	f.h.want(f.owner, "POST", "/api/v1/code/routes/"+f.route.ID+"/publish", nil, headers, 200)
	_, err = f.store.Admit(t.Context(), f.input("disabled", "", nil))
	codeRefusal(t, err, "code_permission_denied")
	if err = f.store.RecordRefusal(t.Context(), f.route, f.key, "", "code_permission_denied"); err != nil {
		t.Fatal(err)
	}
	if err = f.store.RecordRefusal(t.Context(), f.route, f.key, "", "prompt: never persist me"); err == nil {
		t.Fatal("unbounded refusal text accepted")
	}
	for _, path := range []string{"accounts", "pools", "routes", "budgets", "bindings", "attempts", "refusals", "token-windows"} {
		status, body, _ := f.h.request(f.owner, "GET", "/api/v1/code/"+path, nil, nil)
		if status != 200 {
			t.Fatal(path, status, body)
		}
		encoded, _ := json.Marshal(body)
		if strings.Contains(string(encoded), "prompt:") || strings.Contains(string(encoded), "access_token") || strings.Contains(string(encoded), "refresh_token") {
			t.Fatal("sensitive diagnostic data", path)
		}
	}
	op := f.h.invite(f.owner, "code-manager@example.com", "operator")
	profile := f.h.want(op, "GET", "/api/v1/profile", nil, nil, 200)
	f.h.want(f.owner, "PATCH", "/api/v1/users/"+profile["id"].(string), map[string]any{"access_scope": "assigned"}, etagHeader(profile), 200)
	op = login(f.h, "code-manager@example.com")
	for _, path := range []string{"accounts", "pools", "routes", "budgets", "bindings", "attempts", "refusals", "token-windows"} {
		body := f.h.want(op, "GET", "/api/v1/code/"+path, nil, nil, 200)
		if len(body["items"].([]any)) != 0 {
			t.Fatal("project leaked", path)
		}
	}
	f.h.want(op, "GET", "/api/v1/code/accounts?project_id="+f.project, nil, nil, 404)
	f.h.want(op, "GET", "/api/v1/code/routes/"+f.route.ID+"/revisions", nil, nil, 404)
	foreignProject := createProject(f.h, f.owner, "Other")
	input := f.input("wrong-project", "", nil)
	input.Route.ProjectID = foreignProject
	_, err = f.store.Admit(t.Context(), input)
	codeRefusal(t, err, "code_permission_denied")
	f.h.want(f.owner, "POST", "/api/v1/code/pools", map[string]any{"project_id": foreignProject, "name": "Cross project", "kind": "shared", "owner_user_id": nil, "account_ids": []string{f.account}, "api_key_ids": []string{}}, idem("cross-project"), 404)
	if err = f.h.Pool.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCodeFoundationMessagesTranslationPolicyPublication(t *testing.T) {
	f := newCodeFixtureWithProfile(t, codexauth.ProfileID)
	if f.route.MessagesToCodex {
		t.Fatal("translation enabled by default")
	}
	draft := f.h.want(f.owner, "PUT", "/api/v1/code/routes/"+f.route.ID, map[string]any{
		"project_id": f.project, "slug": f.route.Slug, "pool_id": f.pool, "models": f.route.Models,
		"enabled": true, "messages_to_codex": true,
	}, map[string]string{"If-Match": `"` + f.route.ETag + `"`}, 200)
	if draft["messages_to_codex"] != true {
		t.Fatal("translation policy missing from draft", draft)
	}
	f.h.want(f.owner, "GET", "/api/v1/code/routes/"+f.route.ID+"/client-config?client=claude-code&gateway_url=https://olp.example", nil, nil, 422)
	headers := etagHeader(draft)
	headers["Idempotency-Key"] = "translate-messages"
	published := f.h.want(f.owner, "POST", "/api/v1/code/routes/"+f.route.ID+"/publish", nil, headers, 200)
	if published["messages_to_codex"] != true {
		t.Fatal("translation policy missing from publication", published)
	}
	config := f.h.want(f.owner, "GET", "/api/v1/code/routes/"+f.route.ID+"/client-config?client=claude-code&gateway_url=https://olp.example", nil, nil, 200)
	if config["model"] != f.route.Models[0] || !strings.Contains(config["configuration"].(string), "ANTHROPIC_BASE_URL") {
		t.Fatal("translated Claude setup missing", config)
	}
}

func TestCodeFoundationAvailabilityPreservesFreshHealthAndIndependentAllowance(t *testing.T) {
	f := newCodeFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	observed := now.Add(-10 * time.Minute)
	retry := now.Add(2 * time.Hour)
	readAccount := func() codemode.Account {
		listing := f.h.want(f.owner, "GET", "/api/v1/code/accounts?project_id="+f.project, nil, nil, 200)
		raw, err := json.Marshal(listing["items"])
		if err != nil {
			t.Fatal(err)
		}
		var accounts []codemode.Account
		if err := json.Unmarshal(raw, &accounts); err != nil {
			t.Fatal(err)
		}
		if len(accounts) != 1 {
			t.Fatalf("accounts %+v", accounts)
		}
		return accounts[0]
	}
	observeHealth := func(observation codemode.HealthObservation) {
		if err := f.store.ObserveHealth(t.Context(), f.account, observation); err != nil {
			t.Fatal(err)
		}
	}
	observeHealth(codemode.HealthObservation{Status: "unavailable", ObservedAt: observed, RetryAt: &retry})
	observeHealth(codemode.HealthObservation{Status: "healthy", ObservedAt: observed.Add(-time.Minute)})
	observeHealth(codemode.HealthObservation{Status: "unavailable", ObservedAt: observed.Add(time.Minute)})
	blocked := readAccount()
	if blocked.Eligible || blocked.Health != "unavailable" || blocked.UnavailableUntil == nil || !blocked.UnavailableUntil.Equal(retry) || blocked.HealthObservedAt == nil || !blocked.HealthObservedAt.Equal(observed.Add(time.Minute)) {
		t.Fatalf("stale or partial observation erased cooldown %+v", blocked)
	}
	expired := now.Add(-time.Minute)
	observeHealth(codemode.HealthObservation{Status: "quota_limited", ObservedAt: observed.Add(2 * time.Minute), RetryAt: &expired})
	recovered := readAccount()
	if !recovered.Eligible || recovered.Health != "quota_limited" || recovered.Allowance != nil {
		t.Fatalf("retry deadline invented allowance or failed to recover %+v", recovered)
	}
	if err := f.store.ObserveAllowance(t.Context(), f.account, codemode.Allowance{RemainingTokens: new(int64(0)), ObservedAt: observed, TokenObservation: &codemode.CountObservation{ObservedAt: observed, ResetsAt: &retry}}); err != nil {
		t.Fatal(err)
	}
	observeHealth(codemode.HealthObservation{Status: "healthy", ObservedAt: observed.Add(3 * time.Minute)})
	exhausted := readAccount()
	if exhausted.Eligible || exhausted.Health != "healthy" || exhausted.UnavailableUntil != nil || exhausted.Allowance == nil || *exhausted.Allowance.RemainingTokens != 0 {
		t.Fatalf("serving recovery bypassed exhausted allowance %+v", exhausted)
	}
	f.exec(t, `UPDATE olp.provider_grants SET lapsed_at=now() WHERE credential_id=$1`, f.credential)
	rejected := readAccount()
	if rejected.Eligible || rejected.GrantState != "lapsed" || rejected.Health != "healthy" {
		t.Fatalf("refresh rejection conflated with serving health %+v", rejected)
	}
}

func TestCodeFoundationAllowanceRecoveryRequiresSupportedReset(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, test := range []struct {
		name     string
		reset    *time.Time
		eligible bool
	}{
		{"future", new(now.Add(time.Hour)), false},
		{"expired", new(now.Add(-time.Hour)), true},
		{"missing", nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCodeFixture(t)
			observed := now.Add(-2 * time.Hour)
			allowance := codemode.Allowance{
				RemainingTokens: new(int64(0)), RemainingRequests: new(int64(0)), ObservedAt: observed,
				TokenObservation: &codemode.CountObservation{ObservedAt: observed, ResetsAt: test.reset}, RequestObservation: &codemode.CountObservation{ObservedAt: observed, ResetsAt: test.reset},
				Windows: []codemode.AllowanceWindow{{LimitID: "codex", Window: "primary", UsedPercent: 100, RemainingPercent: 0, ResetsAt: test.reset, ObservedAt: observed}},
			}
			if err := f.store.ObserveAllowance(t.Context(), f.account, allowance); err != nil {
				t.Fatal(err)
			}
			listing := f.h.want(f.owner, "GET", "/api/v1/code/accounts?project_id="+f.project, nil, nil, 200)
			raw, err := json.Marshal(listing["items"])
			if err != nil {
				t.Fatal(err)
			}
			var accounts []codemode.Account
			if err := json.Unmarshal(raw, &accounts); err != nil {
				t.Fatal(err)
			}
			if len(accounts) != 1 || accounts[0].Eligible != test.eligible || accounts[0].Allowance == nil || *accounts[0].Allowance.RemainingTokens != 0 || *accounts[0].Allowance.RemainingRequests != 0 || *accounts[0].Allowance.RemainingPercent != 0 {
				t.Fatalf("reset recovery changed reported allowance or guessed a reset %+v", accounts)
			}
		})
	}
}
