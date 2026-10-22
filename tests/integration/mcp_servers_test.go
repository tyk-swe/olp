//go:build integration

package integration_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/configuration"
	"github.com/tyk-swe/olp/internal/secrets"
)

func TestMCPConfigurationAdvancesAuthority(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "MCP authority")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(204)
			return
		}
		var request struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			w.WriteHeader(400)
			return
		}
		var result any
		switch request.Method {
		case "notifications/initialized":
			w.WriteHeader(202)
			return
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "lookup", "inputSchema": map[string]any{"type": "object"}}}}
		default:
			t.Errorf("unexpected certification method %s", request.Method)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	defer upstream.Close()
	created := h.want(owner, "POST", "/api/v1/mcp-servers", map[string]any{"name": "Authority server", "project_id": project, "transport": "streamable_http", "endpoint": upstream.URL, "enabled": true}, idem("mcp-authority-create"), 201)
	document := map[string]any{"api_version": configuration.APIVersion, "mcp_servers": []any{map[string]any{"name": "Authority server", "project": "MCP authority", "transport": "streamable_http", "endpoint": upstream.URL, "enabled": false, "catalog_digest": created["catalog"].(map[string]any)["digest"]}}}
	authority := func() int64 {
		t.Helper()
		var sequence int64
		if err := h.Pool.QueryRow(t.Context(), "SELECT authority_sequence FROM olp.installation WHERE singleton").Scan(&sequence); err != nil {
			t.Fatal(err)
		}
		return sequence
	}
	before := authority()
	h.want(owner, "POST", "/api/v1/configuration/apply", map[string]any{"document": document}, idem("mcp-authority-disable"), 200)
	if after := authority(); after != before+1 {
		t.Fatalf("MCP-only configuration did not invalidate cached authority: %d -> %d", before, after)
	}
	h.want(owner, "POST", "/api/v1/configuration/apply", map[string]any{"document": document}, idem("mcp-authority-reuse"), 200)
	if after := authority(); after != before+1 {
		t.Fatal("reusing unchanged MCP configuration advanced authority")
	}

	// Full exports must retain immutable publication history while revoking
	// the server on which that history depends.
	server := document["mcp_servers"].([]any)[0].(map[string]any)
	server["enabled"] = true
	h.want(owner, "POST", "/api/v1/configuration/apply", map[string]any{"document": document}, idem("mcp-authority-enable"), 200)
	serverID := created["id"].(string)
	current := h.want(owner, "GET", "/api/v1/mcp-servers/"+serverID, nil, nil, 200)
	tool := agentcontract.Tool{ServerID: serverID, RevisionID: current["revision_id"].(string), CatalogDigest: current["catalog"].(map[string]any)["digest"].(string), Name: "lookup", Alias: agentcontract.ToolAlias(serverID, "lookup")}
	draft := map[string]any{"project_id": project, "name": "Retained toolset", "toolset": map[string]any{"tools": []agentcontract.Tool{tool}}}
	toolset := h.want(owner, "POST", "/api/v1/toolsets", draft, idem("mcp-history-toolset"), 201)
	path := "/api/v1/toolsets/" + toolset["id"].(string)
	published := h.want(owner, "POST", path+"/publish", nil, withMatch(toolset, idem("mcp-history-publish")), 200)
	exported := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)["document"].(map[string]any)
	exported["mcp_servers"].([]any)[0].(map[string]any)["enabled"] = false
	for _, phase := range []string{"disable", "retain"} {
		plan := h.want(owner, "POST", "/api/v1/configuration/plan", map[string]any{"document": exported}, nil, 200)
		if len(plan["blockers"].([]any)) != 0 || len(plan["conflicts"].([]any)) != 0 {
			t.Fatalf("retained revoked publication blocked %s: %v", phase, plan)
		}
		h.want(owner, "POST", "/api/v1/configuration/apply", map[string]any{"document": exported}, idem("mcp-history-"+phase), 200)
		if current := h.want(owner, "GET", path, nil, nil, 200); current["revision_id"] != published["revision_id"] {
			t.Fatal("retaining publication history changed its immutable revision")
		}
		var live bool
		if err := h.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM olp.live_agent_publication_revisions WHERE publication_id=$1)`, toolset["id"]).Scan(&live); err != nil || live {
			t.Fatalf("retained revision became executable after revocation: %v", err)
		}
	}
	draft["name"] = "New publication"
	newToolset := h.want(owner, "POST", "/api/v1/toolsets", draft, idem("mcp-history-new"), 201)
	h.want(owner, "POST", "/api/v1/toolsets/"+newToolset["id"].(string)+"/publish", nil, withMatch(newToolset, idem("mcp-history-refuse")), 422)
}

func TestMCPServerCertificationPinsCatalogsAndRechecksAuthorityWithoutLocks(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "MCP project")
	other := createProject(h, owner, "Other MCP")
	const credential = "private-upstream-mcp-bearer"
	var description atomic.Value
	description.Store("Original description")
	var requests atomic.Int64
	var blocked atomic.Bool
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			w.WriteHeader(204)
			return
		}
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+credential {
			w.WriteHeader(401)
			return
		}
		var request struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(400)
			return
		}
		if request.Method == "notifications/initialized" {
			w.WriteHeader(202)
			return
		}
		if request.Method == "initialize" && blocked.Load() {
			select {
			case entered <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "lookup", "description": description.Load(), "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"access_token": map[string]any{"type": "string"}, "query": map[string]any{"type": "string"}, "count": map[string]any{"type": "integer", "minimum": int64(9007199254740993)}}}}}}
		default:
			t.Errorf("certification invoked %s", request.Method)
			w.WriteHeader(400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	defer upstream.Close()
	input := map[string]any{"name": "Search server", "project_id": project, "transport": "streamable_http", "endpoint": upstream.URL, "enabled": true, "credential": credential}
	headers := idem(uuid.NewString())
	created := h.want(owner, "POST", "/api/v1/mcp-servers", input, headers, 201)
	id := created["id"].(string)
	path := "/api/v1/mcp-servers/" + id
	oldRevision := created["revision_id"].(string)
	validateManagementResponse(t, "POST", "/api/v1/mcp-servers", 201, created)
	encoded, _ := json.Marshal(created)
	if strings.Contains(string(encoded), credential) || !created["has_credential"].(bool) {
		t.Fatal("credential leaked or was not stored")
	}
	_, readToken := createToken(h, owner, "Precise MCP metadata", []string{"read"})
	status, _, rawResult := h.call(sweepCaller{token: readToken}, "POST", "/api/v1/mcp", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{
			"name": "get_mcp_server", "arguments": map[string]any{"path": map[string]string{"server_id": id}},
		},
	}, nil)
	if status != 200 || !strings.Contains(rawResult, `"minimum":9007199254740993`) || !strings.Contains(rawResult, `"access_token":{"type":"string"}`) || strings.Contains(rawResult, credential) {
		t.Fatalf("MCP changed precise schema metadata or exposed its credential: status %d", status)
	}
	before := requests.Load()
	replay := h.want(owner, "POST", "/api/v1/mcp-servers", input, headers, 201)
	if replay["id"] != id || requests.Load() != before {
		t.Fatal("idempotent creation repeated network certification")
	}
	var redirectedCalls atomic.Int64
	redirected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedCalls.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("saved credential forwarded to a changed endpoint")
		}
		w.WriteHeader(503)
	}))
	defer redirected.Close()
	changedEndpoint := map[string]any{"name": "Search server", "endpoint": redirected.URL, "enabled": true}
	h.want(owner, "PUT", path, changedEndpoint, map[string]string{"Idempotency-Key": uuid.NewString(), "If-Match": `"` + created["etag"].(string) + `"`}, 422)
	if redirectedCalls.Load() != 0 {
		t.Fatal("endpoint change contacted the new server without an explicit credential decision")
	}
	changedEndpoint["credential"] = nil
	h.want(owner, "PUT", path, changedEndpoint, map[string]string{"Idempotency-Key": uuid.NewString(), "If-Match": `"` + created["etag"].(string) + `"`}, 422)
	if redirectedCalls.Load() == 0 {
		t.Fatal("explicit credential removal did not allow certification")
	}
	description.Store("Changed upstream description")
	unchanged := h.want(owner, "GET", path, nil, nil, 200)
	if unchanged["revision_id"] != oldRevision || strings.Contains(string(mustJSON(unchanged)), "Changed upstream") {
		t.Fatal("read silently adopted changed upstream catalog")
	}
	token := h.want(owner, "POST", "/api/v1/management-tokens", map[string]any{"name": "Other project", "scopes": []any{"read", "configure"}, "project_ids": []any{other}, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, idem(uuid.NewString()), 201)
	caller := sweepCaller{token: token["secret"].(string)}
	for _, route := range []string{path, path + "/revisions/" + oldRevision} {
		if status, _, _ := h.call(caller, "GET", route, nil, nil); status != 404 {
			t.Fatalf("foreign server readable: %d", status)
		}
	}
	update := map[string]any{"name": "Search server changed", "endpoint": upstream.URL, "enabled": true}
	if status, _, _ := h.call(caller, "PUT", path, update, map[string]string{"Idempotency-Key": uuid.NewString(), "If-Match": `"` + created["etag"].(string) + `"`}); status != 404 {
		t.Fatalf("foreign server writable: %d", status)
	}
	h.want(owner, "PUT", path, update, idem(uuid.NewString()), 428)
	updated := h.want(owner, "PUT", path, update, map[string]string{"Idempotency-Key": uuid.NewString(), "If-Match": `"` + created["etag"].(string) + `"`}, 200)
	if updated["revision"].(float64) != 2 || updated["catalog"].(map[string]any)["digest"] == created["catalog"].(map[string]any)["digest"] {
		t.Fatal("explicit certification did not approve new catalog")
	}
	old := h.want(owner, "GET", path+"/revisions/"+oldRevision, nil, nil, 200)
	if !strings.Contains(string(mustJSON(old)), "Original description") {
		t.Fatal("old catalog changed")
	}
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp.mcp_server_revisions SET tools='[]' WHERE id=$1", oldRevision); err == nil {
		t.Fatal("immutable catalog allowed mutation")
	}
	// Block upstream certification. Ordinary management must still mutate,
	// and the pending write must refuse a registration edited in the meantime.
	blocked.Store(true)
	result := make(chan int, 1)
	go func() {
		status, _, _ := h.call(sweepCaller{browser: owner}, "PUT", path, update, map[string]string{"Idempotency-Key": uuid.NewString(), "If-Match": `"` + updated["etag"].(string) + `"`})
		result <- status
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("certification did not reach upstream")
	}
	projectValue := h.want(owner, "GET", "/api/v1/projects/"+project, nil, nil, 200)
	h.want(owner, "PATCH", "/api/v1/projects/"+project, map[string]any{"name": "MCP renamed"}, map[string]string{"If-Match": `"` + projectValue["etag"].(string) + `"`}, 200)
	retire := map[string]string{"Idempotency-Key": uuid.NewString(), "If-Match": `"` + updated["etag"].(string) + `"`}
	h.want(owner, "DELETE", path, nil, retire, 204)
	releaseOnce.Do(func() { close(release) })
	if status := <-result; status != 412 {
		t.Fatalf("concurrent retirement overwritten: %d", status)
	}
	h.want(owner, "DELETE", path, nil, retire, 204)
	retired := h.want(owner, "GET", path, nil, nil, 200)
	if retired["retired_at"] == nil || retired["has_credential"].(bool) {
		t.Fatal("retirement failed to retain identity/remove credential")
	}
	var count int
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.secrets WHERE purpose=$1", secrets.MCPCredential).Scan(&count); err != nil || count != 0 {
		t.Fatalf("retired credential retained: %d %v", count, err)
	}
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.audit WHERE resource_id=$1 AND action IN ('mcp_server.create','mcp_server.update','mcp_server.retire')", id).Scan(&count); err != nil || count != 3 {
		t.Fatalf("mutation audits include replay/failure: %d %v", count, err)
	}
}
func mustJSON(v any) []byte { raw, _ := json.Marshal(v); return raw }
