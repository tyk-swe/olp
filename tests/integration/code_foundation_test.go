//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codeclient"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/routing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/runtime"
)

type codeFixture struct {
	h                                                       *accessHarness
	owner                                                   *browser
	project, user, key, provider, credential, account, pool string
	accountRecord, routeRecord, poolRecord                  map[string]any
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
	if profile != "" {
		f.exec(t, `UPDATE olp.providers SET configuration=(configuration::jsonb || jsonb_build_object('profile_id',$2::text,'profile_revision','fixture-digest'))::json WHERE id=$1`, f.provider, profile)
	}
	f.addCredential(t, f.credential, "fixture-principal", 1)
	f.addKey(t, f.key)
	f.accountRecord = h.want(owner, "POST", "/api/v1/code/accounts", map[string]any{"project_id": f.project, "provider_id": f.provider, "credential_id": f.credential, "name": "Fixture subscription", "enabled": true, "models": []string{"native-model"}}, idem("account"), 201)
	f.account = f.accountRecord["id"].(string)
	pool := h.want(owner, "POST", "/api/v1/code/pools", map[string]any{"project_id": f.project, "name": "Shared coding", "kind": "shared", "owner_user_id": nil, "account_ids": []string{f.account}, "api_key_ids": []string{f.key}}, idem("pool"), 201)
	f.poolRecord = pool
	f.pool = pool["id"].(string)
	draft := h.want(owner, "POST", "/api/v1/code/routes", map[string]any{"project_id": f.project, "slug": "coding", "max_body_bytes": maxBody, "pool_id": f.pool, "models": []string{"native-model"}, "enabled": true}, idem("route"), 201)
	headers := etagHeader(draft)
	headers["Idempotency-Key"] = "publish"
	published := h.want(owner, "POST", "/api/v1/code/routes/"+draft["id"].(string)+"/publish", nil, headers, 200)
	f.routeRecord = published
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
	f.exec(t, `INSERT INTO olp.provider_credentials(id,provider_id,version,plugin_digest,principal,grant_facts,profile_id) SELECT $1,id,$3,'fixture-digest',$4,'{}',configuration->>'profile_id' FROM olp.providers WHERE id=$2`, id, f.provider, version, principal)
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

func TestCodeFoundationInitialRecoverySettlesBeforeReplacementReservation(t *testing.T) {
	f := newCodeFixture(t)
	draft := f.h.want(f.owner, "PUT", "/api/v1/code/routes/"+f.route.ID, map[string]any{
		"project_id": f.project, "slug": f.route.Slug, "pool_id": f.pool, "models": f.route.Models, "enabled": true, "initial_recovery_attempts": 1,
	}, etagHeader(f.routeRecord), 200)
	published := f.h.want(f.owner, "POST", "/api/v1/code/routes/"+f.route.ID+"/publish", nil, withMatch(draft, idem("bounded-recovery-publish")), 200)
	route := codePublicDecode[codemode.Route](t, published)
	f.h.want(f.owner, "POST", "/api/v1/code/budgets", map[string]any{"project_id": f.project, "route_id": route.ID, "api_key_id": nil, "daily_tokens": 10, "monthly_tokens": 10, "enabled": true}, idem("recovery-budget"), 201)
	secondCredential := access.NewID()
	f.addCredential(t, secondCredential, "replacement-principal", 2)
	secondAccount := f.h.want(f.owner, "POST", "/api/v1/code/accounts", map[string]any{"project_id": f.project, "provider_id": f.provider, "credential_id": secondCredential, "name": "Replacement fixture", "enabled": true, "models": route.Models}, idem("replacement-account"), 201)
	f.h.want(f.owner, "PUT", "/api/v1/code/pools/"+f.pool, map[string]any{"project_id": f.project, "name": "Recovery pool", "kind": "shared", "account_ids": []string{f.account, secondAccount["id"].(string)}, "api_key_ids": []string{f.key}}, etagHeader(f.poolRecord), 200)
	in := resources.CodeAdmission{Route: route, APIKeyID: f.key, Providers: []string{f.provider}, InitialRecovery: true,
		Operation: codemode.Operation{Name: "responses", Model: "native-model", Identity: codemode.Identity{Conversation: "reserved-recovery"}}, Bound: &codemode.TokenBound{Tokens: 10, Evidence: "controlled-test-bound"}}
	first, err := f.store.Admit(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.MarkDispatched(t.Context(), first.Attempt.ID); err != nil {
		t.Fatal(err)
	}
	replacement := resources.CodeAdmission{Route: route, APIKeyID: f.key, Providers: in.Providers, InitialRecovery: true, Operation: in.Operation, Bound: in.Bound,
		Recovery: &resources.CodeRecovery{From: first.Attempt.ID, Number: 1}}
	_, err = f.store.Admit(t.Context(), replacement)
	codeRefusal(t, err, "code_recovery_unsafe")
	rejectionStatus := 429
	if err := f.store.ObserveOutcome(t.Context(), first.Attempt.ID, codemode.Outcome{Origin: "upstream", Kind: "rejected", UpstreamStatus: &rejectionStatus, ObservedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ConfirmNonexecution(t.Context(), first.Attempt.ID, 429); err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	if err := f.store.Settle(t.Context(), first.Attempt.ID, codemode.Usage{Total: &zero}); err != nil {
		t.Fatal(err)
	}
	next, err := f.store.Admit(t.Context(), replacement)
	if err != nil {
		t.Fatal("proven rejection did not release capacity before replacement", err)
	}
	if first.Attempt.AccountID == next.Attempt.AccountID || first.Attempt.BindingID != next.Attempt.BindingID {
		t.Fatal("replacement lost original attribution or durable conversation identity")
	}
	if err := f.store.Abort(t.Context(), next.Attempt.ID); err != nil {
		t.Fatal(err)
	}
	_, err = f.store.Admit(t.Context(), replacement)
	codeRefusal(t, err, "code_recovery_unsafe")
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
	observeHealth(codemode.HealthObservation{Status: "quota_limited", ObservedAt: observed, RetryAt: &retry})
	observeHealth(codemode.HealthObservation{Status: "healthy", ObservedAt: observed.Add(-time.Minute)})
	observeHealth(codemode.HealthObservation{Status: "quota_limited", ObservedAt: observed.Add(time.Minute)})
	blocked := readAccount()
	if blocked.Eligible || blocked.Health != "quota_limited" || blocked.UnavailableUntil == nil || !blocked.UnavailableUntil.Equal(retry) || blocked.HealthObservedAt == nil || !blocked.HealthObservedAt.Equal(observed.Add(time.Minute)) {
		t.Fatalf("stale or partial observation erased cooldown %+v", blocked)
	}
	shorter := observed.Add(90 * time.Second)
	observeHealth(codemode.HealthObservation{Status: "quota_limited", ObservedAt: shorter, RetryAt: &shorter})
	shortened := readAccount()
	if shortened.Eligible || shortened.UnavailableUntil == nil || !shortened.UnavailableUntil.Equal(retry) {
		t.Fatalf("shorter concurrent failure erased active reset %+v", shortened)
	}
	observeHealth(codemode.HealthObservation{Status: "healthy", ObservedAt: observed.Add(2 * time.Minute)})
	completed := readAccount()
	if completed.Eligible || completed.Health != "quota_limited" || completed.UnavailableUntil == nil || !completed.UnavailableUntil.Equal(retry) || completed.HealthObservedAt == nil || !completed.HealthObservedAt.Equal(*shortened.HealthObservedAt) {
		t.Fatalf("overlapping completion erased active retry deadline %+v", completed)
	}
	_, cooldownErr := f.store.Admit(t.Context(), f.input("during-retry-after", "", nil))
	codeRefusal(t, cooldownErr, "code_account_unavailable")
	extended := retry.Add(time.Hour)
	if err := f.store.ObserveCooldown(t.Context(), f.account, extended); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ObserveCooldown(t.Context(), f.account, shorter); err != nil {
		t.Fatal(err)
	}
	observeHealth(codemode.HealthObservation{Status: "unavailable", ObservedAt: observed.Add(150 * time.Second)})
	concurrent := readAccount()
	if concurrent.Eligible || concurrent.UnavailableUntil == nil || !concurrent.UnavailableUntil.Equal(extended) {
		t.Fatalf("shorter recovery or transport observation erased active reset %+v", concurrent)
	}
	_, concurrentErr := f.store.Admit(t.Context(), f.input("after-shorter-concurrent-failure", "", nil))
	codeRefusal(t, concurrentErr, "code_account_unavailable")
	expired := now.Add(-time.Minute)
	f.exec(t, `UPDATE olp.code_accounts SET unavailable_until=$2 WHERE id=$1`, f.account, expired)
	observeHealth(codemode.HealthObservation{Status: "quota_limited", ObservedAt: observed.Add(3 * time.Minute), RetryAt: &expired})
	recovered := readAccount()
	if !recovered.Eligible || recovered.Health != "quota_limited" || recovered.Allowance != nil {
		t.Fatalf("retry deadline invented allowance or failed to recover %+v", recovered)
	}
	if _, err := f.store.Admit(t.Context(), f.input("after-retry-after", "", nil)); err != nil {
		t.Fatalf("expired retry deadline still blocks admission %v", err)
	}
	if err := f.store.ObserveAllowance(t.Context(), f.account, codemode.Allowance{RemainingTokens: new(int64(0)), ObservedAt: observed, TokenObservation: &codemode.CountObservation{ObservedAt: observed, ResetsAt: &retry}}); err != nil {
		t.Fatal(err)
	}
	observeHealth(codemode.HealthObservation{Status: "healthy", ObservedAt: observed.Add(4 * time.Minute)})
	exhausted := readAccount()
	if exhausted.Eligible || exhausted.Health != "healthy" || exhausted.UnavailableUntil != nil || exhausted.Allowance == nil || *exhausted.Allowance.RemainingTokens != 0 {
		t.Fatalf("serving recovery bypassed exhausted allowance %+v", exhausted)
	}
	_, allowanceErr := f.store.Admit(t.Context(), f.input("after-retry-with-exhaustion", "", nil))
	codeRefusal(t, allowanceErr, "code_account_unavailable")
	if err := f.store.ObserveAllowance(t.Context(), f.account, codemode.Allowance{RemainingTokens: new(int64(1)), ObservedAt: observed.Add(5 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if !readAccount().Eligible {
		t.Fatal("fresh allowance observation did not restore eligibility after serving recovery")
	}
	f.exec(t, `UPDATE olp.provider_grants SET lapsed_at=now() WHERE credential_id=$1`, f.credential)
	rejected := readAccount()
	if rejected.Eligible || rejected.GrantState != "lapsed" || rejected.Health != "healthy" {
		t.Fatalf("refresh rejection conflated with serving health %+v", rejected)
	}
	_, grantErr := f.store.Admit(t.Context(), f.input("after-retry-with-lapsed-grant", "", nil))
	codeRefusal(t, grantErr, "code_account_unavailable")
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

func TestCodeFoundationExactAliasesPreservePinsAcrossPublication(t *testing.T) {
	f := newCodeFixtureWithProfile(t, codexauth.ProfileID)
	if f.route.ModelRewriting || len(f.route.Aliases) != 0 {
		t.Fatal("model rewriting enabled by default")
	}
	f.exec(t, `UPDATE olp.code_accounts SET models='["native-model","native-next"]' WHERE id=$1`, f.account)
	write := func(target string, rewriting bool, models []string, status int) map[string]any {
		return f.h.want(f.owner, "PUT", "/api/v1/code/routes/"+f.route.ID, map[string]any{
			"project_id": f.project, "slug": f.route.Slug, "pool_id": f.pool, "models": models,
			"aliases": map[string]string{"team-code": target}, "model_rewriting": rewriting, "initial_recovery_attempts": 1, "enabled": true,
		}, map[string]string{"If-Match": `"` + f.route.ETag + `"`}, status)
	}
	models := []string{"native-model", "native-next"}
	write("native-model", false, models, 422)
	write("unpublished-target", true, models, 422)
	publish := func(target string, models []string, idempotency string) {
		draft := write(target, true, models, 200)
		headers := etagHeader(draft)
		headers["Idempotency-Key"] = idempotency
		published := f.h.want(f.owner, "POST", "/api/v1/code/routes/"+f.route.ID+"/publish", nil, headers, 200)
		f.routeRecord = published
		encoded, _ := json.Marshal(published)
		if err := json.Unmarshal(encoded, &f.route); err != nil || !f.route.ModelRewriting || f.route.Aliases["team-code"] != target || f.route.MessagesToCodex || f.route.InitialRecoveryAttempts != 1 {
			t.Fatalf("exact alias publication %+v, %v", f.route, err)
		}
	}
	publish("native-model", models, "alias-first")
	config := f.h.want(f.owner, "GET", "/api/v1/code/routes/"+f.route.ID+"/client-config?client=codex&model=team-code&gateway_url=https://olp.example", nil, nil, 200)
	if config["model"] != "team-code" || !strings.Contains(config["configuration"].(string), `team-code`) {
		t.Fatal("published alias missing from setup", config)
	}
	admit := func(conversation, model string) (resources.CodePermit, error) {
		input := f.input(conversation, "", nil)
		input.Operation.Model = model
		input.InitialRecovery = true
		return f.store.Admit(t.Context(), input)
	}
	first, err := admit("alias-existing", "team-code")
	if err != nil || !first.InitialRecovery || first.Pin.Model != "team-code" || first.Pin.TargetModel != "native-model" || first.Attempt.Model != "native-model" {
		t.Fatalf("alias first admission %+v, %v", first, err)
	}
	native, err := admit("native-existing", "native-model")
	if err != nil {
		t.Fatal(err)
	}
	publish("native-next", models, "alias-repointed")
	continuedInput := f.input("alias-existing", "", nil)
	continuedInput.Operation.Model = "team-code"
	continuedInput.Providers = nil
	continuedInput.ModelProviders = map[string][]string{"native-model": {f.provider}, "native-next": nil}
	continued, err := f.store.Admit(t.Context(), continuedInput)
	if err != nil || continued.Pin.TargetModel != first.Pin.TargetModel || continued.Account.ID != first.Account.ID || continued.Attempt.Model != first.Attempt.Model {
		t.Fatalf("republish moved alias pin %+v, %v", continued, err)
	}
	ineligibleInput := f.input("alias-existing", "", nil)
	ineligibleInput.Operation.Model = "team-code"
	ineligibleInput.ModelProviders = map[string][]string{"native-model": nil, "native-next": {f.provider}}
	_, ineligibleErr := f.store.Admit(t.Context(), ineligibleInput)
	codeRefusal(t, ineligibleErr, "code_account_unavailable")
	fresh, err := admit("alias-new", "team-code")
	if err != nil || fresh.Pin.TargetModel != "native-next" || fresh.Attempt.Model != "native-next" {
		t.Fatalf("new conversation ignored repointed alias %+v, %v", fresh, err)
	}
	continuedNative, err := admit("native-existing", "native-model")
	if err != nil || continuedNative.Pin.TargetModel != "native-model" || continuedNative.Account.ID != native.Account.ID {
		t.Fatalf("pre-alias native pin changed %+v, %v", continuedNative, err)
	}
	publish("native-next", []string{"native-next"}, "alias-old-target-removed")
	_, err = admit("alias-existing", "team-code")
	codeRefusal(t, err, "code_model_denied")
	f.exec(t, `UPDATE olp.code_accounts SET health='unavailable' WHERE id=$1`, f.account)
	_, err = admit("alias-unavailable", "team-code")
	codeRefusal(t, err, "code_account_unavailable")
}

func TestCodeFoundationInitialRecoveryRefreshPreservesConcurrentCooldown(t *testing.T) {
	f := newCodeFixture(t)
	draft := f.h.want(f.owner, "PUT", "/api/v1/code/routes/"+f.route.ID, map[string]any{
		"project_id": f.project, "slug": f.route.Slug, "pool_id": f.pool, "models": f.route.Models, "enabled": true, "initial_recovery_attempts": 1,
	}, etagHeader(f.routeRecord), 200)
	published := f.h.want(f.owner, "POST", "/api/v1/code/routes/"+f.route.ID+"/publish", nil, withMatch(draft, idem("concurrent-cooldown-publish")), 200)
	route := codePublicDecode[codemode.Route](t, published)
	input := f.input("refresh-with-concurrent-cooldown", "", nil)
	input.Route, input.InitialRecovery = route, true
	first, err := f.store.Admit(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.MarkDispatched(t.Context(), first.Attempt.ID); err != nil {
		t.Fatal(err)
	}
	status := 401
	if err := f.store.ObserveOutcome(t.Context(), first.Attempt.ID, codemode.Outcome{Origin: "upstream", Kind: "rejected", UpstreamStatus: &status, ObservedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ConfirmNonexecution(t.Context(), first.Attempt.ID, status); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Settle(t.Context(), first.Attempt.ID, codemode.Usage{Total: new(int64(0))}); err != nil {
		t.Fatal(err)
	}
	var generation int64
	if err := f.h.Pool.QueryRow(t.Context(), `UPDATE olp.provider_grants SET generation=generation+1 WHERE credential_id=$1 RETURNING generation`, f.credential).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	observed := time.Now().UTC().Truncate(time.Microsecond)
	rejectedUntil := observed.Add(time.Minute)
	concurrentUntil := rejectedUntil.Add(time.Hour)
	replacement := input
	replacement.Recovery = &resources.CodeRecovery{From: first.Attempt.ID, Number: 1, AccountID: f.account, CredentialID: f.credential, GrantGeneration: generation - 1, RejectedUntil: rejectedUntil}
	for index, health := range []string{"quota_limited", "unavailable"} {
		if err := f.store.ObserveHealth(t.Context(), f.account, codemode.HealthObservation{Status: health, ObservedAt: observed.Add(time.Duration(index) * time.Second), RetryAt: &concurrentUntil}); err != nil {
			t.Fatal(err)
		}
		_, recoveryErr := f.store.Admit(t.Context(), replacement)
		codeRefusal(t, recoveryErr, "code_account_unavailable")
		accounts := codePublicDecode[[]codemode.Account](t, f.h.want(f.owner, "GET", "/api/v1/code/accounts?project_id="+f.project, nil, nil, 200)["items"])
		if len(accounts) != 1 || accounts[0].Eligible || accounts[0].Health != health || accounts[0].UnavailableUntil == nil || !accounts[0].UnavailableUntil.Equal(concurrentUntil) {
			t.Fatalf("refreshed grant erased concurrent cooldown %+v", accounts)
		}
	}
}

func TestCodeFoundationModelPoolRecoveryPreservesRequestedModelAndAuthority(t *testing.T) {
	for _, test := range []struct {
		name      string
		pool      bool
		portable  bool
		available bool
		change    string
		refusal   string
	}{
		{name: "exact default", portable: true, available: true, refusal: "code_account_unavailable"},
		{name: "explicit pool", pool: true, portable: true, available: true},
		{name: "unavailable alternate", pool: true, portable: true, refusal: "code_account_unavailable"},
		{name: "unportable state", pool: true, available: true, refusal: "code_recovery_unsafe"},
		{name: "live pool authority revoked", pool: true, portable: true, available: true, change: "authority", refusal: "code_pool_denied"},
		{name: "qualified bound unavailable", pool: true, portable: true, available: true, change: "bound", refusal: "code_token_bound_unavailable"},
		{name: "current token budget exhausted", pool: true, portable: true, available: true, change: "budget", refusal: "code_token_budget_exhausted"},
		{name: "qualified bound reserved", pool: true, portable: true, available: true, change: "qualified"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCodeFixtureWithProfile(t, codexauth.ProfileID)
			credential := access.NewID()
			f.exec(t, `INSERT INTO olp.provider_credentials(id,provider_id,version,profile_id,plugin_digest,principal,grant_facts) VALUES($1,$2,2,$3,'fixture-digest','alternate-principal','{}')`, credential, f.provider, codexauth.ProfileID)
			f.exec(t, `INSERT INTO olp.provider_grants(credential_id) VALUES($1)`, credential)
			alternate := f.h.want(f.owner, "POST", "/api/v1/code/accounts", map[string]any{
				"project_id": f.project, "provider_id": f.provider, "credential_id": credential,
				"name": "Alternate model account", "enabled": true, "models": []string{"native-next"},
			}, idem("model-pool-account"), 201)
			pool := f.h.want(f.owner, "POST", "/api/v1/code/pools", map[string]any{
				"project_id": f.project, "name": "Model recovery", "kind": "shared", "owner_user_id": nil,
				"account_ids": []string{f.account, alternate["id"].(string)}, "api_key_ids": []string{f.key},
			}, idem("model-pool"), 201)
			pools := func() map[string][]string {
				if test.pool {
					return map[string][]string{"team-code": {"native-model", "native-next"}}
				}
				return map[string][]string{}
			}()
			draft := f.h.want(f.owner, "PUT", "/api/v1/code/routes/"+f.route.ID, map[string]any{
				"project_id": f.project, "slug": f.route.Slug, "pool_id": pool["id"], "models": []string{"native-model", "native-next"},
				"aliases": map[string]string{"team-code": "native-model"}, "model_rewriting": true,
				"model_pools": pools, "initial_recovery_attempts": 1, "enabled": true,
			}, etagHeader(f.routeRecord), 200)
			published := f.h.want(f.owner, "POST", "/api/v1/code/routes/"+f.route.ID+"/publish", nil, withMatch(draft, idem("model-pool-publish")), 200)
			route := codePublicDecode[codemode.Route](t, published)
			if len(route.ModelPools) != len(pools) {
				t.Fatal("publication lost explicit model pool", published)
			}
			bound := func() *codemode.TokenBound {
				if test.change == "qualified" {
					return &codemode.TokenBound{Tokens: 5, Evidence: "controlled-test-bound"}
				}
				return nil
			}()
			source := &routing.Snapshot{Generation: routing.Generation{ID: access.NewID(), Ordinal: 1}, CodeRoutes: map[string]codemode.Route{route.Slug: route},
				CodeConnections: map[string]routing.Configuration{route.RevisionID + ":" + f.provider: {Kind: "plugin", AuthMode: "grant", ProfileID: codexauth.ProfileID}}}
			if err := source.Compile(); err != nil {
				t.Fatal(err)
			}
			input := resources.CodeAdmission{Route: route, APIKeyID: f.key, Providers: []string{f.provider}, ModelSource: source, ModelPath: "responses", ModelTransport: codeadapter.HTTP, Bound: bound, InitialRecovery: test.portable,
				Operation: codemode.Operation{Name: "responses.create", Model: "team-code", Identity: codemode.Identity{Conversation: "model-recovery"}}}
			first, err := f.store.Admit(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.store.MarkDispatched(t.Context(), first.Attempt.ID); err != nil {
				t.Fatal(err)
			}
			status := 429
			if err := f.store.ObserveOutcome(t.Context(), first.Attempt.ID, codemode.Outcome{Origin: "upstream", Kind: "rejected", UpstreamStatus: &status, ObservedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			if err := f.store.ConfirmNonexecution(t.Context(), first.Attempt.ID, status); err != nil {
				t.Fatal(err)
			}
			if err := f.store.Settle(t.Context(), first.Attempt.ID, codemode.Usage{Total: new(int64(0))}); err != nil {
				t.Fatal(err)
			}
			if !test.available {
				f.h.want(f.owner, "PUT", "/api/v1/code/accounts/"+alternate["id"].(string), map[string]any{
					"project_id": f.project, "provider_id": f.provider, "credential_id": credential,
					"name": "Alternate model account", "enabled": false, "models": []string{"native-next"},
				}, etagHeader(alternate), 200)
			}
			switch test.change {
			case "authority":
				f.h.want(f.owner, "PUT", "/api/v1/code/pools/"+pool["id"].(string), map[string]any{
					"project_id": f.project, "name": "Model recovery", "kind": "shared", "owner_user_id": nil,
					"account_ids": []string{f.account, alternate["id"].(string)}, "api_key_ids": []string{},
				}, etagHeader(pool), 200)
			case "bound", "budget", "qualified":
				f.h.want(f.owner, "POST", "/api/v1/code/budgets", map[string]any{
					"project_id": f.project, "route_id": route.ID, "api_key_id": nil, "daily_tokens": 10, "monthly_tokens": 100, "enabled": true,
				}, idem("model-recovery-budget"), 201)
			}
			recoveryBound := func() *codemode.TokenBound {
				if test.change == "budget" {
					return &codemode.TokenBound{Tokens: 20, Evidence: "controlled-test-bound"}
				}
				return bound
			}()
			recovery := resources.CodeAdmission{Route: route, APIKeyID: f.key, Providers: input.Providers, ModelSource: source, ModelPath: input.ModelPath, ModelTransport: input.ModelTransport, Bound: recoveryBound, InitialRecovery: test.portable,
				Operation: input.Operation, Recovery: &resources.CodeRecovery{From: first.Attempt.ID, Number: 1}}
			recovered, err := f.store.Admit(t.Context(), recovery)
			if test.refusal != "" {
				codeRefusal(t, err, test.refusal)
				if test.change == "bound" || test.change == "budget" {
					retry := resources.CodeAdmission{Route: route, APIKeyID: f.key, Providers: input.Providers, ModelSource: source, ModelPath: input.ModelPath, ModelTransport: input.ModelTransport, Operation: input.Operation,
						Bound: &codemode.TokenBound{Tokens: 5, Evidence: "controlled-test-bound"}}
					unchanged, retryErr := f.store.Admit(t.Context(), retry)
					if retryErr != nil || unchanged.Pin.TargetModel != first.Pin.TargetModel || unchanged.Account.ID != first.Account.ID {
						t.Fatalf("refused budget moved model or account %+v, %v", unchanged, retryErr)
					}
				}
				return
			}
			if err != nil || recovered.Pin.Model != "team-code" || recovered.Pin.TargetModel != "native-next" || recovered.Account.ID != alternate["id"] || recovered.Attempt.Model != "native-next" || recovered.Attempt.RequestedModel == nil || *recovered.Attempt.RequestedModel != "team-code" {
				t.Fatalf("explicit model recovery lost selection or ownership %+v, %v", recovered, err)
			}
			if test.change == "qualified" && (recovered.Attempt.ReservedTokens != bound.Tokens || recovered.Attempt.BoundEvidence == nil || *recovered.Attempt.BoundEvidence != bound.Evidence) {
				t.Fatalf("model change lost qualified reservation %+v", recovered.Attempt)
			}
			if err := f.store.Abort(t.Context(), recovered.Attempt.ID); err != nil {
				t.Fatal(err)
			}
			continued, err := f.store.Admit(t.Context(), input)
			if err != nil || continued.Account.ID != recovered.Account.ID || continued.Pin.TargetModel != recovered.Pin.TargetModel {
				t.Fatalf("continuation lost recovered model %+v, %v", continued, err)
			}
			attempts := codePublicDecode[[]codemode.Attempt](t, f.h.want(f.owner, "GET", "/api/v1/code/attempts?route_id="+route.ID, nil, nil, 200)["items"])
			diagnosed := slices.IndexFunc(attempts, func(attempt codemode.Attempt) bool { return attempt.ID == recovered.Attempt.ID })
			if diagnosed < 0 || attempts[diagnosed].RequestedModel == nil || *attempts[diagnosed].RequestedModel != "team-code" || attempts[diagnosed].Model != recovered.Pin.TargetModel {
				t.Fatalf("diagnostics lost requested or selected model %+v", attempts)
			}
		})
	}
}

func newPaidCodeFixture(t *testing.T, endpoints ...string) (*codeFixture, routing.Provider, routing.Slot) {
	t.Helper()
	f := newCodeFixture(t)
	endpoint := func() string {
		if len(endpoints) > 0 {
			return endpoints[0]
		}
		return "https://fixture.invalid"
	}()
	created := f.h.want(f.owner, "POST", "/api/v1/providers", map[string]any{"project_id": f.project, "name": "Metered fixture", "configuration": map[string]any{"kind": "openai", "auth_mode": "api_key", "profile_id": "openai-responses", "profile_revision": connectors.ProfileRevision, "endpoint": endpoint, "options": map[string]any{"vendor_id": "openai"}}, "credential": "fixture-paid-secret", "model": "native-model"}, idem("paid-provider"), 201)
	providerID := created["id"].(string)
	var slot routing.Slot
	slot.Enabled = true
	slot.Weight = 1
	var credential string
	var version int
	if err := f.h.Pool.QueryRow(t.Context(), `SELECT s.id::text,s.credential_id::text,c.version FROM olp.provider_slots s JOIN olp.provider_credentials c ON c.id=s.credential_id WHERE s.provider_id=$1 AND s.is_default`, providerID).Scan(&slot.ID, &credential, &version); err != nil {
		t.Fatal(err)
	}
	slot.CredentialID = &credential
	slot.CredentialVersion = &version
	models, err := json.Marshal([]routing.RevisionModel{{ID: access.NewID(), UpstreamModel: "native-model", Capabilities: []routing.RevisionCapability{{Operation: "generation", Surface: "openai", Mode: "unary", Source: "certified"}}}})
	if err != nil {
		t.Fatal(err)
	}
	slots, err := json.Marshal([]routing.RevisionSlot{{Slot: slot, Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	revision := access.NewID()
	f.exec(t, `UPDATE olp.provider_models SET enabled=true WHERE provider_id=$1`, providerID)
	f.exec(t, `INSERT INTO olp.provider_revisions(id,provider_id,revision,name,configuration,models,slots,credential_version,source_etag,activated_by) SELECT $2,id,1,name,configuration,$3,$4,$5,etag,created_by FROM olp.providers WHERE id=$1`, providerID, revision, models, slots, version)
	f.exec(t, `UPDATE olp.providers SET state='active',active_revision=1,active_revision_id=$2 WHERE id=$1`, providerID, revision)
	draft := map[string]any{"project_id": f.project, "slug": f.route.Slug, "pool_id": f.pool, "models": f.route.Models, "enabled": true, "paid_participation": true, "paid_targets": map[string]codemode.PaidTarget{"native-model": {ProviderID: providerID, SlotID: slot.ID}}}
	saved := f.h.want(f.owner, "PUT", "/api/v1/code/routes/"+f.route.ID, draft, withMatch(f.routeRecord, idem("paid-draft")), 200)
	published := f.h.want(f.owner, "POST", "/api/v1/code/routes/"+f.route.ID+"/publish", nil, withMatch(saved, idem("paid-publish")), 200)
	encoded, err := json.Marshal(published)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &f.route); err != nil {
		t.Fatal(err)
	}
	tx, err := f.h.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	snapshot, err := runtime.ReadSnapshot(t.Context(), tx)
	if err != nil {
		t.Fatal(err)
	}
	provider, ok := snapshot.CodePaidConnections[f.route.RevisionID+":"+providerID]
	if !ok {
		t.Fatal("paid provider was not published into the serving snapshot")
	}
	return f, provider, slot
}

func TestCodePaidPublicationRetainsOwnershipAndMonetaryUncertainty(t *testing.T) {
	f, provider, slot := newPaidCodeFixture(t)
	offers, err := f.store.Models(t.Context(), f.route, f.key)
	models := codeclient.Catalog(f.route, offers)
	if err != nil || len(models) != 1 || !models[0].Paid || slices.ContainsFunc(offers, func(offer codeclient.Offer) bool { return offer.PaidProtocol != "" && offer.Adapter != "" }) {
		t.Fatalf("paid discovery %+v %v", models, err)
	}
	config := f.h.want(f.owner, "GET", "/api/v1/code/routes/"+f.route.ID+"/client-config?client=codex&model=native-model&gateway_url=https://olp.example", nil, nil, 200)
	if len(config["adapters"].([]any)) != 0 || len(config["paid_models"].([]any)) != 1 || !strings.Contains(config["configuration"].(string), "supports_websockets = false") {
		t.Fatalf("paid setup lost direct target metadata %+v", config)
	}
	in := resources.PaidCodeAdmission{CodeAdmission: f.input("paid-conversation", "", nil), Authority: access.Authority{ID: f.key, LookupID: f.key}, Provider: provider, Slot: slot}
	permit, err := f.store.AdmitPaid(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.MarkDispatched(t.Context(), permit.Attempt.ID); err != nil {
		t.Fatal(err)
	}
	total, input, output := int64(3), int64(2), int64(1)
	if err := f.store.Settle(t.Context(), permit.Attempt.ID, codemode.Usage{Total: &total, Input: &input, Output: &output}); err != nil {
		t.Fatal(err)
	}
	unrelated := f.input("unrelated", "", nil)
	if _, err := f.store.AdmitPaid(t.Context(), resources.PaidCodeAdmission{CodeAdmission: unrelated, Provider: provider, Slot: slot}); err != nil {
		t.Fatal(err)
	}
	attempts := f.h.want(f.owner, "GET", "/api/v1/code/attempts?route_id="+f.route.ID+"&binding_id="+permit.BindingID, nil, nil, 200)["items"].([]any)
	if len(attempts) != 1 {
		t.Fatalf("paid attempt disappeared %+v", attempts)
	}
	attempt := attempts[0].(map[string]any)
	if attempt["account_id"] != nil || attempt["binding_id"] != nil || attempt["paid_binding_id"] != permit.BindingID || attempt["paid_provider_id"] != provider.ID || attempt["state"] != "settled" || attempt["paid_monetary_state"] != "unknown" {
		t.Fatalf("token settlement hid paid money uncertainty %+v", attempt)
	}
	accounting := &limits.DebitStore{Pool: f.h.Pool}
	if err := accounting.CheckOwnerAccounting(t.Context(), f.key); !errors.Is(err, limits.ErrUninitializedCost) {
		t.Fatalf("unresolved paid use lost its durable monetary hold %v", err)
	}
	peer := access.NewID()
	f.addKey(t, peer)
	f.exec(t, `INSERT INTO olp.code_pool_keys(pool_id,api_key_id) VALUES($1,$2)`, f.pool, peer)
	independent := in
	independent.APIKeyID = peer
	independent.Authority = access.Authority{ID: peer, LookupID: peer}
	other, err := f.store.AdmitPaid(t.Context(), independent)
	if err != nil || other.BindingID == permit.BindingID {
		t.Fatalf("paid ownership crossed keys %+v %v", other, err)
	}
	if err := f.store.Abort(t.Context(), other.Attempt.ID); err != nil {
		t.Fatal(err)
	}
	bindings := f.h.want(f.owner, "GET", "/api/v1/code/bindings?api_key_id="+f.key, nil, nil, 200)["items"].([]any)
	if len(bindings) != 1 || bindings[0].(map[string]any)["id"] != permit.BindingID {
		t.Fatalf("paid binding disappeared %+v", bindings)
	}
	f.h.want(f.owner, "POST", "/api/v1/code/bindings/"+permit.BindingID+"/retire", nil, idem("retire-paid"), 200)
	_, err = f.store.AdmitPaid(t.Context(), in)
	codeRefusal(t, err, "code_binding_retired")
}

func TestCodePaidResponseReferencePreservesConversationOwnership(t *testing.T) {
	for _, scenario := range []struct {
		name, first, next, parent string
		refused                   bool
	}{{name: "same conversation", first: "root", next: "root"}, {name: "new child", first: "root", next: "child"}, {name: "generic affinity", first: "olp:affinity/one", next: "olp:affinity/two", refused: true}} {
		t.Run(scenario.name, func(t *testing.T) {
			f, provider, slot := newPaidCodeFixture(t)
			admit := func(conversation, parent, previous string) (resources.PaidCodePermit, error) {
				input := f.input(conversation, parent, nil)
				input.PreviousResponse = previous
				return f.store.AdmitPaid(t.Context(), resources.PaidCodeAdmission{CodeAdmission: input, Provider: provider, Slot: slot})
			}
			first, err := admit(scenario.first, "", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := f.store.MarkDispatched(t.Context(), first.Attempt.ID); err != nil {
				t.Fatal(err)
			}
			if err := f.store.ObserveReference(t.Context(), first.Attempt.ID, "paid-response"); err != nil {
				t.Fatal(err)
			}
			next, err := admit(scenario.next, scenario.parent, "paid-response")
			if scenario.refused {
				codeRefusal(t, err, "code_response_account_conflict")
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario.first == scenario.next && next.BindingID != first.BindingID {
				t.Fatal("same conversation changed paid ownership")
			}
			if scenario.first != scenario.next {
				page := f.h.want(f.owner, "GET", "/api/v1/code/bindings?api_key_id="+f.key, nil, nil, 200)
				bindings := page["items"].([]any)
				child := slices.IndexFunc(bindings, func(item any) bool { return item.(map[string]any)["id"] == next.BindingID })
				if child < 0 || bindings[child].(map[string]any)["parent_id"] != first.BindingID {
					t.Fatalf("paid response reference lost child ownership %+v", bindings)
				}
			}
		})
	}
}
