//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

var strictRealtimeClientFrames = [][]byte{
	[]byte(`{"type":"session.update","event_id":"vad_1","session":{"turn_detection":{"type":"server_vad","threshold":0.42,"prefix_padding_ms":240,"silence_duration_ms":510},"tools":[{"type":"function","name":"weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}],"input_audio_format":"pcm16","output_audio_format":"pcm16"}}`),
	[]byte(`{"type":"input_audio_buffer.append","event_id":"audio_2","audio":"AQIDBA=="}`),
	[]byte(`{"type":"response.cancel","event_id":"interrupt_3"}`),
	[]byte(`{"type":"conversation.item.create","event_id":"tool_4","item":{"type":"function_call_output","call_id":"call_exact","output":"{\"temp\":-0}"}}`),
	[]byte(`{"type":"response.create","event_id":"next_5","response":{"modalities":["audio","text"]}}`),
}

var strictRealtimeProviderFrames = [][]byte{
	[]byte(`{"type":"session.updated","event_id":"vad_1","session":{"turn_detection":{"type":"server_vad","threshold":0.42,"prefix_padding_ms":240,"silence_duration_ms":510},"tools":[{"type":"function","name":"weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]}}`),
	[]byte(`{"type":"input_audio_buffer.speech_started","event_id":"audio_2","audio_start_ms":21,"item_id":"item_exact"}`),
	[]byte(`{"type":"response.done","event_id":"interrupt_3","response":{"id":"response_cancelled","status":"cancelled","output":[]}}`),
	[]byte(`{"type":"conversation.item.created","event_id":"tool_4","item":{"call_id":"call_exact","type":"function_call_output","output":"{\"temp\":-0}"}}`),
	[]byte(`{"type":"response.audio.delta","event_id":"next_5","response_id":"response_next","delta":"AQIDBA==","audio_end_ms":53}`),
}

var strictRealtimeToolCall = []byte(`{"type":"response.function_call_arguments.done","event_id":"call_3","call_id":"call_exact","arguments":"{\"city\":\"Oslo\"}"}`)

type strictRealtimeFixture struct {
	*httptest.Server
	dials atomic.Int64
}

func newStrictRealtimeFixture(t *testing.T, kind string) *strictRealtimeFixture {
	t.Helper()
	f := &strictRealtimeFixture{}
	prefix := "/v1"
	if kind == "azure_openai" {
		prefix = "/openai/v1"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Logf("unmatched strict realtime fixture request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})
	mux.HandleFunc("GET "+prefix+"/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []map[string]string{{"id": vendorModel}}})
	})
	mux.HandleFunc("POST "+prefix+"/responses", func(w http.ResponseWriter, r *http.Request) {
		writeResponsesFixture(w, vendorModel, "OK", false)
	})
	mux.HandleFunc("GET "+prefix+"/realtime", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("model") != vendorModel || len(r.URL.Query()) != 1 {
			http.Error(w, "model mismatch", http.StatusBadRequest)
			return
		}
		if kind == "azure_openai" && r.Header.Get("Api-Key") != vendorSecret ||
			kind == "openai" && r.Header.Get("Authorization") != "Bearer "+vendorSecret {
			http.Error(w, "credential mismatch", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		f.dials.Add(1)
		for i, want := range strictRealtimeClientFrames {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			kind, got, err := conn.Read(ctx)
			cancel()
			if err != nil {
				return // certification intentionally closes after the handshake
			}
			if kind != websocket.MessageText || !bytes.Equal(got, want) {
				t.Errorf("provider received changed native frame %d: %q", i, got)
				return
			}
			// This pause gives the public test an observable event-order/timing
			// boundary without claiming a production latency bound.
			time.Sleep(2 * time.Millisecond)
			if err := conn.Write(r.Context(), websocket.MessageText, strictRealtimeProviderFrames[i]); err != nil {
				return
			}
			if i == 1 {
				if err := conn.Write(r.Context(), websocket.MessageText, strictRealtimeToolCall); err != nil {
					return
				}
			}
		}
		// A completed exchange leaves the duplex session open. The client owns
		// the later disconnect, which must be recorded as cancellation.
		_, _, _ = conn.Read(r.Context())
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func provisionStrictRealtime(t *testing.T, h *accessHarness, kind, endpoint string, networkCredential ...string) (string, string) {
	t.Helper()
	owner := h.owner()
	profile := "openai-responses"
	if kind == "azure_openai" {
		profile = "azure-v1-responses"
	}
	configuration := map[string]any{"kind": kind, "profile_id": profile, "profile_revision": "1", "auth_mode": "api_key", "endpoint": endpoint}
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Strict native realtime", "configuration": configuration, "model": vendorModel, "credential": vendorSecret}, map[string]string{"Idempotency-Key": uuid.NewString()}, http.StatusCreated)
	path := "/api/v1/providers/" + provider["id"].(string)
	if len(networkCredential) != 0 {
		stored := h.want(owner, "POST", path+"/network-credentials", map[string]any{"credential": networkCredential[0]}, withMatch(provider, map[string]string{"Idempotency-Key": uuid.NewString()}), http.StatusCreated)
		configuration["options"] = map[string]any{"network": map[string]any{"credential_id": stored["credential_id"]}}
		provider = h.want(owner, "PATCH", path, map[string]any{"name": "Strict native realtime", "configuration": configuration}, etagHeader(stored), http.StatusOK)
	}
	if probe := h.want(owner, "POST", path+"/probe", nil, etagHeader(provider), http.StatusOK); probe["succeeded"] != true {
		t.Fatalf("native provider probe: %v", probe)
	}
	models := h.want(owner, "GET", path+"/models", nil, nil, http.StatusOK)
	modelID := models["items"].([]any)[0].(map[string]any)["id"].(string)
	provider = h.want(owner, "PATCH", path+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": []any{map[string]any{"operation": "realtime", "surface": "openai", "mode": "realtime"}}}, etagHeader(provider), http.StatusOK)
	if certified := h.want(owner, "POST", path+"/models/"+modelID+"/certify", nil, etagHeader(provider), http.StatusOK); certified["status"] != "certified" {
		t.Fatalf("native realtime certification: %v", certified)
	}
	provider = h.want(owner, "GET", path, nil, nil, http.StatusOK)
	h.want(owner, "POST", path+"/activate", nil, withMatch(provider, map[string]string{"Idempotency-Key": uuid.NewString()}), http.StatusOK)
	slug := "strict-realtime-" + uuid.NewString()[:8]
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{
		"slug": slug, "operations": []string{"realtime"}, "fidelity": map[string]any{"mode": "strict"},
		"overall_timeout_ms": 30000, "max_attempts": 1,
		"targets": []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 20000}},
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, http.StatusCreated)
	h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, map[string]string{"Idempotency-Key": uuid.NewString()}), http.StatusOK)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Strict native realtime", "scopes": []string{"inference"}, "allowed_routes": []string{slug}}, map[string]string{"Idempotency-Key": uuid.NewString()}, http.StatusCreated)["secret"].(string)
	h.refresh()
	return slug, key
}

func TestStrictRealtimeCurrentNetworkCredentialRevocation(t *testing.T) {
	h := newAccessHarness(t)
	sink := &captureSink{}
	h.Gateway.Sink = sink
	fixture := newStrictRealtimeFixture(t, "openai")
	network := newProfileNetworkFixture(t, false)
	slug, key := provisionStrictRealtime(t, h, "openai", fixture.URL+"/v1", network.credential)
	owner := &browser{}
	h.want(owner, "POST", "/api/v1/sessions", map[string]any{"email": "owner@example.com", "password": accessPassword}, nil, http.StatusCreated)
	var providerID, networkID string
	if err := h.Pool.QueryRow(t.Context(), `SELECT p.id::text,n.id::text FROM olp.providers p JOIN olp.provider_network_credentials n ON n.provider_id=p.id`).Scan(&providerID, &networkID); err != nil {
		t.Fatal(err)
	}
	endpoint := strings.Replace(h.HTTP.URL, "http://", "ws://", 1) + "/v1/realtime?model=" + slug
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	before := fixture.dials.Load()
	conn, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + key}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	for deadline := time.Now().Add(3 * time.Second); fixture.dials.Load() == before && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if fixture.dials.Load() != before+1 {
		t.Fatalf("active strict session did not reach one provider: %d", fixture.dials.Load()-before)
	}
	detail := h.want(owner, "GET", "/api/v1/providers/"+providerID, nil, nil, http.StatusOK)
	h.want(owner, "POST", "/api/v1/providers/"+providerID+"/network-credentials/"+networkID+"/revoke", nil, withMatch(detail, map[string]string{"Idempotency-Key": uuid.NewString()}), http.StatusOK)
	h.refresh()
	_, _, err = conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("revoked network credential did not close active native session: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for sink.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if sink.count() != 1 || fixture.dials.Load() != before+1 {
		t.Fatalf("network revocation lost terminal or dispatched again: terminals=%d dials=%d", sink.count(), fixture.dials.Load()-before)
	}
	terminal := sink.last()
	if terminal.Outcome != "failure" || terminal.ErrorClass != "provider_credential_revoked" || !terminal.Committed || len(terminal.Attempts) != 1 {
		t.Fatalf("network revocation lost native Attempt classification: %+v", terminal)
	}
}

func TestStrictRealtimeNativeIdentityAndRefusals(t *testing.T) {
	for _, kind := range []string{"openai", "azure_openai"} {
		t.Run(kind, func(t *testing.T) {
			h := newAccessHarness(t)
			sink := &captureSink{}
			h.Gateway.Sink = sink
			fixture := newStrictRealtimeFixture(t, kind)
			endpoint := fixture.URL + "/v1"
			if kind == "azure_openai" {
				endpoint = fixture.URL
			}
			slug, key := provisionStrictRealtime(t, h, kind, endpoint)
			base := strings.Replace(h.HTTP.URL, "http://", "ws://", 1) + "/v1/realtime?model=" + slug
			header := http.Header{"Authorization": {"Bearer " + key}}
			before := fixture.dials.Load()
			for _, query := range []string{"&voice=alloy", "&model=another", "&api-version=2025-04-01-preview"} {
				conn, response, err := websocket.Dial(t.Context(), base+query, &websocket.DialOptions{HTTPHeader: header})
				if conn != nil {
					conn.CloseNow()
				}
				if err == nil || response == nil || response.StatusCode != http.StatusBadRequest || fixture.dials.Load() != before {
					t.Fatalf("query control %q reached provider or was admitted: status=%v err=%v", query, response, err)
				}
				var body struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				if json.NewDecoder(response.Body).Decode(&body) != nil || body.Error.Code != "realtime_control_unavailable" {
					t.Fatalf("unstructured query refusal for %q: %+v", query, body)
				}
				response.Body.Close()
			}
			for _, name := range []string{"OpenAI-Beta", "OpenAI-Safety-Identifier"} {
				bad := header.Clone()
				bad.Set(name, "unqualified")
				conn, response, err := websocket.Dial(t.Context(), base, &websocket.DialOptions{HTTPHeader: bad})
				if conn != nil {
					conn.CloseNow()
				}
				if err == nil || response == nil || response.StatusCode != http.StatusBadRequest || fixture.dials.Load() != before {
					t.Fatalf("semantic header %q reached provider or was admitted: status=%v err=%v", name, response, err)
				}
				response.Body.Close()
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, base, &websocket.DialOptions{HTTPHeader: header})
			if err != nil {
				t.Fatalf("strict native realtime dial: %v", err)
			}
			defer conn.CloseNow()
			var toolCallAt time.Time
			for i, sent := range strictRealtimeClientFrames {
				if i == 3 && toolCallAt.IsZero() {
					t.Fatal("tool output preceded its native call")
				}
				if err := conn.Write(ctx, websocket.MessageText, sent); err != nil {
					t.Fatalf("client frame %d: %v", i, err)
				}
				kind, got, err := conn.Read(ctx)
				if err != nil || kind != websocket.MessageText || !bytes.Equal(got, strictRealtimeProviderFrames[i]) {
					t.Fatalf("native event %d changed or reordered: kind=%v err=%v got=%q", i, kind, err, got)
				}
				if i == 1 {
					kind, got, err = conn.Read(ctx)
					if err != nil || kind != websocket.MessageText || !bytes.Equal(got, strictRealtimeToolCall) {
						t.Fatalf("native tool call changed or reordered: kind=%v err=%v got=%q", kind, err, got)
					}
					toolCallAt = time.Now()
				}
			}
			if fixture.dials.Load() != before+1 {
				t.Fatalf("positive strict session dispatches=%d, want one", fixture.dials.Load()-before)
			}
			prior := sink.count()
			conn.CloseNow()
			deadline := time.Now().Add(3 * time.Second)
			for sink.count() == prior && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if sink.count() != prior+1 {
				t.Fatalf("disconnect terminal count=%d, want one", sink.count()-prior)
			}
			terminal := sink.last()
			if terminal.Outcome != "cancelled" || terminal.Status != 0 || terminal.ErrorClass != "client_cancelled" ||
				!terminal.Committed || len(terminal.Attempts) != 1 || terminal.Attempts[0].Class != "cancelled" {
				t.Fatalf("strict disconnect lost Attempt/cancel classification: %+v", terminal)
			}
			interaction := terminal.Attempts[0].Interaction
			if interaction == nil || interaction.Fidelity != "strict" || interaction.PlanClass != "native_identity" ||
				interaction.UpstreamState != "outcome-unknown" || interaction.ClientState != "partially-observed" {
				t.Fatalf("strict duplex Attempt lost native observation state: %+v", interaction)
			}
		})
	}
}

func TestStrictRealtimeCurrentProviderCredentialRevocation(t *testing.T) {
	h := newAccessHarness(t)
	sink := &captureSink{}
	h.Gateway.Sink = sink
	fixture := newStrictRealtimeFixture(t, "openai")
	slug, key := provisionStrictRealtime(t, h, "openai", fixture.URL+"/v1")
	owner := &browser{}
	h.want(owner, "POST", "/api/v1/sessions", map[string]any{"email": "owner@example.com", "password": accessPassword}, nil, http.StatusCreated)
	var providerID, credentialID string
	if err := h.Pool.QueryRow(t.Context(), `SELECT p.id::text,s.credential_id::text FROM olp.providers p JOIN olp.provider_slots s ON s.provider_id=p.id AND s.is_default`).Scan(&providerID, &credentialID); err != nil {
		t.Fatal(err)
	}
	endpoint := strings.Replace(h.HTTP.URL, "http://", "ws://", 1) + "/v1/realtime?model=" + slug
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	before := fixture.dials.Load()
	conn, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + key}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	for deadline := time.Now().Add(3 * time.Second); fixture.dials.Load() == before && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if fixture.dials.Load() != before+1 {
		t.Fatalf("active strict session did not reach exactly one provider: %d", fixture.dials.Load()-before)
	}
	detail := h.want(owner, "GET", "/api/v1/providers/"+providerID, nil, nil, http.StatusOK)
	h.want(owner, "POST", "/api/v1/providers/"+providerID+"/credentials/"+credentialID+"/revoke", nil, withMatch(detail, map[string]string{"Idempotency-Key": uuid.NewString()}), http.StatusOK)
	h.refresh()
	_, _, err = conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("revoked provider credential did not close active native session: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for sink.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if sink.count() != 1 || fixture.dials.Load() != before+1 {
		t.Fatalf("revocation caused missing/duplicate terminal or dispatch: terminals=%d dials=%d", sink.count(), fixture.dials.Load()-before)
	}
	terminal := sink.last()
	if terminal.Outcome != "failure" || terminal.ErrorClass != "provider_credential_revoked" || !terminal.Committed ||
		len(terminal.Attempts) != 1 || terminal.Attempts[0].Class != "credential" {
		t.Fatalf("revoked provider credential lost its native Attempt classification: %+v", terminal)
	}
}
