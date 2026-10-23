//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/connectors"
)

type geminiLifecycleCall struct {
	Method, Path, Query string
	Body                []byte
	Key                 string
}

type geminiReplyGate struct {
	method, suffix   string
	reached, release chan struct{}
}

type geminiLifecycleProvider struct {
	*httptest.Server
	mu              sync.Mutex
	calls           []geminiLifecycleCall
	sequence        int
	missingAgents   map[string]bool
	agents          map[string]map[string]any
	agentReadDelay  atomic.Int64
	slowSent        atomic.Int32
	slowDone        chan struct{}
	badAck          atomic.Bool
	escapedCreateID atomic.Bool
	escapedReadID   atomic.Bool
	escapedStreamID atomic.Bool
	replyGate       atomic.Pointer[geminiReplyGate]
	// deleteMissing makes a delete answer that the Interaction does not exist, as
	// it does for one an earlier delete whose response was lost already removed.
	deleteMissing   atomic.Bool
	terminalUsage   atomic.Bool
	partialUsage    atomic.Bool
	accountingReply atomic.Pointer[geminiAccountingReply]
}

func newGeminiLifecycleProvider(t *testing.T) *geminiLifecycleProvider {
	t.Helper()
	p := &geminiLifecycleProvider{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1beta/models" {
			writeJSON(w, map[string]any{"models": []any{map[string]string{"name": "models/" + vendorModel}}})
			return
		}
		if r.URL.Path == "/ws/"+connectors.GeminiLiveMethod {
			p.live(t, w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Error(err)
			return
		}
		if r.Header.Get("X-Goog-Api-Key") != vendorSecret || strings.Contains(string(body), "client-secret") {
			t.Errorf("provider authentication or redaction failed: %q", r.Header.Get("X-Goog-Api-Key"))
			http.Error(w, "bad authentication", 401)
			return
		}
		p.mu.Lock()
		p.calls = append(p.calls, geminiLifecycleCall{r.Method, r.URL.Path, r.URL.RawQuery, bytes.Clone(body), r.Header.Get("X-Goog-Api-Key")})
		p.sequence++
		id := fmt.Sprintf("v1_fixture_%d", p.sequence)
		agentMissing := p.missingAgents[strings.TrimPrefix(r.URL.Path, "/v1beta/agents/")]
		agent := p.agents[strings.TrimPrefix(r.URL.Path, "/v1beta/agents/")]
		p.mu.Unlock()
		if gate := p.replyGate.Load(); gate != nil && r.Method == gate.method && strings.HasSuffix(r.URL.Path, gate.suffix) {
			close(gate.reached)
			select {
			case <-gate.release:
			case <-r.Context().Done():
				return
			}
		}
		if reply := p.accountingReply.Load(); reply != nil && strings.HasPrefix(r.URL.Path, "/v1beta/interactions/") &&
			(r.Method == http.MethodGet || r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel")) {
			id = strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1beta/interactions/"), "/cancel")
			reply.write(w, id, r.URL.Query().Get("stream") == "true")
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1beta/agents":
			if p.badAck.Load() {
				writeJSON(w, map[string]any{"invalid": true})
				return
			}
			created := map[string]any{}
			_ = json.Unmarshal(body, &created)
			created["id"] = id
			p.mu.Lock()
			if p.agents == nil {
				p.agents = make(map[string]map[string]any)
			}
			p.agents[id] = created
			p.mu.Unlock()
			writeJSON(w, created)
		case r.Method == http.MethodGet && r.URL.Path == "/v1beta/agents":
			writeJSON(w, map[string]any{"agents": []any{map[string]any{"id": "foreign-native-agent"}}})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1beta/agents/"):
			if delay := time.Duration(p.agentReadDelay.Load()); delay > 0 {
				select {
				case <-time.After(delay):
				case <-r.Context().Done():
					return
				}
			}
			if agentMissing {
				w.WriteHeader(http.StatusNotFound)
				writeJSON(w, map[string]any{"error": map[string]any{"code": 404, "message": "not found"}})
				return
			}
			if agent != nil {
				writeJSON(w, agent)
			} else {
				writeJSON(w, map[string]any{"id": strings.TrimPrefix(r.URL.Path, "/v1beta/agents/"), "description": "created agent"})
			}
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1beta/agents/"):
			writeJSON(w, map[string]any{})
		case r.Method == http.MethodPost && r.URL.Path == "/v1beta/interactions":
			var input struct {
				Stream     bool `json:"stream"`
				Background bool `json:"background"`
			}
			_ = json.Unmarshal(body, &input)
			if input.Background {
				w.Header().Set("Content-Type", "application/json")
				nativeUsage := ""
				if p.partialUsage.Load() {
					nativeUsage = `,"usage":{"total_input_tokens":3,"total_output_tokens":2}`
				}
				fmt.Fprintf(w, `{"id":%q,"model":%q,"status":"in_progress","object":"interaction"%s}`, id, vendorModel, nativeUsage)
				return
			}
			if input.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "id: cursor-created\ndata: {\"event_type\":\"interaction.created\",\"interaction\":{\"id\":%q,\"model\":%q,\"status\":\"in_progress\"}}\n\n", id, vendorModel)
				fmt.Fprint(w, "id: cursor-start\ndata: {\"event_type\":\"step.start\",\"step\":{\"type\":\"thought\"}}\n\n")
				fmt.Fprint(w, "id: cursor-delta\ndata: {\"event_type\":\"step.delta\",\"delta\":{\"type\":\"text\",\"text\":\"exact\"}}\n\n")
				fmt.Fprint(w, "id: cursor-stop\ndata: {\"event_type\":\"step.stop\",\"step\":{\"type\":\"thought\",\"signature\":\"opaque-native\"}}\n\n")
				fmt.Fprintf(w, "id: cursor-complete\ndata: {\"event_type\":\"interaction.completed\",\"interaction\":{\"id\":%q,\"model\":%q,\"status\":\"completed\",\"usage\":{\"total_input_tokens\":3,\"total_output_tokens\":2}}}\n\n", id, vendorModel)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if p.escapedCreateID.Load() {
				fmt.Fprintf(w, `{"id":%q,"model":%q,"status":"completed","steps":[],"native":{"resource":"%s"}}`, id, vendorModel, strings.ReplaceAll(id, "_", `\u005f`))
				return
			}
			fmt.Fprintf(w, `{ "id":%q,"model":%q,"status":"completed","steps":[{"type":"thought","signature":"opaque-native","content":[{"type":"text","text":"thinking"}]},{"type":"model_output","content":[{"type":"text","text":"OK"}]}],"usage":{"total_input_tokens":3,"total_output_tokens":2},"native":{"number":9007199254740993}}`, id, vendorModel)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1beta/interactions/"):
			id = strings.TrimPrefix(r.URL.Path, "/v1beta/interactions/")
			if r.URL.Query().Get("stream") == "true" {
				w.Header().Set("Content-Type", "text/event-stream")
				if p.escapedStreamID.Load() {
					fmt.Fprintf(w, "id: cursor-created\ndata: {\"event_type\":\"interaction.created\",\"interaction\":{\"id\":%q,\"status\":\"in_progress\"},\"native\":{\"resource\":\"%s\"}}\n\n", id, strings.ReplaceAll(id, "_", `\u005f`))
					return
				}
				if r.URL.Query().Get("last_event_id") == "" {
					fmt.Fprintf(w, "id: cursor-created\ndata: {\"event_type\":\"interaction.created\",\"interaction\":{\"id\":%q,\"status\":\"in_progress\"}}\n\n", id)
					fmt.Fprint(w, "id: cursor-start\ndata: {\"event_type\":\"step.start\",\"step\":{\"type\":\"model_output\"}}\n\n")
				}
				fmt.Fprint(w, "id: cursor-delta\ndata: {\"event_type\":\"step.delta\",\"delta\":{\"type\":\"text\",\"text\":\"retrieved\"}}\n\n")
				fmt.Fprint(w, "id: cursor-stop\ndata: {\"event_type\":\"step.stop\",\"step\":{\"type\":\"model_output\"}}\n\n")
				nativeUsage := ""
				if p.terminalUsage.Load() {
					nativeUsage = `,"usage":{"total_input_tokens":5,"total_output_tokens":4}`
				}
				fmt.Fprintf(w, "id: cursor-complete\ndata: {\"event_type\":\"interaction.completed\",\"interaction\":{\"id\":%q,\"status\":\"completed\"%s}}\n\n", id, nativeUsage)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if p.escapedReadID.Load() {
				fmt.Fprintf(w, `{"id":%q,"model":%q,"status":"completed","steps":[],"native":{"resource":"%s"}}`, id, vendorModel, strings.ReplaceAll(id, "_", `\u005f`))
				return
			}
			nativeUsage := ""
			if p.terminalUsage.Load() {
				nativeUsage = `,"usage":{"total_input_tokens":5,"total_output_tokens":4}`
			}
			fmt.Fprintf(w, `{"id":%q,"model":%q,"status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"retrieved"}]}]%s}`, id, vendorModel, nativeUsage)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
			id = strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1beta/interactions/"), "/cancel")
			w.Header().Set("Content-Type", "application/json")
			nativeUsage := ""
			if p.terminalUsage.Load() {
				nativeUsage = `,"usage":{"total_input_tokens":5,"total_output_tokens":4}`
			}
			fmt.Fprintf(w, `{"id":%q,"model":%q,"status":"cancelled"%s}`, id, vendorModel, nativeUsage)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1beta/interactions/"):
			if p.deleteMissing.Load() {
				http.Error(w, `{"error":{"code":404,"message":"Interaction not found","status":"NOT_FOUND"}}`, http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *geminiLifecycleProvider) live(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	if r.Header.Get("X-Goog-Api-Key") != vendorSecret {
		http.Error(w, "bad provider credential", 401)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		t.Error(err)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	_, setup, err := conn.Read(ctx)
	if err != nil {
		t.Error(err)
		return
	}
	p.mu.Lock()
	p.calls = append(p.calls, geminiLifecycleCall{Method: "WS", Path: r.URL.Path, Body: bytes.Clone(setup), Key: r.Header.Get("X-Goog-Api-Key")})
	p.mu.Unlock()
	if !bytes.Contains(setup, []byte(`"model":"models/`+vendorModel+`"`)) {
		t.Errorf("Live setup model not bound: %s", setup)
	}
	if p.badAck.Load() {
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"setupComplete":null}`))
		return
	}
	withTool := bytes.Contains(setup, []byte(`"tools"`))
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"setupComplete":{}}`)); err != nil {
		t.Error(err)
		return
	}
	if bytes.Contains(setup, []byte("slow-peer-fixture")) {
		defer close(p.slowDone)
		frame := []byte(`{"serverContent":{"modelTurn":{"parts":[{"text":"` + strings.Repeat("x", 60000) + `"}]}}}`)
		for i := 0; i < 4096; i++ {
			if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
				return
			}
			p.slowSent.Add(1)
		}
		return
	}
	if bytes.Contains(setup, []byte("terminal-fixture-")) {
		_, payload, err := conn.Read(ctx)
		if err != nil {
			t.Errorf("Live terminal fixture did not receive client work: %v", err)
			return
		}
		p.mu.Lock()
		p.calls = append(p.calls, geminiLifecycleCall{Method: "WS", Path: r.URL.Path, Body: bytes.Clone(payload), Key: r.Header.Get("X-Goog-Api-Key")})
		p.mu.Unlock()
		if !bytes.Contains(payload, []byte(`"clientContent"`)) {
			t.Errorf("Live terminal fixture received changed work: %s", payload)
			return
		}
		frame := `{"serverContent":{"modelTurn":{"parts":[{"text":"partial"}]}}}`
		if bytes.Contains(setup, []byte("terminal-fixture-error")) {
			frame = `{"error":{"code":500,"message":"native failure"}}`
		}
		if err := conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
			t.Errorf("Live terminal fixture write: %v", err)
			return
		}
		if bytes.Contains(setup, []byte("terminal-fixture-client-close")) {
			_, _, _ = conn.Read(ctx)
			return
		}
		_ = conn.Close(websocket.StatusNormalClosure, "")
		return
	}
	// Certification only needs setup; a public Live session also sends ordered
	// realtime input and receives exact audio, interruption and turn completion.
	for {
		_, payload, err := conn.Read(ctx)
		if err != nil {
			return
		}
		p.mu.Lock()
		p.calls = append(p.calls, geminiLifecycleCall{Method: "WS", Path: r.URL.Path, Body: bytes.Clone(payload), Key: r.Header.Get("X-Goog-Api-Key")})
		p.mu.Unlock()
		if withTool && bytes.Contains(payload, []byte(`"audio/pcm;rate=16000"`)) {
			_ = conn.Write(ctx, websocket.MessageText, []byte(`{"toolCall":{"functionCalls":[{"id":"native-call-1","name":"native","args":{"value":1}}]}}`))
			continue
		}
		if withTool && bytes.Contains(payload, []byte(`"toolResponse"`)) || !withTool && bytes.Contains(payload, []byte(`"AQIDBA=="`)) {
			_ = conn.Write(ctx, websocket.MessageText, []byte(`{"serverContent":{"modelTurn":{"role":"model","parts":[{"inlineData":{"mimeType":"audio/pcm;rate=24000","data":"AQIDBA=="}}]},"interrupted":true,"turnComplete":true},"usageMetadata":{"promptTokenCount":3,"responseTokenCount":2}}`))
		}
	}
}

func (p *geminiLifecycleProvider) captured() []geminiLifecycleCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]geminiLifecycleCall(nil), p.calls...)
}

func provisionGeminiLifecycle(t *testing.T, h *accessHarness, owner *browser, profile string, provider *geminiLifecycleProvider, keyFields ...map[string]any) (string, string, string) {
	t.Helper()
	config := map[string]any{"kind": "gemini", "profile_id": profile, "profile_revision": "1", "endpoint": provider.URL + "/v1beta", "auth_mode": "api_key"}
	created := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": profile + uuid.NewString(), "configuration": config, "model": vendorModel, "credential": vendorSecret}, idem(uuid.NewString()), 201)
	providerPath := "/api/v1/providers/" + created["id"].(string)
	modelID := h.want(owner, "GET", providerPath+"/models", nil, nil, 200)["items"].([]any)[0].(map[string]any)["id"].(string)
	operation, mode := "generation", "unary"
	capabilities := []any{map[string]any{"operation": operation, "surface": "gemini", "mode": mode}}
	if profile == "gemini-interactions" {
		capabilities = append(capabilities, map[string]any{"operation": operation, "surface": "gemini", "mode": "streaming"})
	} else if profile == "gemini-agents" {
		operation = "agent_resource"
		capabilities = []any{map[string]any{"operation": operation, "surface": "gemini", "mode": "unary"}}
	} else {
		capabilities = []any{map[string]any{"operation": "realtime", "surface": "gemini", "mode": "realtime"}}
		operation = "realtime"
	}
	created = h.want(owner, "PATCH", providerPath+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": capabilities}, etagHeader(created), 200)
	h.want(owner, "POST", providerPath+"/models/"+modelID+"/certify", nil, etagHeader(created), 200)
	created = h.want(owner, "GET", providerPath, nil, nil, 200)
	h.want(owner, "POST", providerPath+"/activate", nil, withMatch(created, idem(uuid.NewString())), 200)
	slug := "gemini-" + uuid.NewString()
	draftInput := fidelityDraft(slug, created["id"])
	draftInput["operations"] = []string{operation}
	draftInput["fidelity"] = map[string]any{"mode": "strict"}
	draft := h.want(owner, "POST", "/api/v1/route-drafts", draftInput, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 200)
	keyInput := map[string]any{"name": profile + " key", "scopes": []string{"inference"}, "allowed_routes": []string{slug}, "allow_provider_state": true}
	for _, fields := range keyFields {
		for name, value := range fields {
			keyInput[name] = value
		}
	}
	key := h.want(owner, "POST", "/api/v1/api-keys", keyInput, idem(uuid.NewString()), 201)
	h.refresh()
	return slug, key["secret"].(string), created["id"].(string)
}

func geminiPublic(t *testing.T, h *accessHarness, method, path, key string, body []byte, headers ...map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, h.HTTP.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Goog-Api-Key", key)
	for _, fields := range headers {
		for name, value := range fields {
			req.Header.Set(name, value)
		}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, raw
}

// The fixture CA is scoped to the SDK's local TLS connection. uv also
// consumes SSL_CERT_FILE while installing packages; giving it only this
// fixture CA would make a fresh CI worker reject the package index.
