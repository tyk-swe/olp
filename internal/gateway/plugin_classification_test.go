package gateway

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
	"github.com/tyk-swe/olp/tests/fixtures"
)

// declarePlugin makes provider a a plugin provider whose profile declares the
// classification.
func (h *harness) declarePlugin(classification ...abi.FailureRule) {
	h.t.Helper()
	manifest := abi.Manifest{Name: "acme", Version: "1.0.0", Profiles: []abi.Profile{{
		ID: "acme-chat", Label: "Acme Chat", Dialect: "openai-chat",
		Hosting: abi.Hosting{Address: h.upstream.URL + "/a/v1", Headers: map[string]string{"Authorization": "Bearer {credential}"}, Classification: classification},
	}}}
	digest := strings.Repeat("cd", 32)
	plugin, err := connectors.NewPluginProfile(digest, manifest, "acme-chat")
	if err != nil {
		h.t.Fatal(err)
	}
	secrets := map[string][]byte{}
	for id, provider := range h.rt.release.Snapshot.Providers {
		for _, slot := range provider.Slots {
			secrets[*slot.CredentialID], _ = h.rt.release.Credential(*slot.CredentialID)
		}
		if provider.Slots[0].ID == h.slotA {
			provider.Kind, provider.AuthMode, provider.Plugin = connectors.KindPlugin, connectors.AuthStaticCredential, plugin
			provider.ProfileID, provider.ProfileRevision, provider.Endpoint = "acme-chat", digest, plugin.Address(nil)
			h.rt.release.Snapshot.Providers[id] = provider
		}
	}
	if h.rt.release, err = runtime.NewRelease(uuid.NewString(), 7, h.rt.release.Snapshot, secrets); err != nil {
		h.t.Fatal(err)
	}
}

// A 400 carrying the quota code a plugin profile declares rate limited cools
// the slot and fails over, as a 429 would.
func TestDeclaredRateLimitCoolsTheSlotAndFailsOver(t *testing.T) {
	h := newHarness(t, Config{})
	h.declarePlugin(abi.FailureRule{Status: 400, Code: "insufficient_quota", Class: abi.ClassRateLimited})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		status(http.StatusBadRequest, `{"error":{"message":"quota spent","type":"invalid_request_error","code":"insufficient_quota"}}`)(w, r)
	})
	if resp, _ := h.chat(fullKey, nil); resp.StatusCode != http.StatusOK {
		t.Fatal(resp.Status)
	}
	env := h.sink.last(t)
	if len(env.Attempts) != 2 || env.Attempts[0].Class != classRateLimit || env.Attempts[0].Status != 400 || env.Attempts[1].Class != classSuccess {
		t.Fatalf("attempts %+v", env.Attempts)
	}
	if retry := env.Attempts[0].RetryAfter; retry == nil || *retry == 0 {
		t.Fatalf("the upstream's Retry-After was not kept: %+v", env.Attempts[0])
	}
	if resp, _ := h.chat(fullKey, nil); resp.StatusCode != http.StatusOK {
		t.Fatal(resp.Status)
	}
	if h.mock.count("a") != 1 || len(h.sink.last(t).Attempts) != 1 {
		t.Fatalf("provider a called %d times during cooldown", h.mock.count("a"))
	}
}

// Failures a plugin profile does not declare keep the built-in classes.
func TestUndeclaredPluginFailuresUseTheBuiltInRules(t *testing.T) {
	h := newHarness(t, Config{})
	h.declarePlugin(abi.FailureRule{Status: 400, Code: "insufficient_quota", Class: abi.ClassRateLimited})
	h.mock.set("a", status(http.StatusBadRequest, `{"error":{"message":"bad temperature","type":"invalid_request_error","code":"invalid_value"}}`))
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusBadRequest || errorCode(t, body) != "upstream_rejected" || h.mock.count("b") != 0 {
		t.Fatalf("status %d body %v b=%d", resp.StatusCode, body, h.mock.count("b"))
	}
	if env := h.sink.last(t); len(env.Attempts) != 1 || env.Attempts[0].Class != classUpstreamClient {
		t.Fatalf("attempts %+v", env.Attempts)
	}
	h.mock.set("a", status(http.StatusServiceUnavailable, `{"error":{"message":"down","type":"server_error"}}`))
	if resp, _ = h.chat(fullKey, nil); resp.StatusCode != http.StatusOK {
		t.Fatal(resp.Status)
	}
	if env := h.sink.last(t); len(env.Attempts) != 2 || env.Attempts[0].Class != classUpstreamServer {
		t.Fatalf("attempts %+v", env.Attempts)
	}
}

// A server failure a plugin profile declares terminal does not fail over.
func TestDeclaredTerminalFailureDoesNotFailOver(t *testing.T) {
	h := newHarness(t, Config{})
	h.declarePlugin(abi.FailureRule{Status: 503, Type: "policy_violation", Class: abi.ClassTerminal})
	h.mock.set("a", status(http.StatusServiceUnavailable, `{"error":{"message":"refused by policy","type":"policy_violation"}}`))
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusBadGateway || errorCode(t, body) != "upstream_rejected" || h.mock.count("b") != 0 {
		t.Fatalf("status %d body %v b=%d", resp.StatusCode, body, h.mock.count("b"))
	}
	if env := h.sink.last(t); len(env.Attempts) != 1 || env.Attempts[0].Class != classUpstreamClient {
		t.Fatalf("attempts %+v", env.Attempts)
	}
}

// An in-band stream error of a type a plugin profile declares rate limited
// cools the slot and fails over before anything reaches the client.
func TestDeclaredInBandStreamErrorFailsOver(t *testing.T) {
	h := newHarness(t, Config{})
	h.declarePlugin(abi.FailureRule{Type: "quota_exceeded", Class: abi.ClassRateLimited})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"error":{"message":"quota spent","type":"quota_exceeded"}}`+"\n\n")
	})
	data, err := fixtures.Files.ReadFile("streams/openai-chat.sse")
	if err != nil {
		t.Fatal(err)
	}
	h.mock.set("b", func(w http.ResponseWriter, r *http.Request) {
		if err := testutil.Stream(w, r, data, 1, 0); err != nil {
			t.Error(err)
		}
	})
	resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"hi"}],"stream":true}`), nil)
	streamed, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(streamed), "[DONE]") {
		t.Fatalf("status %d stream %s", resp.StatusCode, streamed)
	}
	env := h.sink.last(t)
	if len(env.Attempts) != 2 || env.Attempts[0].Class != classRateLimit || env.Attempts[1].Class != classSuccess {
		t.Fatalf("attempts %+v", env.Attempts)
	}
	if !h.gateway.health.coolingDown(env.Attempts[0].ProviderID, h.slotA) {
		t.Fatal("the rate limited slot is not cooling down")
	}
}
