//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/mcp"
	"github.com/tyk-swe/olp/internal/secrets"
	"golang.org/x/oauth2"
)

func TestMCPReferenceServerRoundTrip(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "Reference MCP tools")
	type arguments struct {
		Query string `json:"query"`
	}
	var dispatches atomic.Int64
	reference := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "reference-tools", Version: "1"}, &mcpsdk.ServerOptions{SupportedProtocolVersions: []string{"2025-11-25"}})
	mcpsdk.AddTool(reference, &mcpsdk.Tool{Name: "echo", Description: "Return the supplied text"}, func(_ context.Context, _ *mcpsdk.CallToolRequest, input arguments) (*mcpsdk.CallToolResult, any, error) {
		dispatches.Add(1)
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: input.Query}}}, nil, nil
	})
	upstream := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return reference }, &mcpsdk.StreamableHTTPOptions{JSONResponse: true}))
	defer upstream.Close()
	registered := h.want(owner, "POST", "/api/v1/mcp-servers", map[string]any{
		"name": "Reference MCP server", "project_id": project, "transport": "streamable_http",
		"endpoint": upstream.URL, "enabled": true, "unit_price": "0", "per_key_calls_per_minute": 20, "per_key_max_concurrency": 4,
	}, idem("mcp-reference-server"), 201)
	serverID := mcpExecString(t, registered, "id")
	tool := agentcontract.Tool{ServerID: serverID, RevisionID: mcpExecString(t, registered, "revision_id"), CatalogDigest: mcpExecString(t, mcpExecObject(t, registered, "catalog"), "digest"), Name: "echo", Alias: agentcontract.ToolAlias(serverID, "echo")}
	toolset := h.want(owner, "POST", "/api/v1/toolsets", map[string]any{
		"project_id": project, "name": "Reference toolset", "toolset": map[string]any{"tools": []agentcontract.Tool{tool}},
	}, idem("mcp-reference-toolset"), 201)
	toolsetID := mcpExecString(t, toolset, "id")
	h.want(owner, "POST", "/api/v1/toolsets/"+toolsetID+"/publish", nil, withMatch(toolset, idem("mcp-reference-publish")), 200)
	const keyCallsPerMinute = 10
	const keyMaxConcurrency = 2
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{
		"name": "Reference caller", "project_id": project, "scopes": []string{"tools"}, "allowed_toolsets": []string{toolsetID}, "requests_per_minute": keyCallsPerMinute, "max_concurrency": keyMaxConcurrency,
	}, idem("mcp-reference-key"), 201)
	secret := mcpExecString(t, key, "secret")
	valkey, prefix, _ := acctKeyspace(t)
	limiter, limiterErr := limits.New(valkey, strings.TrimSuffix(prefix, ":"))
	if limiterErr != nil {
		t.Fatal(limiterErr)
	}
	sessions, sessionStoreErr := mcp.NewSessionStore(valkey, strings.TrimSuffix(prefix, ":")+":mcp-reference")
	if sessionStoreErr != nil {
		t.Fatal(sessionStoreErr)
	}
	server := &mcp.Server{
		Runtime: h.Runtime, Egress: &h.Policy, Auth: h.AuthKey, Sessions: sessions, Limiter: limiter,
		Ledger: &mcp.DurableLedger{Pool: h.Pool, Store: &limits.DebitStore{Pool: h.Pool, Limiter: limiter}, Auth: h.AuthKey},
	}
	mux := http.NewServeMux()
	server.Register(mux)
	consumer := httptest.NewServer(mux)
	defer consumer.Close()
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &oauth2.Transport{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: secret})}}
	response, responseErr := client.Get(consumer.URL + "/v1/mcp/tools")
	if responseErr != nil {
		t.Fatal(responseErr)
	}
	defer response.Body.Close()
	var capabilities struct {
		Tools []struct {
			Name         string `json:"name"`
			UnitPrice    string `json:"unit_price"`
			PricingState string `json:"pricing_state"`
			Limits       struct {
				KeyCallsPerMinute int64 `json:"key_calls_per_minute"`
				KeyMaxConcurrency int64 `json:"key_max_concurrency"`
			} `json:"limits"`
		} `json:"tools"`
	}
	if err := json.NewDecoder(response.Body).Decode(&capabilities); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || len(capabilities.Tools) != 1 {
		t.Fatalf("reference tool was not discoverable: %d %+v", response.StatusCode, capabilities)
	}
	capability := capabilities.Tools[0]
	if capability.Name != tool.Alias || capability.UnitPrice != "0" || capability.PricingState != "free" || capability.Limits.KeyCallsPerMinute != keyCallsPerMinute || capability.Limits.KeyMaxConcurrency != keyMaxConcurrency {
		t.Fatalf("discovery disagreed with the explicit price or tighter key policy: %+v", capability)
	}
	sdkClient := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "reference-caller", Version: "1"}, nil)
	session, connectErr := sdkClient.Connect(t.Context(), &mcpsdk.StreamableClientTransport{
		Endpoint: consumer.URL + "/mcp?toolset_id=" + toolsetID, HTTPClient: client, MaxRetries: -1, DisableStandaloneSSE: true,
	}, &mcpsdk.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	if connectErr != nil {
		t.Fatal(connectErr)
	}
	defer session.Close()
	listed, listErr := session.ListTools(t.Context(), nil)
	if listErr != nil || len(listed.Tools) != 1 || listed.Tools[0].Name != tool.Alias {
		t.Fatalf("reference client could not list the published tool: %+v %v", listed, listErr)
	}
	result, callErr := session.CallTool(t.Context(), &mcpsdk.CallToolParams{Name: tool.Alias, Arguments: arguments{Query: "reference round trip"}, Meta: mcpsdk.Meta{"olp.call_id": "reference-call"}})
	if callErr != nil || result.IsError || len(result.Content) != 1 || dispatches.Load() != 1 {
		t.Fatalf("reference round trip failed: %+v %v", result, callErr)
	}
	text, isText := result.Content[0].(*mcpsdk.TextContent)
	if !isText || text.Text != "reference round trip" {
		t.Fatalf("reference result did not survive both protocol boundaries: %+v", result)
	}
	attemptID, hasAttemptID := result.Meta["olp.attempt_id"].(string)
	if !hasAttemptID {
		t.Fatalf("reference result omitted its attempt identity: %+v", result.Meta)
	}
	record := h.want(owner, "GET", "/api/v1/mcp-tool-calls/"+attemptID, nil, nil, 200)
	if record["status"] != "succeeded" || record["debit_state"] != "settled" || record["pricing_state"] != capability.PricingState || !mcpAmountIsZero(t, record, "observed_charge") {
		t.Fatalf("reference call and its discovered price did not settle consistently: %v", record)
	}
}

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
		Runtime: h.Runtime, Egress: &h.Policy, Auth: h.AuthKey, Sessions: sessionStore, Limiter: limiter,
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
	capability := mcpExecItem(t, tools, 0, listed)
	if capability["pricing_state"] != "unpriced" || capability["unit_price"] != nil {
		t.Fatalf("discovery did not distinguish an unpriced tool: %v", capability)
	}
	capabilityLimits := mcpExecObject(t, capability, "limits")
	if capabilityLimits["server_calls_per_minute"] != created["calls_per_minute"] || capabilityLimits["key_max_concurrency"] != created["per_key_max_concurrency"] {
		t.Fatalf("discovery limits disagree with the configured server: %v", capability)
	}
	simulatedStatus, simulated := call("POST", "/v1/mcp/tools/simulate", map[string]any{"toolset_id": toolsetID, "name": tool.Alias, "arguments": map[string]any{"query": argument}})
	if simulatedStatus != http.StatusOK || simulated["configuration_valid"] != true || calls.Load() != 0 || debitCount() != 0 {
		t.Fatalf("simulation did not describe the permitted call without executing it: %d %v", simulatedStatus, simulated)
	}
	invalidStatus, invalid := call("POST", "/v1/mcp/tools/simulate", map[string]any{"toolset_id": toolsetID, "name": tool.Alias, "arguments": map[string]any{"query": 3}})
	if invalidStatus != http.StatusUnprocessableEntity || mcpExecObject(t, invalid, "error")["code"] != "invalid_tool_arguments" || calls.Load() != 0 || debitCount() != 0 {
		t.Fatalf("simulation did not refuse the same invalid arguments as execution: %d %v", invalidStatus, invalid)
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
	otherProject := createProject(h, owner, "Other tool history")
	otherCalls := h.want(owner, "GET", "/api/v1/mcp-tool-calls?project_id="+otherProject, nil, nil, 200)
	if len(mcpExecItems(t, otherCalls, "items")) != 0 {
		t.Fatalf("project history filter returned another project's calls: %v", otherCalls)
	}
	h.want(owner, "GET", "/api/v1/mcp-tool-calls?project_id=invalid", nil, nil, 422)
	viewer := h.invite(owner, "tool-history@example.com", "viewer")
	viewerSession := h.want(viewer, "GET", "/api/v1/sessions/current", nil, nil, 200)
	viewerID := mcpExecString(t, mcpExecObject(t, viewerSession, "user"), "id")
	viewerRecord := h.want(owner, "GET", "/api/v1/users/"+viewerID, nil, nil, 200)
	h.want(owner, "PATCH", "/api/v1/users/"+viewerID, map[string]any{"access_scope": "assigned"}, etagHeader(viewerRecord), 200)
	addMember(h, owner, otherProject, viewerID, "viewer")
	assignedViewer := login(h, "tool-history@example.com")
	h.want(assignedViewer, "GET", "/api/v1/mcp-tool-calls?project_id="+project, nil, nil, 404)
	h.want(assignedViewer, "GET", "/api/v1/mcp-tool-calls/"+mcpExecString(t, called, "attempt_id"), nil, nil, 404)
	filteredCalls := h.want(assignedViewer, "GET", "/api/v1/mcp-tool-calls", nil, nil, 200)
	if len(mcpExecItems(t, filteredCalls, "items")) != 0 {
		t.Fatalf("project viewer could read another project's attempts: %v", filteredCalls)
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
	retained := h.want(owner, "GET", "/api/v1/mcp-tool-calls/"+mcpExecString(t, called, "attempt_id"), nil, nil, 200)
	if retained["dispatch_id"] != detail["dispatch_id"] || retained["status"] != detail["status"] || retained["observed_charge"] != detail["observed_charge"] {
		t.Fatalf("disabling execution changed the historical settlement: %v", retained)
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
		Runtime: h.Runtime, Egress: &h.Policy, Auth: h.AuthKey, Sessions: sessionStore, Limiter: limiter,
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

func mcpAmount(t *testing.T, body map[string]any, key string) *big.Rat {
	t.Helper()
	value, ok := new(big.Rat).SetString(mcpExecString(t, body, key))
	if !ok {
		t.Fatalf("response field %q is not an amount: %v", key, body)
	}
	return value
}

func mcpAmountIsZero(t *testing.T, body map[string]any, key string) bool {
	t.Helper()
	return mcpAmount(t, body, key).Sign() == 0
}

func mcpAmountsEqual(t *testing.T, body map[string]any, left, right string) bool {
	t.Helper()
	return mcpAmount(t, body, left).Cmp(mcpAmount(t, body, right)) == 0
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
	var blockChans atomic.Value
	resetBlock := func() {
		blockChans.Store([2]chan struct{}{make(chan struct{}, 1), make(chan struct{})})
	}
	blockGate := func() [2]chan struct{} { return blockChans.Load().([2]chan struct{}) }
	resetBlock()
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
				gate := blockGate()
				select {
				case gate[0] <- struct{}{}:
				default:
				}
				<-gate[1]
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
			Runtime: h.Runtime, Egress: &h.Policy, Auth: h.AuthKey, Sessions: sessionStore, Limiter: limiter,
			Ledger: &mcp.DurableLedger{Pool: h.Pool, Store: &limits.DebitStore{Pool: h.Pool, Limiter: limiter}, Auth: h.AuthKey},
			Guardrails: func(ctx context.Context, evaluation mcp.GuardrailEvaluation) (guardrails.Result, error) {
				return evaluation.Engine.Evaluate(ctx, guardrails.Evaluation{Phase: evaluation.Phase, Segments: evaluation.Segments}, evaluation.Masks)
			},
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
	if resolved["status"] != "not_accepted" || resolved["debit_state"] != "not_accepted" || !mcpAmountIsZero(t, resolved, "remaining_cost") {
		t.Fatalf("evidence-backed finding did not close the held debit: %v", resolved)
	}
	repeatedResolve := h.want(owner, "POST", "/api/v1/mcp-tool-calls/"+unknownAttempt+"/resolve", map[string]any{
		"outcome": "not_accepted", "evidence_digest": evidenceDigest("upstream-log-shows-no-acceptance"),
	}, idem("mcp-identity-resolve-unknown-retry"), 200)
	if repeatedResolve["id"] != resolved["id"] || repeatedResolve["status"] != "not_accepted" {
		t.Fatalf("repeated resolve with the same evidence did not return the existing record: %v", repeatedResolve)
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
	if status != http.StatusConflict || calls.Load() != 2 {
		t.Fatalf("resolved call id redispatched inside its binding: status=%d calls=%d body=%v", status, calls.Load(), afterResolve)
	}
	if code := mcpExecString(t, mcpExecObject(t, afterResolve, "error"), "code"); code != "result_not_retained" || afterResolve["status"] != "not_accepted" || mcpExecString(t, afterResolve, "attempt_id") != unknownAttempt {
		t.Fatalf("repeat after resolve still answered unknown: %v", afterResolve)
	}

	drop.Store(true)
	status, unknownTwo := callOn(restarted, "integration-caller-unknown-2", argument)
	if status != http.StatusBadGateway || calls.Load() != 3 {
		t.Fatalf("second ambiguous dispatch was not held unknown: status=%d calls=%d body=%v", status, calls.Load(), unknownTwo)
	}
	drop.Store(false)
	unknownTwoAttempt := mcpExecString(t, unknownTwo, "attempt_id")
	unknownTwoDispatch := mcpExecString(t, unknownTwo, "dispatch_id")
	h.want(owner, "POST", "/api/v1/mcp-tool-calls/"+unknownTwoAttempt+"/resolve", map[string]any{
		"outcome": "settled", "evidence_digest": evidenceDigest("upstream-log-shows-completion"), "observed_cost": "1.5",
	}, idem("mcp-identity-settle-nonzero"), 422)
	settled := h.want(owner, "POST", "/api/v1/mcp-tool-calls/"+unknownTwoAttempt+"/resolve", map[string]any{
		"outcome": "settled", "evidence_digest": evidenceDigest("upstream-log-shows-completion"), "observed_cost": "0", "observed_charge": "0",
	}, idem("mcp-identity-settle-unknown"), 200)
	if settled["status"] != "settled" || settled["debit_state"] != "settled" {
		t.Fatalf("later settlement did not close the held debit: %v", settled)
	}
	repeatedSettle := h.want(owner, "POST", "/api/v1/mcp-tool-calls/"+unknownTwoAttempt+"/resolve", map[string]any{
		"outcome": "settled", "evidence_digest": evidenceDigest("upstream-log-shows-completion"), "observed_cost": "0", "observed_charge": "0",
	}, idem("mcp-identity-settle-unknown-retry"), 200)
	if repeatedSettle["id"] != settled["id"] {
		t.Fatalf("repeated settle with the same evidence did not return the existing record: %v", repeatedSettle)
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
	rawCallWith := func(caller string) concurrentResult {
		data, _ := json.Marshal(map[string]any{"toolset_id": toolsetID, "name": tool.Alias, "call_id": caller, "arguments": map[string]any{"query": argument}})
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
	go func() { firstCall <- rawCallWith("integration-caller-concurrent") }()
	<-blockGate()[0]
	status, concurrent := callOn(restarted, "integration-caller-concurrent", argument)
	if status != http.StatusConflict || concurrent["status"] != "in_progress" || calls.Load() != 4 {
		t.Fatalf("concurrent repeat dispatched beside the in-flight call: status=%d calls=%d body=%v", status, calls.Load(), concurrent)
	}
	if code := mcpExecString(t, mcpExecObject(t, concurrent, "error"), "code"); code != "call_in_progress" {
		t.Fatalf("concurrent repeat did not report the in-flight call: %v", concurrent)
	}
	concurrentAttempt := mcpExecString(t, concurrent, "attempt_id")
	inFlight := h.want(owner, "GET", "/api/v1/mcp-tool-calls/"+concurrentAttempt, nil, nil, 200)
	if inFlight["status"] != "dispatched" || inFlight["debit_state"] != "dispatched" || inFlight["result_digest"] != nil || inFlight["completed_at"] != nil || inFlight["call_digest"] == nil {
		t.Fatalf("dispatched call had no management row before its outcome: %v", inFlight)
	}
	close(blockGate()[1])
	block.Store(false)
	finished := <-firstCall
	if finished.err != nil || finished.status != http.StatusOK || mcpExecString(t, finished.body, "attempt_id") != concurrentAttempt {
		t.Fatalf("in-flight call did not finish as the attempt its repeat retained: %+v", finished)
	}
	status, afterConcurrent := callOn(restarted, "integration-caller-concurrent", argument)
	if status != http.StatusOK || calls.Load() != 4 || mcpExecString(t, afterConcurrent, "attempt_id") != concurrentAttempt {
		t.Fatalf("settled concurrent call re-executed on repeat: status=%d calls=%d body=%v", status, calls.Load(), afterConcurrent)
	}

	resetBlock()
	block.Store(true)
	crashedCall := make(chan concurrentResult, 1)
	go func() { crashedCall <- rawCallWith("integration-caller-crashed") }()
	<-blockGate()[0]
	if calls.Load() != 5 {
		t.Fatalf("crash window call did not dispatch once: calls=%d", calls.Load())
	}
	toolLedger := &mcp.DurableLedger{Pool: h.Pool}
	recovered, err := toolLedger.RecoverOrphanedDispatches(t.Context(), time.Now().Add(time.Hour), 256)
	if err != nil || recovered < 1 {
		t.Fatalf("restart recovery left the orphaned dispatch behind: recovered=%d err=%v", recovered, err)
	}
	status, orphaned := callOn(newConsumer(), "integration-caller-crashed", argument)
	if status != http.StatusBadGateway || orphaned["status"] != "outcome_unknown" || calls.Load() != 5 {
		t.Fatalf("orphaned dispatch redispatched after restart: status=%d calls=%d body=%v", status, calls.Load(), orphaned)
	}
	orphanRecord := h.want(owner, "GET", "/api/v1/mcp-tool-calls/"+mcpExecString(t, orphaned, "attempt_id"), nil, nil, 200)
	if orphanRecord["status"] != "outcome_unknown" || orphanRecord["debit_state"] != "outcome_unknown" || orphanRecord["call_digest"] == nil {
		t.Fatalf("orphaned dispatch is unreachable in management records: %v", orphanRecord)
	}
	close(blockGate()[1])
	block.Store(false)
	if drained := <-crashedCall; drained.err != nil {
		t.Fatalf("blocked crash window call could not drain: %v", drained.err)
	}
	if _, err = h.Pool.Exec(t.Context(), `UPDATE olp.mcp_call_identities SET created_at=clock_timestamp()-interval '25 hours',expires_at=clock_timestamp()-interval '1 hour' WHERE attempt_id=$1`, attemptID); err != nil {
		t.Fatal(err)
	}
	status, reused := callOn(restarted, callerID, argument)
	if status != http.StatusOK || calls.Load() != 6 || mcpExecString(t, reused, "attempt_id") == attemptID {
		t.Fatalf("expired call identity stayed bound past 24 hours: status=%d calls=%d body=%v", status, calls.Load(), reused)
	}
	oldRecord := h.want(owner, "GET", "/api/v1/mcp-tool-calls/"+attemptID, nil, nil, 200)
	newRecord := h.want(owner, "GET", "/api/v1/mcp-tool-calls/"+mcpExecString(t, reused, "attempt_id"), nil, nil, 200)
	if oldRecord["call_digest"] == nil || oldRecord["call_digest"] != newRecord["call_digest"] {
		t.Fatalf("reusing an expired identifier stripped the digest from the old record: old=%v new=%v", oldRecord, newRecord)
	}
	if _, err = h.Pool.Exec(t.Context(), `UPDATE olp.mcp_call_identities SET created_at=clock_timestamp()-interval '25 hours',expires_at=clock_timestamp()-interval '1 hour' WHERE attempt_id=$1`, mcpExecString(t, reused, "attempt_id")); err != nil {
		t.Fatal(err)
	}
	expired, err := toolLedger.ExpireCallIdentities(t.Context(), 256)
	if err != nil || expired < 1 {
		t.Fatalf("identity expiry sweep kept expired bindings: expired=%d err=%v", expired, err)
	}
	var bindings int
	if err = h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.mcp_call_identities WHERE attempt_id=$1`, mcpExecString(t, reused, "attempt_id")).Scan(&bindings); err != nil || bindings != 0 {
		t.Fatalf("swept call identity remained bound: count=%d err=%v", bindings, err)
	}
	status, swept := callOn(restarted, callerID, argument)
	if status != http.StatusOK || calls.Load() != 7 {
		t.Fatalf("swept call identity could not be reused: status=%d calls=%d body=%v", status, calls.Load(), swept)
	}

	const refusedArgument = "blocked-argument"
	definition := h.want(owner, "POST", "/api/v1/guardrails", map[string]any{
		"name": "Tool refusal", "project_id": project, "type": "builtin.keywords",
		"configuration": map[string]any{"words": []string{refusedArgument, answer}, "substrings": true}, "timeout_ms": 1000, "on_error": "fail_closed",
	}, idem("mcp-identity-guardrail"), 201)
	h.want(owner, "POST", "/api/v1/guardrails/"+definition["id"].(string)+"/activate", map[string]any{"revision_id": definition["revision_id"]}, withMatch(definition, idem("mcp-identity-guardrail-activate")), 204)
	policy := h.want(owner, "POST", "/api/v1/guardrail-policies", map[string]any{
		"name": "Tool refusals", "project_id": project, "entries": []any{
			map[string]any{"guardrail_id": definition["id"], "revision_id": definition["revision_id"], "phase": "input", "action": "block"},
			map[string]any{"guardrail_id": definition["id"], "revision_id": definition["revision_id"], "phase": "output", "action": "block"},
		},
	}, idem("mcp-identity-policy"), 201)
	h.want(owner, "POST", "/api/v1/guardrail-policies/"+policy["id"].(string)+"/activate", map[string]any{"revision_id": policy["revision_id"]}, withMatch(policy, idem("mcp-identity-policy-activate")), 204)
	attachment := h.want(owner, "POST", "/api/v1/guardrail-attachments", map[string]any{
		"scope": "toolset", "scope_id": toolsetID, "policy_revision_id": policy["revision_id"], "order": 0,
	}, idem("mcp-identity-attach"), 201)
	if err = h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	beforeRefusals := calls.Load()
	status, refused := callOn(restarted, "identity-argument-refusal", refusedArgument)
	if status != http.StatusBadRequest || refused["status"] != "guardrail_blocked" || calls.Load() != beforeRefusals {
		t.Fatalf("refused argument dispatched: status=%d body=%v", status, refused)
	}
	refusedID := mcpExecString(t, refused, "attempt_id")
	refusedRecord := h.want(owner, "GET", "/api/v1/mcp-tool-calls/"+refusedID, nil, nil, 200)
	if refusedRecord["status"] != "guardrail_blocked" || refusedRecord["debit_state"] != "released" {
		t.Fatalf("argument refusal did not release its debit: %v", refusedRecord)
	}
	status, outputRefused := callOn(restarted, "identity-result-refusal", "safe")
	if status != http.StatusBadRequest || outputRefused["status"] != "guardrail_blocked" || calls.Load() != beforeRefusals+1 {
		t.Fatalf("result refusal was not retained: status=%d body=%v", status, outputRefused)
	}
	outputRecord := h.want(owner, "GET", "/api/v1/mcp-tool-calls/"+mcpExecString(t, outputRefused, "attempt_id"), nil, nil, 200)
	if outputRecord["status"] != "guardrail_blocked" || outputRecord["debit_state"] != "settled" {
		t.Fatalf("result refusal did not settle its debit: %v", outputRecord)
	}
	attachments := h.want(owner, "GET", "/api/v1/guardrail-attachments", nil, nil, 200)
	attachmentState := mcpExecItem(t, mcpExecItems(t, attachments, "items"), 0, attachments)
	if attachmentState["id"] != attachment["id"] {
		t.Fatalf("refusal policy attachment was not listed: %v", attachments)
	}
	h.want(owner, "DELETE", "/api/v1/guardrail-attachments/"+mcpExecString(t, attachment, "id"), nil, withMatch(attachmentState, idem("mcp-identity-detach")), 204)
	if err = h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	refusalRestart := newConsumer()
	status, refusedRepeat := callOn(refusalRestart, "identity-argument-refusal", refusedArgument)
	if status != http.StatusBadRequest || refusedRepeat["attempt_id"] != refused["attempt_id"] || refusedRepeat["dispatch_id"] != refused["dispatch_id"] {
		t.Fatalf("argument refusal lost its identity after restart and policy removal: %d %v", status, refusedRepeat)
	}
	status, refusedConflict := callOn(refusalRestart, "identity-argument-refusal", "safe")
	if status != http.StatusConflict || mcpExecObject(t, refusedConflict, "error")["code"] != "call_id_conflict" {
		t.Fatalf("refused identifier accepted different input: %d %v", status, refusedConflict)
	}
	mcpPost := func(path, session string, body map[string]any) (http.Header, map[string]any) {
		t.Helper()
		data, _ := json.Marshal(body)
		req, requestErr := http.NewRequestWithContext(t.Context(), http.MethodPost, refusalRestart.URL+path, bytes.NewReader(data))
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
			req.Header.Set("MCP-Protocol-Version", "2025-11-25")
		}
		response, requestErr := http.DefaultClient.Do(req)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		defer response.Body.Close()
		if response.StatusCode == http.StatusAccepted {
			return response.Header, nil
		}
		var result map[string]any
		if decodeErr := json.NewDecoder(response.Body).Decode(&result); decodeErr != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("MCP request failed: status=%d body=%v error=%v", response.StatusCode, result, decodeErr)
		}
		return response.Header, result
	}
	headers, _ := mcpPost("/mcp?toolset_id="+toolsetID, "", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
			"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "integration", "version": "1"},
		},
	})
	refusalSession := headers.Get("Mcp-Session-Id")
	if refusalSession == "" {
		t.Fatal("refusal session has no identity")
	}
	mcpPost("/mcp", refusalSession, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	_, sessionRepeat := mcpPost("/mcp", refusalSession, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{
			"name": tool.Alias, "arguments": map[string]any{"query": refusedArgument}, "_meta": map[string]any{"olp.call_id": "identity-argument-refusal"},
		},
	})
	sessionRefusal := mcpExecObject(t, mcpExecObject(t, sessionRepeat, "error"), "data")
	if sessionRefusal["code"] != "guardrail_blocked" || sessionRefusal["attempt_id"] != refused["attempt_id"] || sessionRefusal["dispatch_id"] != refused["dispatch_id"] {
		t.Fatalf("MCP did not retain the JSON refusal: %v", sessionRepeat)
	}
	_, sessionConflict := mcpPost("/mcp", refusalSession, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{
			"name": tool.Alias, "arguments": json.RawMessage(`{"query":"blocked-\u0061rgument"}`), "_meta": map[string]any{"olp.call_id": "identity-argument-refusal"},
		},
	})
	if mcpExecObject(t, mcpExecObject(t, sessionConflict, "error"), "data")["code"] != "call_id_conflict" || calls.Load() != beforeRefusals+1 {
		t.Fatalf("MCP did not preserve exact argument bytes for the refused identifier: %v", sessionConflict)
	}
}

func TestMCPToolCallPricingLimitsAndOwnersEndToEnd(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "MCP pricing")
	const credential = "integration-pricing-credential"
	const serverPrice = "0.10"
	const toolPrice = "0.25"
	var calls atomic.Int64
	var mode atomic.Value
	mode.Store("ok")
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
			w.Header().Set("Mcp-Session-Id", "pricing-session")
			result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{
				"name": "lookup", "description": "Look up",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []string{"query"}, "additionalProperties": false},
			}}}
		case "tools/call":
			calls.Add(1)
			switch mode.Load() {
			case "drop":
				w.WriteHeader(http.StatusInternalServerError)
				return
			case "error":
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "upstream failed"}}, "isError": true}
			default:
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "priced answer"}}, "isError": false}
			}
		default:
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	defer upstream.Close()

	openProject := createProject(h, owner, "MCP pricing open")
	register := func(projectID, name string, pricing map[string]any) (map[string]any, agentcontract.Tool) {
		t.Helper()
		body := map[string]any{
			"name": name, "project_id": projectID, "transport": "streamable_http",
			"endpoint": upstream.URL, "enabled": true, "credential": credential,
		}
		for key, value := range pricing {
			body[key] = value
		}
		created := h.want(owner, "POST", "/api/v1/mcp-servers", body, idem("mcp-pricing-"+name), 201)
		serverID := mcpExecString(t, created, "id")
		tool := agentcontract.Tool{ServerID: serverID, RevisionID: mcpExecString(t, created, "revision_id"), CatalogDigest: mcpExecString(t, mcpExecObject(t, created, "catalog"), "digest"), Name: "lookup", Alias: agentcontract.ToolAlias(serverID, "lookup")}
		return created, tool
	}
	pricedServer, pricedTool := register(project, "Priced server", map[string]any{
		"unit_price": serverPrice, "tool_prices": map[string]any{"lookup": toolPrice},
		"calls_per_minute": 120, "max_concurrency": 4, "per_key_calls_per_minute": 30, "per_key_max_concurrency": 2,
	})
	if pricedServer["unit_price"] != serverPrice || pricedServer["calls_per_minute"] != float64(120) || pricedServer["per_key_max_concurrency"] != float64(2) {
		t.Fatalf("management create did not round trip pricing: %v", pricedServer)
	}
	if prices, _ := pricedServer["tool_prices"].(map[string]any); prices["lookup"] != toolPrice {
		t.Fatalf("management create lost the tool price: %v", pricedServer)
	}
	_, unpricedTool := register(project, "Unpriced server", nil)
	_, openUnpricedTool := register(openProject, "Open unpriced server", nil)
	_, limitedTool := register(openProject, "Limited server", map[string]any{"unit_price": "0", "per_key_calls_per_minute": 1})
	publish := func(projectID, name string, tool agentcontract.Tool) string {
		t.Helper()
		toolset := h.want(owner, "POST", "/api/v1/toolsets", map[string]any{
			"project_id": projectID, "name": name,
			"toolset": map[string]any{"tools": []agentcontract.Tool{tool}},
		}, idem("mcp-pricing-toolset-"+name), 201)
		id := mcpExecString(t, toolset, "id")
		h.want(owner, "POST", "/api/v1/toolsets/"+id+"/publish", nil, withMatch(toolset, idem("mcp-pricing-publish-"+name)), 200)
		return id
	}
	pricedToolset := publish(project, "Priced toolset", pricedTool)
	unpricedToolset := publish(project, "Unpriced toolset", unpricedTool)
	openUnpricedToolset := publish(openProject, "Open unpriced toolset", openUnpricedTool)
	limitedToolset := publish(openProject, "Limited toolset", limitedTool)

	budgetDetail := h.want(owner, "GET", "/api/v1/projects/"+project+"/budget", nil, nil, 200)
	h.want(owner, "PUT", "/api/v1/projects/"+project+"/budget", map[string]any{"policy": map[string]any{"daily_cost_limit": "100"}}, etagHeader(budgetDetail), 200)
	group := h.want(owner, "POST", "/api/v1/budget-groups", map[string]any{
		"name": "tool pricing group", "project_id": project, "monthly_cost_limit": "100",
	}, idem("mcp-pricing-group"), 201)
	cappedKey := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{
		"name": "Capped tool caller", "project_id": project, "scopes": []string{"tools"},
		"allowed_toolsets": []string{pricedToolset, unpricedToolset}, "daily_cost_limit": "100", "budget_group_id": mcpExecString(t, group, "id"),
	}, idem("mcp-pricing-capped-key"), 201)
	cappedSecret := mcpExecString(t, cappedKey, "secret")
	openKey := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{
		"name": "Open tool caller", "project_id": openProject, "scopes": []string{"tools"},
		"allowed_toolsets": []string{openUnpricedToolset, limitedToolset},
	}, idem("mcp-pricing-open-key"), 201)
	openSecret := mcpExecString(t, openKey, "secret")

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
	sessionStore, err := mcp.NewSessionStore(valkey, strings.TrimSuffix(prefix, ":")+":mcp-pricing")
	if err != nil {
		t.Fatal(err)
	}
	toolServer := &mcp.Server{
		Runtime: h.Runtime, Egress: &h.Policy, Auth: h.AuthKey, Sessions: sessionStore, Limiter: limiter,
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
	windows := limits.BudgetWindows(time.Now())
	for kind, id := range map[string]int64{"day": windows.DailyID, "week": windows.WeeklyID, "month": windows.MonthlyID} {
		for _, seed := range []struct {
			query string
			owner string
		}{
			{`INSERT INTO olp.api_key_cost_windows(api_key_id,window_kind,window_id,accrued,unpriced_attempts,weekly_complete) VALUES($1,$2,$3,0,0,true) ON CONFLICT DO NOTHING`, mcpExecString(t, cappedKey, "id")},
			{`INSERT INTO olp.budget_group_cost_windows(budget_group_id,window_kind,window_id,accrued,unpriced_attempts,weekly_complete) VALUES($1,$2,$3,0,0,true) ON CONFLICT DO NOTHING`, mcpExecString(t, group, "id")},
			{`INSERT INTO olp.aggregate_cost_windows(account_id,window_kind,window_id,accrued,unpriced_attempts,weekly_complete) VALUES($1,$2,$3,0,0,true) ON CONFLICT DO NOTHING`, limits.AggregateBudgetID("project", project)},
		} {
			if _, err := h.Pool.Exec(t.Context(), seed.query, seed.owner, kind, id); err != nil {
				t.Fatalf("seed %s cost window for %s: %v", kind, seed.owner, err)
			}
		}
	}
	call := func(secret, toolsetID string, tool agentcontract.Tool, callID string) (int, map[string]any) {
		t.Helper()
		data, _ := json.Marshal(map[string]any{"toolset_id": toolsetID, "name": tool.Alias, "call_id": callID, "arguments": map[string]any{"query": "pricing probe"}})
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
	for _, scenario := range []struct {
		name      string
		secret    string
		toolsetID string
		tool      agentcontract.Tool
		status    int
	}{
		{name: "priced", secret: cappedSecret, toolsetID: pricedToolset, tool: pricedTool, status: http.StatusOK},
		{name: "unpriced budgeted", secret: cappedSecret, toolsetID: unpricedToolset, tool: unpricedTool, status: http.StatusUnprocessableEntity},
		{name: "unpriced unbudgeted", secret: openSecret, toolsetID: openUnpricedToolset, tool: openUnpricedTool, status: http.StatusOK},
	} {
		t.Run("simulate "+scenario.name, func(t *testing.T) {
			data, err := json.Marshal(map[string]any{"toolset_id": scenario.toolsetID, "name": scenario.tool.Alias, "arguments": map[string]any{"query": "pricing simulation"}})
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: &oauth2.Transport{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: scenario.secret})}}
			response, err := client.Post(consumer.URL+"/v1/mcp/tools/simulate", "application/json", bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var result map[string]any
			if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != scenario.status {
				t.Fatalf("simulation disagreed with the configured price and budget: %d %v", response.StatusCode, result)
			}
			if scenario.status == http.StatusOK && result["configuration_valid"] != true {
				t.Fatalf("permitted configuration was not reported: %v", result)
			}
			if scenario.status != http.StatusOK && mcpExecObject(t, result, "error")["code"] != "tool_unpriced" {
				t.Fatalf("simulation did not explain its price refusal: %v", result)
			}
		})
	}
	var simulatedAttempts int
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.mcp_tool_calls").Scan(&simulatedAttempts); err != nil || simulatedAttempts != 0 || calls.Load() != 0 {
		t.Fatalf("simulation created execution or accounting effects: attempts=%d calls=%d error=%v", simulatedAttempts, calls.Load(), err)
	}
	var pricingCurrency string
	if err = h.Pool.QueryRow(t.Context(), `SELECT COALESCE((SELECT btrim(currency) FROM olp.pricing_currency WHERE singleton),'USD')`).Scan(&pricingCurrency); err != nil {
		t.Fatal(err)
	}

	status, called := call(cappedSecret, pricedToolset, pricedTool, "pricing-priced-1")
	if status != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("priced call did not execute: status=%d calls=%d body=%v", status, calls.Load(), called)
	}
	attemptID := mcpExecString(t, called, "attempt_id")
	dispatchID := mcpExecString(t, called, "dispatch_id")
	record := h.want(owner, "GET", "/api/v1/mcp-tool-calls/"+attemptID, nil, nil, 200)
	price, _ := new(big.Rat).SetString(toolPrice)
	if record["pricing_state"] != "priced" || record["currency"] != pricingCurrency || record["debit_state"] != "settled" ||
		mcpAmount(t, record, "unit_price").Cmp(price) != 0 ||
		!mcpAmountsEqual(t, record, "cost_bound", "unit_price") || !mcpAmountsEqual(t, record, "charge_bound", "unit_price") ||
		!mcpAmountsEqual(t, record, "observed_cost", "cost_bound") || !mcpAmountsEqual(t, record, "observed_charge", "charge_bound") ||
		!mcpAmountIsZero(t, record, "remaining_cost") || !mcpAmountIsZero(t, record, "remaining_charge") {
		t.Fatalf("priced call did not settle at the pinned tool price: %v", record)
	}
	rows, err := h.Pool.Query(t.Context(), `SELECT owner_kind,bound::text FROM olp.execution_debit_owners WHERE dispatch_id=$1`, dispatchID)
	if err != nil {
		t.Fatal(err)
	}
	ownerKinds := map[string]bool{}
	for rows.Next() {
		var kind, bound string
		if err = rows.Scan(&kind, &bound); err != nil {
			t.Fatal(err)
		}
		ownerKinds[kind] = true
		parsed, _ := new(big.Rat).SetString(bound)
		if parsed.Cmp(price) != 0 {
			t.Fatalf("%s owner reserved bound %q instead of the pinned price", kind, bound)
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"key", "group", "aggregate"} {
		if !ownerKinds[kind] {
			t.Fatalf("priced debit recorded no %s owner: %v", kind, ownerKinds)
		}
	}

	mode.Store("error")
	status, failed := call(cappedSecret, pricedToolset, pricedTool, "pricing-priced-2")
	if status != http.StatusOK || failed["is_error"] != true || calls.Load() != 2 {
		t.Fatalf("upstream failure did not return as a tool error: status=%d calls=%d body=%v", status, calls.Load(), failed)
	}
	failureRecord := h.want(owner, "GET", "/api/v1/mcp-tool-calls/"+mcpExecString(t, failed, "attempt_id"), nil, nil, 200)
	if failureRecord["status"] != "upstream_error" || failureRecord["debit_state"] != "settled" ||
		!mcpAmountsEqual(t, failureRecord, "observed_cost", "cost_bound") || mcpAmount(t, failureRecord, "observed_cost").Cmp(price) != 0 {
		t.Fatalf("upstream failure did not settle at the pinned price: %v", failureRecord)
	}
	mode.Store("ok")

	status, refused := call(cappedSecret, unpricedToolset, unpricedTool, "pricing-unpriced-capped")
	if status != http.StatusUnprocessableEntity || calls.Load() != 2 {
		t.Fatalf("unpriced call under a ceiling dispatched: status=%d calls=%d body=%v", status, calls.Load(), refused)
	}
	if code := mcpExecString(t, mcpExecObject(t, refused, "error"), "code"); code != "tool_unpriced" {
		t.Fatalf("unpriced refusal is not typed: %v", refused)
	}
	var refusedRecords int
	var refusedContentFree bool
	if err = h.Pool.QueryRow(t.Context(), `SELECT count(*),COALESCE(bool_and(result_digest IS NULL AND pricing_state='unpriced' AND unit_price IS NULL),false) FROM olp.mcp_tool_calls WHERE api_key_id=$1 AND status='refused_unpriced'`, mcpExecString(t, cappedKey, "id")).Scan(&refusedRecords, &refusedContentFree); err != nil {
		t.Fatal(err)
	}
	if refusedRecords != 1 || !refusedContentFree {
		t.Fatalf("capped missing-price refusal wrote no content-free refused record: count=%d free=%v", refusedRecords, refusedContentFree)
	}
	status, openUnpriced := call(openSecret, openUnpricedToolset, openUnpricedTool, "pricing-unpriced-open")
	if status != http.StatusOK || calls.Load() != 3 {
		t.Fatalf("unpriced call without a key ceiling did not execute: status=%d calls=%d body=%v", status, calls.Load(), openUnpriced)
	}
	unpricedRecord := h.want(owner, "GET", "/api/v1/mcp-tool-calls/"+mcpExecString(t, openUnpriced, "attempt_id"), nil, nil, 200)
	if unpricedRecord["pricing_state"] != "unpriced" || unpricedRecord["unit_price"] != nil || unpricedRecord["debit_state"] != "settled" ||
		!mcpAmountIsZero(t, unpricedRecord, "cost_bound") || !mcpAmountIsZero(t, unpricedRecord, "observed_cost") {
		t.Fatalf("missing price was not recorded as an unpriced fact: %v", unpricedRecord)
	}
	var unpricedAttempts int64
	if err = h.Pool.QueryRow(t.Context(), `SELECT COALESCE(unpriced_attempts,0) FROM olp.api_key_cost_windows WHERE api_key_id=$1 AND window_kind='month'`, mcpExecString(t, openKey, "id")).Scan(&unpricedAttempts); err != nil {
		t.Fatal(err)
	}
	if unpricedAttempts != 1 {
		t.Fatalf("unpriced dispatched call read as free in the spend view: unpriced_attempts=%d", unpricedAttempts)
	}

	status, free := call(openSecret, limitedToolset, limitedTool, "pricing-free-1")
	if status != http.StatusOK || calls.Load() != 4 {
		t.Fatalf("free call did not execute: status=%d calls=%d body=%v", status, calls.Load(), free)
	}
	freeRecord := h.want(owner, "GET", "/api/v1/mcp-tool-calls/"+mcpExecString(t, free, "attempt_id"), nil, nil, 200)
	if freeRecord["pricing_state"] != "free" || freeRecord["unit_price"] == nil || !mcpAmountIsZero(t, freeRecord, "unit_price") || freeRecord["debit_state"] != "settled" {
		t.Fatalf("explicit zero was not recorded as known free: %v", freeRecord)
	}
	status, limited := call(openSecret, limitedToolset, limitedTool, "pricing-free-2")
	if status != http.StatusTooManyRequests || calls.Load() != 4 {
		t.Fatalf("per-key rate limit dispatched: status=%d calls=%d body=%v", status, calls.Load(), limited)
	}
	if code := mcpExecString(t, mcpExecObject(t, limited, "error"), "code"); code != "tool_rate_limited" {
		t.Fatalf("rate refusal is not typed: %v", limited)
	}

	mode.Store("drop")
	status, unknown := call(cappedSecret, pricedToolset, pricedTool, "pricing-unknown-1")
	if status != http.StatusBadGateway || unknown["status"] != "outcome_unknown" || calls.Load() != 5 {
		t.Fatalf("ambiguous priced dispatch was not held unknown: status=%d calls=%d body=%v", status, calls.Load(), unknown)
	}
	mode.Store("ok")
	unknownAttempt := mcpExecString(t, unknown, "attempt_id")
	held := h.want(owner, "GET", "/api/v1/mcp-tool-calls/"+unknownAttempt, nil, nil, 200)
	if held["pricing_state"] != "priced" || mcpAmount(t, held, "cost_bound").Cmp(price) != 0 ||
		!mcpAmountIsZero(t, held, "observed_cost") || !mcpAmountsEqual(t, held, "remaining_cost", "cost_bound") ||
		mcpAmount(t, held, "remaining_cost").Sign() == 0 {
		t.Fatalf("held report folded bound, observed and remaining together: %v", held)
	}
	evidence := func(seed string) string {
		sum := sha256.Sum256([]byte(seed))
		return hex.EncodeToString(sum[:])
	}
	over := new(big.Rat).Add(price, big.NewRat(1, 100)).FloatString(2)
	h.want(owner, "POST", "/api/v1/mcp-tool-calls/"+unknownAttempt+"/resolve", map[string]any{
		"outcome": "settled", "evidence_digest": evidence("priced-upstream-completion"), "observed_cost": over, "observed_charge": toolPrice,
	}, idem("mcp-pricing-resolve-over"), 422)
	resolved := h.want(owner, "POST", "/api/v1/mcp-tool-calls/"+unknownAttempt+"/resolve", map[string]any{
		"outcome": "settled", "evidence_digest": evidence("priced-upstream-completion"), "observed_cost": toolPrice, "observed_charge": toolPrice,
	}, idem("mcp-pricing-resolve"), 200)
	if resolved["status"] != "settled" || resolved["debit_state"] != "settled" ||
		!mcpAmountsEqual(t, resolved, "observed_cost", "cost_bound") || !mcpAmountIsZero(t, resolved, "remaining_cost") {
		t.Fatalf("observed evidence did not settle against the real bound: %v", resolved)
	}
	repeat := h.want(owner, "POST", "/api/v1/mcp-tool-calls/"+unknownAttempt+"/resolve", map[string]any{
		"outcome": "settled", "evidence_digest": evidence("priced-upstream-completion"), "observed_cost": toolPrice, "observed_charge": toolPrice,
	}, idem("mcp-pricing-resolve-retry"), 200)
	if repeat["id"] != resolved["id"] {
		t.Fatalf("repeated resolve did not return the existing record: %v", repeat)
	}
	h.want(owner, "POST", "/api/v1/mcp-tool-calls/"+unknownAttempt+"/resolve", map[string]any{
		"outcome": "settled", "evidence_digest": evidence("priced-upstream-completion"), "observed_cost": "0", "observed_charge": "0",
	}, idem("mcp-pricing-resolve-changed-amounts"), 409)

	exported := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)["document"].(map[string]any)
	foundPricing := false
	for _, item := range exported["mcp_servers"].([]any) {
		entry := item.(map[string]any)
		if entry["name"] == "Priced server" {
			prices, _ := entry["tool_prices"].(map[string]any)
			foundPricing = entry["unit_price"] == serverPrice && prices["lookup"] == toolPrice &&
				entry["calls_per_minute"] == float64(120) && entry["per_key_max_concurrency"] == float64(2) &&
				entry["credential_ref"] != nil && entry["credential_ref"] != credential
		}
	}
	if !foundPricing {
		t.Fatalf("configuration export lost tool pricing or exposed the credential: %v", exported["mcp_servers"])
	}
	h.want(owner, "POST", "/api/v1/configuration/plan", map[string]any{"document": exported}, nil, 200)

	updated := h.want(owner, "PUT", "/api/v1/mcp-servers/"+mcpExecString(t, pricedServer, "id"), map[string]any{
		"name": "Priced server", "endpoint": upstream.URL, "enabled": true, "credential": credential,
		"unit_price": "0.40", "tool_prices": map[string]any{"lookup": "0.40"},
	}, withMatch(pricedServer, idem("mcp-pricing-update")), 200)
	if updated["unit_price"] != "0.40" || updated["revision_id"] == pricedServer["revision_id"] ||
		updated["calls_per_minute"] != float64(120) || updated["max_concurrency"] != float64(4) ||
		updated["per_key_calls_per_minute"] != float64(30) || updated["per_key_max_concurrency"] != float64(2) {
		t.Fatalf("management update did not pin new pricing while preserving absent limits: %v", updated)
	}
	limitsOnly := h.want(owner, "PUT", "/api/v1/mcp-servers/"+mcpExecString(t, updated, "id"), map[string]any{
		"name": "Priced server", "endpoint": upstream.URL, "enabled": true,
		"calls_per_minute": 240, "max_concurrency": 6,
	}, withMatch(updated, idem("mcp-pricing-limits-only")), 200)
	if limitsOnly["revision_id"] != updated["revision_id"] || limitsOnly["unit_price"] != "0.40" ||
		limitsOnly["calls_per_minute"] != float64(240) || limitsOnly["max_concurrency"] != float64(6) ||
		limitsOnly["per_key_calls_per_minute"] != float64(30) {
		t.Fatalf("limits-only update drifted the certified revision or lost preserved fields: %v", limitsOnly)
	}
}

func TestMCPToolCallSettledSpendExhaustsFreshBudgetAndEndUserEndToEnd(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "MCP budget exhaustion")
	const unitPrice = "0.30"
	price, ok := new(big.Rat).SetString(unitPrice)
	if !ok {
		t.Fatal("unit price does not parse")
	}
	capLimit := new(big.Rat).Mul(price, big.NewRat(3, 2)).FloatString(2)
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
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
			w.Header().Set("Mcp-Session-Id", "exhaustion-session")
			result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{
				"name": "lookup", "description": "Look up",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []string{"query"}, "additionalProperties": false},
			}}}
		case "tools/call":
			calls.Add(1)
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "priced answer"}}, "isError": false}
		default:
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	defer upstream.Close()

	created := h.want(owner, "POST", "/api/v1/mcp-servers", map[string]any{
		"name": "Exhaustion server", "project_id": project, "transport": "streamable_http",
		"endpoint": upstream.URL, "enabled": true, "unit_price": unitPrice,
	}, idem("mcp-exhaustion-server"), 201)
	serverID := mcpExecString(t, created, "id")
	tool := agentcontract.Tool{ServerID: serverID, RevisionID: mcpExecString(t, created, "revision_id"), CatalogDigest: mcpExecString(t, mcpExecObject(t, created, "catalog"), "digest"), Name: "lookup", Alias: agentcontract.ToolAlias(serverID, "lookup")}
	toolset := h.want(owner, "POST", "/api/v1/toolsets", map[string]any{
		"project_id": project, "name": "Exhaustion toolset",
		"toolset": map[string]any{"tools": []agentcontract.Tool{tool}},
	}, idem("mcp-exhaustion-toolset"), 201)
	toolsetID := mcpExecString(t, toolset, "id")
	h.want(owner, "POST", "/api/v1/toolsets/"+toolsetID+"/publish", nil, withMatch(toolset, idem("mcp-exhaustion-publish")), 200)
	cappedKey := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{
		"name": "Exhaustion caller", "project_id": project, "scopes": []string{"tools"},
		"allowed_toolsets": []string{toolsetID}, "daily_cost_limit": capLimit,
	}, idem("mcp-exhaustion-key"), 201)
	cappedSecret := mcpExecString(t, cappedKey, "secret")
	endUserKey := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{
		"name": "End-user caller", "project_id": project, "scopes": []string{"tools"},
		"allowed_toolsets": []string{toolsetID}, "end_user_source": "header",
		"end_user_policy": map[string]any{"defaults": map[string]any{"daily_cost_limit": capLimit}},
	}, idem("mcp-exhaustion-end-user-key"), 201)
	endUserSecret := mcpExecString(t, endUserKey, "secret")

	valkey, prefix, _ := acctKeyspace(t)
	limiter, err := limits.New(valkey, strings.TrimSuffix(prefix, ":"))
	if err != nil {
		t.Fatal(err)
	}
	sessionStore, err := mcp.NewSessionStore(valkey, strings.TrimSuffix(prefix, ":")+"mcp-exhaustion")
	if err != nil {
		t.Fatal(err)
	}
	toolServer := &mcp.Server{
		Runtime: h.Runtime, Egress: &h.Policy, Auth: h.AuthKey, Sessions: sessionStore, Limiter: limiter,
		Ledger: &mcp.DurableLedger{Pool: h.Pool, Store: &limits.DebitStore{Pool: h.Pool, Limiter: limiter}, Auth: h.AuthKey},
	}
	mux := http.NewServeMux()
	toolServer.Register(mux)
	consumer := httptest.NewServer(mux)
	defer consumer.Close()
	if err = h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	call := func(secret, endUser, callID string) (int, map[string]any) {
		t.Helper()
		data, _ := json.Marshal(map[string]any{"toolset_id": toolsetID, "name": tool.Alias, "call_id": callID, "arguments": map[string]any{"query": "exhaustion probe"}})
		req, err := http.NewRequest(http.MethodPost, consumer.URL+"/v1/mcp/tools/call", bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("Content-Type", "application/json")
		if endUser != "" {
			req.Header.Set("X-OLP-End-User", endUser)
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
				t.Fatalf("consumer response is not JSON: %s", raw)
			}
		}
		return resp.StatusCode, out
	}

	status, first := call(cappedSecret, "", "exhaustion-1")
	if status != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("first capped call from a fresh owner did not execute: status=%d calls=%d body=%v", status, calls.Load(), first)
	}
	record := h.want(owner, "GET", "/api/v1/mcp-tool-calls/"+mcpExecString(t, first, "attempt_id"), nil, nil, 200)
	var accrued string
	if err = h.Pool.QueryRow(t.Context(), `SELECT accrued::text FROM olp.api_key_cost_windows WHERE api_key_id=$1 AND window_kind='day'`, mcpExecString(t, cappedKey, "id")).Scan(&accrued); err != nil {
		t.Fatal(err)
	}
	accruedAmount, ok := new(big.Rat).SetString(accrued)
	if !ok || accruedAmount.Cmp(mcpAmount(t, record, "observed_cost")) != 0 || accruedAmount.Sign() == 0 {
		t.Fatalf("settled spend did not persist into the owner window: accrued=%q record=%v", accrued, record)
	}
	status, refused := call(cappedSecret, "", "exhaustion-2")
	if status != http.StatusTooManyRequests || calls.Load() != 1 {
		t.Fatalf("call past settled spend dispatched: status=%d calls=%d body=%v", status, calls.Load(), refused)
	}
	if code := mcpExecString(t, mcpExecObject(t, refused, "error"), "code"); code != "budget_exceeded" {
		t.Fatalf("budget refusal is not typed: %v", refused)
	}

	status, endUserFirst := call(endUserSecret, "end-user-alpha", "exhaustion-eu-1")
	if status != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("first end-user call did not execute: status=%d calls=%d body=%v", status, calls.Load(), endUserFirst)
	}
	var endUserOwners int
	if err = h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.execution_debit_owners WHERE dispatch_id=$1 AND owner_kind='end_user'`, mcpExecString(t, endUserFirst, "dispatch_id")).Scan(&endUserOwners); err != nil {
		t.Fatal(err)
	}
	if endUserOwners == 0 {
		t.Fatal("end-user call recorded no end-user debit owner")
	}
	status, endUserRefused := call(endUserSecret, "end-user-alpha", "exhaustion-eu-2")
	if status != http.StatusTooManyRequests || calls.Load() != 2 {
		t.Fatalf("second call for the exhausted end user dispatched: status=%d calls=%d body=%v", status, calls.Load(), endUserRefused)
	}
	status, otherEndUser := call(endUserSecret, "end-user-beta", "exhaustion-eu-3")
	if status != http.StatusOK || calls.Load() != 3 {
		t.Fatalf("a different end user inherited the exhausted budget: status=%d calls=%d body=%v", status, calls.Load(), otherEndUser)
	}
}

func TestMCPToolCallRateLeasesHoldAndRefundEndToEnd(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "MCP rate leases")
	entered := make(chan struct{}, 1)
	releaseUpstream := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseUpstream) }) }
	defer release()
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
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
			w.Header().Set("Mcp-Session-Id", "lease-session")
			result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{
				"name": "lookup", "description": "Look up",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []string{"query"}, "additionalProperties": false},
			}}}
		case "tools/call":
			if calls.Add(1) == 1 {
				entered <- struct{}{}
				select {
				case <-releaseUpstream:
				case <-r.Context().Done():
					return
				}
			}
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "leased answer"}}, "isError": false}
		default:
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	defer upstream.Close()

	created := h.want(owner, "POST", "/api/v1/mcp-servers", map[string]any{
		"name": "Lease server", "project_id": project, "transport": "streamable_http",
		"endpoint": upstream.URL, "enabled": true, "unit_price": "0",
		"max_concurrency": 1, "per_key_calls_per_minute": 1,
	}, idem("mcp-lease-server"), 201)
	serverID := mcpExecString(t, created, "id")
	tool := agentcontract.Tool{ServerID: serverID, RevisionID: mcpExecString(t, created, "revision_id"), CatalogDigest: mcpExecString(t, mcpExecObject(t, created, "catalog"), "digest"), Name: "lookup", Alias: agentcontract.ToolAlias(serverID, "lookup")}
	toolset := h.want(owner, "POST", "/api/v1/toolsets", map[string]any{
		"project_id": project, "name": "Lease toolset",
		"toolset": map[string]any{"tools": []agentcontract.Tool{tool}},
	}, idem("mcp-lease-toolset"), 201)
	toolsetID := mcpExecString(t, toolset, "id")
	h.want(owner, "POST", "/api/v1/toolsets/"+toolsetID+"/publish", nil, withMatch(toolset, idem("mcp-lease-publish")), 200)
	makeKey := func(name string) string {
		t.Helper()
		key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{
			"name": name, "project_id": project, "scopes": []string{"tools"}, "allowed_toolsets": []string{toolsetID},
		}, idem("mcp-lease-key-"+name), 201)
		return mcpExecString(t, key, "secret")
	}
	secretA, secretB := makeKey("Lease caller A"), makeKey("Lease caller B")

	valkey, prefix, _ := acctKeyspace(t)
	limiter, err := limits.New(valkey, strings.TrimSuffix(prefix, ":"))
	if err != nil {
		t.Fatal(err)
	}
	sessionStore, err := mcp.NewSessionStore(valkey, strings.TrimSuffix(prefix, ":")+"mcp-lease")
	if err != nil {
		t.Fatal(err)
	}
	toolServer := &mcp.Server{
		Runtime: h.Runtime, Egress: &h.Policy, Auth: h.AuthKey, Sessions: sessionStore, Limiter: limiter,
		Ledger: &mcp.DurableLedger{Pool: h.Pool, Store: &limits.DebitStore{Pool: h.Pool, Limiter: limiter}, Auth: h.AuthKey},
	}
	mux := http.NewServeMux()
	toolServer.Register(mux)
	consumer := httptest.NewServer(mux)
	defer consumer.Close()
	if err = h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	call := func(secret, callID string) (int, map[string]any) {
		data, _ := json.Marshal(map[string]any{"toolset_id": toolsetID, "name": tool.Alias, "call_id": callID, "arguments": map[string]any{"query": "lease probe"}})
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
		if len(raw) > 0 && json.Unmarshal(raw, &out) != nil {
			t.Fatalf("consumer response is not JSON: %s", raw)
		}
		return resp.StatusCode, out
	}

	type outcome struct {
		status int
		body   map[string]any
	}
	firstDone := make(chan outcome, 1)
	go func() {
		status, body := call(secretA, "lease-a-1")
		firstDone <- outcome{status, body}
	}()
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("first call never reached the upstream tool")
	}
	status, refused := call(secretB, "lease-b-1")
	if status != http.StatusTooManyRequests || calls.Load() != 1 {
		t.Fatalf("second call was not held by the server concurrency lease: status=%d calls=%d body=%v", status, calls.Load(), refused)
	}
	if code := mcpExecString(t, mcpExecObject(t, refused, "error"), "code"); code != "tool_rate_limited" {
		t.Fatalf("concurrency refusal is not typed: %v", refused)
	}
	release()
	first := <-firstDone
	if first.status != http.StatusOK {
		t.Fatalf("held call did not complete after release: status=%d body=%v", first.status, first.body)
	}
	status, retried := call(secretB, "lease-b-2")
	if status != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("refused call consumed the per-key rate allowance instead of refunding it: status=%d calls=%d body=%v", status, calls.Load(), retried)
	}
}
