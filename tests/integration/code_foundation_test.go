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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
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

func newCodeFixture(t *testing.T) *codeFixture {
	t.Helper()
	h := newAccessHarness(t)
	owner := h.owner()
	f := &codeFixture{h: h, owner: owner, project: createProject(h, owner, "Coding"), key: access.NewID(), provider: access.NewID(), credential: access.NewID()}
	f.user = h.want(owner, "GET", "/api/v1/profile", nil, nil, 200)["id"].(string)
	f.exec(t, `INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by,project_id) VALUES($1,'Fixture coding subscription','plugin','draft','{"kind":"plugin","auth_mode":"grant","endpoint":"https://fixture.invalid"}',$2,$3,$4,$5)`, f.provider, access.NewID(), access.NewID(), f.user, f.project)
	f.addCredential(t, f.credential, "fixture-principal", 1)
	f.addKey(t, f.key)
	f.accountRecord = h.want(owner, "POST", "/api/v1/code/accounts", map[string]any{"project_id": f.project, "provider_id": f.provider, "credential_id": f.credential, "name": "Fixture subscription", "enabled": true, "models": []string{"native-model"}}, idem("account"), 201)
	f.account = f.accountRecord["id"].(string)
	pool := h.want(owner, "POST", "/api/v1/code/pools", map[string]any{"project_id": f.project, "name": "Shared coding", "kind": "shared", "owner_user_id": nil, "account_ids": []string{f.account}, "api_key_ids": []string{f.key}}, idem("pool"), 201)
	f.pool = pool["id"].(string)
	draft := h.want(owner, "POST", "/api/v1/code/routes", map[string]any{"project_id": f.project, "slug": "coding", "pool_id": f.pool, "models": []string{"native-model"}, "enabled": true}, idem("route"), 201)
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

func TestCodeFoundationRotationAndLiveAuthority(t *testing.T) {
	f := newCodeFixture(t)
	initial, err := f.store.Admit(t.Context(), f.input("rotation", "", nil))
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE olp.provider_grants SET generation=generation+1 WHERE credential_id=$1`, f.credential)
	if err = f.store.ObserveHealth(t.Context(), f.account, "quota_limited"); err != nil {
		t.Fatal(err)
	}
	_, err = f.store.Admit(t.Context(), f.input("rotation", "", nil))
	codeRefusal(t, err, "code_account_unavailable")
	if err = f.store.ObserveHealth(t.Context(), f.account, "healthy"); err != nil {
		t.Fatal(err)
	}
	_, err = f.store.Admit(t.Context(), f.input("rotation", "", nil))
	codeRefusal(t, err, "code_account_unavailable")
	if codeObservedAccount(t, f).Health != "quota_limited" {
		t.Fatal("late success cleared the quota cooldown")
	}
	f.exec(t, `UPDATE olp.code_accounts SET unavailable_until=now()-interval '1 second' WHERE id=$1`, f.account)
	if err = f.store.ObserveHealth(t.Context(), f.account, "healthy"); err != nil {
		t.Fatal(err)
	}
	next, err := f.store.Admit(t.Context(), f.input("rotation", "", nil))
	if err != nil || next.Binding.ID != initial.Binding.ID || next.Account.Principal != initial.Account.Principal {
		t.Fatal("refresh changed pin", err)
	}
	rotated := access.NewID()
	f.addCredential(t, rotated, "fixture-principal", 2)
	body := map[string]any{"project_id": f.project, "provider_id": strings.ToUpper(f.provider), "credential_id": strings.ToUpper(rotated), "name": "Rotated", "enabled": true, "models": []string{"native-model"}}
	updated := f.h.want(f.owner, "PUT", "/api/v1/code/accounts/"+f.account, body, etagHeader(f.accountRecord), 200)
	if updated["provider_id"] != f.provider || updated["credential_id"] != rotated {
		t.Fatalf("account IDs = %v/%v, want %s/%s", updated["provider_id"], updated["credential_id"], f.provider, rotated)
	}
	next, err = f.store.Admit(t.Context(), f.input("rotation", "", nil))
	if err != nil || next.Binding.ID != initial.Binding.ID || next.Account.CredentialID != rotated {
		t.Fatal("same-principal rotation lost pin", err)
	}
	foreign := access.NewID()
	f.addCredential(t, foreign, "different-principal", 3)
	body["credential_id"] = foreign
	f.h.want(f.owner, "PUT", "/api/v1/code/accounts/"+f.account, body, etagHeader(updated), 422)
	alternative := f.h.want(f.owner, "POST", "/api/v1/code/accounts", body, idem("alternative"), 201)
	f.exec(t, `INSERT INTO olp.code_pool_accounts(pool_id,account_id) VALUES($1,$2)`, f.pool, alternative["id"])
	f.exec(t, `UPDATE olp.provider_credentials SET revoked_at=now() WHERE id=$1`, rotated)
	_, err = f.store.Admit(t.Context(), f.input("rotation", "", nil))
	codeRefusal(t, err, "code_account_unavailable")
	fresh, err := f.store.Admit(t.Context(), f.input("fresh", "", nil))
	if err != nil || fresh.Account.ID != alternative["id"] {
		t.Fatal("fresh root did not select eligible account", err)
	}
	f.exec(t, `DELETE FROM olp.code_pool_keys WHERE api_key_id=$1`, f.key)
	_, err = f.store.Admit(t.Context(), f.input("fresh", "", nil))
	codeRefusal(t, err, "code_pool_denied")
	f.exec(t, `INSERT INTO olp.code_pool_keys(pool_id,api_key_id) VALUES($1,$2)`, f.pool, f.key)
	in := f.input("fresh", "", nil)
	in.Operation.Model = "not-allowed"
	_, err = f.store.Admit(t.Context(), in)
	codeRefusal(t, err, "code_model_denied")
	f.exec(t, `UPDATE olp.api_keys SET revoked_at=now() WHERE id=$1`, f.key)
	_, err = f.store.Admit(t.Context(), f.input("fresh", "", nil))
	codeRefusal(t, err, "code_permission_denied")
}

// A tree holds one account of each subscription family, which the frozen
// connections of the route's revision identify. Editing a provider's draft
// configuration after publication must not move that comparison: it could
// reject a valid account of another frozen family or admit a second account
// of a family the tree already pins.
func TestCodeFoundationFrozenFamiliesSurviveProviderDrafts(t *testing.T) {
	f := newCodeFixture(t)
	h, owner := f.h, f.owner
	f.exec(t, `UPDATE olp.api_keys SET policy=jsonb_set(policy,'{allowed_routes}','["coding","mixed"]') WHERE id=$1`, f.key)
	// Two providers of the Z.ai family through different profiles, and one
	// OpenCode Go provider, frozen into the mixed route's connections.
	add := func(name, profile string, models []string) (provider string, account map[string]any) {
		provider = access.NewID()
		f.exec(t, `INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by,project_id) VALUES($1,$2,'plugin','draft',$3,$4,$5,$6,$7)`,
			provider, name, `{"kind":"plugin","auth_mode":"grant","profile_id":"`+profile+`"}`, access.NewID(), access.NewID(), f.user, f.project)
		credential := access.NewID()
		f.exec(t, `INSERT INTO olp.provider_credentials(id,provider_id,version,plugin_digest,principal,grant_facts) VALUES($1,$2,1,'fixture-digest',$3,'{}')`, credential, provider, "principal-"+profile)
		f.exec(t, `INSERT INTO olp.provider_grants(credential_id) VALUES($1)`, credential)
		account = h.want(owner, "POST", "/api/v1/code/accounts", map[string]any{"project_id": f.project, "provider_id": provider, "credential_id": credential, "name": name, "enabled": true, "models": models}, idem("account-"+profile), 201)
		return provider, account
	}
	zai, glm := add("ZAI plan", codeplans.ZAIProfile, []string{"glm-5.3"})
	bigmodel, kimiB := add("BigModel plan", codeplans.BigModelProfile, []string{"minimax-m3"})
	opencodego, kimiO := add("OpenCode Go", codeplans.OpenCodeGoProfile, []string{"minimax-m3"})
	glmID, kimiBID, kimiOID := glm["id"].(string), kimiB["id"].(string), kimiO["id"].(string)
	pool := h.want(owner, "POST", "/api/v1/code/pools", map[string]any{"project_id": f.project, "name": "Mixed pool", "kind": "shared", "owner_user_id": nil, "account_ids": []string{glmID, kimiBID, kimiOID}, "api_key_ids": []string{f.key}}, idem("mixed-pool"), 201)
	draft := h.want(owner, "POST", "/api/v1/code/routes", map[string]any{"project_id": f.project, "slug": "mixed", "pool_id": pool["id"], "models": []string{"glm-5.3", "minimax-m3"}, "enabled": true}, idem("mixed-route"), 201)
	published := h.want(owner, "POST", "/api/v1/code/routes/"+draft["id"].(string)+"/publish", nil, withMatch(draft, idem("mixed-publish")), 200)
	route := codePublicDecode[codemode.Route](t, published)
	in := func(conversation, model string) resources.CodeAdmission {
		return resources.CodeAdmission{Route: route, APIKeyID: f.key, Providers: []string{zai, bigmodel, opencodego}, Operation: codemode.Operation{Name: "messages.create", Model: model, Identity: codemode.Identity{Conversation: conversation}}}
	}
	edit := func(provider, profile string) {
		f.exec(t, `UPDATE olp.providers SET configuration=jsonb_set(configuration::jsonb,'{profile_id}',to_jsonb($2::text)) WHERE id=$1`, provider, profile)
	}
	permit, err := f.store.Admit(t.Context(), in("c1", "glm-5.3"))
	if err != nil || permit.Account.ID != glmID {
		t.Fatalf("GLM pin: %v %v", permit.Account.ID, err)
	}
	// The OpenCode provider's draft claims the Z.ai family: the tree must
	// still admit its account, the only frozen family it does not pin yet.
	edit(opencodego, codeplans.ZAIProfile)
	permit, err = f.store.Admit(t.Context(), in("c1", "minimax-m3"))
	if err != nil {
		t.Fatal("frozen OpenCode Go family refused after a draft edit:", err)
	}
	if permit.Pin.Model != "minimax-m3" || permit.Account.ID != kimiOID {
		t.Fatalf("minimax pinned %v, want the OpenCode Go account %v", permit.Account.ID, kimiOID)
	}
	// The BigModel provider's draft claims OpenCode Go: the tree must still
	// refuse it as the Z.ai family it already pins, not admit a second one.
	edit(bigmodel, codeplans.OpenCodeGoProfile)
	permit, err = f.store.Admit(t.Context(), in("c2", "glm-5.3"))
	if err != nil || permit.Account.ID != glmID {
		t.Fatalf("second tree's GLM pin: %v %v", permit.Account.ID, err)
	}
	permit, err = f.store.Admit(t.Context(), in("c2", "minimax-m3"))
	if err != nil {
		t.Fatal("second tree's minimax refused:", err)
	}
	if permit.Account.ID != kimiOID {
		t.Fatalf("second tree pinned %v, want %v: the BigModel account shares its frozen Z.ai family", permit.Account.ID, kimiOID)
	}
}

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
	f := newCodeFixture(t)
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
	var snapshot runtime.Snapshot
	if err := json.Unmarshal(document, &snapshot); err != nil {
		t.Fatal(err)
	}
	connection, ok := snapshot.CodeConnection(f.route, f.provider)
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
	compiled, err := runtime.Compile(t.Context(), tx)
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
	if err = f.store.RecordRefusal(t.Context(), f.route, f.key, "code_permission_denied"); err != nil {
		t.Fatal(err)
	}
	if err = f.store.RecordRefusal(t.Context(), f.route, f.key, "prompt: never persist me"); err == nil {
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

func TestCodeFoundationOverlappingBudgetsAndPartialUncertainty(t *testing.T) {
	f := newCodeFixture(t)
	for i, route := range []*string{nil, &f.route.ID} {
		f.h.want(f.owner, "POST", "/api/v1/code/budgets", map[string]any{"project_id": f.project, "route_id": route, "api_key_id": nil, "daily_tokens": 20, "monthly_tokens": 30, "enabled": true}, idem(fmt.Sprintf("budget-%d", i)), 201)
	}
	p, err := f.store.Admit(t.Context(), f.input("overlap", "", &codemode.TokenBound{Tokens: 20, Evidence: "controlled-test-bound"}))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.store.MarkDispatched(t.Context(), p.Attempt.ID); err != nil {
		t.Fatal(err)
	}
	input := int64(5)
	if err = f.store.Settle(t.Context(), p.Attempt.ID, codemode.Usage{Input: &input}); err != nil {
		t.Fatal(err)
	}
	var state string
	var measured *int64
	var partial int64
	if err = f.h.Pool.QueryRow(t.Context(), `SELECT state,reported_tokens,input_tokens FROM olp.code_attempts WHERE id=$1`, p.Attempt.ID).Scan(&state, &measured, &partial); err != nil || state != "uncertain" || measured != nil || partial != 5 {
		t.Fatal("partial usage was not retained as uncertainty", err)
	}
	f.store = &resources.CodeStore{Pool: f.h.Pool}
	_, err = f.store.Admit(t.Context(), f.input("blocked", "", &codemode.TokenBound{Tokens: 1, Evidence: "controlled-test-bound"}))
	codeRefusal(t, err, "code_token_budget_exhausted")
	actual := int64(21)
	if err = f.store.Settle(t.Context(), p.Attempt.ID, codemode.Usage{Total: &actual}); err != nil {
		t.Fatal(err)
	}
	var windows int
	if err = f.h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.code_token_windows WHERE reserved=0 AND measured=21`).Scan(&windows); err != nil || windows != 4 {
		t.Fatal("overlapping periods not settled", windows, err)
	}
	if err = f.h.Pool.QueryRow(t.Context(), `SELECT state FROM olp.code_attempts WHERE id=$1`, p.Attempt.ID).Scan(&state); err != nil || state != "bound_violation" {
		t.Fatal("bound violation not visible", state, err)
	}
	f.exec(t, `UPDATE olp.code_token_budgets SET daily_tokens=1000,monthly_tokens=1000`)
	_, err = f.store.Admit(t.Context(), f.input("violation", "", &codemode.TokenBound{Tokens: 1, Evidence: "controlled-test-bound"}))
	codeRefusal(t, err, "code_token_budget_exhausted")
}

func TestCodeFoundationPoolAssignmentsRequireOperatorAndPersonalOwner(t *testing.T) {
	f := newCodeFixture(t)
	dev := f.h.invite(f.owner, "code-developer@example.com", "developer")
	input := map[string]any{"project_id": f.project, "name": "Personal", "kind": "personal", "owner_user_id": f.user, "account_ids": []string{f.account}, "api_key_ids": []string{f.key}}
	f.h.want(dev, "POST", "/api/v1/code/pools", input, idem("self-grant"), 403)
	personal := f.h.want(f.owner, "POST", "/api/v1/code/pools", input, idem("personal"), 201)
	profile := f.h.want(dev, "GET", "/api/v1/profile", nil, nil, 200)
	addMember(f.h, f.owner, f.project, profile["id"].(string), "viewer")
	input["owner_user_id"] = profile["id"]
	f.h.want(f.owner, "POST", "/api/v1/code/pools", input, idem("foreign-owner"), 422)
	input["kind"] = "shared"
	input["owner_user_id"] = nil
	f.h.want(f.owner, "PUT", "/api/v1/code/pools/"+personal["id"].(string), input, etagHeader(personal), 422)
}
