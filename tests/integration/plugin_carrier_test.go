//go:build integration

package integration_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/testutil"
)

// carriedCredential is the static credential of the providers the carrier
// plugin serves.
const carriedCredential = "sk-carried"

// carriedUpstream answers only chat requests the carrier plugin carried,
// which it marks. A stream whose prompt is "hold" pauses after its first
// event until the test releases it.
type carriedUpstream struct {
	*httptest.Server
	carried, refused atomic.Int64
	release          chan struct{}
}

func newCarriedUpstream(t *testing.T) *carriedUpstream {
	t.Helper()
	u := &carriedUpstream{release: make(chan struct{})}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Carried-By") != "carrier" || r.Header.Get("Authorization") != "Bearer "+carriedCredential {
			u.refused.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			writeJSON(w, map[string]any{"error": map[string]any{"message": "The plugin did not carry this request.", "type": "invalid_request_error", "code": "invalid_api_key"}})
			return
		}
		u.carried.Add(1)
		var body struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		usage := map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}
		if !body.Stream {
			writeJSON(w, map[string]any{"id": "chatcmpl-carried", "object": "chat.completion", "created": 1, "model": body.Model,
				"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "Hello from the carried upstream"}, "finish_reason": "stop"}}, "usage": usage})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		event := func(chunk map[string]any) {
			chunk["id"], chunk["object"], chunk["created"], chunk["model"] = "chatcmpl-carried", "chat.completion.chunk", 1, body.Model
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			w.(http.Flusher).Flush()
		}
		event(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": "Hello"}, "finish_reason": nil}}})
		if len(body.Messages) > 0 && body.Messages[0].Content == "hold" {
			select {
			case <-u.release:
			case <-r.Context().Done():
				return
			}
		}
		event(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": " again"}, "finish_reason": nil}}})
		event(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": usage})
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(u.Close)
	return u
}

// An owner permits an unconfined plugin that carries its profile's traffic:
// control's probe and certification and the gateway's unary and streaming
// requests all travel through its subprocess, and each stream event reaches
// the caller as the upstream sends it. A strict route refuses its target. A
// failure the plugin reports as not sent fails over; any other leaves the
// upstream's outcome unknown and does not.
func TestUnconfinedPluginCarriesATargetsTraffic(t *testing.T) {
	base := newAccessHarness(t)
	owner := base.owner()
	upstream := newCarriedUpstream(t)
	fallback := newVendor(t)
	dir := t.TempDir()
	testutil.BuildExecutablePlugin(t, filepath.Join(dir, "carrier"), "./internal/plugins/testdata/carrier", "-X=main.upstream="+upstream.URL+"/v1")
	h := newUnconfinedHarness(t, base.Pool, base.DBURL, dir)
	review := h.want(owner, "POST", "/api/v1/unconfined-plugins/carrier/review", nil, nil, 200)
	if profile := review["manifest"].(map[string]any)["profiles"].([]any)[0].(map[string]any); profile["carries_traffic"] != true {
		t.Fatalf("the owner reviewed %v", profile)
	}
	digest := review["digest"].(string)
	h.want(owner, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "plugin_permit"}, nil, 204)
	h.want(owner, "POST", "/api/v1/unconfined-plugins/carrier/permit", map[string]any{"digest": digest, "acknowledge_risk": true}, nil, 201)

	// The upstream answers only what the plugin carries, so control's probe
	// and certification travel through it too.
	carried := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Carried", "credential": carriedCredential, "model": vendorModel,
		"configuration": map[string]any{"kind": "plugin", "auth_mode": "static_credential", "profile_id": "carrier-chat", "profile_revision": digest}}, idem(uuid.NewString()), 201)
	certifyPluginProvider(t, h, owner, "/api/v1/providers/"+carried["id"].(string))
	if upstream.carried.Load() < 3 || upstream.refused.Load() != 0 {
		t.Fatalf("control carried %d requests, and the upstream refused %d", upstream.carried.Load(), upstream.refused.Load())
	}
	backup := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Fallback", "credential": vendorSecret, "model": vendorModel,
		"configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": fallback.URL + "/v1"}}, idem(uuid.NewString()), 201)
	certifyPluginProvider(t, h, owner, "/api/v1/providers/"+backup["id"].(string))

	strict := h.want(owner, "POST", "/api/v1/route-drafts", fidelityDraft("carried-strict", carried["id"]), idem(uuid.NewString()), 201)
	problem := h.want(owner, "POST", "/api/v1/route-drafts/"+strict["id"].(string)+"/activate", nil, withMatch(strict, idem(uuid.NewString())), 422)
	if problemCode(t, problem) != "target_capability" || !strings.Contains(problem["detail"].(string), "its plugin carries its traffic. Declare the route transformed") {
		t.Fatalf("a strict route's carried target was refused with %v", problem)
	}
	draft := transformed(fidelityDraft("carried", carried["id"]))
	draft["max_attempts"] = 2
	draft["targets"] = append(draft["targets"].([]any), map[string]any{"provider_id": backup["id"], "provider_model": vendorModel, "priority": 1, "weight": 1, "timeout_ms": 5000})
	route := h.want(owner, "POST", "/api/v1/route-drafts", draft, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v1/route-drafts/"+route["id"].(string)+"/activate", nil, withMatch(route, idem(uuid.NewString())), 200)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Carried", "scopes": []string{"inference"}, "allowed_routes": []string{"carried"}}, idem(uuid.NewString()), 201)["secret"].(string)
	h.refresh()
	chat := func(prompt string, stream bool) map[string]any {
		return map[string]any{"model": "carried", "stream": stream, "messages": []any{map[string]any{"role": "user", "content": prompt}}}
	}

	if status, reply, _ := h.gateway("POST", "/v1/chat/completions", key, chat("hi", false)); status != 200 || !strings.Contains(fmt.Sprint(reply), "Hello from the carried upstream") {
		t.Fatalf("unary through the carrier: %d %v", status, reply)
	}
	data, _ := json.Marshal(chat("hold", true))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.HTTP.URL+"/v1/chat/completions", strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("stream through the carrier: %v %v", resp, err)
	}
	defer resp.Body.Close()
	events := bufio.NewReader(resp.Body)
	for {
		line, err := events.ReadString('\n')
		if err != nil {
			t.Fatalf("the stream ended before its first event: %v", err)
		}
		if strings.Contains(line, `"Hello"`) {
			break
		}
	}
	// The upstream holds the rest of the stream until the caller has read
	// its first event.
	close(upstream.release)
	if rest, err := io.ReadAll(events); err != nil || !strings.Contains(string(rest), `" again"`) || !strings.Contains(string(rest), "data: [DONE]") {
		t.Fatalf("the rest of the stream %q: %v", rest, err)
	}

	chats, sent := fallback.chats.Load(), upstream.carried.Load()
	if status, reply, _ := h.gateway("POST", "/v1/chat/completions", key, chat("carrier:not-sent", false)); status != 200 || !strings.Contains(fmt.Sprint(reply), vendorAnswer) || fallback.chats.Load() != chats+1 || upstream.carried.Load() != sent {
		t.Fatalf("a request the plugin did not send: %d %v", status, reply)
	}
	status, reply, _ := h.gateway("POST", "/v1/chat/completions", key, chat("carrier:fail", false))
	if status != 502 || h.gatewayCode(status, reply) != "ambiguous_upstream_result" || fallback.chats.Load() != chats+1 || upstream.carried.Load() != sent+1 {
		t.Fatalf("a request the plugin failed after sending: %d %v", status, reply)
	}
}
