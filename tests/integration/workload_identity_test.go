//go:build integration && oidctest

package integration_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/runtime"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/tyk-swe/olp/internal/gateway"
)

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
	client := limClient(t)
	limiter := limLimiter(t, client, limNamespace(t, client, "workload"))
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
func TestWorkloadPromotionRemapsProjectsAndPreservesLocalPrincipals(t *testing.T) {
	source := newAccessHarness(t)
	owner := source.owner()
	project := source.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "CI"}, idem("project"), 201)
	id := project["id"].(string)
	workloadProject(t, source, owner, id, map[string]any{"generation": []string{"future-route"}})
	issuer := newWorkloadFixture(t, source, owner, id, []string{"generation"})
	source.refresh()
	token := issuer.token(t, "private-ci-subject", time.Now().Add(5*time.Minute))
	principal, err := source.Runtime.Authenticate(token)
	if err != nil {
		t.Fatal(err)
	}
	exported := source.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	doc := exported["document"].(map[string]any)
	encoded, _ := json.Marshal(doc)
	for _, private := range []string{id, issuer.issuer["id"].(string), principal.ID, principal.WorkloadDigest, "private-ci-subject", token} {
		if strings.Contains(string(encoded), private) {
			t.Fatal("workload artifact retained installation identity")
		}
	}
	destination := newAccessHarness(t)
	destOwner := destination.owner()
	restricted := destination.want(destOwner, "POST", "/api/v1/management-tokens", map[string]any{"name": "configure only", "scopes": []string{"configure"}, "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)}, idem("token"), 201)["secret"].(string)
	destination.machineWant(restricted, "POST", "/api/v1/configuration/plan", map[string]any{"document": doc}, nil, 403)
	destination.machineWant(restricted, "POST", "/api/v1/configuration/apply", map[string]any{"document": doc}, idem("denied"), 403)
	destination.want(destOwner, "POST", "/api/v1/configuration/plan", map[string]any{"document": doc}, nil, 200)
	destination.want(destOwner, "POST", "/api/v1/configuration/apply", map[string]any{"document": doc}, idem("apply"), 200)
	destination.refresh()
	other, err := destination.Runtime.Authenticate(token)
	if err != nil {
		t.Fatal(err)
	}
	if other.ProjectID == nil || *other.ProjectID == id || other.ID == principal.ID || other.WorkloadDigest == principal.WorkloadDigest {
		t.Fatal("destination inherited source identities")
	}
	destination.machineWant(restricted, "GET", "/api/v1/configuration/export", nil, nil, 403)
	destination.machineWant(restricted, "POST", "/api/v1/configuration/apply", map[string]any{"document": doc}, idem("apply"), 403)
	destination.want(destOwner, "POST", "/api/v1/configuration/apply", map[string]any{"document": doc}, idem("unchanged"), 200)
	issuer.endpoint.Close()
	doc["workload_issuers"].([]any)[0].(map[string]any)["enabled"] = false
	destination.want(destOwner, "POST", "/api/v1/configuration/apply", map[string]any{"document": doc}, idem("disable"), 200)
	destination.refresh()
	if _, err = destination.Runtime.Authenticate(token); err == nil {
		t.Fatal("disabled promoted issuer authenticated")
	}
	var count int
	if err = destination.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.api_keys WHERE workload_issuer_id IS NOT NULL").Scan(&count); err != nil || count != 1 {
		t.Fatalf("principals changed by promotion: %d %v", count, err)
	}
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

func TestWorkloadRealtimeExpiresAndFollowsIssuerDisablement(t *testing.T) {
	for _, action := range []string{"expiry", "disabled", "claim_changed"} {
		t.Run(action, func(t *testing.T) {
			h := newAccessHarness(t)
			owner := h.owner()
			project := createProject(h, owner, "Realtime workload")
			fixture := newStrictRealtimeFixture(t, "openai")
			slug, _ := provisionStrictRealtimeInProject(t, h, owner, "openai", fixture.URL+"/v1", nil, &project)
			workloadProject(t, h, owner, project, map[string]any{"live": []string{slug}})
			issuer := newWorkloadFixture(t, h, owner, project, []string{"live"})
			client := limClient(t)
			h.Gateway.Admission = gateway.NewAdmission(limLimiter(t, client, limNamespace(t, client, "workload-live")), nil, slog.New(slog.DiscardHandler))
			h.refresh()
			expiry := time.Now().Add(time.Minute)
			if action == "expiry" {
				expiry = time.Now().Add(3 * time.Second)
			}
			token := issuer.token(t, "private-live-subject", expiry)
			sink := &captureSink{}
			h.Gateway.Sink = sink
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.HTTP.URL, "http")+"/v1/realtime?model="+slug, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			for i, frame := range strictRealtimeClientFrames {
				if err = conn.Write(ctx, websocket.MessageText, frame); err != nil {
					t.Fatal(err)
				}
				if _, _, err = conn.Read(ctx); err != nil {
					t.Fatal(err)
				}
				if i == 1 {
					if _, _, err = conn.Read(ctx); err != nil {
						t.Fatal(err)
					}
				}
			}
			if action != "expiry" {
				path := "/api/v1/workload-issuers/" + issuer.issuer["id"].(string)
				current := h.want(owner, "GET", path, nil, nil, 200)
				if action == "disabled" {
					issuer.input["enabled"] = false
				} else {
					issuer.input["mappings"].([]any)[0].(map[string]any)["end_user_claim"] = "/sub"
				}
				h.want(owner, "PUT", path, issuer.input, etagHeader(current), 200)
				h.refresh()
			}
			if _, _, err = conn.Read(ctx); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
				t.Fatalf("live workload retained authority: %v", err)
			}
			glEventually(t, "workload realtime accounting", func() bool { return sink.count() == 1 })
			event := sink.last()
			if event.KeyID == "" || event.EndUserDigest == "" || len(event.Attempts) != 1 {
				t.Fatal("live workload lost attribution or retried")
			}
		})
	}
}

func TestWorkloadRenewalKeepsRetainedResponsesOwnedBySubject(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "Retained workload")
	fixture := newOpenAIFixture(t, "")
	_, _, slug, _ := provisionOpenAIInProject(t, h, owner, &project, fixture.URL, []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}, []string{"generation"}, map[string]any{"fidelity": map[string]any{"mode": "strict"}}, map[string]any{"profile_id": "azure-legacy-responses", "profile_revision": "1"})
	workloadProject(t, h, owner, project, map[string]any{"state": []string{slug}})
	issuer := newWorkloadFixture(t, h, owner, project, []string{"state"})
	client := limClient(t)
	h.Gateway.Admission = gateway.NewAdmission(limLimiter(t, client, limNamespace(t, client, "workload-state")), nil, slog.New(slog.DiscardHandler))
	h.refresh()
	first := issuer.token(t, "retained-subject", time.Now().Add(2*time.Second))
	principal, err := h.Runtime.Authenticate(first)
	if err != nil {
		t.Fatal(err)
	}
	fixture.resps["resp-up-1"]["status"] = "in_progress"
	fixture.resps["resp-up-1"]["usage"] = nil
	status, raw, _ := h.gatewayRaw("POST", "/v1/responses", first, strings.NewReader(`{"model":"`+slug+`","input":"queued","background":true,"store":true}`), map[string]string{"Content-Type": "application/json"})
	if status != 200 {
		t.Fatalf("create retained JWT response: %d %s", status, raw)
	}
	id, ok := jsonStringField(raw, "id")
	if !ok {
		t.Fatal("missing response handle")
	}
	time.Sleep(time.Until(*principal.ExpiresAt) + 50*time.Millisecond)
	if status, _, _ = h.gatewayRaw("GET", "/v1/responses/"+id, first, nil, nil); status != 401 {
		t.Fatalf("expired credential read: %d", status)
	}
	second := issuer.token(t, "retained-subject", time.Now().Add(time.Minute))
	renewed, err := h.Runtime.Authenticate(second)
	if err != nil || renewed.ID != principal.ID {
		t.Fatal("renewal changed workload principal")
	}
	fixture.resps["resp-up-1"]["status"] = "completed"
	fixture.resps["resp-up-1"]["usage"] = map[string]any{"input_tokens": 4, "output_tokens": 6, "total_tokens": 10}
	for range 2 {
		if status, raw, _ = h.gatewayRaw("GET", "/v1/responses/"+id, second, nil, nil); status != 200 {
			t.Fatalf("renewed JWT cannot read: %d %s", status, raw)
		}
	}
	other := issuer.token(t, "other-subject", time.Now().Add(time.Minute))
	if status, _, _ = h.gatewayRaw("GET", "/v1/responses/"+id, other, nil, nil); status != 404 {
		t.Fatalf("another subject read retained work: %d", status)
	}
	var count int
	if err = h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.attempt_usage_facts WHERE operation='generation' AND api_key_id=$1 AND end_user_digest=$2", principal.ID, principal.EndUserDigest).Scan(&count); err != nil || count != 1 {
		t.Fatalf("renewal changed/duplicated accounting: %d %v", count, err)
	}
}

func TestWorkloadSubscriptionSocketExpiresWhileIdle(t *testing.T) {
	f := newCodeFixture(t)
	workloadProject(t, f.h, f.owner, f.project, map[string]any{"subscription": []string{"coding"}})
	issuer := newWorkloadFixture(t, f.h, f.owner, f.project, []string{"subscription"})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, _, _ = conn.Read(r.Context())
	}))
	defer upstream.Close()
	server, _, _ := codeForwardServer(t, f, upstream.URL)
	f.h.Gateway.Runtime = workloadCodeRuntime{f.h.Gateway.Runtime.(*codeForwardRuntime), f.h.Runtime}
	f.h.refresh()
	token := issuer.token(t, "socket-subject", time.Now().Add(3*time.Second))
	principal, err := f.h.Runtime.Authenticate(token)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, `INSERT INTO olp.code_pool_keys(pool_id,api_key_id) VALUES($1,$2)`, f.pool, principal.ID)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/code/coding/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}, "Thread-Id": {"jwt-socket"}, "X-OLP-Code-Model": {"native-model"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if _, _, err = conn.Read(ctx); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("expired workload socket stayed open: %v", err)
	}
}

func TestWorkloadSigningRotationKeepsPrincipalAndRetiresOldKey(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "Rotating issuer")
	workloadProject(t, h, owner, project, map[string]any{"worker": []string{"future"}})
	issuer := newWorkloadFixture(t, h, owner, project, []string{"worker"})
	h.refresh()
	old := issuer.token(t, "rotating-subject", time.Now().Add(5*time.Minute))
	first, err := h.Runtime.Authenticate(old)
	if err != nil {
		t.Fatal(err)
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuer.private = key
	issuer.kid = "next"
	issuer.keys.Store(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: pub, KeyID: "next", Algorithm: "EdDSA", Use: "sig"}}})
	next := issuer.token(t, "rotating-subject", time.Now().Add(5*time.Minute))
	deadline := time.Now().Add(20 * time.Second)
	for {
		h.refresh()
		current, e := h.Runtime.Authenticate(next)
		if e == nil {
			if current.ID != first.ID {
				t.Fatal("signing rotation changed principal")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("declared signing rotation did not refresh")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err = h.Runtime.Authenticate(old); err == nil {
		t.Fatal("retired signing key remained trusted after successful refresh")
	}
}

func TestDisabledWorkloadMappingsDoNotBlockUnrelatedKeyAuthority(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "Disabled trust"}, idem("disabled-project"), 201)["id"].(string)
	workloadProject(t, h, owner, project, map[string]any{"generation": []string{"future-route"}})
	issuer := newWorkloadFixture(t, h, owner, project, []string{"generation"})
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "independent key", "project_id": project, "scopes": []string{"models_read"}, "allowed_routes": []string{}, "limit_template": "worker"}, idem("independent-key"), 201)
	h.refresh()
	token := issuer.token(t, "enrolled-subject", time.Now().Add(time.Minute))
	if _, err := h.Runtime.Authenticate(token); err != nil {
		t.Fatal(err)
	}
	issuer.endpoint.Close()
	issuer.input["enabled"] = false
	issuer.input["mappings"].([]any)[0].(map[string]any)["limit_template"] = "not-available"
	h.want(owner, "PUT", "/api/v1/workload-issuers/"+issuer.issuer["id"].(string), issuer.input, etagHeader(issuer.issuer), 200)
	h.refresh()
	if _, err := h.Runtime.Authenticate(token); err == nil {
		t.Fatal("disabled trust still authenticates")
	}
	authority, err := h.Runtime.Authenticate(key["secret"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if authority.Policy.RequestsPerMinute == nil || *authority.Policy.RequestsPerMinute != 100 {
		t.Fatal("unrelated template authority was lost")
	}
	document := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)["document"]
	h.want(owner, "POST", "/api/v1/configuration/plan", map[string]any{"document": document}, nil, 200)
	h.want(owner, "POST", "/api/v1/configuration/apply", map[string]any{"document": document}, idem("disabled-roundtrip"), 200)
}
