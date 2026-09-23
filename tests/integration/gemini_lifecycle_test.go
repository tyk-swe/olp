//go:build integration

package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

type geminiLifecycleProvider struct {
	*httptest.Server
	mu       sync.Mutex
	calls    []geminiLifecycleCall
	sequence int
	slowSent atomic.Int32
	slowDone chan struct{}
	badAck   atomic.Bool
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
		p.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1beta/interactions":
			var input struct {
				Stream     bool `json:"stream"`
				Background bool `json:"background"`
			}
			_ = json.Unmarshal(body, &input)
			if input.Background {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"id":%q,"model":%q,"status":"in_progress","object":"interaction"}`, id, vendorModel)
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
			fmt.Fprintf(w, `{ "id":%q,"model":%q,"status":"completed","steps":[{"type":"thought","signature":"opaque-native","content":[{"type":"text","text":"thinking"}]},{"type":"model_output","content":[{"type":"text","text":"OK"}]}],"usage":{"total_input_tokens":3,"total_output_tokens":2},"native":{"number":9007199254740993}}`, id, vendorModel)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1beta/interactions/"):
			id = strings.TrimPrefix(r.URL.Path, "/v1beta/interactions/")
			if r.URL.Query().Get("stream") == "true" {
				w.Header().Set("Content-Type", "text/event-stream")
				if r.URL.Query().Get("last_event_id") == "" {
					fmt.Fprintf(w, "id: cursor-created\ndata: {\"event_type\":\"interaction.created\",\"interaction\":{\"id\":%q,\"status\":\"in_progress\"}}\n\n", id)
					fmt.Fprint(w, "id: cursor-start\ndata: {\"event_type\":\"step.start\",\"step\":{\"type\":\"model_output\"}}\n\n")
				}
				fmt.Fprint(w, "id: cursor-delta\ndata: {\"event_type\":\"step.delta\",\"delta\":{\"type\":\"text\",\"text\":\"retrieved\"}}\n\n")
				fmt.Fprint(w, "id: cursor-stop\ndata: {\"event_type\":\"step.stop\",\"step\":{\"type\":\"model_output\"}}\n\n")
				fmt.Fprintf(w, "id: cursor-complete\ndata: {\"event_type\":\"interaction.completed\",\"interaction\":{\"id\":%q,\"status\":\"completed\"}}\n\n", id)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":%q,"model":%q,"status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"retrieved"}]}]}`, id, vendorModel)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
			id = strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1beta/interactions/"), "/cancel")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":%q,"model":%q,"status":"cancelled"}`, id, vendorModel)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1beta/interactions/"):
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

func provisionGeminiLifecycle(t *testing.T, h *accessHarness, owner *browser, profile string, provider *geminiLifecycleProvider) (string, string, string) {
	t.Helper()
	config := map[string]any{"kind": "gemini", "profile_id": profile, "profile_revision": "1", "endpoint": provider.URL + "/v1beta", "auth_mode": "api_key"}
	created := h.want(owner, "POST", "/api/v3/providers", map[string]any{"name": profile + uuid.NewString(), "configuration": config, "model": vendorModel, "credential": vendorSecret}, idem(uuid.NewString()), 201)
	providerPath := "/api/v3/providers/" + created["id"].(string)
	modelID := h.want(owner, "GET", providerPath+"/models", nil, nil, 200)["items"].([]any)[0].(map[string]any)["id"].(string)
	operation, mode := "generation", "unary"
	capabilities := []any{map[string]any{"operation": operation, "surface": "gemini", "mode": mode}}
	if profile == "gemini-interactions" {
		capabilities = append(capabilities, map[string]any{"operation": operation, "surface": "gemini", "mode": "streaming"})
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
	draft := h.want(owner, "POST", "/api/v3/route-drafts", draftInput, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v3/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 200)
	key := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": profile + " key", "scopes": []string{"inference"}, "allowed_routes": []string{slug}, "allow_provider_state": true}, idem(uuid.NewString()), 201)
	h.refresh()
	return slug, key["secret"].(string), created["id"].(string)
}

func geminiPublic(t *testing.T, h *accessHarness, method, path, key string, body []byte) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, h.HTTP.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Goog-Api-Key", key)
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

func TestGeminiInteractionsPublicOwnedTwoTurnAndResourceLifecycle(t *testing.T) {
	h := newAccessHarness(t)
	provider := newGeminiLifecycleProvider(t)
	owner := h.owner()
	slug, key, _ := provisionGeminiLifecycle(t, h, owner, "gemini-interactions", provider)
	path := "/gemini/v1beta/interactions"
	firstRequest := []byte(fmt.Sprintf(`{"model":%q,"input":"hello","generation_config":{"max_output_tokens":64,"seed":0},"tools":[{"type":"function","name":"weather","parameters":{"type":"object"}}],"store":true}`, slug))
	response, raw := geminiPublic(t, h, http.MethodPost, path, key, firstRequest)
	if response.StatusCode != 200 {
		t.Fatalf("first Interaction: %d %s", response.StatusCode, raw)
	}
	var first struct {
		ID    string            `json:"id"`
		Steps []json.RawMessage `json:"steps"`
	}
	if json.Unmarshal(raw, &first) != nil || !strings.HasPrefix(first.ID, "interaction_") || !bytes.Contains(raw, []byte(`"signature":"opaque-native"`)) || !bytes.Contains(raw, []byte(`"number":9007199254740993`)) {
		t.Fatalf("lost native first result: %s", raw)
	}
	secondRequest := []byte(fmt.Sprintf(`{"model":%q,"input":"next","previous_interaction_id":%q,"store":true}`, slug, first.ID))
	response, raw = geminiPublic(t, h, http.MethodPost, path, key, secondRequest)
	if response.StatusCode != 200 {
		t.Fatalf("second Interaction: %d %s", response.StatusCode, raw)
	}
	var second struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &second) != nil || second.ID == first.ID || !strings.HasPrefix(second.ID, "interaction_") {
		t.Fatalf("second Interaction identity %s", raw)
	}
	prior := provider.captured()
	if len(prior) < 4 || !bytes.Contains(prior[len(prior)-1].Body, []byte(`"previous_interaction_id":"v1_fixture_3"`)) || bytes.Contains(prior[len(prior)-1].Body, []byte(first.ID)) || !bytes.Contains(prior[len(prior)-1].Body, []byte(`"model":"`+vendorModel+`"`)) {
		t.Fatalf("native previous turn was not pinned: %+v", prior)
	}
	response, raw = geminiPublic(t, h, http.MethodGet, path+"/"+first.ID, key, nil)
	if response.StatusCode != 200 || !bytes.Contains(raw, []byte(`"id":"`+first.ID+`"`)) {
		t.Fatalf("GET Interaction: %d %s", response.StatusCode, raw)
	}
	restarted := newAccessHarnessOn(t, h.Pool, h.DBURL)
	restarted.refresh()
	response, raw = geminiPublic(t, restarted, http.MethodGet, path+"/"+second.ID, key, nil)
	if response.StatusCode != 200 || !bytes.Contains(raw, []byte(`"id":"`+second.ID+`"`)) {
		t.Fatalf("fresh gateway did not recover encrypted Interaction: %d %s", response.StatusCode, raw)
	}
	response, raw = geminiPublic(t, h, http.MethodGet, path+"/"+first.ID+"?stream=true&last_event_id=cursor-start", key, nil)
	if response.StatusCode != 200 || !bytes.Contains(raw, []byte("id: cursor-delta")) || !bytes.Contains(raw, []byte(`"id":"`+first.ID+`"`)) || bytes.Contains(raw, []byte(`"id":"v1_fixture_3"`)) {
		t.Fatalf("cursor resume changed the resource or event ordering: %d %s", response.StatusCode, raw)
	}
	background := []byte(fmt.Sprintf(`{"model":%q,"input":"long running","background":true}`, slug))
	response, raw = geminiPublic(t, h, http.MethodPost, path, key, background)
	var queued struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if response.StatusCode != 200 || json.Unmarshal(raw, &queued) != nil || !strings.HasPrefix(queued.ID, "interaction_") || queued.Status != "in_progress" {
		t.Fatalf("background acceptance was not retained: %d %s", response.StatusCode, raw)
	}
	response, raw = geminiPublic(t, h, http.MethodGet, path+"/"+queued.ID, key, nil)
	if response.StatusCode != 200 || !bytes.Contains(raw, []byte(`"status":"completed"`)) || !bytes.Contains(raw, []byte(`"id":"`+queued.ID+`"`)) {
		t.Fatalf("background retrieval lost ownership or steps: %d %s", response.StatusCode, raw)
	}
	response, raw = geminiPublic(t, h, http.MethodPost, path, key, background)
	if response.StatusCode != 200 || json.Unmarshal(raw, &queued) != nil || queued.Status != "in_progress" {
		t.Fatalf("second background acceptance: %d %s", response.StatusCode, raw)
	}
	response, raw = geminiPublic(t, h, http.MethodPost, path+"/"+queued.ID+"/cancel", key, nil)
	if response.StatusCode != 200 || !bytes.Contains(raw, []byte(`"status":"cancelled"`)) || !bytes.Contains(raw, []byte(`"id":"`+queued.ID+`"`)) {
		t.Fatalf("background cancellation: %d %s", response.StatusCode, raw)
	}
	response, raw = geminiPublic(t, h, http.MethodPost, path+"/"+first.ID+"/cancel", key, nil)
	if response.StatusCode != 200 || !bytes.Contains(raw, []byte(`"status":"cancelled"`)) {
		t.Fatalf("cancel Interaction: %d %s", response.StatusCode, raw)
	}
	response, raw = geminiPublic(t, h, http.MethodDelete, path+"/"+first.ID, key, nil)
	if response.StatusCode != 200 || len(raw) != 0 {
		t.Fatalf("delete Interaction: %d %s", response.StatusCode, raw)
	}
	response, _ = geminiPublic(t, h, http.MethodGet, path+"/"+first.ID, key, nil)
	if response.StatusCode != 404 {
		t.Fatalf("deleted Interaction remained available: %d", response.StatusCode)
	}
	var residualSecret bool
	if err := h.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM olp_go.secrets s JOIN olp_go.provider_resources r ON r.id=s.id WHERE r.kind='interaction' AND r.state='deleted')`).Scan(&residualSecret); err != nil || residualSecret {
		t.Fatalf("deleted Interaction retained encrypted provider ID: %v %v", residualSecret, err)
	}
	var storedNativeID bool
	if err := h.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM olp_go.provider_resources WHERE kind='interaction' AND (upstream_id LIKE 'v1_%' OR metadata::text LIKE '%v1_fixture_%'))`).Scan(&storedNativeID); err != nil || storedNativeID {
		t.Fatalf("native ID escaped encryption: %v %v", storedNativeID, err)
	}
}

func TestGeminiLivePublicNativeAudioAndSetupAffinity(t *testing.T) {
	h := newAccessHarness(t)
	sink := &captureSink{}
	h.Gateway.Sink = sink
	provider := newGeminiLifecycleProvider(t)
	owner := h.owner()
	slug, key, _ := provisionGeminiLifecycle(t, h, owner, "gemini-live", provider)
	address := "ws" + strings.TrimPrefix(h.HTTP.URL, "http") + "/gemini/ws/" + connectors.GeminiLiveMethod + "?key=" + url.QueryEscape(key)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	client, response, err := websocket.Dial(ctx, address, nil)
	if err != nil {
		t.Fatalf("Live handshake: %v status=%v", err, response)
	}
	defer client.Close(websocket.StatusNormalClosure, "")
	setup := []byte(fmt.Sprintf(`{"setup":{"model":"models/%s","generationConfig":{"responseModalities":["AUDIO"]},"tools":[{"functionDeclarations":[{"name":"native","parameters":{"type":"OBJECT","properties":{"value":{"type":"INTEGER"}}}}]}],"sessionResumption":{},"realtimeInputConfig":{"automaticActivityDetection":{"disabled":true}}}}`, slug))
	if err := client.Write(ctx, websocket.MessageText, setup); err != nil {
		t.Fatal(err)
	}
	_, ack, err := client.Read(ctx)
	if err != nil || !bytes.Contains(ack, []byte(`"setupComplete"`)) {
		t.Fatalf("Live setup acknowledgement %s: %v", ack, err)
	}
	for _, frame := range []string{`{"clientContent":{"turns":[{"role":"user","parts":[{"text":"exact text"}]}],"turnComplete":true}}`, `{"realtimeInput":{"activityStart":{}}}`, `{"realtimeInput":{"video":{"data":"AQIDBA==","mimeType":"image/jpeg"}}}`, `{"realtimeInput":{"audio":{"data":"AQIDBA==","mimeType":"audio/pcm;rate=16000"}}}`} {
		if err := client.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
			t.Fatal(err)
		}
	}
	_, toolCall, err := client.Read(ctx)
	if err != nil || !bytes.Contains(toolCall, []byte(`"id":"native-call-1"`)) || !bytes.Contains(toolCall, []byte(`"toolCall"`)) {
		t.Fatalf("Live native tool call %s: %v", toolCall, err)
	}
	toolResult := []byte(`{"toolResponse":{"functionResponses":[{"id":"native-call-1","name":"native","response":{"value":9007199254740993}}]}}`)
	if err := client.Write(ctx, websocket.MessageText, toolResult); err != nil {
		t.Fatal(err)
	}
	_, audio, err := client.Read(ctx)
	if err != nil || !bytes.Contains(audio, []byte(`"data":"AQIDBA=="`)) || !bytes.Contains(audio, []byte(`"interrupted":true`)) || !bytes.Contains(audio, []byte(`"turnComplete":true`)) {
		t.Fatalf("Live audio/interruption %s: %v", audio, err)
	}
	if err := client.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("completed Live client close: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for sink.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if sink.count() != 1 {
		t.Fatalf("completed Live session emitted %d terminal records", sink.count())
	}
	terminal := sink.last()
	if terminal.Outcome != "success" || terminal.Status != http.StatusSwitchingProtocols || terminal.ErrorClass != "" || !terminal.Committed || len(terminal.Attempts) != 1 {
		t.Fatalf("completed Live work lost successful terminal: %+v", terminal)
	}
	attempt := terminal.Attempts[0]
	if attempt.Class != "success" || attempt.Interaction == nil || attempt.Interaction.UpstreamState != "terminal" || attempt.Interaction.ClientState != "terminal" || attempt.Usage == nil || attempt.Usage.InputTokens != 3 || attempt.Usage.OutputTokens != 2 || !attempt.UsageObserved || !attempt.UsageComplete || attempt.BillingUncertain {
		t.Fatalf("completed Live work lost exact usage and terminal states: %+v", attempt)
	}
	var sawSetup, sawAudio, sawVideo, sawTool, sawText bool
	var nativeOrder []string
	publicSession := false
	for _, call := range provider.captured() {
		if call.Method == "WS" && bytes.Contains(call.Body, []byte(`"model":"models/`+vendorModel+`"`)) {
			sawSetup = true
			if bytes.Contains(call.Body, []byte(`"tools"`)) {
				publicSession = true
				nativeOrder = append(nativeOrder, "setup")
			}
		}
		if call.Method == "WS" && bytes.Contains(call.Body, []byte(`"audio/pcm;rate=16000"`)) && bytes.Contains(call.Body, []byte(`"AQIDBA=="`)) {
			sawAudio = true
			if publicSession {
				nativeOrder = append(nativeOrder, "audio")
			}
		}
		if call.Method == "WS" && bytes.Contains(call.Body, []byte(`"image/jpeg"`)) && bytes.Contains(call.Body, []byte(`"AQIDBA=="`)) {
			sawVideo = true
			if publicSession {
				nativeOrder = append(nativeOrder, "video")
			}
		}
		if call.Method == "WS" && bytes.Contains(call.Body, []byte(`"toolResponse"`)) && bytes.Contains(call.Body, []byte(`9007199254740993`)) {
			sawTool = true
			if publicSession {
				nativeOrder = append(nativeOrder, "tool_result")
			}
		}
		if call.Method == "WS" && bytes.Contains(call.Body, []byte(`"exact text"`)) {
			sawText = true
			if publicSession {
				nativeOrder = append(nativeOrder, "client_turn")
			}
		}
		if call.Method == "WS" && bytes.Contains(call.Body, []byte(`"activityStart"`)) && publicSession {
			nativeOrder = append(nativeOrder, "activity_start")
		}
	}
	if !sawSetup || !sawAudio || !sawVideo || !sawTool || !sawText {
		t.Fatalf("Live model, text, media, or tool result missing upstream: %+v", provider.captured())
	}
	if !slices.Equal(nativeOrder, []string{"setup", "client_turn", "activity_start", "video", "audio", "tool_result"}) {
		t.Fatalf("Live lost native client frame order: %v", nativeOrder)
	}
}

func TestGeminiLiveStrictTerminalContracts(t *testing.T) {
	for _, scenario := range []struct {
		name, marker, frame, outcome, code, class, upstream string
		clientCloses                                        bool
	}{
		{name: "provider_closes_partial_turn", marker: "terminal-fixture-partial", frame: `{"serverContent":{"modelTurn":{"parts":[{"text":"partial"}]}}}`, outcome: "failure", code: "live_incomplete", class: "ambiguous", upstream: "outcome-unknown"},
		{name: "provider_closes_after_native_error", marker: "terminal-fixture-error", frame: `{"error":{"code":500,"message":"native failure"}}`, outcome: "failure", code: "live_provider_error", class: "upstream_server", upstream: "terminal"},
		{name: "client_closes_partial_turn", marker: "terminal-fixture-client-close", frame: `{"serverContent":{"modelTurn":{"parts":[{"text":"partial"}]}}}`, outcome: "cancelled", code: "client_cancelled", class: "cancelled", upstream: "outcome-unknown", clientCloses: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			h := newAccessHarness(t)
			sink := &captureSink{}
			h.Gateway.Sink = sink
			provider := newGeminiLifecycleProvider(t)
			slug, key, _ := provisionGeminiLifecycle(t, h, h.owner(), "gemini-live", provider)
			beforeCalls, beforeTerminals := len(provider.captured()), sink.count()
			address := "ws" + strings.TrimPrefix(h.HTTP.URL, "http") + "/gemini/ws/" + connectors.GeminiLiveMethod + "?key=" + url.QueryEscape(key)
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			client, response, err := websocket.Dial(ctx, address, nil)
			if err != nil {
				t.Fatalf("Live strict handshake: %v status=%v", err, response)
			}
			defer client.CloseNow()
			setup := []byte(fmt.Sprintf(`{"setup":{"model":"models/%s","systemInstruction":{"parts":[{"text":%q}]}}}`, slug, scenario.marker))
			if err := client.Write(ctx, websocket.MessageText, setup); err != nil {
				t.Fatal(err)
			}
			kind, ack, err := client.Read(ctx)
			if err != nil || kind != websocket.MessageText || !bytes.Equal(ack, []byte(`{"setupComplete":{}}`)) {
				t.Fatalf("Live setup acknowledgement changed: kind=%v err=%v frame=%s", kind, err, ack)
			}
			work := []byte(`{"clientContent":{"turns":[{"role":"user","parts":[{"text":"hello"}]}],"turnComplete":true}}`)
			if err := client.Write(ctx, websocket.MessageText, work); err != nil {
				t.Fatal(err)
			}
			kind, got, err := client.Read(ctx)
			if err != nil || kind != websocket.MessageText || !bytes.Equal(got, []byte(scenario.frame)) {
				t.Fatalf("Live native event changed: kind=%v err=%v frame=%s", kind, err, got)
			}
			if scenario.clientCloses {
				if err := client.Close(websocket.StatusNormalClosure, ""); err != nil {
					t.Fatalf("Live client close: %v", err)
				}
			} else {
				_, _, err = client.Read(ctx)
				if err == nil || websocket.CloseStatus(err) == websocket.StatusNormalClosure || websocket.CloseStatus(err) == websocket.StatusGoingAway {
					t.Fatalf("failed Live work ended with successful close: %v", err)
				}
			}
			deadline := time.Now().Add(3 * time.Second)
			for sink.count() == beforeTerminals && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if sink.count() != beforeTerminals+1 || len(provider.captured()) != beforeCalls+2 {
				t.Fatalf("Live close duplicated or lost the Attempt: calls=%d terminals=%d", len(provider.captured())-beforeCalls, sink.count()-beforeTerminals)
			}
			terminal := sink.last()
			if terminal.Outcome != scenario.outcome || terminal.ErrorClass != scenario.code || terminal.Status != map[bool]int{true: 0, false: http.StatusSwitchingProtocols}[scenario.clientCloses] || !terminal.Committed || len(terminal.Attempts) != 1 {
				t.Fatalf("Live terminal outcome disagrees with native frame: %+v", terminal)
			}
			attempt := terminal.Attempts[0]
			if attempt.Class != scenario.class || attempt.Interaction == nil || attempt.Interaction.Fidelity != "strict" || attempt.Interaction.PlanClass != "native_identity" || attempt.Interaction.UpstreamState != scenario.upstream || attempt.Interaction.ClientState != "partially-observed" || attempt.UsageObserved || attempt.UsageComplete || !attempt.BillingUncertain {
				t.Fatalf("Live terminal Attempt falsely completed work or usage: %+v", attempt)
			}
		})
	}
}

func TestGeminiLifecycleRefusesUnauthorizedStateAndInvalidSetupBeforeProviderWork(t *testing.T) {
	h := newAccessHarness(t)
	provider := newGeminiLifecycleProvider(t)
	owner := h.owner()
	interactionRoute, ownerKey, providerID := provisionGeminiLifecycle(t, h, owner, "gemini-interactions", provider)
	liveRoute, liveKey, _ := provisionGeminiLifecycle(t, h, owner, "gemini-live", provider)
	path := "/gemini/v1beta/interactions"
	first, raw := geminiPublic(t, h, http.MethodPost, path, ownerKey, []byte(fmt.Sprintf(`{"model":%q,"input":"owner"}`, interactionRoute)))
	if first.StatusCode != 200 {
		t.Fatalf("stored first turn %d %s", first.StatusCode, raw)
	}
	var local struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &local) != nil || !strings.HasPrefix(local.ID, "interaction_") {
		t.Fatalf("owner mapping %s", raw)
	}
	other := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "other Gemini owner", "scopes": []string{"inference"}, "allowed_routes": []string{interactionRoute}, "allow_provider_state": true}, idem(uuid.NewString()), 201)["secret"].(string)
	noState := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "stateless Gemini owner", "scopes": []string{"inference"}, "allowed_routes": []string{interactionRoute}}, idem(uuid.NewString()), 201)["secret"].(string)
	h.refresh()
	before := len(provider.captured())
	response, _ := geminiPublic(t, h, http.MethodGet, path+"/"+local.ID, other, nil)
	if response.StatusCode != 404 {
		t.Fatalf("cross-owner GET status %d", response.StatusCode)
	}
	response, _ = geminiPublic(t, h, http.MethodPost, path, other, []byte(fmt.Sprintf(`{"model":%q,"input":"other","previous_interaction_id":%q}`, interactionRoute, local.ID)))
	if response.StatusCode != 400 {
		t.Fatalf("cross-owner continuation status %d", response.StatusCode)
	}
	response, _ = geminiPublic(t, h, http.MethodPost, path, noState, []byte(fmt.Sprintf(`{"model":%q,"input":"default storage"}`, interactionRoute)))
	if response.StatusCode != 400 {
		t.Fatalf("implicit store=true bypassed policy: %d", response.StatusCode)
	}
	response, _ = geminiPublic(t, h, http.MethodPost, path, noState, []byte(fmt.Sprintf(`{"model":%q,"model":"second","input":"duplicate"}`, interactionRoute)))
	if response.StatusCode != 400 {
		t.Fatalf("duplicate JSON member accepted: %d", response.StatusCode)
	}
	if len(provider.captured()) != before {
		t.Fatal("unauthorized Interaction dispatched to provider")
	}
	response, raw = geminiPublic(t, h, http.MethodPost, path, noState, []byte(fmt.Sprintf(`{"model":%q,"input":"stateless","store":false}`, interactionRoute)))
	if response.StatusCode != 200 || !bytes.Contains(raw, []byte(`"id":"v1_fixture_`)) {
		t.Fatalf("explicit stateless Interaction failed or fabricated a local ID: %d %s", response.StatusCode, raw)
	}
	before = len(provider.captured())
	address := "ws" + strings.TrimPrefix(h.HTTP.URL, "http") + "/gemini/ws/" + connectors.GeminiLiveMethod
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	client, upgrade, err := websocket.Dial(ctx, address+"?key="+url.QueryEscape(liveKey)+"&key="+url.QueryEscape(liveKey), nil)
	if err == nil {
		client.Close(websocket.StatusNormalClosure, "")
		t.Fatal("duplicate Live query keys upgraded")
	}
	if upgrade == nil || upgrade.StatusCode != 400 {
		t.Fatalf("duplicate Live query key status %v: %v", upgrade, err)
	}
	client, upgrade, err = websocket.Dial(ctx, address+"?key="+url.QueryEscape(liveKey), nil)
	if err != nil {
		t.Fatalf("valid Live upgrade: %v %v", upgrade, err)
	}
	_ = client.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(`{"setup":{"model":"models/%s","generationConfig":{},"generation_config":{}}}`, liveRoute)))
	_, _, _ = client.Read(ctx)
	client.Close(websocket.StatusNormalClosure, "")
	if len(provider.captured()) != before {
		t.Fatal("malformed Live setup dispatched to provider")
	}
	client, upgrade, err = websocket.Dial(ctx, address+"?key="+url.QueryEscape(liveKey), nil)
	if err != nil {
		t.Fatalf("Live resumption refusal upgrade: %v %v", upgrade, err)
	}
	if err := client.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(`{"setup":{"model":"models/%s","sessionResumption":{"handle":"unowned-native-handle"}}}`, liveRoute))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.Read(ctx); err == nil {
		t.Fatal("unowned Live resumption handle received a successful setup response")
	}
	client.Close(websocket.StatusNormalClosure, "")
	if len(provider.captured()) != before {
		t.Fatal("unowned Live resumption handle dispatched to provider")
	}
	var credentialID string
	if err := h.Pool.QueryRow(t.Context(), `SELECT credential_id::text FROM olp_go.provider_slots WHERE provider_id=$1 AND is_default`, providerID).Scan(&credentialID); err != nil {
		t.Fatal(err)
	}
	detail := h.want(owner, "GET", "/api/v3/providers/"+providerID, nil, nil, 200)
	h.want(owner, "POST", "/api/v3/providers/"+providerID+"/credentials/"+credentialID+"/revoke", nil, withMatch(detail, idem(uuid.NewString())), 200)
	h.refresh()
	response, raw = geminiPublic(t, h, http.MethodGet, path+"/"+local.ID, ownerKey, nil)
	if response.StatusCode != 409 || !bytes.Contains(raw, []byte("provider_resource_credential_unavailable")) || len(provider.captured()) != before {
		t.Fatalf("revoked historical credential reached provider: %d %s", response.StatusCode, raw)
	}
}

func TestGeminiInteractionEncryptedMappingFailsClosedOnTamperAndExpiry(t *testing.T) {
	h := newAccessHarness(t)
	provider := newGeminiLifecycleProvider(t)
	owner := h.owner()
	slug, key, _ := provisionGeminiLifecycle(t, h, owner, "gemini-interactions", provider)
	path := "/gemini/v1beta/interactions"
	create := func(input string) string {
		t.Helper()
		response, raw := geminiPublic(t, h, http.MethodPost, path, key, []byte(fmt.Sprintf(`{"model":%q,"input":%q}`, slug, input)))
		var result struct {
			ID string `json:"id"`
		}
		if response.StatusCode != 200 || json.Unmarshal(raw, &result) != nil || !strings.HasPrefix(result.ID, "interaction_") {
			t.Fatalf("create encrypted fixture: %d %s", response.StatusCode, raw)
		}
		return result.ID
	}
	tampered := create("tamper")
	expired := create("expiry")
	before := len(provider.captured())
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp_go.secrets SET ciphertext=decode('00','hex') WHERE id=(SELECT id FROM olp_go.provider_resources WHERE kind='interaction' AND replace(id::text,'-','')=$1)`, strings.TrimPrefix(tampered, "interaction_")); err != nil {
		t.Fatal(err)
	}
	response, raw := geminiPublic(t, h, http.MethodGet, path+"/"+tampered, key, nil)
	if response.StatusCode != 409 || !bytes.Contains(raw, []byte("provider_resource_unavailable")) {
		t.Fatalf("tampered ciphertext was read: %d %s", response.StatusCode, raw)
	}
	response, raw = geminiPublic(t, h, http.MethodPost, path, key, []byte(fmt.Sprintf(`{"model":%q,"input":"next","previous_interaction_id":%q}`, slug, tampered)))
	if response.StatusCode != 409 || !bytes.Contains(raw, []byte("provider_resource_unavailable")) {
		t.Fatalf("tampered previous ID reached provider: %d %s", response.StatusCode, raw)
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp_go.provider_resources SET expires_at=now()-interval '1 second' WHERE kind='interaction' AND replace(id::text,'-','')=$1`, strings.TrimPrefix(expired, "interaction_")); err != nil {
		t.Fatal(err)
	}
	response, raw = geminiPublic(t, h, http.MethodGet, path+"/"+expired, key, nil)
	if response.StatusCode != 404 || len(provider.captured()) != before {
		t.Fatalf("expired Interaction reached provider: %d %s", response.StatusCode, raw)
	}
}

func TestGeminiLiveSlowReaderAppliesBackpressureAndReleasesSession(t *testing.T) {
	h := newAccessHarness(t)
	provider := newGeminiLifecycleProvider(t)
	provider.slowDone = make(chan struct{})
	owner := h.owner()
	slug, key, _ := provisionGeminiLifecycle(t, h, owner, "gemini-live", provider)
	origin, err := url.Parse(h.HTTP.URL)
	if err != nil {
		t.Fatal(err)
	}
	client, err := net.DialTimeout("tcp", origin.Host, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	path := "/gemini/ws/" + connectors.GeminiLiveMethod + "?key=" + url.QueryEscape(key)
	if _, err := fmt.Fprintf(client, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", path, origin.Host, base64.StdEncoding.EncodeToString(nonce[:])); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(client)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(line, "101 Switching Protocols") {
		t.Fatalf("raw Live upgrade: %s %v", line, err)
	}
	for {
		line, err = reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	setup := []byte(fmt.Sprintf(`{"setup":{"model":"models/%s","systemInstruction":{"parts":[{"text":"slow-peer-fixture"}]}}}`, slug))
	var frame bytes.Buffer
	frame.WriteByte(0x81)
	frame.WriteByte(0x80 | 126)
	var size [2]byte
	binary.BigEndian.PutUint16(size[:], uint16(len(setup)))
	frame.Write(size[:])
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		t.Fatal(err)
	}
	frame.Write(mask[:])
	for i, value := range setup {
		frame.WriteByte(value ^ mask[i%4])
	}
	if _, err := client.Write(frame.Bytes()); err != nil {
		t.Fatal(err)
	}
	// The raw peer never reads a WebSocket frame, so the SDK cannot quietly
	// drain the socket in the background. A stalled or disconnected client
	// must release the provider connection and the gateway's session slot.
	time.Sleep(200 * time.Millisecond)
	if sent := provider.slowSent.Load(); sent >= 4096 {
		t.Fatalf("slow client did not backpressure provider: sent all %d frames", sent)
	}
	client.Close()
	select {
	case <-provider.slowDone:
	case <-time.After(3 * time.Second):
		t.Fatal("slow Live reader did not release the provider after disconnect")
	}
}

func TestGeminiLiveMalformedProviderAckClosesBeforeClientEvents(t *testing.T) {
	h := newAccessHarness(t)
	provider := newGeminiLifecycleProvider(t)
	owner := h.owner()
	slug, key, _ := provisionGeminiLifecycle(t, h, owner, "gemini-live", provider)
	provider.badAck.Store(true)
	before := len(provider.captured())
	address := "ws" + strings.TrimPrefix(h.HTTP.URL, "http") + "/gemini/ws/" + connectors.GeminiLiveMethod + "?key=" + url.QueryEscape(key)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	client, response, err := websocket.Dial(ctx, address, nil)
	if err != nil {
		t.Fatalf("Live malformed-ack handshake: %v %v", response, err)
	}
	defer client.CloseNow()
	setup := []byte(fmt.Sprintf(`{"setup":{"model":"models/%s"}}`, slug))
	if err := client.Write(ctx, websocket.MessageText, setup); err != nil {
		t.Fatal(err)
	}
	_, delivered, err := client.Read(ctx)
	if err == nil || len(delivered) > 0 || websocket.CloseStatus(err) != websocket.StatusProtocolError {
		t.Fatalf("malformed provider setup was exposed to client: %s %v", delivered, err)
	}
	if len(provider.captured()) != before+1 {
		t.Fatalf("malformed ack triggered another provider Attempt: %+v", provider.captured())
	}
}

func geminiTrustedTLSServer(t *testing.T, handler http.Handler) (*httptest.Server, string) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "OLP Gemini qualification CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caParsed, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverCertificate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), BasicConstraintsValid: true,
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverCertificate, caParsed, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(serverKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}))
	if err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(t.TempDir(), "gemini-qualification-ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, caFile
}

func TestGeminiLifecyclePinnedOfficialSDKsThroughTrustedTLS(t *testing.T) {
	h := newAccessHarness(t)
	provider := newGeminiLifecycleProvider(t)
	owner := h.owner()
	interactionRoute, _, _ := provisionGeminiLifecycle(t, h, owner, "gemini-interactions", provider)
	liveRoute, _, _ := provisionGeminiLifecycle(t, h, owner, "gemini-live", provider)
	key := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "Gemini SDK lifecycle", "scopes": []string{"inference"},
		"allowed_routes": []string{interactionRoute, liveRoute}, "allow_provider_state": true}, idem(uuid.NewString()), 201)["secret"].(string)
	h.refresh()
	var pathsMu sync.Mutex
	var nativePaths []string
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/ws/"+connectors.GeminiLiveMethod) {
			pathsMu.Lock()
			nativePaths = append(nativePaths, r.RequestURI)
			pathsMu.Unlock()
		}
		h.HTTP.Config.Handler.ServeHTTP(w, r)
	})
	tlsServer, caFile := geminiTrustedTLSServer(t, wrapped)
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	shared := []string{
		"OLP_GEMINI_BASE=" + tlsServer.URL + "/gemini",
		"OLP_GEMINI_KEY=" + key,
		"OLP_GEMINI_INTERACTION_ROUTE=" + interactionRoute,
		"OLP_GEMINI_LIVE_ROUTE=" + liveRoute,
		"SSL_CERT_FILE=" + caFile,
		"NODE_EXTRA_CA_CERTS=" + caFile,
	}
	// The fixture CA is scoped to the SDK's local TLS connection. uv also
	// consumes SSL_CERT_FILE while installing packages; giving it only this
	// fixture CA would make a fresh CI worker reject the package index.
	sync := exec.CommandContext(t.Context(), "uv", "sync", "--project", "tests/sdk-smoke-python", "--frozen")
	sync.Dir = root
	if output, err := sync.CombinedOutput(); err != nil {
		t.Fatalf("sync pinned Gemini Python SDK before fixture TLS: %v\n%s", err, output)
	}
	for _, command := range [][]string{{"node", "tests/sdk-smoke/gemini-lifecycle.mjs"}, {"uv", "run", "--project", "tests/sdk-smoke-python", "--frozen", "--no-sync", "python", "tests/sdk-smoke-python/gemini_lifecycle.py"}} {
		cmd := exec.CommandContext(t.Context(), command[0], command[1:]...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), shared...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("pinned Gemini %s SDK through public OLP: %v\n%s", command[0], err, output)
		}
		if !bytes.Contains(output, []byte(`audio/interruption`)) {
			t.Fatalf("SDK did not finish its Live contract: %s", output)
		}
	}
	hello, next, streamed, liveAudio, liveActivity := 0, 0, 0, 0, 0
	for _, call := range provider.captured() {
		if call.Key != vendorSecret || bytes.Contains(call.Body, []byte(key)) {
			t.Fatal("public key reached the provider")
		}
		if call.Method == "WS" {
			if bytes.Contains(call.Body, []byte(`"AQIDBA=="`)) && bytes.Contains(call.Body, []byte(`"audio/pcm;rate=16000"`)) {
				liveAudio++
			}
			if bytes.Contains(call.Body, []byte(`"activityStart"`)) {
				liveActivity++
			}
			continue
		}
		if call.Method != http.MethodPost || call.Path != "/v1beta/interactions" {
			continue
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(call.Body, &fields) != nil || string(fields["model"]) != `"`+vendorModel+`"` {
			t.Fatalf("SDK request model was not bound: %s", call.Body)
		}
		switch string(fields["input"]) {
		case `"hello"`:
			hello++
			if string(fields["store"]) != "true" || !bytes.Contains(fields["generation_config"], []byte(`"max_output_tokens":64`)) || !bytes.Contains(fields["generation_config"], []byte(`"seed":0`)) || len(fields["tools"]) == 0 {
				t.Fatalf("pinned SDK dropped first-turn controls: %s", call.Body)
			}
		case `"next"`:
			next++
			var previous string
			if json.Unmarshal(fields["previous_interaction_id"], &previous) != nil || !strings.HasPrefix(previous, "v1_fixture_") || len(fields["tools"]) > 0 || len(fields["generation_config"]) > 0 || len(fields["system_instruction"]) > 0 {
				t.Fatalf("pinned SDK next turn lost native affinity or inherited controls: %s", call.Body)
			}
		case `"stream"`:
			streamed++
			if string(fields["stream"]) != "true" || string(fields["store"]) != "true" {
				t.Fatalf("pinned SDK stream options changed: %s", call.Body)
			}
		}
	}
	if hello != 2 || next != 2 || streamed != 2 || liveAudio != 2 || liveActivity != 2 {
		t.Fatalf("pinned SDKs did not complete exact upstream workflows: hello=%d next=%d stream=%d audio=%d activity=%d", hello, next, streamed, liveAudio, liveActivity)
	}
	pathsMu.Lock()
	defer pathsMu.Unlock()
	if len(nativePaths) != 3 {
		t.Fatalf("the SDK origin trap and two supported Live handshakes did not reach public OLP: %v", nativePaths)
	}
	doubleSlash, javascript, python := false, false, false
	for _, path := range nativePaths {
		switch path {
		case "//ws/" + connectors.GeminiLiveMethod + "?key=" + url.QueryEscape(key):
			doubleSlash = true
		case "/gemini/ws/" + connectors.GeminiLiveMethod + "?key=" + url.QueryEscape(key):
			javascript = true
		case "/gemini/ws/" + connectors.GeminiLiveMethod:
			python = true
		default:
			t.Fatalf("unexpected SDK Live path: %s", path)
		}
	}
	if !doubleSlash || !javascript || !python {
		t.Fatalf("SDK transport shapes were not independently observed: %v", nativePaths)
	}
}
