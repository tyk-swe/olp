package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/mcpservers"
	"github.com/tyk-swe/olp/internal/runtime"
)

func latestProtocol() string { return mcpservers.SupportedProtocols()[0] }

func otherProtocol() string {
	supported := mcpservers.SupportedProtocols()
	return supported[len(supported)-1]
}

type fakeSessionEntry struct {
	fields    map[string]string
	expiresAt int64
}

type fakeSessionCommander struct {
	mu       sync.Mutex
	now      func() time.Time
	sessions map[string]fakeSessionEntry
	indexes  map[string]map[string]int64
}

func newFakeSessionCommander(now func() time.Time) *fakeSessionCommander {
	return &fakeSessionCommander{
		now:      now,
		sessions: map[string]fakeSessionEntry{},
		indexes:  map[string]map[string]int64{},
	}
}

func (f *fakeSessionCommander) Do(_ context.Context, args ...string) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(args) < 3 || (args[0] != "EVALSHA" && args[0] != "EVAL") {
		return nil, fmt.Errorf("unsupported session command %v", args)
	}
	keyCount, err := strconv.Atoi(args[2])
	if err != nil || len(args) < 3+keyCount {
		return nil, fmt.Errorf("malformed session command %v", args)
	}
	keys, argv := args[3:3+keyCount], args[3+keyCount:]
	switch args[1] {
	case sessionCreateScript.digest, sessionCreateScript.source:
		return f.createSession(keys, argv), nil
	case sessionTouchScript.digest, sessionTouchScript.source:
		return f.touchSession(keys, argv), nil
	case sessionInitializeScript.digest, sessionInitializeScript.source:
		return f.initializeSession(keys), nil
	case sessionDeleteScript.digest, sessionDeleteScript.source:
		return f.removeSession(keys, argv), nil
	case sessionCertifyScript.digest, sessionCertifyScript.source:
		return f.certifySession(keys, argv), nil
	default:
		return nil, fmt.Errorf("unknown session script %q", args[1])
	}
}

func (f *fakeSessionCommander) nowMS() int64 { return f.now().UnixMilli() }

func (f *fakeSessionCommander) liveSession(key string) (fakeSessionEntry, bool) {
	entry, ok := f.sessions[key]
	if !ok {
		return fakeSessionEntry{}, false
	}
	if f.nowMS() >= entry.expiresAt {
		delete(f.sessions, key)
		return fakeSessionEntry{}, false
	}
	return entry, true
}

func (f *fakeSessionCommander) indexFor(key string) map[string]int64 {
	index, ok := f.indexes[key]
	if !ok {
		index = map[string]int64{}
		f.indexes[key] = index
	}
	return index
}

func (f *fakeSessionCommander) createSession(keys, argv []string) any {
	now := f.nowMS()
	ttlMS, _ := strconv.ParseInt(argv[1], 10, 64)
	capLimit, _ := strconv.ParseInt(argv[2], 10, 64)
	index := f.indexFor(keys[1])
	for member, expiry := range index {
		if expiry <= now {
			delete(index, member)
		}
	}
	if int64(len(index)) >= capLimit {
		return int64(0)
	}
	fields := map[string]string{
		"installation_id": argv[3], "project_id": argv[4], "principal_digest": argv[5],
		"protocol": argv[6], "toolset_id": argv[7], "toolset_revision_id": argv[8],
		"release_id":        argv[9],
		"created_at_ms":     strconv.FormatInt(now, 10),
		"last_active_at_ms": strconv.FormatInt(now, 10),
		"initialized":       "0",
	}
	for index := 10; index+1 < len(argv); index += 2 {
		fields["cert:"+argv[index]] = argv[index+1]
	}
	f.sessions[keys[0]] = fakeSessionEntry{fields: fields, expiresAt: now + ttlMS}
	index[argv[0]] = now + ttlMS
	return int64(1)
}

func (f *fakeSessionCommander) touchSession(keys, argv []string) any {
	entry, ok := f.liveSession(keys[0])
	if !ok {
		delete(f.indexFor(keys[1]), argv[0])
		return []any{}
	}
	now := f.nowMS()
	ttlMS, _ := strconv.ParseInt(argv[1], 10, 64)
	fields := maps.Clone(entry.fields)
	fields["last_active_at_ms"] = strconv.FormatInt(now, 10)
	f.sessions[keys[0]] = fakeSessionEntry{fields: fields, expiresAt: now + ttlMS}
	f.indexFor(keys[1])[argv[0]] = now + ttlMS
	flat := []any{}
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		flat = append(flat, name, fields[name])
	}
	return flat
}

func (f *fakeSessionCommander) initializeSession(keys []string) any {
	entry, ok := f.liveSession(keys[0])
	if !ok {
		return int64(0)
	}
	fields := maps.Clone(entry.fields)
	fields["initialized"] = "1"
	f.sessions[keys[0]] = fakeSessionEntry{fields: fields, expiresAt: entry.expiresAt}
	return int64(1)
}

func (f *fakeSessionCommander) certifySession(keys, argv []string) any {
	entry, ok := f.liveSession(keys[0])
	if !ok {
		return int64(0)
	}
	fields := maps.Clone(entry.fields)
	fields["cert:"+argv[0]] = argv[1]
	f.sessions[keys[0]] = fakeSessionEntry{fields: fields, expiresAt: entry.expiresAt}
	return int64(1)
}

func (f *fakeSessionCommander) removeSession(keys, argv []string) any {
	_, ok := f.liveSession(keys[0])
	if ok {
		delete(f.sessions, keys[0])
	}
	delete(f.indexFor(keys[1]), argv[0])
	if ok {
		return int64(1)
	}
	return int64(0)
}

func (f *fakeSessionCommander) dump() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := []string{}
	for key, entry := range f.sessions {
		parts = append(parts, key)
		for name, value := range entry.fields {
			parts = append(parts, name+"="+value)
		}
	}
	for key, index := range f.indexes {
		parts = append(parts, key)
		for member := range index {
			parts = append(parts, member)
		}
	}
	slices.Sort(parts)
	return strings.Join(parts, "\n")
}

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
	for _, protocol := range mcpservers.SupportedProtocols() {
		t.Run(protocol, func(t *testing.T) {
			server, fx, ledger, tl, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
			sessionID := openSession(t, server, "test-key", protocol)
			tl.reset()

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
				`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"`+alias()+`","arguments":{"query":"secret-argument"},"_meta":{"olp.call_id":"existing-session-1"}}}`)
			result := rpcResult(t, called)
			content, _ := result["content"].([]any)
			if len(content) != 1 || fx.calls.Load() != 1 {
				t.Fatalf("session call did not reach the upstream exactly once: calls=%d result=%v", fx.calls.Load(), result)
			}
			if got := strings.Join(tl.snapshot(), ","); got != "reserve,initialize,list,dispatched,upstream,settled" {
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

func TestStreamableHTTPSessionExpiresAfterIdleTimeout(t *testing.T) {
	server, _, _, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	current := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	server.Now = func() time.Time { return current }
	sessionID := openSession(t, server, "test-key", latestProtocol())

	current = current.Add(sessionIdleTimeout - time.Minute)
	if response := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp", listCall); response.status != http.StatusOK {
		t.Fatalf("session expired before the idle timeout: %d %v", response.status, response.body)
	}
	current = current.Add(sessionIdleTimeout - time.Minute)
	if response := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp", listCall); response.status != http.StatusOK {
		t.Fatalf("activity did not extend the idle window: %d %v", response.status, response.body)
	}
	current = current.Add(sessionIdleTimeout + time.Second)
	response := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp", listCall)
	if response.status != http.StatusNotFound {
		t.Fatalf("session survived the idle timeout: %d %v", response.status, response.body)
	}
	if data, _ := rpcError(t, response)["data"].(map[string]any); data["code"] != "session_not_found" {
		t.Fatalf("expired session did not refuse as missing: %v", response.body)
	}
}

func TestStreamableHTTPSessionCapRefusesBeyondPerKeyCap(t *testing.T) {
	authority := testAuthority([]string{"tools"}, []string{testToolset})
	server, _, _, _, rt := newTestServer(t, authority, "")
	other := testAuthority([]string{"tools"}, []string{testToolset})
	other.ID = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50711"
	rt.otherAuthority = other
	for range maxSessionsPerKey {
		if response := rpcInitialize(t, server, "test-key", latestProtocol(), testToolset); response.status != http.StatusOK {
			t.Fatalf("session within the cap refused: %d %v", response.status, response.body)
		}
	}
	response := rpcInitialize(t, server, "test-key", latestProtocol(), testToolset)
	if response.status != http.StatusTooManyRequests || response.headers.Get("Mcp-Session-Id") != "" {
		t.Fatalf("session beyond the cap for one key was created: %d %v", response.status, response.headers)
	}
	if data, _ := rpcError(t, response)["data"].(map[string]any); data["code"] != "session_limit_exceeded" || data["limit"] != float64(maxSessionsPerKey) {
		t.Fatalf("session cap refusal is not explicit: %v", response.body)
	}
	if response := rpcInitialize(t, server, "other-key", latestProtocol(), testToolset); response.status != http.StatusOK {
		t.Fatalf("one key at its cap blocked another key: %d %v", response.status, response.body)
	}
}

func TestStreamableHTTPSessionsAreSharedAcrossReplicas(t *testing.T) {
	authority := testAuthority([]string{"tools"}, []string{testToolset})
	first, _, _, _, _ := newTestServer(t, authority, "")
	second, secondFixture, secondLedger, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	second.Sessions = first.Sessions

	sessionID := openSession(t, first, "test-key", latestProtocol())
	listed := rpcRequest(t, second, "test-key", sessionID, latestProtocol(), "/mcp", listCall)
	if tools, _ := rpcResult(t, listed)["tools"].([]any); len(tools) != 1 {
		t.Fatalf("session opened on one replica listed nothing on another: %v", listed.body)
	}
	called := rpcRequest(t, second, "test-key", sessionID, latestProtocol(), "/mcp",
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"`+alias()+`","arguments":{"query":"x"},"_meta":{"olp.call_id":"existing-session-2"}}}`)
	rpcResult(t, called)
	if secondFixture.calls.Load() != 1 || len(secondLedger.evidence) != 1 {
		t.Fatalf("cross-replica session call did not dispatch and settle: calls=%d evidence=%v", secondFixture.calls.Load(), secondLedger.evidence)
	}

	for range maxSessionsPerKey - 1 {
		if response := rpcInitialize(t, first, "test-key", latestProtocol(), testToolset); response.status != http.StatusOK {
			t.Fatalf("session within the shared cap refused: %d %v", response.status, response.body)
		}
	}
	if response := rpcInitialize(t, second, "test-key", latestProtocol(), testToolset); response.status != http.StatusTooManyRequests {
		t.Fatalf("second replica ignored sessions the first one holds: %d %v", response.status, response.body)
	}

	if status := rpcDelete(t, second, "test-key", sessionID, latestProtocol()); status != http.StatusNoContent {
		t.Fatalf("cross-replica session delete status %d", status)
	}
	if response := rpcRequest(t, first, "test-key", sessionID, latestProtocol(), "/mcp", listCall); response.status != http.StatusNotFound {
		t.Fatalf("session deleted on one replica stayed usable on another: %d %v", response.status, response.body)
	}
}

func TestStreamableHTTPSessionCapHoldsUnderConcurrentCreation(t *testing.T) {
	server, _, _, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	initialize := func() int {
		mux := http.NewServeMux()
		server.Register(mux)
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":%q,"capabilities":{},"clientInfo":{"name":"fixture","version":"1"}}}`, latestProtocol())
		req := httptest.NewRequest(http.MethodPost, "/mcp?toolset_id="+testToolset, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer test-key")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	var created, refused atomic.Int32
	var group sync.WaitGroup
	for range maxSessionsPerKey + 16 {
		group.Add(1)
		go func() {
			defer group.Done()
			switch initialize() {
			case http.StatusOK:
				created.Add(1)
			case http.StatusTooManyRequests:
				refused.Add(1)
			}
		}()
	}
	group.Wait()
	if created.Load() != maxSessionsPerKey || refused.Load() != 16 {
		t.Fatalf("concurrent creation breached the cap: created=%d refused=%d", created.Load(), refused.Load())
	}
}

func TestStreamableHTTPSessionRefusesOtherPrincipals(t *testing.T) {
	server, fx, ledger, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	other := testAuthority([]string{"tools"}, []string{testToolset})
	other.ID = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50711"
	rt.otherAuthority = other
	sessionID := openSession(t, server, "test-key", latestProtocol())

	for _, body := range []string{
		listCall,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"` + alias() + `","arguments":{"query":"x"},"_meta":{"olp.call_id":"existing-session-3"}}}`,
	} {
		if response := rpcRequest(t, server, "other-key", sessionID, latestProtocol(), "/mcp", body); response.status != http.StatusNotFound {
			t.Fatalf("another principal used the session: %d %v", response.status, response.body)
		}
	}
	if status := rpcDelete(t, server, "other-key", sessionID, latestProtocol()); status != http.StatusNotFound {
		t.Fatalf("another principal deleted the session: %d", status)
	}
	if fx.calls.Load() != 0 || len(ledger.reserved) != 0 {
		t.Fatalf("cross-principal use dispatched: calls=%d reserved=%d", fx.calls.Load(), len(ledger.reserved))
	}
	if response := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp", listCall); response.status != http.StatusOK {
		t.Fatalf("owner lost the session after a foreign attempt: %d %v", response.status, response.body)
	}
}

func TestStreamableHTTPSessionBindsPinnedToolsetRevision(t *testing.T) {
	server, fx, _, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset, testOtherTS}), "")
	ambiguous := rpcRequest(t, server, "test-key", "", "", "/mcp",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+latestProtocol()+`","capabilities":{},"clientInfo":{"name":"fixture","version":"1"}}}`)
	if ambiguous.status != http.StatusBadRequest || ambiguous.headers.Get("Mcp-Session-Id") != "" {
		t.Fatalf("session bound without a toolset selection: %d %v", ambiguous.status, ambiguous.body)
	}
	foreign := rpcInitialize(t, server, "test-key", latestProtocol(), "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50799")
	if foreign.status != http.StatusNotFound || foreign.headers.Get("Mcp-Session-Id") != "" {
		t.Fatalf("foreign toolset opened a session: %d %v", foreign.status, foreign.body)
	}

	sessionID := openSession(t, server, "test-key", latestProtocol())
	changed := testSnapshot(t, fx.server.URL)
	changed.MCPServers[0].RevisionID = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50712"
	changed.MCPServers[0].Digest = strings.Repeat("b", 64)
	changed.MCPServers[0].Tools[0].Description = "Changed after pinning"
	if err := changed.Compile(); err != nil {
		t.Fatal(err)
	}
	rt.release = &runtime.Release{ID: testGenID, Snapshot: changed}
	listed := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp", listCall)
	if failure := rpcError(t, listed); failure["code"] != float64(rpcServerError) {
		t.Fatalf("session listing did not refuse as a server error: %v", listed.body)
	}
	if code := rpcDataCode(t, listed); code != "tool_definition_changed" {
		t.Fatalf("session listing silently omitted definitions changed after pinning: %v", listed.body)
	}
	called := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp",
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"`+alias()+`","arguments":{"query":"x"},"_meta":{"olp.call_id":"existing-session-4"}}}`)
	if data, _ := rpcError(t, called)["data"].(map[string]any); data["code"] != "tool_definition_changed" {
		t.Fatalf("session called a definition changed after pinning: %v", called.body)
	}
	if fx.calls.Load() != 0 {
		t.Fatalf("changed definition dispatched: calls=%d", fx.calls.Load())
	}
}

func TestStreamableHTTPRefusesUnsupportedCapabilitiesExplicitly(t *testing.T) {
	server, fx, ledger, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	sessionID := openSession(t, server, "test-key", latestProtocol())
	for method, capability := range map[string]string{
		"sampling/createMessage":   "sampling",
		"elicitation/create":       "elicitation",
		"roots/list":               "roots",
		"resources/list":           "resources",
		"resources/read":           "resources",
		"resources/templates/list": "resources",
		"prompts/list":             "prompts",
	} {
		response := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp",
			`{"jsonrpc":"2.0","id":9,"method":"`+method+`","params":{}}`)
		failure := rpcError(t, response)
		data, _ := failure["data"].(map[string]any)
		if response.status != http.StatusOK || failure["code"] != float64(rpcMethodNotFound) || data["code"] != "unsupported_capability" || data["capability"] != capability {
			t.Fatalf("%s was not refused explicitly: %d %v", method, response.status, response.body)
		}
	}
	unknown := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp", `{"jsonrpc":"2.0","id":10,"method":"no/such","params":{}}`)
	if data, _ := rpcError(t, unknown)["data"].(map[string]any); data["code"] != "method_not_found" {
		t.Fatalf("unknown method masqueraded as a capability refusal: %v", unknown.body)
	}
	if fx.calls.Load() != 0 || len(ledger.reserved) != 0 {
		t.Fatalf("refused capability dispatched: calls=%d reserved=%d", fx.calls.Load(), len(ledger.reserved))
	}
}

func TestStreamableHTTPSessionCallRefusalsMatchJSONPath(t *testing.T) {
	server, fx, ledger, tl, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	sessionID := openSession(t, server, "test-key", latestProtocol())
	tl.reset()
	for name, body := range map[string]string{
		"invalid arguments": `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"` + alias() + `","arguments":{"query":3},"_meta":{"olp.call_id":"existing-session-5"}}}`,
		"unknown alias":     `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"lookup","arguments":{"query":"x"},"_meta":{"olp.call_id":"existing-session-6"}}}`,
	} {
		response := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp", body)
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
	sessionID := openSession(t, server, "test-key", latestProtocol())
	tl.reset()
	response := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp",
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"`+alias()+`","arguments":{"query":"x"},"_meta":{"olp.call_id":"existing-session-7"}}}`)
	data, _ := rpcError(t, response)["data"].(map[string]any)
	if data["code"] != "outcome_unknown" || data["attempt_id"] == nil || fx.calls.Load() != 1 {
		t.Fatalf("ambiguous session call was not reported unknown: %v", response.body)
	}
	if got := strings.Join(tl.snapshot(), ","); got != "reserve,initialize,list,dispatched,upstream,unknown" {
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
	if response := rpcRequest(t, server, "test-key", "no-such-session", latestProtocol(), "/mcp", listCall); response.status != http.StatusNotFound {
		t.Fatalf("call with an unknown session was served: %d %v", response.status, response.body)
	}
	if response := rpcRequest(t, server, "", "", "", "/mcp", listCall); response.status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated call was served: %d %v", response.status, response.body)
	}
	legacy := rpcInitialize(t, server, "test-key", "2024-11-05", testToolset)
	if result := rpcResult(t, legacy); result["protocolVersion"] != latestProtocol() {
		t.Fatalf("legacy client was silently served at its own version: %v", result)
	}
	uninitializedID := legacy.headers.Get("Mcp-Session-Id")
	for _, body := range []string{
		listCall,
		`{"jsonrpc":"2.0","id":7,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":8,"method":"resources/list","params":{}}`,
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"` + alias() + `","arguments":{"query":"x"},"_meta":{"olp.call_id":"existing-session-8"}}}`,
	} {
		early := rpcRequest(t, server, "test-key", uninitializedID, latestProtocol(), "/mcp", body)
		if failure := rpcError(t, early); failure["code"] != float64(rpcNotInitialized) {
			t.Fatalf("session served %s before notifications/initialized: %v", body, early.body)
		}
	}
	sessionID := openSession(t, server, "test-key", latestProtocol())
	mismatched := rpcRequest(t, server, "test-key", sessionID, otherProtocol(), "/mcp", listCall)
	if mismatched.status != http.StatusBadRequest {
		t.Fatalf("session accepted a different protocol version: %d %v", mismatched.status, mismatched.body)
	}
	batch := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp", `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`)
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
	server, _, _, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	sessionID := openSession(t, server, "test-key", latestProtocol())
	called := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp",
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"`+alias()+`","arguments":{"query":"secret-argument"},"_meta":{"olp.call_id":"existing-session-9"}}}`)
	rpcResult(t, called)
	commander, ok := server.Sessions.client.(*fakeSessionCommander)
	if !ok {
		t.Fatal("session store is not backed by the shared store fake")
	}
	stored := commander.dump()
	for _, forbidden := range []string{"secret-argument", "answer for", "static-token", "test-key", "Bearer", testKeyID} {
		if strings.Contains(stored, forbidden) {
			t.Fatalf("shared session state retained %q: %s", forbidden, stored)
		}
	}
	for _, expected := range []string{sessionID, testToolset, testRevision, testProject, latestProtocol(), principalOf(*rt.authority).scope()} {
		if !strings.Contains(stored, expected) {
			t.Fatalf("shared session state lost its %q binding: %s", expected, stored)
		}
	}
	if response := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp", listCall); response.status != http.StatusOK {
		t.Fatalf("session stopped working after its call: %d %v", response.status, response.body)
	}
}

func rpcDataCode(t *testing.T, response rpcResponse) string {
	t.Helper()
	data, _ := rpcError(t, response)["data"].(map[string]any)
	code, _ := data["code"].(string)
	return code
}

var sessionCallSequence atomic.Int32

func sessionCall(t *testing.T, server *Server, sessionID string) rpcResponse {
	t.Helper()
	callID := "existing-session-call-" + strconv.Itoa(int(sessionCallSequence.Add(1)))
	return rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp",
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"`+alias()+`","arguments":{"query":"x"},"_meta":{"olp.call_id":"`+callID+`"}}}`)
}

func TestStreamableHTTPInitializeCertifiesLiveCatalogAndBlocksDriftedCalls(t *testing.T) {
	server, fx, ledger, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	fx.setDescription("Changed after publication")
	sessionID := openSession(t, server, "test-key", latestProtocol())
	listed := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp", listCall)
	if tools, _ := rpcResult(t, listed)["tools"].([]any); len(tools) != 1 {
		t.Fatalf("pinned definitions stopped being readable in a drifted session: %v", listed.body)
	}
	called := sessionCall(t, server, sessionID)
	if code := rpcDataCode(t, called); code != "tool_definition_changed" {
		t.Fatalf("drifted session call was not refused as a definition change: %v", called.body)
	}
	if fx.calls.Load() != 0 || len(ledger.reserved) != 0 {
		t.Fatalf("drift certification invoked or reserved a tool: calls=%d reserved=%d", fx.calls.Load(), len(ledger.reserved))
	}
}

func TestStreamableHTTPSessionCallsCertifyEveryDispatch(t *testing.T) {
	server, fx, _, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	sessionID := openSession(t, server, "test-key", latestProtocol())
	lists := fx.lists.Load()
	for range 2 {
		rpcResult(t, sessionCall(t, server, sessionID))
	}
	if fx.calls.Load() != 2 {
		t.Fatalf("certified session calls did not dispatch: calls=%d", fx.calls.Load())
	}
	if fx.lists.Load() != lists+2 {
		t.Fatalf("session dispatch skipped live catalog certification: lists %d -> %d", lists, fx.lists.Load())
	}
	fx.setDescription("Changed mid-session without a notification")
	called := sessionCall(t, server, sessionID)
	if code := rpcDataCode(t, called); code != "tool_definition_changed" {
		t.Fatalf("mid-session drift without a notification was served: %v", called.body)
	}
	if fx.calls.Load() != 2 {
		t.Fatalf("mid-session drift dispatched: calls=%d", fx.calls.Load())
	}
}

func TestStreamableHTTPListChangedNotificationInvalidatesCertification(t *testing.T) {
	server, fx, _, tl, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	sessionID := openSession(t, server, "test-key", latestProtocol())
	fx.notify.Store(true)
	rpcResult(t, sessionCall(t, server, sessionID))
	if fx.calls.Load() != 1 {
		t.Fatalf("call carrying the notification did not complete: calls=%d", fx.calls.Load())
	}
	fx.notify.Store(false)
	fx.setDescription("Changed after the notification")
	tl.reset()
	called := sessionCall(t, server, sessionID)
	if code := rpcDataCode(t, called); code != "tool_definition_changed" {
		t.Fatalf("call after invalidation served the changed catalog: %v", called.body)
	}
	if got := strings.Join(tl.snapshot(), ","); got != "reserve,initialize,list,released" {
		t.Fatalf("invalidated session did not recertify before refusing: %s", got)
	}
	lists := fx.lists.Load()
	called = sessionCall(t, server, sessionID)
	if code := rpcDataCode(t, called); code != "tool_definition_changed" {
		t.Fatalf("drifted session unblocked itself: %v", called.body)
	}
	if fx.calls.Load() != 1 || fx.lists.Load() != lists {
		t.Fatalf("drifted session dispatched or rediscovered per call: calls=%d lists %d -> %d", fx.calls.Load(), lists, fx.lists.Load())
	}
}

func TestStreamableHTTPListChangedWithoutDriftRecertifiesAndServes(t *testing.T) {
	server, fx, _, tl, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	sessionID := openSession(t, server, "test-key", latestProtocol())
	fx.notify.Store(true)
	rpcResult(t, sessionCall(t, server, sessionID))
	fx.notify.Store(false)
	tl.reset()
	rpcResult(t, sessionCall(t, server, sessionID))
	if got := strings.Join(tl.snapshot(), ","); got != "reserve,initialize,list,dispatched,upstream,settled" {
		t.Fatalf("invalidated session did not recertify before serving again: %s", got)
	}
	lists := fx.lists.Load()
	rpcResult(t, sessionCall(t, server, sessionID))
	if fx.calls.Load() != 3 || fx.lists.Load() != lists+1 {
		t.Fatalf("recertified session stopped certifying per dispatch: calls=%d lists %d -> %d", fx.calls.Load(), lists, fx.lists.Load())
	}
}

func TestStreamableHTTPSessionEndsWhenAuthorityIsRevoked(t *testing.T) {
	server, fx, _, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	sessionID := openSession(t, server, "test-key", latestProtocol())
	rt.revoked.Store(true)
	refused := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp", listCall)
	if code := rpcDataCode(t, refused); code != "toolset_not_found" {
		t.Fatalf("revoked session still listed tools: %v", refused.body)
	}
	ended := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp", listCall)
	if ended.status != http.StatusNotFound || rpcDataCode(t, ended) != "session_not_found" {
		t.Fatalf("revoked session stayed eligible: %d %v", ended.status, ended.body)
	}

	rt.revoked.Store(false)
	sessionID = openSession(t, server, "test-key", latestProtocol())
	rt.revoked.Store(true)
	called := sessionCall(t, server, sessionID)
	if code := rpcDataCode(t, called); code != "toolset_not_found" {
		t.Fatalf("revoked session call was not refused: %v", called.body)
	}
	if fx.calls.Load() != 0 {
		t.Fatalf("revoked session dispatched: calls=%d", fx.calls.Load())
	}
	ended = rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp", listCall)
	if ended.status != http.StatusNotFound || rpcDataCode(t, ended) != "session_not_found" {
		t.Fatalf("session stayed eligible after a revoked call: %d %v", ended.status, ended.body)
	}
}

func TestStreamableHTTPSessionEndsWhenPinnedPublicationDisappears(t *testing.T) {
	server, fx, _, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	sessionID := openSession(t, server, "test-key", latestProtocol())
	retired := testSnapshot(t, fx.server.URL)
	retired.AgentPublications = slices.DeleteFunc(slices.Clone(retired.AgentPublications), func(revision agentcontract.Revision) bool {
		return revision.ID == testRevision
	})
	if err := retired.Compile(); err != nil {
		t.Fatal(err)
	}
	rt.release = &runtime.Release{ID: testGenID, Snapshot: retired}
	called := sessionCall(t, server, sessionID)
	if code := rpcDataCode(t, called); code != "toolset_not_found" {
		t.Fatalf("call on a retired publication was not refused: %v", called.body)
	}
	if fx.calls.Load() != 0 {
		t.Fatalf("retired publication dispatched: calls=%d", fx.calls.Load())
	}
	ended := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp", listCall)
	if ended.status != http.StatusNotFound || rpcDataCode(t, ended) != "session_not_found" {
		t.Fatalf("session stayed eligible after its publication retired: %d %v", ended.status, ended.body)
	}
}

func TestStreamableHTTPSessionSurvivesStaleAuthorityRefusal(t *testing.T) {
	server, _, _, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	sessionID := openSession(t, server, "test-key", latestProtocol())
	rt.stale.Store(true)
	refused := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp", listCall)
	if code := rpcDataCode(t, refused); code != "authority_unavailable" {
		t.Fatalf("stale authority was not reported as unavailable: %v", refused.body)
	}
	rt.stale.Store(false)
	if response := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp", listCall); response.status != http.StatusOK {
		t.Fatalf("session ended on a transient authority outage: %d %v", response.status, response.body)
	}
}

func TestStreamableHTTPRepublishedRevisionServesNewSessions(t *testing.T) {
	server, fx, _, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	fx.setDescription("Changed after publication")
	blockedID := openSession(t, server, "test-key", latestProtocol())
	if code := rpcDataCode(t, sessionCall(t, server, blockedID)); code != "tool_definition_changed" {
		t.Fatalf("drifted session dispatched before re-publish: calls=%d", fx.calls.Load())
	}
	rt.live = map[string]string{testToolset: testRevision2, testOtherTS: testOtherRv}
	rt.release = &runtime.Release{ID: testGenID, Snapshot: republishedSnapshot(t, fx.server.URL)}
	sessionID := openSession(t, server, "test-key", latestProtocol())
	rpcResult(t, sessionCall(t, server, sessionID))
	if fx.calls.Load() != 1 {
		t.Fatalf("republished revision did not serve new sessions: calls=%d", fx.calls.Load())
	}
	refused := sessionCall(t, server, blockedID)
	rpcError(t, refused)
	if fx.calls.Load() != 1 {
		t.Fatalf("session pinned before re-publish dispatched against new definitions: calls=%d", fx.calls.Load())
	}
}

func TestStreamableHTTPSessionEndsWhenPinnedServerLeavesSnapshot(t *testing.T) {
	server, fx, _, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	sessionID := openSession(t, server, "test-key", latestProtocol())
	gone := testSnapshot(t, fx.server.URL)
	gone.MCPServers = nil
	if err := gone.Compile(); err != nil {
		t.Fatal(err)
	}
	rt.release = &runtime.Release{ID: testGenID, Snapshot: gone}
	called := sessionCall(t, server, sessionID)
	if code := rpcDataCode(t, called); code != "toolset_not_found" {
		t.Fatalf("call whose server left the snapshot was not refused as ended eligibility: %v", called.body)
	}
	if fx.calls.Load() != 0 {
		t.Fatalf("server that left the snapshot dispatched: calls=%d", fx.calls.Load())
	}
	ended := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp", listCall)
	if ended.status != http.StatusNotFound || rpcDataCode(t, ended) != "session_not_found" {
		t.Fatalf("session stayed eligible after its server left the snapshot: %d %v", ended.status, ended.body)
	}
}

func TestStreamableHTTPInitializeRefusesWhenCatalogCannotBeVerified(t *testing.T) {
	server, fx, _, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	fx.setMode("dial_fail")
	response := rpcInitialize(t, server, "test-key", latestProtocol(), testToolset)
	if response.headers.Get("Mcp-Session-Id") != "" {
		t.Fatalf("initialize opened a session it could not certify: %v", response.body)
	}
	if failure := rpcError(t, response); failure["code"] != float64(rpcServerError) {
		t.Fatalf("uncertifiable initialize did not refuse as a server error: %v", response.body)
	}
	if code := rpcDataCode(t, response); code != "upstream_unavailable" {
		t.Fatalf("uncertifiable initialize was not refused as unavailable: %v", response.body)
	}
	if fx.calls.Load() != 0 {
		t.Fatalf("uncertifiable initialize invoked a tool: calls=%d", fx.calls.Load())
	}
}

func TestStreamableHTTPInitializeDriftEncodesAsServerError(t *testing.T) {
	server, fx, _, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	rt.release = &runtime.Release{ID: testGenID, Snapshot: driftedSnapshot(t, fx.server.URL)}
	rt.failAuthzOn = 1
	response := rpcInitialize(t, server, "test-key", latestProtocol(), testToolset)
	if response.headers.Get("Mcp-Session-Id") != "" {
		t.Fatalf("drifted initialize opened a session: %v", response.body)
	}
	if failure := rpcError(t, response); failure["code"] != float64(rpcServerError) {
		t.Fatalf("drifted initialize did not encode as -32000: %v", response.body)
	}
	if code := rpcDataCode(t, response); code != "tool_definition_changed" {
		t.Fatalf("drifted initialize was not refused as a definition change: %v", response.body)
	}
}

func TestStreamableHTTPSessionSnapshotDriftScopesPerServer(t *testing.T) {
	server, fx, _, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	rt.release = &runtime.Release{ID: testGenID, Snapshot: twoServerSnapshot(t, fx.server.URL)}
	sessionID := openSession(t, server, "test-key", latestProtocol())

	rpcResult(t, sessionCall(t, server, sessionID))
	if fx.calls.Load() != 1 {
		t.Fatalf("healthy server in a partially drifted toolset did not serve in session: calls=%d", fx.calls.Load())
	}
	driftedCall := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp",
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"`+agentcontract.ToolAlias(testServer2, "lookup")+`","arguments":{"query":"x"},"_meta":{"olp.call_id":"existing-session-11"}}}`)
	if code := rpcDataCode(t, driftedCall); code != "tool_definition_changed" {
		t.Fatalf("drifted server beside a healthy one was not refused as drift in session: %v", driftedCall.body)
	}
	if fx.calls.Load() != 1 {
		t.Fatalf("drifted server dispatched in session: calls=%d", fx.calls.Load())
	}
	listed := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp", listCall)
	if code := rpcDataCode(t, listed); code != "tool_definition_changed" {
		t.Fatalf("session listing silently omitted the drifted server: %v", listed.body)
	}
	rpcResult(t, sessionCall(t, server, sessionID))
	if fx.calls.Load() != 2 {
		t.Fatalf("drift refusal ended the session for its healthy server: calls=%d", fx.calls.Load())
	}
}

func sessionCallBody(id int, callID, query string) string {
	meta := ""
	if callID != "" {
		meta = `,"_meta":{"olp.call_id":"` + callID + `"}`
	}
	return `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"tools/call","params":{"name":"` + alias() + `","arguments":{"query":"` + query + `"}` + meta + `}}`
}

func TestStreamableHTTPCallRequiresMetaCallID(t *testing.T) {
	server, fx, ledger, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	sessionID := openSession(t, server, "test-key", latestProtocol())
	response := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp", sessionCallBody(2, "", "x"))
	if rpcDataCode(t, response) != "call_id_required" || fx.calls.Load() != 0 || len(ledger.reserved) != 0 {
		t.Fatalf("session call without a meta call id dispatched: %v calls=%d", response.body, fx.calls.Load())
	}
}

func TestStreamableHTTPExactRepeatReturnsRetainedResult(t *testing.T) {
	server, fx, ledger, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	sessionID := openSession(t, server, "test-key", latestProtocol())
	first := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp", sessionCallBody(2, "order-session", "x"))
	firstResult := rpcResult(t, first)
	if fx.calls.Load() != 1 {
		t.Fatalf("first session call did not execute once: %v", first.body)
	}
	ledger.repeat = retainedFromMeta(t, firstResult, StatusSucceeded)
	second := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp", sessionCallBody(3, "order-session", "x"))
	secondResult := rpcResult(t, second)
	if fx.calls.Load() != 1 {
		t.Fatalf("exact session repeat re-executed upstream: calls=%d body=%v", fx.calls.Load(), second.body)
	}
	firstMeta, _ := firstResult["_meta"].(map[string]any)
	secondMeta, _ := secondResult["_meta"].(map[string]any)
	if secondMeta["olp.attempt_id"] != firstMeta["olp.attempt_id"] || secondMeta["olp.dispatch_id"] != firstMeta["olp.dispatch_id"] {
		t.Fatalf("session repeat lost the retained attempt identity: first=%v second=%v", firstResult, secondResult)
	}
	firstContent, _ := firstResult["content"].([]any)
	secondContent, _ := secondResult["content"].([]any)
	if len(firstContent) != 1 || len(secondContent) != 1 {
		t.Fatalf("session repeat did not return the retained result: first=%v second=%v", firstResult, secondResult)
	}
}

func TestStreamableHTTPChangedBodyUnderSameMetaCallIDConflicts(t *testing.T) {
	server, fx, ledger, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	sessionID := openSession(t, server, "test-key", latestProtocol())
	first := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp", sessionCallBody(2, "order-clash", "one"))
	ledger.repeat = retainedFromMeta(t, rpcResult(t, first), StatusSucceeded)
	ledger.repeat.Conflict = true
	second := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp", sessionCallBody(3, "order-clash", "two"))
	if rpcDataCode(t, second) != "call_id_conflict" || fx.calls.Load() != 1 {
		t.Fatalf("changed session body under one call id executed: %v calls=%d", second.body, fx.calls.Load())
	}
	data, _ := rpcError(t, second)["data"].(map[string]any)
	if data["attempt_id"] == nil || data["dispatch_id"] == nil {
		t.Fatalf("session conflict did not name the original attempt: %v", second.body)
	}
}

func retainedFromMeta(t *testing.T, result map[string]any, status Status) *RetainedCall {
	t.Helper()
	meta, _ := result["_meta"].(map[string]any)
	attempt, _ := meta["olp.attempt_id"].(string)
	dispatch, _ := meta["olp.dispatch_id"].(string)
	if attempt == "" || dispatch == "" {
		t.Fatalf("session result carries no attempt identity to retain: %v", result)
	}
	return &RetainedCall{AttemptID: attempt, DispatchID: dispatch, Status: status}
}

func TestStreamableHTTPSameCallIDUnderAnotherPrincipalExecutesIndependently(t *testing.T) {
	server, fx, _, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	other := testAuthority([]string{"tools"}, []string{testToolset})
	other.ID = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50711"
	rt.otherAuthority = other
	sessionID := openSession(t, server, "test-key", latestProtocol())
	first := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp", sessionCallBody(2, "shared-name", "x"))
	_ = rpcResult(t, first)
	otherSession := openSession(t, server, "other-key", latestProtocol())
	second := rpcRequest(t, server, "other-key", otherSession, latestProtocol(), "/mcp", sessionCallBody(2, "shared-name", "x"))
	_ = rpcResult(t, second)
	if fx.calls.Load() != 2 {
		t.Fatalf("one principal's call id bound another principal: calls=%d body=%v", fx.calls.Load(), second.body)
	}
}
