//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/mcp"
	"github.com/tyk-swe/olp/internal/secrets"
)

func TestMCPToolCallEndToEndSettlesDurableDebit(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "MCP execution")
	const credential = "integration-static-credential"
	const argument = "integration-secret-argument"
	const answer = "integration-secret-answer"
	var calls atomic.Int64
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
			result = map[string]any{"tools": []any{map[string]any{
				"name": "lookup", "description": "Look up",
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
	serverID := created["id"].(string)
	tool := agentcontract.Tool{ServerID: serverID, RevisionID: created["revision_id"].(string), CatalogDigest: created["catalog"].(map[string]any)["digest"].(string), Name: "lookup", Alias: agentcontract.ToolAlias(serverID, "lookup")}
	toolset := h.want(owner, "POST", "/api/v1/toolsets", map[string]any{
		"project_id": project, "name": "Execution toolset",
		"toolset": map[string]any{"tools": []agentcontract.Tool{tool}},
	}, idem("mcp-exec-toolset"), 201)
	toolsetID := toolset["id"].(string)
	h.want(owner, "POST", "/api/v1/toolsets/"+toolsetID+"/publish", nil, withMatch(toolset, idem("mcp-exec-publish")), 200)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{
		"name": "Tool caller", "project_id": project, "scopes": []string{"tools"}, "allowed_toolsets": []string{toolsetID},
	}, idem("mcp-exec-key"), 201)
	secret := key["secret"].(string)

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
	toolServer := &mcp.Server{
		Runtime: h.Runtime, Egress: &h.Policy, Auth: h.AuthKey,
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
	mcpCall := func(method, path, sessionID string, body any) (int, http.Header, map[string]any) {
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
	debitCount := func() int {
		t.Helper()
		var count int
		if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.execution_debits WHERE api_key_id=$1 AND kind='tool'`, key["id"]).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}

	status, listed := call("GET", "/v1/mcp/tools", nil)
	tools, _ := listed["tools"].([]any)
	if status != http.StatusOK || len(tools) != 1 || tools[0].(map[string]any)["name"] != tool.Alias {
		t.Fatalf("pinned tools were not listed under their alias: %d %v", status, listed)
	}

	status, _ = call("POST", "/v1/mcp/tools/call", map[string]any{"toolset_id": toolsetID, "name": tool.Alias, "arguments": map[string]any{"query": 3}})
	if status != http.StatusUnprocessableEntity || calls.Load() != 0 || debitCount() != 0 {
		t.Fatalf("invalid arguments reserved or dispatched: status=%d calls=%d debits=%d", status, calls.Load(), debitCount())
	}

	status, called := call("POST", "/v1/mcp/tools/call", map[string]any{"toolset_id": toolsetID, "name": tool.Alias, "arguments": map[string]any{"query": argument}})
	if status != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("valid call did not reach the upstream once: status=%d calls=%d body=%v", status, calls.Load(), called)
	}
	content, _ := called["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["text"] != answer {
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
	detail := h.want(owner, "GET", "/api/v1/mcp-tool-calls/"+called["attempt_id"].(string), nil, nil, 200)
	if detail["status"] != "succeeded" || detail["tool_alias"] != tool.Alias {
		t.Fatalf("management record does not describe the attempt: %v", detail)
	}
	listedCalls := h.want(owner, "GET", "/api/v1/mcp-tool-calls?toolset_id="+toolsetID, nil, nil, 200)
	if items, _ := listedCalls["items"].([]any); len(items) != 1 {
		t.Fatalf("management list lost the attempt: %v", listedCalls)
	}

	status, _ = call("POST", "/v1/mcp/tools/call", map[string]any{"toolset_id": "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50799", "name": tool.Alias, "arguments": map[string]any{"query": "x"}})
	if status != http.StatusNotFound || calls.Load() != 1 || debitCount() != 1 {
		t.Fatalf("foreign toolset dispatched: status=%d calls=%d debits=%d", status, calls.Load(), debitCount())
	}

	status, headers, initialized := mcpCall("POST", "/mcp?toolset_id="+toolsetID, "", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "integration", "version": "1"}},
	})
	sessionID := headers.Get("Mcp-Session-Id")
	initResult, _ := initialized["result"].(map[string]any)
	if status != http.StatusOK || sessionID == "" || initResult["protocolVersion"] != "2025-11-25" {
		t.Fatalf("streamable session did not initialize: status=%d body=%v", status, initialized)
	}
	if status, _, _ = mcpCall("POST", "/mcp", sessionID, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); status != http.StatusAccepted {
		t.Fatalf("initialized notification refused: %d", status)
	}
	status, _, sessionListed := mcpCall("POST", "/mcp", sessionID, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": map[string]any{}})
	sessionTools, _ := sessionListed["result"].(map[string]any)["tools"].([]any)
	if status != http.StatusOK || len(sessionTools) != 1 || sessionTools[0].(map[string]any)["name"] != tool.Alias {
		t.Fatalf("session did not list the pinned tool: status=%d body=%v", status, sessionListed)
	}
	if _, ok := sessionTools[0].(map[string]any)["inputSchema"]; !ok {
		t.Fatalf("session listing lost the wire schema: %v", sessionTools)
	}
	_, _, refused := mcpCall("POST", "/mcp", sessionID, map[string]any{"jsonrpc": "2.0", "id": 3, "method": "resources/list", "params": map[string]any{}})
	if data, _ := refused["error"].(map[string]any)["data"].(map[string]any); data["code"] != "unsupported_capability" {
		t.Fatalf("arbitrary resource access was not refused explicitly: %v", refused)
	}
	status, _, sessionCalled := mcpCall("POST", "/mcp", sessionID, map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "tools/call",
		"params": map[string]any{"name": tool.Alias, "arguments": map[string]any{"query": argument}},
	})
	callResult, _ := sessionCalled["result"].(map[string]any)
	sessionContent, _ := callResult["content"].([]any)
	if status != http.StatusOK || calls.Load() != 2 || len(sessionContent) != 1 || sessionContent[0].(map[string]any)["text"] != answer {
		t.Fatalf("session call did not return the upstream result: status=%d calls=%d body=%v", status, calls.Load(), sessionCalled)
	}
	meta, _ := callResult["_meta"].(map[string]any)
	var sessionState string
	if err = h.Pool.QueryRow(t.Context(), `SELECT state FROM olp.execution_debits WHERE id=$1`, meta["olp.dispatch_id"]).Scan(&sessionState); err != nil || sessionState != "settled" || debitCount() != 2 {
		t.Fatalf("session dispatch did not settle on the shared ledger: state=%q err=%v debits=%d", sessionState, err, debitCount())
	}
	if status, _, _ = mcpCall("DELETE", "/mcp", sessionID, nil); status != http.StatusNoContent {
		t.Fatalf("session delete refused: %d", status)
	}
	if status, _, _ = mcpCall("POST", "/mcp", sessionID, map[string]any{"jsonrpc": "2.0", "id": 5, "method": "tools/list", "params": map[string]any{}}); status != http.StatusNotFound {
		t.Fatalf("deleted session stayed usable: %d", status)
	}
}
