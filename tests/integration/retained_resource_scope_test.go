//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

const nativeScopeCredentialB = "retained-account-b-fixture"

type nativeScopeCall struct {
	account string
	method  string
	path    string
}

type nativeScopeFixture struct {
	*httptest.Server
	mu    sync.Mutex
	calls []nativeScopeCall
}

func newNativeScopeFixture(t *testing.T) *nativeScopeFixture {
	t.Helper()
	base := newOpenAIFixture(t, "native-source-input\n")
	fixture := &nativeScopeFixture{}
	fixture.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/responses")) ||
			(r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/responses/")) {
			account := ""
			switch r.Header.Get("api-key") {
			case vendorSecret:
				account = "A"
			case nativeScopeCredentialB:
				account = "B"
			default:
				http.Error(w, "unknown fixture account", http.StatusUnauthorized)
				return
			}
			fixture.mu.Lock()
			fixture.calls = append(fixture.calls, nativeScopeCall{account: account, method: r.Method, path: r.URL.Path})
			fixture.mu.Unlock()
			response := map[string]any{
				"id": "resp-local-1", "object": "response", "created_at": 1, "model": vendorModel,
				"status": "completed", "error": nil, "incomplete_details": nil,
				"output": []any{map[string]any{"id": "msg-account-" + account, "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "account-" + account, "annotations": []any{}}}}},
				"usage":  map[string]any{"input_tokens": 4, "output_tokens": 6, "total_tokens": 10},
			}
			if r.Method == http.MethodPost {
				request := decodeBody(t, r)
				input, err := json.Marshal(request["input"])
				if err != nil {
					t.Error(err)
					return
				}
				switch {
				case strings.Contains(string(input), "source-only-A"):
					response["id"] = "resp-only-A"
				case strings.Contains(string(input), "unbound previous"):
					response["id"], response["previous_response_id"] = "resp-child-B", "resp-only-A"
				}
				if request["stream"] == true {
					w.Header().Set("Content-Type", "text/event-stream")
					initial := maps.Clone(response)
					initial["status"], initial["output"], initial["usage"] = "in_progress", []any{}, nil
					for _, event := range []map[string]any{
						{"type": "response.created", "sequence_number": 0, "response": initial},
						{"type": "response.completed", "sequence_number": 1, "response": response},
					} {
						encoded, err := json.Marshal(event)
						if err != nil {
							t.Error(err)
							return
						}
						fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], encoded)
					}
					fmt.Fprint(w, "data: [DONE]\n\n")
					return
				}
			}
			writeJSON(w, response)
			return
		}
		base.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(fixture.Close)
	return fixture
}

func (f *nativeScopeFixture) captured() []nativeScopeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]nativeScopeCall(nil), f.calls...)
}

func TestRetainedNativeResponseIdentityIncludesAccountSource(t *testing.T) {
	for _, mode := range []string{"transformed", "strict"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", mode, stream), func(t *testing.T) {
				testRetainedNativeResponseIdentityIncludesAccountSource(t, mode, stream)
			})
		}
	}
}

func testRetainedNativeResponseIdentityIncludesAccountSource(t *testing.T, mode string, stream bool) {
	h := newAccessHarness(t)
	fixture := newNativeScopeFixture(t)
	providerFields := map[string]any{}
	if mode == "strict" {
		providerFields["profile_id"], providerFields["profile_revision"] = "azure-legacy-responses", "1"
	}
	owner, provider, routeA, _ := provisionOpenAIWith(t, h, fixture.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"}},
		[]string{"generation"}, map[string]any{"fidelity": map[string]any{"mode": mode}}, providerFields)
	routeB := "native-source-b-" + uuid.NewString()[:8]
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{
		"slug": routeB, "operations": []string{"generation"}, "overall_timeout_ms": 10000, "max_attempts": 1,
		"fidelity": map[string]any{"mode": mode},
		"targets":  []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}},
	}, idem(uuid.NewString()), http.StatusCreated)
	h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem(uuid.NewString())), http.StatusOK)
	providerPath := "/api/v1/providers/" + provider["id"].(string)
	poolPath := providerPath + "/credential-slots"
	pool := h.want(owner, "GET", poolPath, nil, nil, http.StatusOK)
	slotA := pool["items"].([]any)[0].(map[string]any)["id"].(string)
	pool = h.want(owner, "PUT", poolPath+"/"+slotA, map[string]any{"slot": map[string]any{"name": "Account A", "allowed_routes": []string{routeA}}}, withMatch(pool, idem(uuid.NewString())), http.StatusOK)
	slotB := uuid.NewString()
	h.want(owner, "PUT", poolPath+"/"+slotB, map[string]any{"slot": map[string]any{"name": "Account B", "allowed_routes": []string{routeB}}, "credential": nativeScopeCredentialB}, withMatch(pool, idem(uuid.NewString())), http.StatusOK)
	pool = h.want(owner, "GET", poolPath, nil, nil, http.StatusOK)
	h.want(owner, "POST", poolPath+"/"+slotB+"/validate", nil, withMatch(pool, nil), http.StatusOK)
	provider = h.want(owner, "GET", providerPath, nil, nil, http.StatusOK)
	h.want(owner, "POST", providerPath+"/activate", nil, withMatch(provider, idem(uuid.NewString())), http.StatusOK)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "both account routes", "scopes": []string{"inference"}, "allow_provider_state": true, "allowed_routes": []string{routeA, routeB}}, idem(uuid.NewString()), http.StatusCreated)
	secret := key["secret"].(string)
	h.refresh()
	prefix := "response_"
	if mode == "strict" {
		prefix = "strict_response_"
	}
	ids := make([]string, 2)
	for i, route := range []string{routeA, routeB} {
		status, body, _ := h.gatewayRaw("POST", "/v1/responses", secret, strings.NewReader(fmt.Sprintf(`{"model":%q,"input":"source identity","store":true,"stream":%t}`, route, stream)), map[string]string{"Content-Type": "application/json"})
		if status != http.StatusOK {
			t.Fatalf("account %d create: %d %s", i, status, body)
		}
		id := nativeScopeResponseID(t, body, stream)
		if !strings.HasPrefix(id, prefix) || !strings.Contains(string(body), "account-"+string(rune('A'+i))) {
			t.Fatalf("account %d native reply lost its output/local identity: %s", i, body)
		}
		ids[i] = id
	}
	status, body, _ := h.gatewayRaw("GET", "/v1/responses/"+ids[1], secret, nil, nil)
	calls := fixture.captured()
	if len(calls) < 3 || calls[len(calls)-3].account != "A" || calls[len(calls)-2].account != "B" {
		t.Fatalf("route restrictions did not select distinct account sources: %v", calls)
	}
	if status != http.StatusOK || ids[0] == ids[1] || calls[len(calls)-1].account != "B" || !strings.Contains(string(body), "account-B") {
		t.Fatalf("native IDs crossed accounts: same_local_id=%t retrieval_status=%d retrieval_account=%s body=%s", ids[0] == ids[1], status, calls[len(calls)-1].account, body)
	}
	if mode == "transformed" {
		status, parentBody, _ := h.gatewayRaw("POST", "/v1/responses", secret, strings.NewReader(fmt.Sprintf(`{"model":%q,"input":"source-only-A","store":true,"stream":%t}`, routeA, stream)), map[string]string{"Content-Type": "application/json"})
		if status != http.StatusOK {
			t.Fatalf("seed account-A-only parent: %d %s", status, parentBody)
		}
		foreignParent := nativeScopeResponseID(t, parentBody, stream)
		status, childBody, _ := h.gatewayRaw("POST", "/v1/responses", secret, strings.NewReader(fmt.Sprintf(`{"model":%q,"input":"unbound previous","store":true,"stream":%t}`, routeB, stream)), map[string]string{"Content-Type": "application/json"})
		if status != http.StatusOK || strings.Contains(string(childBody), foreignParent) || strings.Contains(string(childBody), "resp-only-A") || !strings.Contains(string(childBody), `"previous_response_id":null`) {
			t.Fatalf("unbound previous echo selected another account's parent: %d %s", status, childBody)
		}
	}
	create := func(key, route string) string {
		t.Helper()
		status, body, _ := h.gatewayRaw("POST", "/v1/responses", key, strings.NewReader(fmt.Sprintf(`{"model":%q,"input":"source identity","store":true,"stream":%t}`, route, stream)), map[string]string{"Content-Type": "application/json"})
		if status != http.StatusOK {
			t.Fatalf("scoped create: %d %s", status, body)
		}
		return nativeScopeResponseID(t, body, stream)
	}
	if same := create(secret, routeA); same != ids[0] {
		t.Fatal("same caller and exact source did not retain its existing local identity")
	}
	otherKey := stateKey(t, h, owner, routeA, true)
	h.refresh()
	otherID := create(otherKey, routeA)
	if otherID == ids[0] || otherID == ids[1] {
		t.Fatal("different caller reused another caller's native-resource namespace")
	}
	before := len(fixture.captured())
	status, _, _ = h.gatewayRaw("GET", "/v1/responses/"+ids[0], otherKey, nil, nil)
	if status != http.StatusNotFound || len(fixture.captured()) != before {
		t.Fatal("foreign local alias dispatched provider work")
	}
	pool = h.want(owner, "GET", poolPath, nil, nil, http.StatusOK)
	h.want(owner, "PUT", poolPath+"/"+slotA, map[string]any{"slot": map[string]any{"name": "Rotated account A", "allowed_routes": []string{routeA}}, "credential": nativeScopeCredentialB}, withMatch(pool, idem(uuid.NewString())), http.StatusOK)
	pool = h.want(owner, "GET", poolPath, nil, nil, http.StatusOK)
	h.want(owner, "POST", poolPath+"/"+slotA+"/validate", nil, withMatch(pool, nil), http.StatusOK)
	provider = h.want(owner, "GET", providerPath, nil, nil, http.StatusOK)
	h.want(owner, "POST", providerPath+"/activate", nil, withMatch(provider, idem(uuid.NewString())), http.StatusOK)
	h.refresh()
	rotatedID := create(secret, routeA)
	if rotatedID == ids[0] || rotatedID == ids[1] || rotatedID == otherID {
		t.Fatal("new credential version/publication reused another immutable source alias")
	}
	for _, expected := range []struct{ id, account string }{{ids[0], "A"}, {rotatedID, "B"}} {
		status, body, _ := h.gatewayRaw("GET", "/v1/responses/"+expected.id, secret, nil, nil)
		calls := fixture.captured()
		if status != http.StatusOK || calls[len(calls)-1].account != expected.account || !strings.Contains(string(body), "account-"+expected.account) {
			t.Fatalf("immutable source alias repinned after rotation: %d %s", status, body)
		}
	}
}

func nativeScopeResponseID(t *testing.T, body []byte, stream bool) string {
	t.Helper()
	if stream {
		for _, line := range strings.Split(string(body), "\n") {
			if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
				continue
			}
			var event struct {
				Response struct {
					ID string `json:"id"`
				} `json:"response"`
			}
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) == nil && event.Response.ID != "" {
				return event.Response.ID
			}
		}
		t.Fatalf("stream had no retained response identity: %s", body)
	}
	var response struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	return response.ID
}
