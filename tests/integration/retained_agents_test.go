//go:build integration

package integration_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/retainedapi"
)

func TestRetainedAgentsOwnershipAndSubmissionFence(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	provider := newGeminiLifecycleProvider(t)
	t.Cleanup(provider.Close)
	route, key, _ := provisionGeminiLifecycle(t, h, owner, "gemini-agents", provider)
	headers := map[string]string{"X-OLP-Route": route, "Idempotency-Key": uuid.NewString()}
	create := func() (*http.Response, []byte) {
		return geminiPublic(t, h, http.MethodPost, "/gemini/v1beta/agents", key, []byte(`{"description":"test","system_instruction":"Be brief."}`), headers)
	}
	response, body := create()
	if response.StatusCode != 200 {
		t.Fatalf("create %d: %s", response.StatusCode, body)
	}
	var agent struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &agent); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(agent.ID, "agent_resource_") || strings.Contains(string(body), "v1_fixture_") {
		t.Fatalf("native identity escaped: %s", body)
	}
	authority, authErr := h.Runtime.Authenticate(key)
	if authErr != nil {
		t.Fatal(authErr)
	}
	stored, storeErr := h.Gateway.Resources.Get(t.Context(), "agent_resource", authority.ID, agent.ID)
	if storeErr != nil || stored.ContractVersion == nil || *stored.ContractVersion != retainedapi.Revision {
		t.Fatalf("creation lost its contract: %+v %v", stored, storeErr)
	}
	count := len(provider.captured())
	response, body = create()
	if response.StatusCode != 409 || response.Header.Get("X-OLP-Resource-ID") != agent.ID {
		t.Fatalf("replay %d: %s", response.StatusCode, body)
	}
	if len(provider.captured()) != count {
		t.Fatal("replay reached provider")
	}
	response, body = geminiPublic(t, h, http.MethodGet, "/gemini/v1beta/agents/"+agent.ID, key, nil)
	if response.StatusCode != 200 || strings.Contains(string(body), "v1_fixture_") {
		t.Fatalf("get %d: %s", response.StatusCode, body)
	}
	response, body = geminiPublic(t, h, http.MethodGet, "/gemini/v1beta/agents", key, nil)
	if response.StatusCode != 200 || strings.Contains(string(body), "foreign-native-agent") || !strings.Contains(string(body), agent.ID) {
		t.Fatalf("list %d: %s", response.StatusCode, body)
	}
	foreign := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "other owner", "scopes": []string{"inference"}, "allowed_routes": []string{route}, "allow_provider_state": true}, idem(uuid.NewString()), 201)["secret"].(string)
	h.refresh()
	count = len(provider.captured())
	response, body = geminiPublic(t, h, http.MethodGet, "/gemini/v1beta/agents/"+agent.ID, foreign, nil)
	if response.StatusCode != 404 || len(provider.captured()) != count {
		t.Fatalf("foreign owner reached resource: %d %s", response.StatusCode, body)
	}
	response, body = geminiPublic(t, h, http.MethodDelete, "/gemini/v1beta/agents/"+agent.ID, key, nil)
	if response.StatusCode != 200 {
		t.Fatalf("delete %d: %s", response.StatusCode, body)
	}
	response, _ = geminiPublic(t, h, http.MethodGet, "/gemini/v1beta/agents/"+agent.ID, key, nil)
	if response.StatusCode != 404 {
		t.Fatalf("deleted resource returned %d", response.StatusCode)
	}
}

func TestRetainedAgentsReplaySurvivesRoutePublication(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	provider := newGeminiLifecycleProvider(t)
	t.Cleanup(provider.Close)
	route, key, providerID := provisionGeminiLifecycle(t, h, owner, "gemini-agents", provider)
	headers := map[string]string{"X-OLP-Route": route, "Idempotency-Key": uuid.NewString()}
	input := []byte(`{"description":"original"}`)
	response, body := geminiPublic(t, h, http.MethodPost, "/gemini/v1beta/agents", key, input, headers)
	if response.StatusCode != 200 {
		t.Fatalf("create %d: %s", response.StatusCode, body)
	}
	id := response.Header.Get("X-OLP-Resource-ID")
	previous := h.Runtime.Release().Snapshot.Routes[route].RevisionID
	draftInput := strictDraft(fidelityDraft(route, providerID))
	draftInput["operations"] = []string{"agent_resource"}
	draft := h.want(owner, "POST", "/api/v1/route-drafts", draftInput, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 200)
	h.refresh()
	if h.Runtime.Release().Snapshot.Routes[route].RevisionID == previous {
		t.Fatal("route was not republished")
	}
	// Disabling the newly selected provider must not prevent replay recovery.
	path := "/api/v1/providers/" + providerID
	current := h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "POST", path+"/disable", nil, withMatch(current, idem(uuid.NewString())), 200)
	h.refresh()
	count := len(provider.captured())
	response, body = geminiPublic(t, h, http.MethodPost, "/gemini/v1beta/agents", key, []byte(`{ "description" : "original" }`), headers)
	if response.StatusCode != 409 || response.Header.Get("X-OLP-Resource-ID") != id || !strings.Contains(string(body), "submission_exists") || len(provider.captured()) != count {
		t.Fatalf("lost replay after publication: %d %s", response.StatusCode, body)
	}
	response, body = geminiPublic(t, h, http.MethodPost, "/gemini/v1beta/agents", key, []byte(`{"description":"different"}`), headers)
	if response.StatusCode != 409 || !strings.Contains(string(body), "idempotency_conflict") || len(provider.captured()) != count {
		t.Fatalf("changed request reused submission: %d %s", response.StatusCode, body)
	}
}

func TestRetainedAgentsReadDeleteRouteAdmission(t *testing.T) {
	for _, dimension := range []string{"requests_per_minute", "max_concurrency"} {
		t.Run(dimension, func(t *testing.T) {
			h := newAccessHarness(t)
			owner := h.owner()
			provider := newGeminiLifecycleProvider(t)
			t.Cleanup(provider.Close)
			route, key, _ := provisionGeminiLifecycle(t, h, owner, "gemini-agents", provider)
			response, body := geminiPublic(t, h, http.MethodPost, "/gemini/v1beta/agents", key, []byte(`{"description":"limited"}`), map[string]string{"X-OLP-Route": route, "Idempotency-Key": uuid.NewString()})
			if response.StatusCode != 200 {
				t.Fatalf("create %d: %s", response.StatusCode, body)
			}
			id := response.Header.Get("X-OLP-Resource-ID")
			authority, err := h.Runtime.Authenticate(key)
			if err != nil {
				t.Fatal(err)
			}
			keyPath := "/api/v1/api-keys/" + authority.ID
			current := h.want(owner, "GET", keyPath, nil, nil, 200)
			h.want(owner, "PATCH", keyPath, map[string]any{"route_limits": map[string]any{route: map[string]any{dimension: 1}}}, etagHeader(current), 200)
			_, _, limiter := limSetup(t, "retained-route-"+dimension)
			h.Gateway.Admission = gateway.NewAdmission(limiter, func() limits.OutagePolicy { return limits.FailClosed }, slog.New(slog.DiscardHandler))
			h.refresh()
			var gate *geminiReplyGate
			if dimension == "max_concurrency" {
				gate = &geminiReplyGate{method: http.MethodGet, suffix: "/agents/", reached: make(chan struct{}), release: make(chan struct{})}
				// Bind the gate to the exact native identity.
				stored, err := h.Gateway.Resources.Get(t.Context(), "agent_resource", authority.ID, id)
				if err != nil {
					t.Fatal(err)
				}
				gate.suffix = "/agents/" + stored.UpstreamID
				provider.replyGate.Store(gate)
				t.Cleanup(func() {
					select {
					case <-gate.release:
					default:
						close(gate.release)
					}
				})
			}
			completed := make(chan int, 1)
			go func() {
				response, _ := geminiPublic(t, h, http.MethodGet, "/gemini/v1beta/agents/"+id, key, nil)
				completed <- response.StatusCode
			}()
			if gate != nil {
				select {
				case <-gate.reached:
				case <-time.After(5 * time.Second):
					t.Fatal("read did not reach provider")
				}
			} else {
				if status := <-completed; status != 200 {
					t.Fatalf("read returned %d", status)
				}
			}
			count := len(provider.captured())
			response, body = geminiPublic(t, h, http.MethodDelete, "/gemini/v1beta/agents/"+id, key, nil)
			if response.StatusCode != 429 || len(provider.captured()) != count {
				t.Fatalf("delete bypassed route limit: %d %s", response.StatusCode, body)
			}
			if gate != nil {
				close(gate.release)
				if status := <-completed; status != 200 {
					t.Fatalf("read returned %d", status)
				}
			}
		})
	}
}

func TestRetainedAgentsListReconcilesProviderDeletion(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	provider := newGeminiLifecycleProvider(t)
	t.Cleanup(provider.Close)
	route, key, _ := provisionGeminiLifecycle(t, h, owner, "gemini-agents", provider)
	var ids []string
	for range 2 {
		response, body := geminiPublic(t, h, http.MethodPost, "/gemini/v1beta/agents", key, []byte(`{"description":"list"}`), map[string]string{"X-OLP-Route": route, "Idempotency-Key": uuid.NewString()})
		if response.StatusCode != 200 {
			t.Fatalf("create %d: %s", response.StatusCode, body)
		}
		ids = append(ids, response.Header.Get("X-OLP-Resource-ID"))
	}
	authority, err := h.Runtime.Authenticate(key)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := h.Gateway.Resources.Get(t.Context(), "agent_resource", authority.ID, ids[1])
	if err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	provider.missingAgents = map[string]bool{missing.UpstreamID: true}
	provider.mu.Unlock()
	response, body := geminiPublic(t, h, http.MethodGet, "/gemini/v1beta/agents?page_size=1", key, nil)
	var page struct {
		Agents []json.RawMessage `json:"agents"`
		Next   string            `json:"next_page_token"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || len(page.Agents) != 0 || page.Next == "" {
		t.Fatalf("deleted row blocked page: %d %s", response.StatusCode, body)
	}
	var state string
	if err := h.Pool.QueryRow(t.Context(), `SELECT state FROM olp.provider_resources WHERE id=$1`, missing.UUID).Scan(&state); err != nil || state != "deleted" {
		t.Fatalf("stale row was not tombstoned: %s %v", state, err)
	}
	response, body = geminiPublic(t, h, http.MethodGet, "/gemini/v1beta/agents?page_size=1&page_token="+page.Next, key, nil)
	if response.StatusCode != 200 || !strings.Contains(string(body), ids[0]) {
		t.Fatalf("tombstoned cursor lost next page: %d %s", response.StatusCode, body)
	}
	response, body = geminiPublic(t, h, http.MethodGet, "/gemini/v1beta/agents", key, nil)
	if response.StatusCode != 200 || !strings.Contains(string(body), ids[0]) || strings.Contains(string(body), ids[1]) {
		t.Fatalf("stale row hid active agent: %d %s", response.StatusCode, body)
	}
}

func TestRetainedAgentsUnknownAcceptanceNeverRedispatches(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	provider := newGeminiLifecycleProvider(t)
	t.Cleanup(provider.Close)
	route, key, _ := provisionGeminiLifecycle(t, h, owner, "gemini-agents", provider)
	provider.badAck.Store(true)
	headers := map[string]string{"X-OLP-Route": route, "Idempotency-Key": uuid.NewString()}
	response, body := geminiPublic(t, h, http.MethodPost, "/gemini/v1beta/agents", key, []byte(`{"description":"test"}`), headers)
	if response.StatusCode != 502 || response.Header.Get("X-OLP-Resource-ID") == "" {
		t.Fatalf("missing unknown acceptance identity: %d %s", response.StatusCode, body)
	}
	count := len(provider.captured())
	provider.badAck.Store(false)
	response, body = geminiPublic(t, h, http.MethodPost, "/gemini/v1beta/agents", key, []byte(`{"description":"test"}`), headers)
	if response.StatusCode != 409 || len(provider.captured()) != count {
		t.Fatalf("unknown create was resubmitted: %d %s", response.StatusCode, body)
	}
}
