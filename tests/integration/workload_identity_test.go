//go:build integration && oidctest

package integration_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/tyk-swe/olp/internal/gateway"
)

func TestWorkloadConfigurationPublishesBeforeGrantingResources(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "Published workloads")
	workloadProject(t, h, owner, project, map[string]any{"generation": []string{}})
	fixture := newWorkloadFixture(t, h, owner, project, []string{"generation"})
	created := h.want(owner, "POST", "/api/v1/toolsets", map[string]any{"project_id": project, "name": "workflow", "toolset": map[string]any{"tools": []any{}}}, idem("workload-toolset"), 201)
	exported := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	document := exported["document"].(map[string]any)
	publication := document["agent_publications"].([]any)[0].(map[string]any)
	publication["published"] = publication["draft"]
	issuer := document["workload_issuers"].([]any)[0].(map[string]any)
	mapping := issuer["mappings"].([]any)[0].(map[string]any)
	mapping["scopes"] = []string{"tools"}
	mapping["allowed_toolsets"] = []string{created["id"].(string)}
	body := map[string]any{"document": document}
	plan := h.want(owner, "POST", "/api/v1/configuration/plan", body, nil, 200)
	if len(plan["blockers"].([]any)) != 0 || len(plan["conflicts"].([]any)) != 0 {
		t.Fatalf("combined publication and workload promotion cannot be planned: %v", plan)
	}
	h.want(owner, "POST", "/api/v1/configuration/apply", body, idem("workload-publication-apply"), 200)
	read := h.want(owner, "GET", "/api/v1/toolsets/"+created["id"].(string), nil, nil, 200)
	if read["revision_id"] == nil {
		t.Fatal("workload grant committed without its publication")
	}
	h.want(owner, "DELETE", "/api/v1/toolsets/"+created["id"].(string), nil, withMatch(read, idem("retire-workload-toolset")), 204)
	currentIssuer := h.want(owner, "GET", "/api/v1/workload-issuers/"+fixture.issuer["id"].(string), nil, nil, 200)
	inputMapping := fixture.input["mappings"].([]any)[0].(map[string]any)
	inputMapping["scopes"] = []string{"tools"}
	inputMapping["allowed_toolsets"] = []string{created["id"].(string)}
	fixture.input["disabled_key_ids"] = []string{"current"}
	fixture.endpoint.Close()
	updated := h.want(owner, "PUT", "/api/v1/workload-issuers/"+fixture.issuer["id"].(string), fixture.input, etagHeader(currentIssuer), 200)
	inputMapping["allowed_toolsets"] = []string{}
	updated = h.want(owner, "PUT", "/api/v1/workload-issuers/"+fixture.issuer["id"].(string), fixture.input, etagHeader(updated), 200)
	inputMapping["allowed_toolsets"] = []string{created["id"].(string)}
	h.want(owner, "PUT", "/api/v1/workload-issuers/"+fixture.issuer["id"].(string), fixture.input, etagHeader(updated), 422)
}

func TestWorkloadConfigurationValidatesFinalTemplateReferences(t *testing.T) {
	for _, mode := range []string{"disable", "migrate"} {
		t.Run(mode, func(t *testing.T) {
			h := newAccessHarness(t)
			owner := h.owner()
			project := createProject(h, owner, "Workload template promotion")
			workloadProject(t, h, owner, project, map[string]any{"generation": []string{}})
			newWorkloadFixture(t, h, owner, project, []string{"generation"})
			exported := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
			document := exported["document"].(map[string]any)
			projectEntry := document["projects"].([]any)[0].(map[string]any)
			issuer := document["workload_issuers"].([]any)[0].(map[string]any)
			if mode == "disable" {
				issuer["enabled"] = false
				projectEntry["limit_templates"] = map[string]any{}
			} else {
				issuer["mappings"].([]any)[0].(map[string]any)["limit_template"] = "replacement"
				projectEntry["limit_templates"] = map[string]any{"replacement": map[string]any{"requests_per_minute": 100}}
			}
			h.want(owner, "POST", "/api/v1/configuration/apply", map[string]any{"document": document}, idem("workload-template-"+mode), 200)
		})
	}
}

func TestWorkloadIdentityMapsAndAccountsWithoutRetainingCredentials(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "Workloads"}, idem("project"), 201)
	projectID := project["id"].(string)
	base := "/api/v1/projects/" + projectID
	templates := h.want(owner, "GET", base+"/limit-templates", nil, nil, 200)
	h.want(owner, "PUT", base+"/limit-templates", map[string]any{"templates": map[string]any{"worker": map[string]any{"requests_per_minute": 2}}}, etagHeader(templates), 200)
	groups := h.want(owner, "GET", base+"/route-groups", nil, nil, 200)
	h.want(owner, "PUT", base+"/route-groups", map[string]any{"groups": map[string]any{"generation": []string{"workload-chat"}}}, etagHeader(groups), 200)
	vendor := newOpenAIFixture(t, "")
	provider := activeAzureProviderInProject(t, h, owner, "Workload provider", vendor.URL, &projectID, []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}})
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{"project_id": projectID, "slug": "workload-chat", "overall_timeout_ms": 10000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"}, "targets": []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}}}, idem("draft"), 201)
	path := "/api/v1/route-drafts/" + draft["id"].(string)
	validated := h.want(owner, "POST", path+"/validate", nil, etagHeader(draft), 200)
	h.want(owner, "POST", path+"/activate", nil, withMatch(validated, idem("activate")), 200)
	public, private, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: public, KeyID: "current", Algorithm: "EdDSA", Use: "sig"}}})
	}))
	defer jwks.Close()
	input := map[string]any{"name": "CI issuer", "issuer": jwks.URL, "jwks_url": jwks.URL, "enabled": true, "audiences": []string{"olp"}, "algorithms": []string{"EdDSA"}, "max_lifetime_seconds": 600, "disabled_key_ids": []string{}, "mappings": []any{map[string]any{"name": "build", "match": map[string]string{"/repository": "example/repo"}, "project_id": projectID, "limit_template": "worker", "route_groups": []string{"generation"}, "scopes": []string{"inference", "models_read"}, "end_user_claim": "/customer"}}}
	issuer := h.want(owner, "POST", "/api/v1/workload-issuers", input, idem("issuer"), 201)
	issuerPath := "/api/v1/workload-issuers/" + issuer["id"].(string)
	client, _, limiter := limSetup(t, "workload")
	h.Gateway.Admission = gateway.NewAdmission(limiter, nil, slog.New(slog.DiscardHandler))
	await := endUserAccounting(t, h, limiter)
	replica := newAccessHarnessOn(t, h.Pool, h.DBURL)
	replica.Gateway.Admission = gateway.NewAdmission(limiter, nil, slog.New(slog.DiscardHandler))
	replica.Gateway.Sink = h.Gateway.Sink
	h.refresh()
	replica.refresh()
	limSettleInMinute(t, client, 5*time.Second)
	subject := "private-workload-subject-sentinel"
	sign := func(subject, repository string) string {
		s, e := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: private}, (&jose.SignerOptions{}).WithHeader("kid", "current"))
		if e != nil {
			t.Fatal(e)
		}
		now := time.Now()
		body, _ := json.Marshal(map[string]any{"iss": jwks.URL, "aud": "olp", "sub": subject, "iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(), "repository": repository, "customer": "customer-1"})
		j, e := s.Sign(body)
		if e != nil {
			t.Fatal(e)
		}
		raw, e := j.CompactSerialize()
		if e != nil {
			t.Fatal(e)
		}
		return raw
	}
	token := sign(subject, "example/repo")
	for i, host := range []*accessHarness{h, replica, h} {
		want := 200
		if i == 2 {
			want = 429
		}
		status, body := attributionCall(t, host, "workload-chat", token, nil)
		if status != want {
			t.Fatalf("request %d: %d %s", i, status, body)
		}
		await()
	}
	var principal, digest string
	var count int
	if e = h.Pool.QueryRow(context.Background(), "SELECT id::text,workload_digest FROM olp.api_keys WHERE workload_issuer_id=$1", issuer["id"]).Scan(&principal, &digest); e != nil || len(digest) != 64 {
		t.Fatalf("principal: %v", e)
	}
	if e = h.Pool.QueryRow(context.Background(), "SELECT count(*) FROM olp.requests WHERE api_key_id=$1", principal).Scan(&count); e != nil || count != 3 {
		t.Fatalf("accounting owners: %d %v", count, e)
	}
	for _, table := range []string{"api_keys", "requests", "attempt_usage_facts", "workload_issuers", "audit"} {
		var leaked bool
		e = h.Pool.QueryRow(context.Background(), "SELECT EXISTS(SELECT 1 FROM olp."+table+" r WHERE position($1 in to_jsonb(r)::text)>0 OR position($2 in to_jsonb(r)::text)>0)", subject, token).Scan(&leaked)
		if e != nil || leaked {
			t.Fatalf("privacy in %s: %v", table, e)
		}
	}
	key := h.want(owner, "GET", "/api/v1/api-keys/"+principal, nil, nil, 200)
	h.want(owner, "POST", "/api/v1/api-keys/"+principal+"/rotate", nil, withMatch(key, idem("rotate-managed")), 409)
	if status, _ := attributionCall(t, h, "workload-chat", sign(subject, "foreign/repo"), nil); status != 401 {
		t.Fatalf("mapping mismatch: %d", status)
	}
	jwks.Close() // Emergency key-ID disablement must not depend on the issuer endpoint.
	input["disabled_key_ids"] = []string{"current"}
	current := h.want(owner, "GET", issuerPath, nil, nil, 200)
	h.want(owner, "PUT", issuerPath, input, etagHeader(current), 200)
	replica.refresh()
	if status, _ := attributionCall(t, replica, "workload-chat", token, nil); status != 401 {
		t.Fatalf("disabled signing key: %d", status)
	}
	input["disabled_key_ids"] = []string{}
	input["enabled"] = false
	jwks.Close()
	current = h.want(owner, "GET", issuerPath, nil, nil, 200)
	h.want(owner, "PUT", issuerPath, input, etagHeader(current), 200)
	h.refresh()
	if status, body := attributionCall(t, h, "workload-chat", token, nil); status != 401 || strings.Contains(string(body), subject) {
		t.Fatalf("disabled issuer: %d", status)
	}
}

type workloadFixture struct {
	input    map[string]any
	issuer   map[string]any
	endpoint *httptest.Server
	private  ed25519.PrivateKey
	keys     *atomic.Value
	kid      string
}

func newWorkloadFixture(t *testing.T, h *accessHarness, owner *browser, project string, groups []string) *workloadFixture {
	t.Helper()
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keys := new(atomic.Value)
	keys.Store(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: pub, KeyID: "current", Algorithm: "EdDSA", Use: "sig"}}})
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(keys.Load())
	}))
	t.Cleanup(endpoint.Close)
	input := map[string]any{"name": "Portable CI", "issuer": endpoint.URL, "jwks_url": endpoint.URL, "enabled": true, "audiences": []string{"olp"}, "algorithms": []string{"EdDSA"}, "max_lifetime_seconds": 600, "disabled_key_ids": []string{}, "mappings": []any{map[string]any{"name": "worker", "match": map[string]string{}, "project_id": project, "limit_template": "worker", "route_groups": groups, "scopes": []string{"inference", "models_read"}, "end_user_claim": "/customer", "allow_provider_state": true}}}
	issuer := h.want(owner, "POST", "/api/v1/workload-issuers", input, idem("workload"), 201)
	return &workloadFixture{input: input, issuer: issuer, endpoint: endpoint, private: private, keys: keys, kid: "current"}
}
func (f *workloadFixture) token(t *testing.T, subject string, expiry time.Time) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: f.private}, (&jose.SignerOptions{}).WithHeader("kid", f.kid))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"iss": f.endpoint.URL, "aud": "olp", "sub": subject, "iat": time.Now().Unix(), "exp": expiry.Unix(), "customer": "worker-customer"})
	sig, err := signer.Sign(body)
	if err != nil {
		t.Fatal(err)
	}
	token, err := sig.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return token
}
func workloadProject(t *testing.T, h *accessHarness, owner *browser, project string, groups map[string]any) {
	t.Helper()
	base := "/api/v1/projects/" + project
	templates := h.want(owner, "GET", base+"/limit-templates", nil, nil, 200)
	h.want(owner, "PUT", base+"/limit-templates", map[string]any{"templates": map[string]any{"worker": map[string]any{"requests_per_minute": 100}}}, etagHeader(templates), 200)
	current := h.want(owner, "GET", base+"/route-groups", nil, nil, 200)
	h.want(owner, "PUT", base+"/route-groups", map[string]any{"groups": groups}, etagHeader(current), 200)
}

type workloadCodeRuntime struct {
	*codeForwardRuntime
	manager *runtime.Manager
}

func (r workloadCodeRuntime) Authenticate(token string) (*access.Authority, error) {
	return r.manager.Authenticate(token)
}

func TestWorkloadSubscriptionRebindsCurrentPolicyAndRetainsDigest(t *testing.T) {
	f := newCodeFixture(t)
	workloadProject(t, f.h, f.owner, f.project, map[string]any{"subscription": []string{"coding"}})
	issuer := newWorkloadFixture(t, f.h, f.owner, f.project, []string{"subscription"})
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-subscription" {
			t.Error("JWT reached upstream")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_%d\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n", calls.Add(1))
	}))
	defer upstream.Close()
	server, _, _ := codeForwardServer(t, f, upstream.URL)
	f.h.Gateway.Runtime = workloadCodeRuntime{f.h.Gateway.Runtime.(*codeForwardRuntime), f.h.Runtime}
	f.h.refresh()
	token := issuer.token(t, "private-subscription-subject", time.Now().Add(5*time.Minute))
	principal, err := f.h.Runtime.Authenticate(token)
	if err != nil {
		t.Fatal(err)
	}
	request := func(thread string, want int) {
		t.Helper()
		r, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/code/coding/responses", strings.NewReader(`{"model":"native-model","input":"hello","stream":true}`))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Thread-Id", thread)
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("subscription %d: %s", response.StatusCode, body)
		}
	}
	request("unassigned", 403)
	f.exec(t, `INSERT INTO olp.code_pool_keys(pool_id,api_key_id) VALUES($1,$2)`, f.pool, principal.ID)
	request("first", 200)
	codeForwardAwait(t, f, "settled", 1)
	base := "/api/v1/projects/" + f.project + "/limit-templates"
	templates := f.h.want(f.owner, "GET", base, nil, nil, 200)
	f.h.want(f.owner, "PUT", base, map[string]any{"templates": map[string]any{"worker": map[string]any{"requests_per_minute": 2}}}, etagHeader(templates), 200)
	// Leave runtime cached. The durable admission must rebind current limits
	// without dropping the already verified end-user claim.
	request("second", 200)
	codeForwardAwait(t, f, "settled", 2)
	var count int
	if err = f.h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.code_attempts WHERE api_key_id=$1 AND end_user_digest=$2 AND state='settled'", principal.ID, principal.EndUserDigest).Scan(&count); err != nil || count != 2 {
		t.Fatalf("workload accounting: %d %v", count, err)
	}
	issuerPath := "/api/v1/workload-issuers/" + issuer.issuer["id"].(string)
	current := f.h.want(f.owner, "GET", issuerPath, nil, nil, 200)
	issuer.input["enabled"] = false
	f.h.want(f.owner, "PUT", issuerPath, issuer.input, etagHeader(current), 200)
	request("disabled", 403)
	if calls.Load() != 2 {
		t.Fatal("disabled workload dispatched")
	}
}

func TestWorkloadAgentDelegatesKeyForOneInference(t *testing.T) {
	h := newAccessHarness(t)
	persisted := make(chan error, 2)
	h.Gateway.Sink = &gateway.PersistingSink{Persist: func(ctx context.Context, event *usage.Event, payload []byte) error {
		_, err := usage.PersistEvent(ctx, h.Pool, event, payload)
		persisted <- err
		return err
	}}
	owner := h.owner()
	project := createProject(h, owner, "Agent delegation")
	workloadProject(t, h, owner, project, map[string]any{"generation": []string{"delegated-chat"}})
	vendor := newOpenAIFixture(t, "")
	provider := activeAzureProviderInProject(t, h, owner, "Delegated provider", vendor.URL, &project, []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}})
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{"project_id": project, "slug": "delegated-chat", "overall_timeout_ms": 10000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"}, "targets": []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}}}, idem("delegated-route"), 201)
	path := "/api/v1/route-drafts/" + draft["id"].(string)
	validated := h.want(owner, "POST", path+"/validate", nil, etagHeader(draft), 200)
	h.want(owner, "POST", path+"/activate", nil, withMatch(validated, idem("activate-delegated-route")), 200)
	issuer := newWorkloadFixture(t, h, owner, project, []string{"generation"})
	issuer.input["mappings"] = []any{map[string]any{"name": "agent", "match": map[string]string{"/sub": "approved-agent", "/client_id": "approved-client"}, "project_id": project, "limit_template": "worker", "route_groups": []string{"generation"}, "scopes": []string{"inference"}, "agent_principal": true}}
	issuerPath := "/api/v1/workload-issuers/" + issuer.issuer["id"].(string)
	current := h.want(owner, "GET", issuerPath, nil, nil, 200)
	mapping := issuer.input["mappings"].([]any)[0].(map[string]any)
	issuer.input["max_lifetime_seconds"] = 1200
	mapping["match"] = map[string]string{"/sub": "approved-agent"}
	h.want(owner, "PUT", issuerPath, issuer.input, etagHeader(current), 422)
	mapping["match"] = map[string]string{"/client_id": "approved-client"}
	h.want(owner, "PUT", issuerPath, issuer.input, etagHeader(current), 422)
	mapping["match"] = map[string]string{"/sub": "approved-agent", "/client_id": "approved-client"}
	h.want(owner, "PUT", issuerPath, issuer.input, etagHeader(current), 200)
	exported := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	document := exported["document"].(map[string]any)
	exportedIssuer := document["workload_issuers"].([]any)[0].(map[string]any)
	exportedMapping := exportedIssuer["mappings"].([]any)[0].(map[string]any)
	if exportedMapping["agent_principal"] != true {
		t.Fatal("configuration export lost the agent principal setting")
	}
	h.want(owner, "POST", "/api/v1/configuration/apply", map[string]any{"document": document}, idem("apply-agent-issuer"), 200)
	delegating := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "delegating", "project_id": project, "scopes": []string{"inference", "delegate"}, "allowed_routes": []string{"delegated-chat"}}, idem("delegating-key"), 201)
	ordinary := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "ordinary", "project_id": project, "scopes": []string{"inference"}, "allowed_routes": []string{"delegated-chat"}}, idem("ordinary-key"), 201)
	otherProject := createProject(h, owner, "Other delegation project")
	otherKey := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "other-project", "project_id": otherProject, "scopes": []string{"inference", "delegate"}, "allowed_routes": []string{}}, idem("other-project-key"), 201)
	h.refresh()
	sign := func(subject, client, audience string) string {
		t.Helper()
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: issuer.private}, (&jose.SignerOptions{}).WithHeader("kid", issuer.kid))
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		body, err := json.Marshal(map[string]any{"iss": issuer.endpoint.URL, "aud": audience, "sub": subject, "client_id": client, "iat": now.Unix(), "exp": now.Add(19 * time.Minute).Unix()})
		if err != nil {
			t.Fatal(err)
		}
		signed, err := signer.Sign(body)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := signed.CompactSerialize()
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	agent := sign("approved-agent", "approved-client", "olp")
	bounds := map[string]any{"scopes": []string{"inference"}, "allowed_routes": []string{"delegated-chat"}}
	exchangeBounds := func(assertion, key string, body map[string]any, want int) string {
		t.Helper()
		started := time.Now()
		response, raw := h.do(nil, "POST", "/v1/delegations", body, map[string]string{"Authorization": "Bearer " + assertion, "X-OLP-Delegating-Key": key})
		if response.StatusCode != want {
			t.Fatalf("delegation exchange: %d %s", response.StatusCode, raw)
		}
		if want != http.StatusCreated {
			return ""
		}
		var grant struct {
			Token     string    `json:"token"`
			ExpiresAt time.Time `json:"expires_at"`
		}
		if err := json.Unmarshal(raw, &grant); err != nil || grant.Token == "" {
			t.Fatalf("delegation token: %s %v", raw, err)
		}
		ttl := 300
		if requested, ok := body["ttl_seconds"].(int); ok && requested > 0 {
			ttl = requested
		}
		if grant.ExpiresAt.Before(started.Add(time.Duration(ttl)*time.Second-time.Second)) || grant.ExpiresAt.After(time.Now().Add(time.Duration(ttl)*time.Second+time.Second)) {
			t.Fatalf("delegation expiry outside requested bounds: %v", grant.ExpiresAt)
		}
		return grant.Token
	}
	exchange := func(assertion, key string, want int) string {
		t.Helper()
		return exchangeBounds(assertion, key, bounds, want)
	}
	exchange(agent, ordinary["secret"].(string), http.StatusForbidden)
	exchange(agent, otherKey["secret"].(string), http.StatusForbidden)
	exchange(sign("other-agent", "approved-client", "olp"), delegating["secret"].(string), http.StatusUnauthorized)
	exchange(sign("approved-agent", "other-client", "olp"), delegating["secret"].(string), http.StatusUnauthorized)
	exchange(sign("approved-agent", "approved-client", "foreign"), delegating["secret"].(string), http.StatusUnauthorized)
	exchangeBounds(agent, delegating["secret"].(string), map[string]any{"scopes": []string{"delegate"}, "allowed_routes": []string{"delegated-chat"}}, http.StatusBadRequest)
	exchangeBounds(agent, delegating["secret"].(string), map[string]any{"scopes": []string{"inference"}, "allowed_routes": []string{"delegated-chat", "other-route"}}, http.StatusForbidden)
	exchangeBounds(agent, delegating["secret"].(string), map[string]any{"scopes": []string{"inference"}, "allowed_routes": []string{"delegated-chat"}, "ttl_seconds": 901}, http.StatusBadRequest)
	grant := exchange(agent, delegating["secret"].(string), http.StatusCreated)
	exchange(agent, grant, http.StatusUnauthorized)
	var leaked bool
	err := h.Pool.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM olp.agent_delegations d WHERE position($1 in to_jsonb(d)::text)>0 OR position($2 in to_jsonb(d)::text)>0 OR position($3 in to_jsonb(d)::text)>0 OR position($4 in to_jsonb(d)::text)>0)`, grant, agent, "approved-agent", "approved-client").Scan(&leaked)
	if err != nil || leaked {
		t.Fatalf("grant retained raw credentials or claims: %v", err)
	}
	infer := func(token, assertion string, want int) {
		t.Helper()
		response, raw := h.do(nil, "POST", "/v1/chat/completions", map[string]any{"model": "delegated-chat", "messages": []any{map[string]string{"role": "user", "content": "hello"}}}, map[string]string{"Authorization": "Bearer " + token, "X-OLP-Agent-Identity": assertion})
		if response.StatusCode != want {
			t.Fatalf("delegated inference: %d %s", response.StatusCode, raw)
		}
	}
	infer(grant, sign("other-agent", "approved-client", "olp"), http.StatusUnauthorized)
	infer(grant, agent, http.StatusOK)
	if err := <-persisted; err != nil {
		t.Fatal(err)
	}
	infer(grant, agent, http.StatusUnauthorized)
	renewed := exchangeBounds(agent, delegating["secret"].(string), map[string]any{"scopes": []string{"inference"}, "allowed_routes": []string{"delegated-chat"}, "ttl_seconds": 900}, http.StatusCreated)
	infer(renewed, agent, http.StatusOK)
	if err := <-persisted; err != nil {
		t.Fatal(err)
	}
	var agentID, ownerID string
	var attempts, pairCount int
	err = h.Pool.QueryRow(context.Background(), `SELECT min(agent_principal_id::text),min(api_key_id::text),count(*),count(DISTINCT delegation_pair_digest) FROM olp.attempt_usage_facts WHERE api_key_id=$1`, delegating["id"]).Scan(&agentID, &ownerID, &attempts, &pairCount)
	if err != nil || agentID == "" || ownerID != delegating["id"] || attempts != 2 || pairCount != 1 {
		t.Fatalf("delegated accounting owners and stable pair: %s %s %d %d %v", agentID, ownerID, attempts, pairCount, err)
	}
	keyPath := "/api/v1/api-keys/" + delegating["id"].(string)
	key := h.want(owner, "GET", keyPath, nil, nil, 200)
	restricted := exchange(agent, delegating["secret"].(string), http.StatusCreated)
	key = h.want(owner, "PATCH", keyPath, map[string]any{"scopes": []string{"delegate"}}, etagHeader(key), 200)
	h.refresh()
	infer(restricted, agent, http.StatusUnauthorized)
	key = h.want(owner, "PATCH", keyPath, map[string]any{"scopes": []string{"inference", "delegate"}}, etagHeader(key), 200)
	h.refresh()
	restricted = exchange(agent, delegating["secret"].(string), http.StatusCreated)
	groupsPath := "/api/v1/projects/" + project + "/route-groups"
	groups := h.want(owner, "GET", groupsPath, nil, nil, 200)
	groups = h.want(owner, "PUT", groupsPath, map[string]any{"groups": map[string]any{"generation": []string{}}}, etagHeader(groups), 200)
	h.refresh()
	infer(restricted, agent, http.StatusUnauthorized)
	h.want(owner, "PUT", groupsPath, map[string]any{"groups": map[string]any{"generation": []string{"delegated-chat"}}}, etagHeader(groups), 200)
	h.refresh()
	revokedKeyGrant := exchange(agent, delegating["secret"].(string), http.StatusCreated)
	h.want(owner, "POST", keyPath+"/revoke", nil, withMatch(key, idem("revoke-delegating")), 200)
	h.refresh()
	infer(revokedKeyGrant, agent, http.StatusUnauthorized)
	secondKey := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "second-delegating", "project_id": project, "scopes": []string{"inference", "delegate"}, "allowed_routes": []string{"delegated-chat"}}, idem("second-delegating"), 201)
	h.refresh()
	revokedAgentGrant := exchange(agent, secondKey["secret"].(string), http.StatusCreated)
	agentPath := "/api/v1/api-keys/" + agentID
	agentKey := h.want(owner, "GET", agentPath, nil, nil, 200)
	h.want(owner, "POST", agentPath+"/revoke", nil, withMatch(agentKey, idem("revoke-agent")), 200)
	h.refresh()
	infer(revokedAgentGrant, agent, http.StatusUnauthorized)
	currentIssuer := h.want(owner, "GET", issuerPath, nil, nil, 200)
	mapping["match"] = map[string]string{"/sub": "approved-agent", "/client_id": "replacement-client"}
	h.want(owner, "PUT", issuerPath, issuer.input, etagHeader(currentIssuer), 200)
	h.refresh()
	replacement := sign("approved-agent", "replacement-client", "olp")
	replacementGrant := exchange(replacement, secondKey["secret"].(string), http.StatusCreated)
	infer(replacementGrant, replacement, http.StatusOK)
	if err := <-persisted; err != nil {
		t.Fatal(err)
	}
	var replacementID string
	err = h.Pool.QueryRow(context.Background(), `SELECT agent_principal_id::text FROM olp.attempt_usage_facts WHERE api_key_id=$1`, secondKey["id"]).Scan(&replacementID)
	if err != nil || replacementID == agentID {
		t.Fatalf("distinct clients must have distinct agent principals: %s %v", replacementID, err)
	}
}
