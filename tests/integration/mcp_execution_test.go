//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/mcp"
	"github.com/tyk-swe/olp/internal/secrets"
)

func mcpExecString(t *testing.T, body map[string]any, key string) string {
	t.Helper()
	value, ok := body[key].(string)
	if !ok {
		t.Fatalf("response field %q is not a string: %v", key, body)
	}
	return value
}

func mcpExecObject(t *testing.T, body map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := body[key].(map[string]any)
	if !ok {
		t.Fatalf("response field %q is not an object: %v", key, body)
	}
	return value
}

func mcpExecItems(t *testing.T, body map[string]any, key string) []any {
	t.Helper()
	value, ok := body[key].([]any)
	if !ok {
		t.Fatalf("response field %q is not a list: %v", key, body)
	}
	return value
}

func mcpExecItem(t *testing.T, items []any, index int, body map[string]any) map[string]any {
	t.Helper()
	if index < 0 || index >= len(items) {
		t.Fatalf("response list has no item %d: %v", index, body)
	}
	value, ok := items[index].(map[string]any)
	if !ok {
		t.Fatalf("response list item %d is not an object: %v", index, body)
	}
	return value
}

func TestMCPToolCallEndToEndSettlesDurableDebit(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "MCP execution")
	const credential = "integration-static-credential"
	const argument = "integration-secret-argument"
	const answer = "integration-secret-answer"
	var calls atomic.Int64
	var description atomic.Value
	description.Store("Look up")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+credential {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var request struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var result any
		switch request.Method {
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "integration-session")
			result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			text, _ := description.Load().(string)
			result = map[string]any{"tools": []any{map[string]any{
				"name": "lookup", "description": text,
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []string{"query"}, "additionalProperties": false},
			}}}
		case "tools/call":
			calls.Add(1)
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": answer}}, "isError": false}
		default:
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	defer upstream.Close()

	created := h.want(owner, "POST", "/api/v1/mcp-servers", map[string]any{
		"name": "Execution server", "project_id": project, "transport": "streamable_http",
		"endpoint": upstream.URL, "enabled": true, "credential": credential,
	}, idem("mcp-exec-server"), 201)
	serverID := mcpExecString(t, created, "id")
	tool := agentcontract.Tool{ServerID: serverID, RevisionID: mcpExecString(t, created, "revision_id"), CatalogDigest: mcpExecString(t, mcpExecObject(t, created, "catalog"), "digest"), Name: "lookup", Alias: agentcontract.ToolAlias(serverID, "lookup")}
	toolset := h.want(owner, "POST", "/api/v1/toolsets", map[string]any{
		"project_id": project, "name": "Execution toolset",
		"toolset": map[string]any{"tools": []agentcontract.Tool{tool}},
	}, idem("mcp-exec-toolset"), 201)
	toolsetID := mcpExecString(t, toolset, "id")
	h.want(owner, "POST", "/api/v1/toolsets/"+toolsetID+"/publish", nil, withMatch(toolset, idem("mcp-exec-publish")), 200)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{
		"name": "Tool caller", "project_id": project, "scopes": []string{"tools"}, "allowed_toolsets": []string{toolsetID},
	}, idem("mcp-exec-key"), 201)
	secret := mcpExecString(t, key, "secret")
	otherKey := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{
		"name": "Other tool caller", "project_id": project, "scopes": []string{"tools"}, "allowed_toolsets": []string{toolsetID},
	}, idem("mcp-exec-other-key"), 201)
	otherSecret := mcpExecString(t, otherKey, "secret")

	valkey, prefix, _ := acctKeyspace(t)
	limiter, err := limits.New(valkey, strings.TrimSuffix(prefix, ":"))
	if err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.ParseRing([]byte(h.Ring))
	if err != nil {
		t.Fatal(err)
	}
	installation, err := database.Installation(t.Context(), h.Pool)
	if err != nil {
		t.Fatal(err)
	}
	sessionStore, err := mcp.NewSessionStore(valkey, strings.TrimSuffix(prefix, ":")+":mcp")
	if err != nil {
		t.Fatal(err)
	}
	toolServer := &mcp.Server{
		Runtime: h.Runtime, Egress: &h.Policy, Auth: h.AuthKey, Sessions: sessionStore,
		Ledger: &mcp.DurableLedger{Pool: h.Pool, Store: &limits.DebitStore{Pool: h.Pool, Limiter: limiter}, Auth: h.AuthKey},
		Credentials: func(ctx context.Context, credentialID string) ([]byte, error) {
			return ring.Read(ctx, h.Pool, installation, credentialID, secrets.MCPCredential)
		},
	}
	mux := http.NewServeMux()
	toolServer.Register(mux)
	consumer := httptest.NewServer(mux)
	defer consumer.Close()
	if err = h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	call := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		var data []byte
		if body != nil {
			data, _ = json.Marshal(body)
		}
		req, err := http.NewRequest(method, consumer.URL+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		out := map[string]any{}
		if len(raw) > 0 {
			if err = json.Unmarshal(raw, &out); err != nil {
				t.Fatalf("consumer response is not JSON: %s", raw)
			}
		}
		return resp.StatusCode, out
	}
	mcpCallAs := func(token, method, path, sessionID string, body any) (int, http.Header, map[string]any) {
		t.Helper()
		var data []byte
		if body != nil {
			data, _ = json.Marshal(body)
		}
		req, err := http.NewRequest(method, consumer.URL+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if sessionID != "" {
			req.Header.Set("Mcp-Session-Id", sessionID)
			req.Header.Set("MCP-Protocol-Version", "2025-11-25")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		out := map[string]any{}
		if len(raw) > 0 {
			if err = json.Unmarshal(raw, &out); err != nil {
				t.Fatalf("consumer MCP response is not JSON: %s", raw)
			}
		}
		return resp.StatusCode, resp.Header, out
	}
	mcpCall := func(method, path, sessionID string, body any) (int, http.Header, map[string]any) {
		t.Helper()
		return mcpCallAs(secret, method, path, sessionID, body)
	}
	debitCount := func() int {
		t.Helper()
		var count int
		if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.execution_debits WHERE api_key_id=$1 AND kind='tool'`, key["id"]).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}

	status, listed := call("GET", "/v1/mcp/tools", nil)
	tools := mcpExecItems(t, listed, "tools")
	if status != http.StatusOK || len(tools) != 1 {
		t.Fatalf("pinned tools were not listed under their alias: %d %v", status, listed)
	}
	if got := mcpExecString(t, mcpExecItem(t, tools, 0, listed), "name"); got != tool.Alias {
		t.Fatalf("pinned tools were not listed under their alias: %d %v", status, listed)
	}

	status, _ = call("POST", "/v1/mcp/tools/call", map[string]any{"toolset_id": toolsetID, "name": tool.Alias, "call_id": "existing-json-1", "arguments": map[string]any{"query": 3}})
	if status != http.StatusUnprocessableEntity || calls.Load() != 0 || debitCount() != 0 {
		t.Fatalf("invalid arguments reserved or dispatched: status=%d calls=%d debits=%d", status, calls.Load(), debitCount())
	}

	status, called := call("POST", "/v1/mcp/tools/call", map[string]any{"toolset_id": toolsetID, "name": tool.Alias, "call_id": "existing-json-2", "arguments": map[string]any{"query": argument}})
	if status != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("valid call did not reach the upstream once: status=%d calls=%d body=%v", status, calls.Load(), called)
	}
	content := mcpExecItems(t, called, "content")
	if len(content) != 1 {
		t.Fatalf("upstream result was not returned: %v", called)
	}
	if got := mcpExecString(t, mcpExecItem(t, content, 0, called), "text"); got != answer {
		t.Fatalf("upstream result was not returned: %v", called)
	}
	var state, record, recordStatus string
	var results int
	if err = h.Pool.QueryRow(t.Context(), `SELECT state FROM olp.execution_debits WHERE id=$1`, called["dispatch_id"]).Scan(&state); err != nil || state != "settled" {
		t.Fatalf("dispatch did not settle: state=%q err=%v", state, err)
	}
	if err = h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.execution_debit_results WHERE dispatch_id=$1`, called["dispatch_id"]).Scan(&results); err != nil || results != 1 {
		t.Fatalf("settlement was not exactly once: results=%d err=%v", results, err)
	}
	if err = h.Pool.QueryRow(t.Context(), `SELECT row_to_json(c)::text,status FROM olp.mcp_tool_calls c WHERE id=$1`, called["attempt_id"]).Scan(&record, &recordStatus); err != nil || recordStatus != "succeeded" {
		t.Fatalf("attempt record missing: status=%q err=%v", recordStatus, err)
	}
	for _, forbidden := range []string{argument, answer, credential, secret} {
		if strings.Contains(record, forbidden) {
			t.Fatal("attempt record persisted request or credential content")
		}
	}
	detail := h.want(owner, "GET", "/api/v1/mcp-tool-calls/"+mcpExecString(t, called, "attempt_id"), nil, nil, 200)
	if detail["status"] != "succeeded" || detail["tool_alias"] != tool.Alias {
		t.Fatalf("management record does not describe the attempt: %v", detail)
	}
	listedCalls := h.want(owner, "GET", "/api/v1/mcp-tool-calls?toolset_id="+toolsetID, nil, nil, 200)
	if items := mcpExecItems(t, listedCalls, "items"); len(items) != 1 {
		t.Fatalf("management list lost the attempt: %v", listedCalls)
	}

	status, _ = call("POST", "/v1/mcp/tools/call", map[string]any{"toolset_id": "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50799", "name": tool.Alias, "call_id": "existing-json-3", "arguments": map[string]any{"query": "x"}})
	if status != http.StatusNotFound || calls.Load() != 1 || debitCount() != 1 {
		t.Fatalf("foreign toolset dispatched: status=%d calls=%d debits=%d", status, calls.Load(), debitCount())
	}

	status, headers, initialized := mcpCall("POST", "/mcp?toolset_id="+toolsetID, "", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "integration", "version": "1"}},
	})
	sessionID := headers.Get("Mcp-Session-Id")
	initResult := mcpExecObject(t, initialized, "result")
	if status != http.StatusOK || sessionID == "" || initResult["protocolVersion"] != "2025-11-25" {
		t.Fatalf("streamable session did not initialize: status=%d body=%v", status, initialized)
	}
	if status, _, _ = mcpCall("POST", "/mcp", sessionID, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); status != http.StatusAccepted {
		t.Fatalf("initialized notification refused: %d", status)
	}
	status, _, sessionListed := mcpCall("POST", "/mcp", sessionID, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": map[string]any{}})
	sessionTools := mcpExecItems(t, mcpExecObject(t, sessionListed, "result"), "tools")
	if status != http.StatusOK || len(sessionTools) != 1 {
		t.Fatalf("session did not list the pinned tool: status=%d body=%v", status, sessionListed)
	}
	sessionTool := mcpExecItem(t, sessionTools, 0, sessionListed)
	if got := mcpExecString(t, sessionTool, "name"); got != tool.Alias {
		t.Fatalf("session did not list the pinned tool: status=%d body=%v", status, sessionListed)
	}
	if _, ok := sessionTool["inputSchema"]; !ok {
		t.Fatalf("session listing lost the wire schema: %v", sessionListed)
	}
	_, _, refused := mcpCall("POST", "/mcp", sessionID, map[string]any{"jsonrpc": "2.0", "id": 3, "method": "resources/list", "params": map[string]any{}})
	if data := mcpExecObject(t, mcpExecObject(t, refused, "error"), "data"); mcpExecString(t, data, "code") != "unsupported_capability" {
		t.Fatalf("arbitrary resource access was not refused explicitly: %v", refused)
	}
	status, _, sessionCalled := mcpCall("POST", "/mcp", sessionID, map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "tools/call",
		"params": map[string]any{"name": tool.Alias, "arguments": map[string]any{"query": argument}, "_meta": map[string]any{"olp.call_id": "existing-session-1"}},
	})
	callResult := mcpExecObject(t, sessionCalled, "result")
	sessionContent := mcpExecItems(t, callResult, "content")
	if status != http.StatusOK || calls.Load() != 2 || len(sessionContent) != 1 {
		t.Fatalf("session call did not return the upstream result: status=%d calls=%d body=%v", status, calls.Load(), sessionCalled)
	}
	if got := mcpExecString(t, mcpExecItem(t, sessionContent, 0, sessionCalled), "text"); got != answer {
		t.Fatalf("session call did not return the upstream result: status=%d calls=%d body=%v", status, calls.Load(), sessionCalled)
	}
	meta := mcpExecObject(t, callResult, "_meta")
	var sessionState string
	if err = h.Pool.QueryRow(t.Context(), `SELECT state FROM olp.execution_debits WHERE id=$1`, meta["olp.dispatch_id"]).Scan(&sessionState); err != nil || sessionState != "settled" || debitCount() != 2 {
		t.Fatalf("session dispatch did not settle on the shared ledger: state=%q err=%v debits=%d", sessionState, err, debitCount())
	}
	if status, _, _ = mcpCallAs(otherSecret, "POST", "/mcp", sessionID, map[string]any{"jsonrpc": "2.0", "id": 6, "method": "tools/list", "params": map[string]any{}}); status != http.StatusNotFound {
		t.Fatalf("another principal listed through the session at the gateway boundary: %d", status)
	}
	if status, _, _ = mcpCallAs(otherSecret, "POST", "/mcp", sessionID, map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{"name": tool.Alias, "arguments": map[string]any{"query": argument}, "_meta": map[string]any{"olp.call_id": "existing-session-2"}},
	}); status != http.StatusNotFound {
		t.Fatalf("another principal called through the session at the gateway boundary: %d", status)
	}
	if status, _, _ = mcpCallAs(otherSecret, "DELETE", "/mcp", sessionID, nil); status != http.StatusNotFound {
		t.Fatalf("another principal deleted the session at the gateway boundary: %d", status)
	}
	if calls.Load() != 2 || debitCount() != 2 {
		t.Fatalf("cross-principal session use dispatched at the gateway boundary: calls=%d debits=%d", calls.Load(), debitCount())
	}
	if status, _, _ = mcpCall("POST", "/mcp", sessionID, map[string]any{"jsonrpc": "2.0", "id": 8, "method": "tools/list", "params": map[string]any{}}); status != http.StatusOK {
		t.Fatalf("owner lost the session after a foreign attempt at the gateway boundary: %d", status)
	}

	description.Store("Changed after publication")
	status, driftedCall := call("POST", "/v1/mcp/tools/call", map[string]any{"toolset_id": toolsetID, "name": tool.Alias, "call_id": "existing-json-4", "arguments": map[string]any{"query": argument}})
	if status != http.StatusConflict || mcpExecString(t, mcpExecObject(t, driftedCall, "error"), "code") != "tool_definition_changed" || calls.Load() != 2 {
		t.Fatalf("drifted catalog was called over the JSON path: status=%d calls=%d body=%v", status, calls.Load(), driftedCall)
	}
	_, _, liveDriftedCall := mcpCall("POST", "/mcp", sessionID, map[string]any{
		"jsonrpc": "2.0", "id": 9, "method": "tools/call",
		"params": map[string]any{"name": tool.Alias, "arguments": map[string]any{"query": argument}, "_meta": map[string]any{"olp.call_id": "existing-session-3"}},
	})
	if data := mcpExecObject(t, mcpExecObject(t, liveDriftedCall, "error"), "data"); mcpExecString(t, data, "code") != "tool_definition_changed" || calls.Load() != 2 {
		t.Fatalf("drifted catalog was called in an established session: calls=%d body=%v", calls.Load(), liveDriftedCall)
	}
	status, driftedHeaders, driftedInit := mcpCall("POST", "/mcp?toolset_id="+toolsetID, "", map[string]any{
		"jsonrpc": "2.0", "id": 10, "method": "initialize",
		"params": map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "integration", "version": "1"}},
	})
	driftedSessionID := driftedHeaders.Get("Mcp-Session-Id")
	if status != http.StatusOK || driftedSessionID == "" {
		t.Fatalf("session did not initialize against a drifted catalog for review: status=%d body=%v", status, driftedInit)
	}
	if status, _, _ = mcpCall("POST", "/mcp", driftedSessionID, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); status != http.StatusAccepted {
		t.Fatalf("drifted session initialized notification refused: %d", status)
	}
	_, _, driftedSessionCall := mcpCall("POST", "/mcp", driftedSessionID, map[string]any{
		"jsonrpc": "2.0", "id": 11, "method": "tools/call",
		"params": map[string]any{"name": tool.Alias, "arguments": map[string]any{"query": argument}, "_meta": map[string]any{"olp.call_id": "existing-session-4"}},
	})
	if data := mcpExecObject(t, mcpExecObject(t, driftedSessionCall, "error"), "data"); mcpExecString(t, data, "code") != "tool_definition_changed" || calls.Load() != 2 {
		t.Fatalf("drifted catalog was called in a fresh session: calls=%d body=%v", calls.Load(), driftedSessionCall)
	}
	h.want(owner, "GET", "/api/v1/toolsets/"+toolsetID, nil, nil, 200)
	published := h.want(owner, "GET", "/api/v1/mcp-servers/"+serverID+"/revisions/"+tool.RevisionID, nil, nil, 200)
	if mcpExecString(t, mcpExecObject(t, published, "catalog"), "digest") != tool.CatalogDigest {
		t.Fatalf("published definition stopped being readable while drifted: %v", published)
	}
	description.Store("Look up")
	if status, _, _ = mcpCall("DELETE", "/mcp", sessionID, nil); status != http.StatusNoContent {
		t.Fatalf("session delete refused: %d", status)
	}
	if status, _, _ = mcpCall("POST", "/mcp", sessionID, map[string]any{"jsonrpc": "2.0", "id": 5, "method": "tools/list", "params": map[string]any{}}); status != http.StatusNotFound {
		t.Fatalf("deleted session stayed usable: %d", status)
	}

	status, headers, reopened := mcpCall("POST", "/mcp?toolset_id="+toolsetID, "", map[string]any{
		"jsonrpc": "2.0", "id": 12, "method": "initialize",
		"params": map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "integration", "version": "1"}},
	})
	liveSessionID := headers.Get("Mcp-Session-Id")
	if status != http.StatusOK || liveSessionID == "" {
		t.Fatalf("session did not reopen before retirement: status=%d body=%v", status, reopened)
	}
	if status, _, _ = mcpCall("POST", "/mcp", liveSessionID, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); status != http.StatusAccepted {
		t.Fatalf("reopened session initialized notification refused: %d", status)
	}
	current := h.want(owner, "GET", "/api/v1/mcp-servers/"+serverID, nil, nil, 200)
	retire := idem("mcp-exec-retire")
	retire["If-Match"] = `"` + mcpExecString(t, current, "etag") + `"`
	h.want(owner, "DELETE", "/api/v1/mcp-servers/"+serverID, nil, retire, 204)
	if err = h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, _ = call("POST", "/v1/mcp/tools/call", map[string]any{"toolset_id": toolsetID, "name": tool.Alias, "call_id": "existing-json-5", "arguments": map[string]any{"query": argument}})
	if status != http.StatusNotFound || calls.Load() != 2 {
		t.Fatalf("retired server was called over the JSON path: status=%d calls=%d", status, calls.Load())
	}
	_, _, refusedList := mcpCall("POST", "/mcp", liveSessionID, map[string]any{"jsonrpc": "2.0", "id": 13, "method": "tools/list", "params": map[string]any{}})
	if _, ok := refusedList["error"]; !ok {
		t.Fatalf("live session kept serving after its server retired: %v", refusedList)
	}
	if status, _, _ = mcpCall("POST", "/mcp", liveSessionID, map[string]any{"jsonrpc": "2.0", "id": 14, "method": "tools/list", "params": map[string]any{}}); status != http.StatusNotFound {
		t.Fatalf("live session stayed eligible after its server retired: %d", status)
	}
	if calls.Load() != 2 {
		t.Fatalf("retired server dispatched: calls=%d", calls.Load())
	}
}

func TestMCPToolCallSnapshotDriftScopesPerServerEndToEnd(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "MCP per-server drift")
	const credential = "integration-per-server-credential"
	upstreamFor := func() (*httptest.Server, *atomic.Value, *atomic.Int64) {
		description := &atomic.Value{}
		description.Store("Look up")
		calls := &atomic.Int64{}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if r.Header.Get("Authorization") != "Bearer "+credential {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			var request struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
			}
			if json.NewDecoder(r.Body).Decode(&request) != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var result any
			switch request.Method {
			case "notifications/initialized":
				w.WriteHeader(http.StatusAccepted)
				return
			case "initialize":
				w.Header().Set("Mcp-Session-Id", "per-server-session")
				result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}}
			case "tools/list":
				text, _ := description.Load().(string)
				result = map[string]any{"tools": []any{map[string]any{
					"name": "lookup", "description": text,
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []string{"query"}, "additionalProperties": false},
				}}}
			case "tools/call":
				calls.Add(1)
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "answer"}}, "isError": false}
			default:
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
		}))
		t.Cleanup(server.Close)
		return server, description, calls
	}
	upstreamA, _, callsA := upstreamFor()
	upstreamB, descriptionB, callsB := upstreamFor()
	register := func(name string, upstream *httptest.Server, key string) map[string]any {
		return h.want(owner, "POST", "/api/v1/mcp-servers", map[string]any{
			"name": name, "project_id": project, "transport": "streamable_http",
			"endpoint": upstream.URL, "enabled": true, "credential": credential,
		}, idem(key), 201)
	}
	pinOf := func(created map[string]any) agentcontract.Tool {
		serverID := mcpExecString(t, created, "id")
		return agentcontract.Tool{ServerID: serverID, RevisionID: mcpExecString(t, created, "revision_id"), CatalogDigest: mcpExecString(t, mcpExecObject(t, created, "catalog"), "digest"), Name: "lookup", Alias: agentcontract.ToolAlias(serverID, "lookup")}
	}
	createdA := register("Healthy server", upstreamA, "mcp-per-server-a")
	createdB := register("Drifting server", upstreamB, "mcp-per-server-b")
	pinA, pinB := pinOf(createdA), pinOf(createdB)
	toolset := h.want(owner, "POST", "/api/v1/toolsets", map[string]any{
		"project_id": project, "name": "Per-server drift toolset",
		"toolset": map[string]any{"tools": []agentcontract.Tool{pinA, pinB}},
	}, idem("mcp-per-server-toolset"), 201)
	toolsetID := mcpExecString(t, toolset, "id")
	h.want(owner, "POST", "/api/v1/toolsets/"+toolsetID+"/publish", nil, withMatch(toolset, idem("mcp-per-server-publish")), 200)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{
		"name": "Per-server drift caller", "project_id": project, "scopes": []string{"tools"}, "allowed_toolsets": []string{toolsetID},
	}, idem("mcp-per-server-key"), 201)
	secret := mcpExecString(t, key, "secret")

	valkey, prefix, _ := acctKeyspace(t)
	limiter, err := limits.New(valkey, strings.TrimSuffix(prefix, ":"))
	if err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.ParseRing([]byte(h.Ring))
	if err != nil {
		t.Fatal(err)
	}
	installation, err := database.Installation(t.Context(), h.Pool)
	if err != nil {
		t.Fatal(err)
	}
	sessionStore, err := mcp.NewSessionStore(valkey, strings.TrimSuffix(prefix, ":")+":mcp")
	if err != nil {
		t.Fatal(err)
	}
	toolServer := &mcp.Server{
		Runtime: h.Runtime, Egress: &h.Policy, Auth: h.AuthKey, Sessions: sessionStore,
		Ledger: &mcp.DurableLedger{Pool: h.Pool, Store: &limits.DebitStore{Pool: h.Pool, Limiter: limiter}, Auth: h.AuthKey},
		Credentials: func(ctx context.Context, credentialID string) ([]byte, error) {
			return ring.Read(ctx, h.Pool, installation, credentialID, secrets.MCPCredential)
		},
	}
	mux := http.NewServeMux()
	toolServer.Register(mux)
	consumer := httptest.NewServer(mux)
	defer consumer.Close()
	if err = h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	call := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		var data []byte
		if body != nil {
			data, _ = json.Marshal(body)
		}
		req, err := http.NewRequest(method, consumer.URL+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		out := map[string]any{}
		if len(raw) > 0 {
			if err = json.Unmarshal(raw, &out); err != nil {
				t.Fatalf("consumer response is not JSON: %s", raw)
			}
		}
		return resp.StatusCode, out
	}

	if status, body := call("POST", "/v1/mcp/tools/call", map[string]any{"toolset_id": toolsetID, "name": pinA.Alias, "call_id": "existing-json-6", "arguments": map[string]any{"query": "x"}}); status != http.StatusOK || callsA.Load() != 1 {
		t.Fatalf("healthy server did not serve before drift: status=%d calls=%d body=%v", status, callsA.Load(), body)
	}

	descriptionB.Store("Changed after publication")
	serverBID := mcpExecString(t, createdB, "id")
	currentB := h.want(owner, "GET", "/api/v1/mcp-servers/"+serverBID, nil, nil, 200)
	h.want(owner, "PUT", "/api/v1/mcp-servers/"+serverBID,
		map[string]any{"name": "Drifting server", "endpoint": upstreamB.URL, "enabled": true},
		withMatch(currentB, idem("mcp-per-server-recertify")), 200)
	if err = h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}

	if status, body := call("POST", "/v1/mcp/tools/call", map[string]any{"toolset_id": toolsetID, "name": pinA.Alias, "call_id": "existing-json-7", "arguments": map[string]any{"query": "x"}}); status != http.StatusOK || callsA.Load() != 2 {
		t.Fatalf("snapshot drift on one server blocked the healthy server: status=%d calls=%d body=%v", status, callsA.Load(), body)
	}
	status, drifted := call("POST", "/v1/mcp/tools/call", map[string]any{"toolset_id": toolsetID, "name": pinB.Alias, "call_id": "existing-json-8", "arguments": map[string]any{"query": "x"}})
	if status != http.StatusConflict || mcpExecString(t, mcpExecObject(t, drifted, "error"), "code") != "tool_definition_changed" || callsB.Load() != 0 {
		t.Fatalf("snapshot-drifted server beside a healthy one was not refused as drift: status=%d calls=%d body=%v", status, callsB.Load(), drifted)
	}
	status, listed := call("GET", "/v1/mcp/tools?toolset_id="+toolsetID, nil)
	if status != http.StatusConflict || mcpExecString(t, mcpExecObject(t, listed, "error"), "code") != "tool_definition_changed" {
		t.Fatalf("explicit listing silently skipped the snapshot-drifted server: status=%d body=%v", status, listed)
	}
	h.want(owner, "GET", "/api/v1/toolsets/"+toolsetID, nil, nil, 200)
}

func mcpAmount(t *testing.T, body map[string]any, key string) float64 {
	t.Helper()
	value, err := strconv.ParseFloat(mcpExecString(t, body, key), 64)
	if err != nil {
		t.Fatalf("response field %q is not an amount: %v", key, body)
	}
	return value
}

func mcpAmountIsZero(t *testing.T, body map[string]any, key string) bool {
	t.Helper()
	return mcpAmount(t, body, key) == 0
}

func mcpAmountsEqual(t *testing.T, body map[string]any, left, right string) bool {
	t.Helper()
	return mcpAmount(t, body, left) == mcpAmount(t, body, right)
}

func TestMCPCallIdentitySurvivesRestartAndResolvesUnknownOnce(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "MCP call identity")
	const credential = "integration-identity-credential"
	const argument = "integration-identity-argument"
	const answer = "integration-identity-answer"
	const callerID = "integration-caller-order-9"
	var calls atomic.Int64
	var drop atomic.Bool
	var block atomic.Bool
	blockEntered := make(chan struct{}, 1)
	blockRelease := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+credential {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var request struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var result any
		switch request.Method {
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "identity-session")
			result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{
				"name": "lookup", "description": "Look up",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []string{"query"}, "additionalProperties": false},
			}}}
		case "tools/call":
			calls.Add(1)
			if drop.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if block.Load() {
				select {
				case blockEntered <- struct{}{}:
				default:
				}
				<-blockRelease
			}
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": answer}}, "isError": false}
		default:
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	defer upstream.Close()

	created := h.want(owner, "POST", "/api/v1/mcp-servers", map[string]any{
		"name": "Identity server", "project_id": project, "transport": "streamable_http",
		"endpoint": upstream.URL, "enabled": true, "credential": credential,
	}, idem("mcp-identity-server"), 201)
	serverID := mcpExecString(t, created, "id")
	tool := agentcontract.Tool{ServerID: serverID, RevisionID: mcpExecString(t, created, "revision_id"), CatalogDigest: mcpExecString(t, mcpExecObject(t, created, "catalog"), "digest"), Name: "lookup", Alias: agentcontract.ToolAlias(serverID, "lookup")}
	toolset := h.want(owner, "POST", "/api/v1/toolsets", map[string]any{
		"project_id": project, "name": "Identity toolset",
		"toolset": map[string]any{"tools": []agentcontract.Tool{tool}},
	}, idem("mcp-identity-toolset"), 201)
	toolsetID := mcpExecString(t, toolset, "id")
	h.want(owner, "POST", "/api/v1/toolsets/"+toolsetID+"/publish", nil, withMatch(toolset, idem("mcp-identity-publish")), 200)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{
		"name": "Identity caller", "project_id": project, "scopes": []string{"tools"}, "allowed_toolsets": []string{toolsetID},
	}, idem("mcp-identity-key"), 201)
	secret := mcpExecString(t, key, "secret")

	valkey, prefix, _ := acctKeyspace(t)
	limiter, err := limits.New(valkey, strings.TrimSuffix(prefix, ":"))
	if err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.ParseRing([]byte(h.Ring))
	if err != nil {
		t.Fatal(err)
	}
	installation, err := database.Installation(t.Context(), h.Pool)
	if err != nil {
		t.Fatal(err)
	}
	sessionStore, err := mcp.NewSessionStore(valkey, strings.TrimSuffix(prefix, ":")+":mcp-identity")
	if err != nil {
		t.Fatal(err)
	}
	newConsumer := func() *httptest.Server {
		toolServer := &mcp.Server{
			Runtime: h.Runtime, Egress: &h.Policy, Auth: h.AuthKey, Sessions: sessionStore,
			Ledger: &mcp.DurableLedger{Pool: h.Pool, Store: &limits.DebitStore{Pool: h.Pool, Limiter: limiter}, Auth: h.AuthKey},
			Credentials: func(ctx context.Context, credentialID string) ([]byte, error) {
				return ring.Read(ctx, h.Pool, installation, credentialID, secrets.MCPCredential)
			},
		}
		mux := http.NewServeMux()
		toolServer.Register(mux)
		consumer := httptest.NewServer(mux)
		t.Cleanup(consumer.Close)
		return consumer
	}
	if err = h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	callOn := func(consumer *httptest.Server, callID, query string) (int, map[string]any) {
		t.Helper()
		data, _ := json.Marshal(map[string]any{"toolset_id": toolsetID, "name": tool.Alias, "call_id": callID, "arguments": map[string]any{"query": query}})
		req, err := http.NewRequest(http.MethodPost, consumer.URL+"/v1/mcp/tools/call", bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		out := map[string]any{}
		if len(raw) > 0 {
			if err = json.Unmarshal(raw, &out); err != nil {
				t.Fatalf("consumer response is not JSON: %s", raw)
			}
		}
		return resp.StatusCode, out
	}
	evidenceDigest := func(seed string) string {
		sum := sha256.Sum256([]byte(seed))
		return hex.EncodeToString(sum[:])
	}

	first := newConsumer()
	status, called := callOn(first, callerID, argument)
	if status != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("first identified call did not execute once: status=%d calls=%d body=%v", status, calls.Load(), called)
	}
	attemptID := mcpExecString(t, called, "attempt_id")
	status, repeated := callOn(first, callerID, argument)
	if status != http.StatusOK || calls.Load() != 1 || mcpExecString(t, repeated, "attempt_id") != attemptID {
		t.Fatalf("exact repeat re-executed or lost identity: status=%d calls=%d body=%v", status, calls.Load(), repeated)
	}
	status, conflicted := callOn(first, callerID, "changed-argument")
	if status != http.StatusConflict || calls.Load() != 1 {
		t.Fatalf("changed body under one call id executed: status=%d calls=%d body=%v", status, calls.Load(), conflicted)
	}
	if code := mcpExecString(t, mcpExecObject(t, conflicted, "error"), "code"); code != "call_id_conflict" {
		t.Fatalf("changed body did not conflict by call id: %v", conflicted)
	}
	var identityRecord string
	if err = h.Pool.QueryRow(t.Context(), `SELECT row_to_json(i)::text FROM olp.mcp_call_identities i WHERE i.attempt_id=$1`, attemptID).Scan(&identityRecord); err != nil {
		t.Fatalf("durable call identity missing: %v", err)
	}
	for _, forbidden := range []string{callerID, argument, answer, credential, secret} {
		if strings.Contains(identityRecord, forbidden) {
			t.Fatal("durable call identity persisted caller content")
		}
	}
	detail := h.want(owner, "GET", "/api/v1/mcp-tool-calls/"+attemptID, nil, nil, 200)
	if detail["call_digest"] == nil || detail["call_digest"] == callerID || detail["debit_state"] != "settled" {
		t.Fatalf("management record lost the durable call identity: %v", detail)
	}

	restarted := newConsumer()
	status, gone := callOn(restarted, callerID, argument)
	if status != http.StatusConflict || calls.Load() != 1 {
		t.Fatalf("repeat after restart replayed upstream: status=%d calls=%d body=%v", status, calls.Load(), gone)
	}
	if code := mcpExecString(t, mcpExecObject(t, gone, "error"), "code"); code != "result_not_retained" || mcpExecString(t, gone, "attempt_id") != attemptID || gone["status"] != "succeeded" {
		t.Fatalf("repeat after restart did not report the unretained retained outcome: %v", gone)
	}

	drop.Store(true)
	status, unknown := callOn(restarted, "integration-caller-unknown-1", argument)
	if status != http.StatusBadGateway || unknown["status"] != "outcome_unknown" || calls.Load() != 2 {
		t.Fatalf("ambiguous dispatch was not held unknown: status=%d calls=%d body=%v", status, calls.Load(), unknown)
	}
	unknownAttempt := mcpExecString(t, unknown, "attempt_id")
	unknownDispatch := mcpExecString(t, unknown, "dispatch_id")
	drop.Store(false)
	store := &limits.DebitStore{Pool: h.Pool, Limiter: limiter}
	if _, err = store.RecoverPrepared(t.Context(), 256); err != nil {
		t.Fatal(err)
	}
	var heldState string
	if err = h.Pool.QueryRow(t.Context(), `SELECT state FROM olp.execution_debits WHERE id=$1`, unknownDispatch).Scan(&heldState); err != nil || heldState != "outcome_unknown" {
		t.Fatalf("recovery released the held unknown debit: state=%q err=%v", heldState, err)
	}
	status, stillUnknown := callOn(newConsumer(), "integration-caller-unknown-1", argument)
	if status != http.StatusBadGateway || stillUnknown["status"] != "outcome_unknown" || calls.Load() != 2 || mcpExecString(t, stillUnknown, "attempt_id") != unknownAttempt {
		t.Fatalf("unknown outcome redispatched after restart: status=%d calls=%d body=%v", status, calls.Load(), stillUnknown)
	}
	held := h.want(owner, "GET", "/api/v1/mcp-tool-calls/"+unknownAttempt, nil, nil, 200)
	if held["status"] != "outcome_unknown" || held["debit_state"] != "outcome_unknown" || !mcpAmountIsZero(t, held, "observed_cost") || !mcpAmountsEqual(t, held, "remaining_cost", "cost_bound") || !mcpAmountsEqual(t, held, "remaining_charge", "charge_bound") {
		t.Fatalf("report folded the unresolved bound into observed cost: %v", held)
	}
	debits := h.want(owner, "GET", "/api/v1/execution-debits?api_key_id="+mcpExecString(t, key, "id"), nil, nil, 200)
	foundHeld := false
	for _, item := range mcpExecItems(t, debits, "items") {
		debit, _ := item.(map[string]any)
		if debit["id"] == unknownDispatch {
			foundHeld = debit["state"] == "outcome_unknown" && mcpAmountIsZero(t, debit, "observed_cost") && mcpAmountsEqual(t, debit, "remaining_cost", "cost_bound")
		}
	}
	if !foundHeld {
		t.Fatalf("execution debit report lost the held unknown exposure: %v", debits)
	}

	resolved := h.want(owner, "POST", "/api/v1/mcp-tool-calls/"+unknownAttempt+"/resolve", map[string]any{
		"outcome": "not_accepted", "evidence_digest": evidenceDigest("upstream-log-shows-no-acceptance"),
	}, idem("mcp-identity-resolve-unknown"), 200)
	if resolved["debit_state"] != "not_accepted" || !mcpAmountIsZero(t, resolved, "remaining_cost") {
		t.Fatalf("evidence-backed finding did not close the held debit: %v", resolved)
	}
	var resolutions, audits int
	if err = h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.execution_debit_resolutions WHERE dispatch_id=$1`, unknownDispatch).Scan(&resolutions); err != nil || resolutions != 1 {
		t.Fatalf("resolution evidence was not recorded exactly once: count=%d err=%v", resolutions, err)
	}
	if err = h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.audit WHERE action='mcp_tool_call.resolve' AND resource_id=$1`, unknownAttempt).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("resolution was not audited exactly once: count=%d err=%v", audits, err)
	}
	h.want(owner, "POST", "/api/v1/mcp-tool-calls/"+unknownAttempt+"/resolve", map[string]any{
		"outcome": "settled", "evidence_digest": evidenceDigest("contradictory-late-evidence"),
	}, idem("mcp-identity-resolve-again"), 409)
	if err = h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.execution_debit_resolutions WHERE dispatch_id=$1`, unknownDispatch).Scan(&resolutions); err != nil || resolutions != 1 {
		t.Fatalf("second resolution rewrote the first finding: count=%d err=%v", resolutions, err)
	}
	status, afterResolve := callOn(restarted, "integration-caller-unknown-1", argument)
	if status != http.StatusBadGateway || calls.Load() != 2 {
		t.Fatalf("resolved call id redispatched inside its binding: status=%d calls=%d body=%v", status, calls.Load(), afterResolve)
	}

	drop.Store(true)
	status, unknownTwo := callOn(restarted, "integration-caller-unknown-2", argument)
	if status != http.StatusBadGateway || calls.Load() != 3 {
		t.Fatalf("second ambiguous dispatch was not held unknown: status=%d calls=%d body=%v", status, calls.Load(), unknownTwo)
	}
	drop.Store(false)
	unknownTwoAttempt := mcpExecString(t, unknownTwo, "attempt_id")
	unknownTwoDispatch := mcpExecString(t, unknownTwo, "dispatch_id")
	settled := h.want(owner, "POST", "/api/v1/mcp-tool-calls/"+unknownTwoAttempt+"/resolve", map[string]any{
		"outcome": "settled", "evidence_digest": evidenceDigest("upstream-log-shows-completion"),
	}, idem("mcp-identity-settle-unknown"), 200)
	if settled["debit_state"] != "settled" {
		t.Fatalf("later settlement did not close the held debit: %v", settled)
	}
	var resultCount int
	if err = h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.execution_debit_results WHERE dispatch_id=$1`, unknownTwoDispatch).Scan(&resultCount); err != nil || resultCount != 1 {
		t.Fatalf("later settlement was not recorded exactly once: count=%d err=%v", resultCount, err)
	}
	h.want(owner, "POST", "/api/v1/mcp-tool-calls/"+unknownTwoAttempt+"/resolve", map[string]any{
		"outcome": "settled", "evidence_digest": evidenceDigest("different-late-evidence"),
	}, idem("mcp-identity-settle-again"), 409)
	if err = h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.execution_debit_results WHERE dispatch_id=$1`, unknownTwoDispatch).Scan(&resultCount); err != nil || resultCount != 1 {
		t.Fatalf("second settlement appended another result: count=%d err=%v", resultCount, err)
	}

	block.Store(true)
	type concurrentResult struct {
		status int
		body   map[string]any
		err    error
	}
	rawCall := func() concurrentResult {
		data, _ := json.Marshal(map[string]any{"toolset_id": toolsetID, "name": tool.Alias, "call_id": "integration-caller-concurrent", "arguments": map[string]any{"query": argument}})
		resp, err := func() (*http.Response, error) {
			req, err := http.NewRequest(http.MethodPost, restarted.URL+"/v1/mcp/tools/call", bytes.NewReader(data))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Authorization", "Bearer "+secret)
			req.Header.Set("Content-Type", "application/json")
			return http.DefaultClient.Do(req)
		}()
		if err != nil {
			return concurrentResult{err: err}
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		out := map[string]any{}
		if err = json.Unmarshal(raw, &out); err != nil {
			return concurrentResult{err: err}
		}
		return concurrentResult{status: resp.StatusCode, body: out}
	}
	firstCall := make(chan concurrentResult, 1)
	go func() { firstCall <- rawCall() }()
	<-blockEntered
	status, concurrent := callOn(restarted, "integration-caller-concurrent", argument)
	if status != http.StatusBadGateway || concurrent["status"] != "outcome_unknown" || calls.Load() != 4 {
		t.Fatalf("concurrent repeat dispatched beside the in-flight call: status=%d calls=%d body=%v", status, calls.Load(), concurrent)
	}
	concurrentAttempt := mcpExecString(t, concurrent, "attempt_id")
	close(blockRelease)
	block.Store(false)
	finished := <-firstCall
	if finished.err != nil || finished.status != http.StatusOK || mcpExecString(t, finished.body, "attempt_id") != concurrentAttempt {
		t.Fatalf("in-flight call did not finish as the attempt its repeat retained: %+v", finished)
	}
	status, afterConcurrent := callOn(restarted, "integration-caller-concurrent", argument)
	if status != http.StatusOK || calls.Load() != 4 || mcpExecString(t, afterConcurrent, "attempt_id") != concurrentAttempt {
		t.Fatalf("settled concurrent call re-executed on repeat: status=%d calls=%d body=%v", status, calls.Load(), afterConcurrent)
	}

	if _, err = h.Pool.Exec(t.Context(), `UPDATE olp.mcp_call_identities SET created_at=clock_timestamp()-interval '25 hours',expires_at=clock_timestamp()-interval '1 hour' WHERE attempt_id=$1`, attemptID); err != nil {
		t.Fatal(err)
	}
	status, reused := callOn(restarted, callerID, argument)
	if status != http.StatusOK || calls.Load() != 5 || mcpExecString(t, reused, "attempt_id") == attemptID {
		t.Fatalf("expired call identity stayed bound past 24 hours: status=%d calls=%d body=%v", status, calls.Load(), reused)
	}
}
