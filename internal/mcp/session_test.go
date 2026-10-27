package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/runtime"
)

type rpcResponse struct {
	status  int
	headers http.Header
	body    map[string]any
	raw     string
}

func rpcRequest(t *testing.T, server *Server, token, sessionID, protocol, path, body string) rpcResponse {
	t.Helper()
	mux := http.NewServeMux()
	server.Register(mux)
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(http.MethodPost, path, reader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	if protocol != "" {
		req.Header.Set("MCP-Protocol-Version", protocol)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	out := rpcResponse{status: rec.Code, headers: rec.Header(), raw: rec.Body.String()}
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out.body); err != nil {
			t.Fatalf("MCP response is not JSON: %s", rec.Body.String())
		}
	}
	return out
}

func rpcDelete(t *testing.T, server *Server, token, sessionID, protocol string) int {
	t.Helper()
	mux := http.NewServeMux()
	server.Register(mux)
	req := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	if protocol != "" {
		req.Header.Set("MCP-Protocol-Version", protocol)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code
}

func rpcInitialize(t *testing.T, server *Server, token, protocol, toolsetID string) rpcResponse {
	t.Helper()
	path := "/mcp?toolset_id=" + toolsetID
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":%q,"capabilities":{},"clientInfo":{"name":"fixture-client","version":"1"}}}`, protocol)
	return rpcRequest(t, server, token, "", "", path, body)
}

func rpcResult(t *testing.T, response rpcResponse) map[string]any {
	t.Helper()
	if message, ok := response.body["error"]; ok {
		t.Fatalf("MCP call returned an error: %v", message)
	}
	result, _ := response.body["result"].(map[string]any)
	if result == nil {
		t.Fatalf("MCP response has no result: %d %v", response.status, response.body)
	}
	return result
}

func rpcError(t *testing.T, response rpcResponse) map[string]any {
	t.Helper()
	failure, _ := response.body["error"].(map[string]any)
	if failure == nil {
		t.Fatalf("MCP response has no error: %d %v", response.status, response.body)
	}
	return failure
}

func openSession(t *testing.T, server *Server, token, protocol string) string {
	t.Helper()
	response := rpcInitialize(t, server, token, protocol, testToolset)
	if response.status != http.StatusOK {
		t.Fatalf("initialize status %d: %v", response.status, response.body)
	}
	result := rpcResult(t, response)
	if result["protocolVersion"] != protocol {
		t.Fatalf("initialize did not negotiate %s: %v", protocol, result)
	}
	sessionID := response.headers.Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatalf("initialize returned no session header: %v", response.headers)
	}
	accepted := rpcRequest(t, server, token, sessionID, protocol, "/mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if accepted.status != http.StatusAccepted {
		t.Fatalf("initialized notification status %d: %v", accepted.status, accepted.body)
	}
	return sessionID
}

func TestStreamableHTTPSessionLifecycleAtCertifiedVersions(t *testing.T) {
	for _, protocol := range []string{"2025-11-25", "2025-06-18", "2025-03-26"} {
		t.Run(protocol, func(t *testing.T) {
			server, fx, ledger, tl, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
			sessionID := openSession(t, server, "test-key", protocol)

			listed := rpcRequest(t, server, "test-key", sessionID, protocol, "/mcp", `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
			if listed.status != http.StatusOK {
				t.Fatalf("tools/list status %d: %v", listed.status, listed.body)
			}
			tools, _ := rpcResult(t, listed)["tools"].([]any)
			if len(tools) != 1 {
				t.Fatalf("session listed tools outside its pinned toolset: %v", rpcResult(t, listed))
			}
			tool, _ := tools[0].(map[string]any)
			if tool["name"] != alias() || tool["description"] != "Look up" {
				t.Fatalf("listed tool is not the pinned alias: %v", tool)
			}
			if _, ok := tool["inputSchema"].(map[string]any); !ok {
				t.Fatalf("listed tool lost its pinned schema on the wire: %v", tool)
			}

			called := rpcRequest(t, server, "test-key", sessionID, protocol, "/mcp",
				`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"`+alias()+`","arguments":{"query":"secret-argument"}}}`)
			result := rpcResult(t, called)
			content, _ := result["content"].([]any)
			if len(content) != 1 || fx.calls.Load() != 1 {
				t.Fatalf("session call did not reach the upstream exactly once: calls=%d result=%v", fx.calls.Load(), result)
			}
			if got := strings.Join(tl.snapshot(), ","); got != "reserve,initialize,dispatched,upstream,settled" {
				t.Fatalf("session call bypassed the shared call path: %s", got)
			}
			if len(ledger.evidence) != 1 || ledger.evidence[0].Status != StatusSucceeded {
				t.Fatalf("session call settled without evidence: %+v", ledger.evidence)
			}
			meta, _ := result["_meta"].(map[string]any)
			if meta["olp.attempt_id"] == nil || meta["olp.dispatch_id"] == nil {
				t.Fatalf("session call lost its attempt identity: %v", result)
			}

			if status := rpcDelete(t, server, "test-key", sessionID, protocol); status != http.StatusNoContent {
				t.Fatalf("session delete status %d", status)
			}
			after := rpcRequest(t, server, "test-key", sessionID, protocol, "/mcp", `{"jsonrpc":"2.0","id":4,"method":"tools/list","params":{}}`)
			if after.status != http.StatusNotFound {
				t.Fatalf("deleted session stayed usable: %d %v", after.status, after.body)
			}
		})
	}
}

const listCall = `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`

func TestStreamableHTTPSessionExpiresAfterFifteenIdleMinutes(t *testing.T) {
	server, _, _, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	current := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	server.Now = func() time.Time { return current }
	sessionID := openSession(t, server, "test-key", "2025-11-25")

	current = current.Add(14 * time.Minute)
	if response := rpcRequest(t, server, "test-key", sessionID, "2025-11-25", "/mcp", listCall); response.status != http.StatusOK {
		t.Fatalf("session expired before fifteen idle minutes: %d %v", response.status, response.body)
	}
	current = current.Add(14 * time.Minute)
	if response := rpcRequest(t, server, "test-key", sessionID, "2025-11-25", "/mcp", listCall); response.status != http.StatusOK {
		t.Fatalf("activity did not extend the idle window: %d %v", response.status, response.body)
	}
	current = current.Add(15*time.Minute + time.Second)
	response := rpcRequest(t, server, "test-key", sessionID, "2025-11-25", "/mcp", listCall)
	if response.status != http.StatusNotFound {
		t.Fatalf("session survived fifteen idle minutes: %d %v", response.status, response.body)
	}
	if data, _ := rpcError(t, response)["data"].(map[string]any); data["code"] != "session_not_found" {
		t.Fatalf("expired session did not refuse as missing: %v", response.body)
	}
}

func TestStreamableHTTPSessionCapRefusesBeyondSixtyFourPerKey(t *testing.T) {
	authority := testAuthority([]string{"tools"}, []string{testToolset})
	server, _, _, _, rt := newTestServer(t, authority, "")
	other := testAuthority([]string{"tools"}, []string{testToolset})
	other.ID = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50711"
	rt.otherAuthority = other
	for range 64 {
		if response := rpcInitialize(t, server, "test-key", "2025-11-25", testToolset); response.status != http.StatusOK {
			t.Fatalf("session within the cap refused: %d %v", response.status, response.body)
		}
	}
	response := rpcInitialize(t, server, "test-key", "2025-11-25", testToolset)
	if response.status != http.StatusTooManyRequests || response.headers.Get("Mcp-Session-Id") != "" {
		t.Fatalf("sixty-fifth session for one key was created: %d %v", response.status, response.headers)
	}
	if data, _ := rpcError(t, response)["data"].(map[string]any); data["code"] != "session_limit_exceeded" {
		t.Fatalf("session cap refusal is not explicit: %v", response.body)
	}
	if response := rpcInitialize(t, server, "other-key", "2025-11-25", testToolset); response.status != http.StatusOK {
		t.Fatalf("one key at its cap blocked another key: %d %v", response.status, response.body)
	}
}

func TestStreamableHTTPSessionRefusesOtherPrincipals(t *testing.T) {
	server, fx, ledger, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	other := testAuthority([]string{"tools"}, []string{testToolset})
	other.ID = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50711"
	rt.otherAuthority = other
	sessionID := openSession(t, server, "test-key", "2025-11-25")

	for _, body := range []string{
		listCall,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"` + alias() + `","arguments":{"query":"x"}}}`,
	} {
		if response := rpcRequest(t, server, "other-key", sessionID, "2025-11-25", "/mcp", body); response.status != http.StatusNotFound {
			t.Fatalf("another principal used the session: %d %v", response.status, response.body)
		}
	}
	if status := rpcDelete(t, server, "other-key", sessionID, "2025-11-25"); status != http.StatusNotFound {
		t.Fatalf("another principal deleted the session: %d", status)
	}
	if fx.calls.Load() != 0 || len(ledger.reserved) != 0 {
		t.Fatalf("cross-principal use dispatched: calls=%d reserved=%d", fx.calls.Load(), len(ledger.reserved))
	}
	if response := rpcRequest(t, server, "test-key", sessionID, "2025-11-25", "/mcp", listCall); response.status != http.StatusOK {
		t.Fatalf("owner lost the session after a foreign attempt: %d %v", response.status, response.body)
	}
}

func TestStreamableHTTPSessionBindsPinnedToolsetRevision(t *testing.T) {
	server, fx, _, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset, testOtherTS}), "")
	ambiguous := rpcRequest(t, server, "test-key", "", "", "/mcp",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"fixture","version":"1"}}}`)
	if ambiguous.status != http.StatusBadRequest || ambiguous.headers.Get("Mcp-Session-Id") != "" {
		t.Fatalf("session bound without a toolset selection: %d %v", ambiguous.status, ambiguous.body)
	}
	foreign := rpcInitialize(t, server, "test-key", "2025-11-25", "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50799")
	if foreign.status != http.StatusNotFound || foreign.headers.Get("Mcp-Session-Id") != "" {
		t.Fatalf("foreign toolset opened a session: %d %v", foreign.status, foreign.body)
	}

	sessionID := openSession(t, server, "test-key", "2025-11-25")
	changed := testSnapshot(t, fx.server.URL)
	changed.MCPServers[0].Tools[0].Description = "Changed after pinning"
	if err := changed.Compile(); err != nil {
		t.Fatal(err)
	}
	rt.release = &runtime.Release{ID: testGenID, Snapshot: changed}
	listed := rpcRequest(t, server, "test-key", sessionID, "2025-11-25", "/mcp", listCall)
	tools, _ := rpcResult(t, listed)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["description"] != "Look up" {
		t.Fatalf("session stopped serving its pinned definitions: %v", tools)
	}
}

func TestStreamableHTTPRefusesUnsupportedCapabilitiesExplicitly(t *testing.T) {
	server, fx, ledger, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	sessionID := openSession(t, server, "test-key", "2025-11-25")
	for method, capability := range map[string]string{
		"sampling/createMessage":   "sampling",
		"elicitation/create":       "elicitation",
		"roots/list":               "roots",
		"resources/list":           "resources",
		"resources/read":           "resources",
		"resources/templates/list": "resources",
		"prompts/list":             "prompts",
	} {
		response := rpcRequest(t, server, "test-key", sessionID, "2025-11-25", "/mcp",
			`{"jsonrpc":"2.0","id":9,"method":"`+method+`","params":{}}`)
		failure := rpcError(t, response)
		data, _ := failure["data"].(map[string]any)
		if response.status != http.StatusOK || failure["code"] != float64(rpcMethodNotFound) || data["code"] != "unsupported_capability" || data["capability"] != capability {
			t.Fatalf("%s was not refused explicitly: %d %v", method, response.status, response.body)
		}
	}
	unknown := rpcRequest(t, server, "test-key", sessionID, "2025-11-25", "/mcp", `{"jsonrpc":"2.0","id":10,"method":"no/such","params":{}}`)
	if data, _ := rpcError(t, unknown)["data"].(map[string]any); data["code"] != "method_not_found" {
		t.Fatalf("unknown method masqueraded as a capability refusal: %v", unknown.body)
	}
	if fx.calls.Load() != 0 || len(ledger.reserved) != 0 {
		t.Fatalf("refused capability dispatched: calls=%d reserved=%d", fx.calls.Load(), len(ledger.reserved))
	}
}

func TestStreamableHTTPSessionCallRefusalsMatchJSONPath(t *testing.T) {
	server, fx, ledger, tl, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	sessionID := openSession(t, server, "test-key", "2025-11-25")
	for name, body := range map[string]string{
		"invalid arguments": `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"` + alias() + `","arguments":{"query":3}}}`,
		"unknown alias":     `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"lookup","arguments":{"query":"x"}}}`,
	} {
		response := rpcRequest(t, server, "test-key", sessionID, "2025-11-25", "/mcp", body)
		if failure := rpcError(t, response); failure["code"] != float64(rpcInvalidParams) {
			t.Fatalf("%s did not refuse as invalid params: %v", name, response.body)
		}
	}
	if fx.calls.Load() != 0 || len(ledger.reserved) != 0 || len(tl.snapshot()) != 0 {
		t.Fatalf("refused session call reserved or dispatched: calls=%d events=%v", fx.calls.Load(), tl.snapshot())
	}
}

func TestStreamableHTTPSessionAmbiguousCallStaysUnknown(t *testing.T) {
	server, fx, ledger, tl, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "drop")
	sessionID := openSession(t, server, "test-key", "2025-11-25")
	response := rpcRequest(t, server, "test-key", sessionID, "2025-11-25", "/mcp",
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"`+alias()+`","arguments":{"query":"x"}}}`)
	data, _ := rpcError(t, response)["data"].(map[string]any)
	if data["code"] != "outcome_unknown" || data["attempt_id"] == nil || fx.calls.Load() != 1 {
		t.Fatalf("ambiguous session call was not reported unknown: %v", response.body)
	}
	if got := strings.Join(tl.snapshot(), ","); got != "reserve,initialize,dispatched,upstream,unknown" {
		t.Fatalf("ambiguous session dispatch settled or released: %s", got)
	}
	if len(ledger.evidence) != 1 || ledger.evidence[0].Status != StatusOutcomeUnknown {
		t.Fatalf("unknown session attempt not recorded: %+v", ledger.evidence)
	}
}

func TestStreamableHTTPRequiresBoundSessionAndNegotiatedProtocol(t *testing.T) {
	server, fx, _, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	if response := rpcRequest(t, server, "test-key", "", "", "/mcp", listCall); response.status != http.StatusBadRequest {
		t.Fatalf("call without a session was served: %d %v", response.status, response.body)
	}
	if response := rpcRequest(t, server, "test-key", "no-such-session", "2025-11-25", "/mcp", listCall); response.status != http.StatusNotFound {
		t.Fatalf("call with an unknown session was served: %d %v", response.status, response.body)
	}
	if response := rpcRequest(t, server, "", "", "", "/mcp", listCall); response.status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated call was served: %d %v", response.status, response.body)
	}
	legacy := rpcInitialize(t, server, "test-key", "2024-11-05", testToolset)
	if result := rpcResult(t, legacy); result["protocolVersion"] != "2025-11-25" {
		t.Fatalf("legacy client was silently served at its own version: %v", result)
	}
	uninitializedID := legacy.headers.Get("Mcp-Session-Id")
	early := rpcRequest(t, server, "test-key", uninitializedID, "2025-11-25", "/mcp", listCall)
	if failure := rpcError(t, early); failure["code"] != float64(rpcNotInitialized) {
		t.Fatalf("session served tools before notifications/initialized: %v", early.body)
	}
	sessionID := openSession(t, server, "test-key", "2025-11-25")
	mismatched := rpcRequest(t, server, "test-key", sessionID, "2025-03-26", "/mcp", listCall)
	if mismatched.status != http.StatusBadRequest {
		t.Fatalf("session accepted a different protocol version: %d %v", mismatched.status, mismatched.body)
	}
	batch := rpcRequest(t, server, "test-key", sessionID, "2025-11-25", "/mcp", `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`)
	if batch.status != http.StatusBadRequest {
		t.Fatalf("batched message was served: %d %v", batch.status, batch.body)
	}
	if fx.calls.Load() != 0 {
		t.Fatalf("refused transport traffic dispatched: %d", fx.calls.Load())
	}
}

func TestStreamableHTTPGetRefusesStandaloneStream(t *testing.T) {
	server, _, _, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	mux := http.NewServeMux()
	server.Register(mux)
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST, DELETE" {
		t.Fatalf("standalone stream was offered: %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestStreamableHTTPSessionStateHoldsNoBodiesOrCredentials(t *testing.T) {
	server, _, _, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	sessionID := openSession(t, server, "test-key", "2025-11-25")
	called := rpcRequest(t, server, "test-key", sessionID, "2025-11-25", "/mcp",
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"`+alias()+`","arguments":{"query":"secret-argument"}}}`)
	rpcResult(t, called)
	stored, ok := server.sessions[sessionID]
	if !ok {
		t.Fatal("session vanished after its call")
	}
	rendered := fmt.Sprintf("%+v", stored)
	for _, forbidden := range []string{"secret-argument", "answer for", "static-token", "test-key", "Bearer"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("session state retained %q: %s", forbidden, rendered)
		}
	}
	for _, expected := range []string{testToolset, testRevision, testProject, "2025-11-25"} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("session state lost its %q binding: %s", expected, rendered)
		}
	}
}
