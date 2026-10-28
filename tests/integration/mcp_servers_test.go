//go:build integration

package integration_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/tyk-swe/olp/internal/workload"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/configuration"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/mcp"
	"github.com/tyk-swe/olp/internal/mcpgrants"
	"github.com/tyk-swe/olp/internal/mcpservers"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/testutil"
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

type mcpPersonalKey struct {
	ID     string `json:"id"`
	Secret string `json:"secret"`
}

type mcpPersonalClaimInput struct {
	EnrollmentID string `json:"enrollment_id"`
	HandoffToken string `json:"handoff_token"`
}

func TestMCPPersonalGrantEnrollmentRequiresOneUseOwnerAndCurrentBinding(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	other := h.invite(owner, "other-grant-user@example.test", "viewer")
	project := createProject(h, owner, "Personal tool grants")
	const personalCredential = "personal-grant-fixture-token"
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
		if request.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if request.Method == "tools/call" && r.Header.Get("Authorization") != "Bearer "+personalCredential {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		result := func() json.RawMessage {
			switch request.Method {
			case "tools/call":
				return mustJSON(&mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "personal result"}}})
			case "initialize":
				return mustJSON(mcpsdk.InitializeResult{ProtocolVersion: "2025-11-25", Capabilities: &mcpsdk.ServerCapabilities{Tools: &mcpsdk.ToolCapabilities{}}, ServerInfo: &mcpsdk.Implementation{Name: "personal-fixture", Version: "1"}})
			default:

				return mustJSON(mcpsdk.ListToolsResult{Tools: []*mcpsdk.Tool{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}})
			}
		}()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      int             `json:"id"`
			Result  json.RawMessage `json:"result"`
		}{JSONRPC: "2.0", ID: request.ID, Result: result})
	}))
	defer upstream.Close()
	oauth := map[string]any{"client_id": "personal-client", "audience": "tool-api", "scopes": []string{"tools.read"}, "authorization_endpoint": upstream.URL + "/authorize", "token_endpoint": upstream.URL + "/token"}
	registered := h.want(owner, "POST", "/api/v1/mcp-servers", map[string]any{"name": "Personal server", "project_id": project, "transport": "streamable_http", "endpoint": upstream.URL, "enabled": true, "oauth": oauth, "unit_price": "0"}, idem("personal-server"), 201)
	serverID := mcpExecString(t, registered, "id")
	tool := agentcontract.Tool{ServerID: serverID, RevisionID: mcpExecString(t, registered, "revision_id"), CatalogDigest: mcpExecString(t, mcpExecObject(t, registered, "catalog"), "digest"), Name: "lookup", Alias: agentcontract.ToolAlias(serverID, "lookup")}
	toolset := h.want(owner, "POST", "/api/v1/toolsets", map[string]any{"project_id": project, "name": "Personal tools", "toolset": map[string]any{"tools": []agentcontract.Tool{tool}}}, idem("personal-toolset"), 201)
	toolsetID := mcpExecString(t, toolset, "id")
	h.want(owner, "POST", "/api/v1/toolsets/"+toolsetID+"/publish", nil, withMatch(toolset, idem("personal-publish")), 200)
	keyResponse := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Personal caller", "project_id": project, "scopes": []string{"tools"}, "allowed_toolsets": []string{toolsetID}}, idem("personal-caller"), 201)
	var key mcpPersonalKey
	if err := json.Unmarshal(mustJSON(keyResponse), &key); err != nil {
		t.Fatal(err)
	}
	keyID := key.ID
	start := func() mcpgrants.Handoff {
		response := h.want(owner, "POST", "/api/v1/mcp-servers/"+serverID+"/grant-enrollments", struct {
			APIKeyID string `json:"api_key_id"`
		}{keyID}, nil, 200)
		var handoff mcpgrants.Handoff
		if err := json.Unmarshal(mustJSON(response), &handoff); err != nil {
			t.Fatal(err)
		}
		return handoff
	}
	claim := func(handoff mcpgrants.Handoff) mcpPersonalClaimInput {
		_, fragment, hasFragment := strings.Cut(handoff.EnrollmentURL, "#")
		if !hasFragment {
			t.Fatal("browser enrollment link has no private fragment")
		}
		id, token, hasToken := strings.Cut(fragment, ".")
		if !hasToken {
			t.Fatal("browser enrollment link is incomplete")
		}
		return mcpPersonalClaimInput{EnrollmentID: id, HandoffToken: token}
	}
	first := start()
	firstClaim := claim(first)
	h.want(other, "POST", "/api/v1/mcp-grant-enrollments/claim", firstClaim, nil, 404)
	claimedResponse := h.want(owner, "POST", "/api/v1/mcp-grant-enrollments/claim", firstClaim, nil, 200)
	var claimed struct {
		Enrollment mcpgrants.Enrollment `json:"enrollment"`
	}
	if err := json.Unmarshal(mustJSON(claimedResponse), &claimed); err != nil {
		t.Fatal(err)
	}
	h.want(owner, "POST", "/api/v1/mcp-grant-enrollments/claim", firstClaim, nil, 409)
	standing := h.want(owner, "GET", "/api/v1/mcp-user-grants", nil, nil, 200)
	item := mcpExecItem(t, mcpExecItems(t, standing, "items"), 0, standing)
	if item["state"] != "pending" || mcpExecObject(t, mcpExecObject(t, item, "binding"), "identity")["id"] != keyID {
		t.Fatalf("claimed enrollment has incorrect standing or individual: %v", standing)
	}
	exercisePersonalGrantDispatch(t, h, owner, key, toolsetID, tool.Alias, claimed.Enrollment, personalCredential)
	exported := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	encoded := string(mustJSON(exported))
	if strings.Contains(encoded, firstClaim.HandoffToken) || strings.Contains(encoded, first.EnrollmentID) {
		t.Fatal("portable configuration contains personal enrollment material")
	}
	document := mcpExecObject(t, exported, "document")
	exportedServer := mcpExecItem(t, mcpExecItems(t, document, "mcp_servers"), 0, document)
	if mcpExecObject(t, exportedServer, "oauth")["client_id"] != oauth["client_id"] {
		t.Fatal("portable server configuration lost the declared OAuth client")
	}
	second := start()
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.mcp_grant_enrollments SET expires_at=now()-interval '1 second' WHERE id=$1`, second.EnrollmentID); err != nil {
		t.Fatal(err)
	}
	h.want(owner, "POST", "/api/v1/mcp-grant-enrollments/claim", claim(second), nil, 410)
	third := start()
	changedOAuth := map[string]any{"client_id": "different-client", "audience": "tool-api", "scopes": []string{"tools.read"}, "authorization_endpoint": upstream.URL + "/authorize", "token_endpoint": upstream.URL + "/token"}
	h.want(owner, "PUT", "/api/v1/mcp-servers/"+serverID, map[string]any{"name": "Personal server", "endpoint": upstream.URL, "enabled": true, "oauth": changedOAuth}, withMatch(registered, idem("personal-client-changed")), 200)
	h.want(owner, "POST", "/api/v1/mcp-grant-enrollments/claim", claim(third), nil, 409)
}

func exercisePersonalGrantDispatch(t *testing.T, h *accessHarness, owner *browser, key mcpPersonalKey, toolsetID, alias string, claimed mcpgrants.Enrollment, credential string) {
	t.Helper()
	bound := claimed.Binding
	valkey, prefix, _ := acctKeyspace(t)
	limiter, err := limits.New(valkey, strings.TrimSuffix(prefix, ":"))
	if err != nil {
		t.Fatal(err)
	}
	store := &mcpgrants.Store{Pool: h.Pool, Keys: h.Server.Keys, Auth: h.AuthKey, Installation: h.Server.Installation}
	server := &mcp.Server{Runtime: h.Runtime, Egress: &h.Policy, Auth: h.AuthKey, Grants: store, Limiter: limiter, Ledger: &mcp.DurableLedger{Pool: h.Pool, Store: &limits.DebitStore{Pool: h.Pool, Limiter: limiter}, Auth: h.AuthKey}}
	mux := http.NewServeMux()
	server.Register(mux)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	call := func(secret, callID string, want int) {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/v1/mcp/tools/call", bytes.NewReader(mustJSON(map[string]any{"toolset_id": toolsetID, "name": alias, "arguments": map[string]any{}, "call_id": callID})))
		r.Header.Set("Authorization", "Bearer "+secret)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("personal grant dispatch returned %d, want %d: %s", w.Code, want, w.Body.String())
		}
	}
	secret := key.Secret
	call(secret, "pending-grant", http.StatusForbidden)
	grantID, secretID := uuid.NewString(), uuid.NewString()
	expires := time.Now().Add(time.Hour)
	tx, err := h.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := h.Server.Keys.Store(t.Context(), tx, h.Server.Installation, secretID, secrets.MCPGrantAccess, []byte(credential), &expires); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO olp.mcp_user_grants(id,enrollment_id,server_id,project_id,binding,binding_digest,member_id,access_secret_id,expires_at) SELECT $1,id,server_id,project_id,binding,$2,claimed_by,$3,$4 FROM olp.mcp_grant_enrollments WHERE id=$5`, grantID, bound.Digest(), secretID, expires, claimed.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	call(secret, "active-grant", http.StatusOK)
	other := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Other personal caller", "project_id": bound.ProjectID, "scopes": []string{"tools"}, "allowed_toolsets": []string{toolsetID}}, idem("other-personal-caller"), 201)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	call(mcpExecString(t, other, "secret"), "different-key-grant", http.StatusForbidden)
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.mcp_user_grants SET expires_at=now()-interval '1 second' WHERE id=$1`, grantID); err != nil {
		t.Fatal(err)
	}
	call(secret, "expired-grant", http.StatusForbidden)
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.mcp_user_grants SET expires_at=$2 WHERE id=$1`, grantID, expires); err != nil {
		t.Fatal(err)
	}
	h.want(owner, "DELETE", "/api/v1/mcp-user-grants/"+grantID, nil, nil, http.StatusNoContent)
	call(secret, "revoked-grant", http.StatusForbidden)
}

type personalOAuthServerInput struct {
	Name      string           `json:"name"`
	ProjectID string           `json:"project_id"`
	Transport string           `json:"transport"`
	Endpoint  string           `json:"endpoint"`
	Enabled   bool             `json:"enabled"`
	OAuth     routing.MCPOAuth `json:"oauth"`
	UnitPrice string           `json:"unit_price"`
}

type personalOAuthToolsetInput struct {
	ProjectID string                `json:"project_id"`
	Name      string                `json:"name"`
	Toolset   agentcontract.Toolset `json:"toolset"`
}

type personalOAuthKeyInput struct {
	Name            string   `json:"name"`
	ProjectID       string   `json:"project_id"`
	Scopes          []string `json:"scopes"`
	AllowedToolsets []string `json:"allowed_toolsets"`
}

type personalOAuthAuthorizeInput struct {
	Flow mcpgrants.Flow `json:"flow"`
}

type personalOAuthCompletionInput struct {
	Code  string `json:"code"`
	State string `json:"state"`
}

type personalOAuthFixture struct {
	h         *accessHarness
	owner     *browser
	upstream  *testutil.OAuthServer
	serverID  string
	toolsetID string
	alias     string
	key       mcpPersonalKey
}

func newPersonalOAuthFixture(t *testing.T) personalOAuthFixture {
	t.Helper()
	return newPersonalOAuthFixtureWithHook(t, nil)
}

func newPersonalOAuthFixtureWithHook(t *testing.T, hook func(*http.Request)) personalOAuthFixture {
	t.Helper()
	return newPersonalOAuthFixtureWithCall(t, hook, personalOAuthCallbacks{})
}

type personalOAuthCallbacks struct {
	Result  func() json.RawMessage
	Call    func(*http.Request) mcpsdk.CallToolResult
	Catalog func()
}

func newPersonalOAuthFixtureWithCall(t *testing.T, hook func(*http.Request), callbacks personalOAuthCallbacks) personalOAuthFixture {
	t.Helper()
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "OAuth tools")
	authority := testutil.NewOAuthServer(t)
	oauthEndpoint := func() string {
		if hook == nil {
			return authority.URL
		}
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/token" {
				hook(r)
			}
			authority.Config.Handler.ServeHTTP(w, r)
		}))
		t.Cleanup(proxy.Close)
		return proxy.URL
	}()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := func() json.RawMessage {
			switch request.Method {
			case "initialize":
				return mustJSON(mcpsdk.InitializeResult{ProtocolVersion: "2025-11-25", Capabilities: &mcpsdk.ServerCapabilities{Tools: &mcpsdk.ToolCapabilities{}}, ServerInfo: &mcpsdk.Implementation{Name: "oauth-fixture", Version: "1"}})
			case "tools/call":
				if callbacks.Result != nil {
					return callbacks.Result()
				}
				if callbacks.Call != nil {
					return mustJSON(callbacks.Call(r))
				}
				identity, authorized := authority.Authorized(r)
				if !authorized {
					return mustJSON(&mcpsdk.CallToolResult{IsError: true, Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "unauthorized"}}})
				}
				return mustJSON(&mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: identity.Account}}})
			default:
				if callbacks.Catalog != nil {
					callbacks.Catalog()
				}
				return mustJSON(mcpsdk.ListToolsResult{Tools: []*mcpsdk.Tool{{Name: "identity", InputSchema: json.RawMessage(`{"type":"object"}`)}}})
			}
		}()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      int             `json:"id"`
			Result  json.RawMessage `json:"result"`
		}{"2.0", request.ID, result})
	}))
	t.Cleanup(upstream.Close)
	oauth := routing.MCPOAuth{ClientID: "fixture-client", Audience: "fixture-tools", Scopes: []string{"tools.read"}, AuthorizationEndpoint: oauthEndpoint + "/authorize", TokenEndpoint: oauthEndpoint + "/token", DeviceAuthorizationEndpoint: oauthEndpoint + "/device/code"}
	registered := h.want(owner, "POST", "/api/v1/mcp-servers", personalOAuthServerInput{Name: "OAuth server", ProjectID: project, Transport: "streamable_http", Endpoint: upstream.URL, Enabled: true, OAuth: oauth, UnitPrice: "0"}, idem("oauth-server"), 201)
	id := mcpExecString(t, registered, "id")
	tool := agentcontract.Tool{ServerID: id, RevisionID: mcpExecString(t, registered, "revision_id"), CatalogDigest: mcpExecString(t, mcpExecObject(t, registered, "catalog"), "digest"), Name: "identity", Alias: agentcontract.ToolAlias(id, "identity")}
	toolset := h.want(owner, "POST", "/api/v1/toolsets", personalOAuthToolsetInput{ProjectID: project, Name: "OAuth tools", Toolset: agentcontract.Toolset{Tools: []agentcontract.Tool{tool}}}, idem("oauth-toolset"), 201)
	toolsetID := mcpExecString(t, toolset, "id")
	h.want(owner, "POST", "/api/v1/toolsets/"+toolsetID+"/publish", nil, withMatch(toolset, idem("oauth-publish")), 200)
	keyResponse := h.want(owner, "POST", "/api/v1/api-keys", personalOAuthKeyInput{Name: "OAuth caller", ProjectID: project, Scopes: []string{"tools"}, AllowedToolsets: []string{toolsetID}}, idem("oauth-key"), 201)
	var key mcpPersonalKey
	if err := json.Unmarshal(mustJSON(keyResponse), &key); err != nil {
		t.Fatal(err)
	}
	return personalOAuthFixture{h: h, owner: owner, upstream: authority, serverID: id, toolsetID: toolsetID, alias: tool.Alias, key: key}
}

func (f personalOAuthFixture) claim(t *testing.T) string {
	t.Helper()
	return f.claimKey(t, f.key)
}

func (f personalOAuthFixture) claimKey(t *testing.T, key mcpPersonalKey) string {
	t.Helper()
	response := f.h.want(f.owner, "POST", "/api/v1/mcp-servers/"+f.serverID+"/grant-enrollments", struct {
		APIKeyID string `json:"api_key_id"`
	}{key.ID}, nil, 200)
	var handoff mcpgrants.Handoff
	if err := json.Unmarshal(mustJSON(response), &handoff); err != nil {
		t.Fatal(err)
	}
	_, fragment, _ := strings.Cut(handoff.EnrollmentURL, "#")
	id, token, _ := strings.Cut(fragment, ".")
	f.h.want(f.owner, "POST", "/api/v1/mcp-grant-enrollments/claim", mcpPersonalClaimInput{EnrollmentID: id, HandoffToken: token}, nil, 200)
	return id
}

func TestMCPPersonalOAuthPKCECompletesAndRunsTool(t *testing.T) {
	f := newPersonalOAuthFixture(t)
	id := f.claim(t)
	authorization := f.authorize(t, id, mcpgrants.AuthorizationCode)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get(authorization.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	callback, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	f.h.want(f.owner, "POST", "/api/v1/mcp-grant-enrollments/"+id+"/complete", personalOAuthCompletionInput{Code: callback.Query().Get("code"), State: callback.Query().Get("state")}, nil, 200)
	f.h.want(f.owner, "POST", "/api/v1/mcp-grant-enrollments/"+id+"/complete", personalOAuthCompletionInput{Code: callback.Query().Get("code"), State: callback.Query().Get("state")}, nil, 409)
	handler := f.consumer(t)
	assertPersonalCall(t, handler, f, f.key, "pkce-dispatch", http.StatusOK, "acct-reference")
	standing := f.h.want(f.owner, "GET", "/api/v1/mcp-user-grants", nil, nil, 200)
	if mcpExecItem(t, mcpExecItems(t, standing, "items"), 0, standing)["state"] != "active" {
		t.Fatalf("grant did not become active: %v", standing)
	}
}

func (f personalOAuthFixture) consumer(t *testing.T) http.Handler {
	t.Helper()
	valkey, prefix, _ := acctKeyspace(t)
	limiter, err := limits.New(valkey, strings.TrimSuffix(prefix, ":"))
	if err != nil {
		t.Fatal(err)
	}
	sessions, sessionErr := mcp.NewSessionStore(valkey, strings.TrimSuffix(prefix, ":"))
	if sessionErr != nil {
		t.Fatal(sessionErr)
	}
	server := &mcp.Server{Runtime: f.h.Runtime, Egress: &f.h.Policy, Auth: f.h.AuthKey, Grants: &mcpgrants.Store{Pool: f.h.Pool, Keys: f.h.Server.Keys, Auth: f.h.AuthKey, Installation: f.h.Server.Installation, Egress: &f.h.Policy}, Limiter: limiter, Sessions: sessions, Ledger: &mcp.DurableLedger{Pool: f.h.Pool, Store: &limits.DebitStore{Pool: f.h.Pool, Limiter: limiter}, Auth: f.h.AuthKey}}
	mux := http.NewServeMux()
	server.Register(mux)
	if err := f.h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	return mux
}

func personalCall(handler http.Handler, f personalOAuthFixture, key mcpPersonalKey, id string) (int, string) {
	request := httptest.NewRequest(http.MethodPost, "/v1/mcp/tools/call", bytes.NewReader(mustJSON(struct {
		ToolsetID string   `json:"toolset_id"`
		Name      string   `json:"name"`
		Arguments struct{} `json:"arguments"`
		CallID    string   `json:"call_id"`
	}{ToolsetID: f.toolsetID, Name: f.alias, CallID: id})))
	request.Header.Set("Authorization", "Bearer "+key.Secret)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response.Code, response.Body.String()
}

func assertPersonalCall(t *testing.T, handler http.Handler, f personalOAuthFixture, key mcpPersonalKey, id string, want int, account string) {
	t.Helper()
	status, body := personalCall(handler, f, key, id)
	if status != want {
		t.Fatalf("personal call status %d, want %d: %s", status, want, body)
	}
	if account != "" {
		var result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal([]byte(body), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Content) != 1 || result.Content[0].Text != account {
			t.Fatalf("personal call returned another account: %s", body)
		}
	}
}

func (f personalOAuthFixture) completePKCE(t *testing.T, id string, identity testutil.OAuthIdentity) {
	t.Helper()
	f.upstream.SignInAs(identity)
	authorization := f.authorize(t, id, mcpgrants.AuthorizationCode)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get(authorization.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	callback, parseErr := url.Parse(response.Header.Get("Location"))
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	f.h.want(f.owner, "POST", "/api/v1/mcp-grant-enrollments/"+id+"/complete", personalOAuthCompletionInput{Code: callback.Query().Get("code"), State: callback.Query().Get("state")}, nil, 200)
}

func TestMCPPersonalOAuthDeviceApprovalDenialAndPolling(t *testing.T) {
	f := newPersonalOAuthFixture(t)
	handler := f.consumer(t)
	id := f.claim(t)
	authorization := f.authorize(t, id, mcpgrants.DeviceCode)
	challenge := authorization.Device
	if challenge == nil {
		t.Fatal("authorization did not return a device challenge")
	}
	poll := func() mcpgrants.Authorization { return f.poll(t, id) }
	if poll().State != mcpgrants.Pending {
		t.Fatal("unapproved device became active")
	}
	before := f.upstream.DevicePolls()
	poll()
	if f.upstream.DevicePolls() != before {
		t.Fatal("poll lease allowed an early token request")
	}
	if _, err := f.h.Pool.Exec(t.Context(), `UPDATE olp.mcp_grant_enrollments SET poll_at=now()-interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	poll()
	var interval int64
	if err := f.h.Pool.QueryRow(t.Context(), `SELECT poll_interval FROM olp.mcp_grant_enrollments WHERE id=$1`, id).Scan(&interval); err != nil {
		t.Fatal(err)
	}
	if interval <= int64(testutil.DeviceInterval/time.Second) {
		t.Fatal("slow_down did not extend the polling interval")
	}
	testutil.DecideDevice(t, challenge.VerificationURL, challenge.UserCode, "approve")
	if _, err := f.h.Pool.Exec(t.Context(), `UPDATE olp.mcp_grant_enrollments SET poll_at=now()-interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if poll().State != mcpgrants.Active {
		t.Fatal("approved device did not become active")
	}
	assertPersonalCall(t, handler, f, f.key, "device-approved", http.StatusOK, "acct-reference")
	deniedID := f.claim(t)
	denied := f.authorize(t, deniedID, mcpgrants.DeviceCode)
	deniedChallenge := denied.Device
	if deniedChallenge == nil {
		t.Fatal("authorization did not return a device challenge")
	}
	testutil.DecideDevice(t, deniedChallenge.VerificationURL, deniedChallenge.UserCode, "deny")
	if f.poll(t, deniedID).State != mcpgrants.Denied {
		t.Fatal("denied device did not report denied standing")
	}
	expiredID := f.claim(t)
	f.authorize(t, expiredID, mcpgrants.DeviceCode)
	f.upstream.ExpireDevices()
	if f.poll(t, expiredID).State != mcpgrants.Expired {
		t.Fatal("expired upstream device remained pending")
	}

}

func TestMCPPersonalOAuthRefreshSerializesAndKeepsIndividualsSeparate(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	resume := sync.OnceFunc(func() { close(release) })
	var refreshes atomic.Int64
	f := newPersonalOAuthFixtureWithHook(t, func(r *http.Request) {
		if r.ParseForm() != nil || r.Form.Get("grant_type") != "refresh_token" {
			return
		}
		refreshes.Add(1)
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	})
	t.Cleanup(resume)
	firstID := f.claim(t)
	f.completePKCE(t, firstID, testutil.OAuthIdentity{Subject: "first", Account: "first-account"})
	server := f.h.want(f.owner, "GET", "/api/v1/mcp-servers/"+f.serverID, nil, nil, 200)
	secondResponse := f.h.want(f.owner, "POST", "/api/v1/api-keys", personalOAuthKeyInput{Name: "Second OAuth caller", ProjectID: mcpExecString(t, server, "project_id"), Scopes: []string{"tools"}, AllowedToolsets: []string{f.toolsetID}}, idem("second-oauth-key"), 201)
	var second mcpPersonalKey
	if err := json.Unmarshal(mustJSON(secondResponse), &second); err != nil {
		t.Fatal(err)
	}
	secondID := f.claimKey(t, second)
	f.completePKCE(t, secondID, testutil.OAuthIdentity{Subject: "second", Account: "second-account"})
	handler := f.consumer(t)
	assertPersonalCall(t, handler, f, second, "before-refresh-second", http.StatusOK, "second-account")
	firstSession := f.session(t, handler, f.key)
	secondSession := f.session(t, handler, second)
	if firstSession == secondSession {
		t.Fatal("individuals shared an MCP session")
	}
	stolen := personalRPC(handler, second, "/mcp", firstSession, "tools/list", json.RawMessage(`{}`))
	if stolen.Code != http.StatusNotFound {
		t.Fatalf("another individual adopted a session: %d", stolen.Code)
	}

	if _, err := f.h.Pool.Exec(t.Context(), `UPDATE olp.mcp_user_grants SET expires_at=now()-interval '1 second' WHERE enrollment_id=$1`, firstID); err != nil {
		t.Fatal(err)
	}
	type callResult struct {
		status int
		body   string
	}
	results := make(chan callResult, 4)
	for n := range 4 {
		go func(index int) {
			status, body := personalCall(handler, f, f.key, fmt.Sprintf("concurrent-refresh-%d", index))
			results <- callResult{status, body}
		}(n)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not reach token endpoint")
	}
	tx, beginErr := f.h.Pool.Begin(t.Context())
	if beginErr != nil {
		t.Fatal(beginErr)
	}
	if _, err := tx.Exec(t.Context(), `SET LOCAL lock_timeout='500ms'`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `SELECT id FROM olp.installation WHERE singleton FOR UPDATE`); err != nil {
		t.Errorf("token exchange held the installation mutation lock: %v", err)
	}
	if _, err := tx.Exec(t.Context(), `SELECT id FROM olp.mcp_user_grants WHERE enrollment_id=$1 FOR UPDATE`, firstID); err != nil {
		t.Errorf("token exchange held the grant row lock: %v", err)
	}
	_ = tx.Rollback(t.Context())
	assertPersonalCall(t, handler, f, second, "during-refresh-second", http.StatusOK, "second-account")
	resume()
	for range 4 {
		select {
		case result := <-results:
			if result.status != http.StatusOK || !strings.Contains(result.body, `"text":"first-account"`) {
				t.Errorf("refresh crossed account or refused: %d %s", result.status, result.body)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("concurrent refresh did not finish")
		}
	}
	if refreshes.Load() != 1 || len(f.upstream.Reused()) != 0 {
		t.Fatal("concurrent callers repeated a rotating refresh")
	}
	f.upstream.RevokeRefreshTokens()
	if _, err := f.h.Pool.Exec(t.Context(), `UPDATE olp.mcp_user_grants SET expires_at=now()-interval '1 second' WHERE enrollment_id=$1`, firstID); err != nil {
		t.Fatal(err)
	}
	assertPersonalCall(t, handler, f, f.key, "revoked-refresh", http.StatusForbidden, "")
	assertPersonalCall(t, handler, f, f.key, "revoked-refresh-again", http.StatusForbidden, "")
	if refreshes.Load() != 2 {
		t.Fatal("failed refresh was retried")
	}
}

func TestMCPPersonalOAuthRefusesForeignCompletionAndTokenRedirect(t *testing.T) {
	f := newPersonalOAuthFixture(t)
	hits := &atomic.Int64{}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(http.StatusTeapot) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	registered := f.h.want(f.owner, "GET", "/api/v1/mcp-servers/"+f.serverID, nil, nil, 200)
	var current struct {
		Name     string           `json:"name"`
		Endpoint string           `json:"endpoint"`
		OAuth    routing.MCPOAuth `json:"oauth"`
	}
	if err := json.Unmarshal(mustJSON(registered), &current); err != nil {
		t.Fatal(err)
	}
	changed := routing.MCPOAuth{ClientID: current.OAuth.ClientID, Audience: current.OAuth.Audience, Scopes: current.OAuth.Scopes, AuthorizationEndpoint: current.OAuth.AuthorizationEndpoint, TokenEndpoint: redirect.URL, DeviceAuthorizationEndpoint: current.OAuth.DeviceAuthorizationEndpoint}
	f.h.want(f.owner, "PUT", "/api/v1/mcp-servers/"+f.serverID, struct {
		Name     string           `json:"name"`
		Endpoint string           `json:"endpoint"`
		Enabled  bool             `json:"enabled"`
		OAuth    routing.MCPOAuth `json:"oauth"`
	}{current.Name, current.Endpoint, true, changed}, withMatch(registered, idem("redirecting-oauth")), 200)
	id := f.claim(t)
	authorization := f.authorize(t, id, mcpgrants.AuthorizationCode)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get(authorization.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	callback, parseErr := url.Parse(response.Header.Get("Location"))
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	other := f.h.invite(f.owner, "foreign-oauth@example.test", "viewer")
	completion := personalOAuthCompletionInput{Code: callback.Query().Get("code"), State: callback.Query().Get("state")}
	f.h.want(other, "POST", "/api/v1/mcp-grant-enrollments/"+id+"/complete", completion, nil, 404)
	f.h.want(f.owner, "POST", "/api/v1/mcp-grant-enrollments/"+id+"/complete", personalOAuthCompletionInput{Code: callback.Query().Get("code"), State: "wrong"}, nil, 422)
	f.h.want(f.owner, "POST", "/api/v1/mcp-grant-enrollments/"+id+"/complete", completion, nil, 422)
	if hits.Load() != 0 {
		t.Fatal("token exchange followed a redirect")
	}
	f.h.want(f.owner, "POST", "/api/v1/mcp-grant-enrollments/"+id+"/complete", completion, nil, 409)
}

func TestMCPPersonalOAuthRefreshRejectsAbandonedAttempt(t *testing.T) {
	attempts := &atomic.Int64{}
	f := newPersonalOAuthFixtureWithHook(t, func(r *http.Request) {
		if r.ParseForm() == nil && r.Form.Get("grant_type") == "refresh_token" {
			attempts.Add(1)
		}
	})
	id := f.claim(t)
	f.completePKCE(t, id, testutil.OAuthIdentity{Subject: "abandoned", Account: "abandoned"})
	if _, err := f.h.Pool.Exec(t.Context(), `UPDATE olp.mcp_user_grants SET expires_at=now()-interval '1 second',refresh_attempt_id=$2 WHERE enrollment_id=$1`, id, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	assertPersonalCall(t, f.consumer(t), f, f.key, "abandoned-refresh", http.StatusForbidden, "")
	if attempts.Load() != 0 {
		t.Fatal("an abandoned rotating token was reused")
	}
}

func personalRPC(handler http.Handler, key mcpPersonalKey, path, session, method string, params json.RawMessage) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(mustJSON(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}{"2.0", 1, method, params})))
	request.Header.Set("Authorization", "Bearer "+key.Secret)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Mcp-Protocol-Version", "2025-11-25")
	if session != "" {
		request.Header.Set("Mcp-Session-Id", session)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func (f personalOAuthFixture) session(t *testing.T, handler http.Handler, key mcpPersonalKey) string {
	t.Helper()
	response := personalRPC(handler, key, "/mcp?toolset_id="+f.toolsetID, "", "initialize", mustJSON(mcpsdk.InitializeParams{Capabilities: &mcpsdk.ClientCapabilities{}, ClientInfo: &mcpsdk.Implementation{Name: "personal-fixture", Version: "1"}, ProtocolVersion: "2025-11-25"}))
	if response.Code != http.StatusOK || response.Header().Get("Mcp-Session-Id") == "" {
		t.Fatalf("personal session failed: %d %s", response.Code, response.Body.String())
	}
	return response.Header().Get("Mcp-Session-Id")
}

func (f personalOAuthFixture) authorize(t *testing.T, id string, flow mcpgrants.Flow) mcpgrants.Authorization {
	t.Helper()
	response := f.h.want(f.owner, "POST", "/api/v1/mcp-grant-enrollments/"+id+"/authorize", personalOAuthAuthorizeInput{Flow: flow}, nil, http.StatusOK)
	var authorization mcpgrants.Authorization
	if err := json.Unmarshal(mustJSON(response), &authorization); err != nil {
		t.Fatal(err)
	}
	return authorization
}

func (f personalOAuthFixture) poll(t *testing.T, id string) mcpgrants.Authorization {
	t.Helper()
	response := f.h.want(f.owner, "POST", "/api/v1/mcp-grant-enrollments/"+id+"/poll", nil, nil, http.StatusOK)
	var authorization mcpgrants.Authorization
	if err := json.Unmarshal(mustJSON(response), &authorization); err != nil {
		t.Fatal(err)
	}
	return authorization
}

type personalDelegatedClaims struct {
	jwt.RegisteredClaims
	ServerID  string `json:"olp_server_id"`
	ProjectID string `json:"olp_project_id"`
}

func TestMCPDelegatedIdentityIsAudienceBoundAndRecorded(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, marshalErr := x509.MarshalPKCS8PrivateKey(private)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	material := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}))
	const issuer = "https://gateway.example.test"
	const audience = "https://tools.example.test/first"
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if _, parseErr := jwt.Parse(token, func(*jwt.Token) (any, error) { return public, nil }, jwt.WithIssuer(issuer), jwt.WithAudience("https://tools.example.test/second"), jwt.WithValidMethods([]string{"EdDSA"})); parseErr == nil {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer peer.Close()
	fixture := newPersonalOAuthFixtureWithCall(t, nil, personalOAuthCallbacks{Call: func(r *http.Request) mcpsdk.CallToolResult {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		claims := &personalDelegatedClaims{}
		if _, parseErr := jwt.ParseWithClaims(token, claims, func(*jwt.Token) (any, error) { return public, nil }, jwt.WithIssuer(issuer), jwt.WithAudience(audience), jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithExpirationRequired()); parseErr != nil {
			return mcpsdk.CallToolResult{IsError: true}
		}
		request, requestErr := http.NewRequestWithContext(r.Context(), http.MethodGet, peer.URL, nil)
		if requestErr != nil {
			t.Error(requestErr)
			return mcpsdk.CallToolResult{IsError: true}
		}
		request.Header.Set("Authorization", r.Header.Get("Authorization"))
		response, callErr := http.DefaultClient.Do(request)
		if callErr != nil {
			t.Error(callErr)
			return mcpsdk.CallToolResult{IsError: true}
		}
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Error("delegated token was accepted by another audience")
		}
		return mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: claims.Subject}}}
	}})
	current := fixture.h.want(fixture.owner, "GET", "/api/v1/mcp-servers/"+fixture.serverID, nil, nil, 200)
	var definition mcpservers.Definition
	if decodeErr := json.Unmarshal(mustJSON(current), &definition); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	enabled := true
	updated := fixture.h.want(fixture.owner, "PUT", "/api/v1/mcp-servers/"+fixture.serverID, mcpservers.Input{Name: definition.Name, Endpoint: definition.Endpoint, Enabled: &enabled, OAuth: definition.OAuth, UnitPrice: definition.UnitPrice, Delegation: &routing.MCPDelegation{Issuer: issuer, Audience: audience}, DelegationPrivateKey: &material}, withMatch(current, idem("delegation")), 200)
	publication := fixture.h.want(fixture.owner, "GET", "/api/v1/toolsets/"+fixture.toolsetID, nil, nil, 200)
	tool := agentcontract.Tool{ServerID: fixture.serverID, RevisionID: mcpExecString(t, updated, "revision_id"), CatalogDigest: definition.Catalog.Digest, Name: "identity", Alias: fixture.alias}
	revised := fixture.h.want(fixture.owner, "PUT", "/api/v1/toolsets/"+fixture.toolsetID, personalOAuthToolsetInput{ProjectID: definition.ProjectID, Name: "OAuth tools", Toolset: agentcontract.Toolset{Tools: []agentcontract.Tool{tool}}}, withMatch(publication, idem("delegated-toolset")), 200)
	fixture.h.want(fixture.owner, "POST", "/api/v1/toolsets/"+fixture.toolsetID+"/publish", nil, withMatch(revised, idem("delegated-publish")), 200)
	handler := fixture.consumer(t)
	assertPersonalCall(t, handler, fixture, fixture.key, "delegated-missing-grant", http.StatusForbidden, "")
	fixture.completePKCE(t, fixture.claim(t), testutil.OAuthIdentity{Subject: "first", Account: "first-account"})
	assertPersonalCall(t, handler, fixture, fixture.key, "delegated-json", http.StatusOK, "api_key:"+fixture.key.ID)
	session := fixture.session(t, handler, fixture.key)
	initializedRequest := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(mustJSON(struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
	}{JSONRPC: "2.0", Method: "notifications/initialized"})))
	initializedRequest.Header.Set("Authorization", "Bearer "+fixture.key.Secret)
	initializedRequest.Header.Set("Content-Type", "application/json")
	initializedRequest.Header.Set("Accept", "application/json, text/event-stream")
	initializedRequest.Header.Set("Mcp-Protocol-Version", "2025-11-25")
	initializedRequest.Header.Set("Mcp-Session-Id", session)
	initializedResponse := httptest.NewRecorder()
	handler.ServeHTTP(initializedResponse, initializedRequest)
	if initializedResponse.Code != http.StatusAccepted {
		t.Fatalf("session initialization failed: %d %s", initializedResponse.Code, initializedResponse.Body.String())
	}

	params := func(id string) json.RawMessage {
		return mustJSON(struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Meta      struct {
				CallID string `json:"olp.call_id"`
			} `json:"_meta"`
		}{Name: fixture.alias, Arguments: json.RawMessage(`{}`), Meta: struct {
			CallID string `json:"olp.call_id"`
		}{CallID: id}})
	}
	rpc := personalRPC(handler, fixture.key, "/mcp", session, "tools/call", params("delegated-mcp"))
	if rpc.Code != http.StatusOK || !strings.Contains(rpc.Body.String(), "api_key:"+fixture.key.ID) {
		t.Fatalf("MCP delegation failed: %d %s", rpc.Code, rpc.Body.String())
	}
	if _, err := fixture.h.Pool.Exec(t.Context(), "UPDATE olp.mcp_user_grants SET revoked_at=now() WHERE binding->'identity'->>'id'=$1", fixture.key.ID); err != nil {
		t.Fatal(err)
	}
	assertPersonalCall(t, handler, fixture, fixture.key, "delegated-revoked-grant", http.StatusForbidden, "")
	refused := personalRPC(handler, fixture.key, "/mcp", session, "tools/call", params("delegated-mcp-revoked"))
	if refused.Code != http.StatusOK || !strings.Contains(refused.Body.String(), "user_grant_required") {
		t.Fatalf("MCP reused a revoked grant: %d %s", refused.Code, refused.Body.String())
	}
	attempts := fixture.h.want(fixture.owner, "GET", "/api/v1/mcp-tool-calls", nil, nil, 200)
	var records struct {
		Items []mcp.ToolCall `json:"items"`
	}
	if decodeErr := json.Unmarshal(mustJSON(attempts), &records); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if len(records.Items) != 5 {
		t.Fatalf("missing identity attempts: %v", records.Items)
	}
	for _, attempt := range records.Items {
		if len(attempt.IdentityDecisions) != 3 {
			t.Fatalf("missing bounded identity decisions: %v", attempt)
		}
		for _, decision := range attempt.IdentityDecisions {
			expected := func() mcp.IdentityOutcome {
				if attempt.Status == mcp.StatusGuardrailBlocked && decision.Hook != mcp.RegisteredServerHook {
					return mcp.IdentityDenied
				}
				return mcp.IdentityPassed
			}()
			if decision.Outcome != expected {
				t.Fatalf("incorrect identity outcome for %s: %v", attempt.Status, decision)
			}
		}
	}
	exported := fixture.h.want(fixture.owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	encodedExport := string(mustJSON(exported))
	if !strings.Contains(encodedExport, issuer) || !strings.Contains(encodedExport, audience) || strings.Contains(encodedExport, "BEGIN PRIVATE KEY") || strings.Contains(encodedExport, "delegation_key_id") {
		t.Fatal("portable delegation lost metadata or leaked its local key")
	}
	currentKey := fixture.h.want(fixture.owner, "GET", "/api/v1/api-keys/"+fixture.key.ID, nil, nil, http.StatusOK)
	fixture.h.want(fixture.owner, "POST", "/api/v1/api-keys/"+fixture.key.ID+"/revoke", nil, withMatch(currentKey, idem("delegated-revoke-key")), http.StatusOK)
	if err := fixture.h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertPersonalCall(t, handler, fixture, fixture.key, "delegated-revoked-key", http.StatusUnauthorized, "")
	if strings.Contains(string(mustJSON(attempts)), "BEGIN PRIVATE KEY") {
		t.Fatal("attempt records contain signing material")
	}
}

func TestMCPGrantRevocationDuringCatalogCheckStopsDispatch(t *testing.T) {
	gate := make(chan func() error, 1)
	var calls atomic.Int64
	fixture := newPersonalOAuthFixtureWithCall(t, nil, personalOAuthCallbacks{
		Catalog: func() {
			select {
			case revoke := <-gate:
				if err := revoke(); err != nil {
					t.Error(err)
				}
			default:
			}
		},
		Call: func(*http.Request) mcpsdk.CallToolResult {
			calls.Add(1)
			return mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "dispatched"}}}
		},
	})
	fixture.completePKCE(t, fixture.claim(t), testutil.OAuthIdentity{Subject: "individual", Account: "individual-account"})
	gate <- func() error {
		_, err := fixture.h.Pool.Exec(t.Context(), "UPDATE olp.mcp_user_grants SET revoked_at=now() WHERE binding->'identity'->>'id'=$1", fixture.key.ID)
		return err
	}
	assertPersonalCall(t, fixture.consumer(t), fixture, fixture.key, "revoked-during-discovery", http.StatusForbidden, "")
	if calls.Load() != 0 {
		t.Fatal("a revoked individual grant reached the tool")
	}
}

type entraTestProfile struct {
	Issuer string `json:"issuer"`
}
type entraTestOAuth struct {
	ClientID      string           `json:"client_id"`
	Audience      string           `json:"audience"`
	Scopes        []string         `json:"scopes"`
	TokenEndpoint string           `json:"token_endpoint"`
	EntraOBO      entraTestProfile `json:"entra_obo"`
}
type entraTestRegistration struct {
	Name              string         `json:"name"`
	Endpoint          string         `json:"endpoint"`
	Enabled           bool           `json:"enabled"`
	OAuth             entraTestOAuth `json:"oauth"`
	EntraClientSecret string         `json:"entra_client_secret,omitempty"`
}
type entraTestClaims struct {
	Issuer       string `json:"iss"`
	Audience     string `json:"aud"`
	Subject      string `json:"sub"`
	ObjectID     string `json:"oid"`
	Scopes       string `json:"scp,omitempty"`
	IdentityType string `json:"idtyp,omitempty"`
	IssuedAt     int64  `json:"iat"`
	Expires      int64  `json:"exp"`
}

func TestMCPEntraOBORequiresDelegatedUserIdentity(t *testing.T) {
	var calls atomic.Int64
	f := newPersonalOAuthFixtureWithCall(t, nil, personalOAuthCallbacks{Call: func(r *http.Request) mcpsdk.CallToolResult {
		calls.Add(1)
		value := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer obo-access-")
		return mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: value}}}
	}})
	current := f.h.want(f.owner, "GET", "/api/v1/mcp-servers/"+f.serverID, nil, nil, 200)
	project := mcpExecString(t, current, "project_id")
	templates := f.h.want(f.owner, "GET", "/api/v1/projects/"+project+"/limit-templates", nil, nil, 200)
	f.h.want(f.owner, "PUT", "/api/v1/projects/"+project+"/limit-templates", struct {
		Templates map[string]struct {
			RequestsPerMinute int `json:"requests_per_minute"`
		} `json:"templates"`
	}{Templates: map[string]struct {
		RequestsPerMinute int `json:"requests_per_minute"`
	}{"worker": {RequestsPerMinute: 100}}}, etagHeader(templates), 200)
	public, private, keyErr := ed25519.GenerateKey(rand.Reader)
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: public, KeyID: "entra", Algorithm: "EdDSA", Use: "sig"}}})
	}))
	defer issuer.Close()
	const clientID = "middle-tier-fixture"
	userClaim := "/oid"
	f.h.want(f.owner, "POST", "/api/v1/workload-issuers", workload.Config{Name: "Entra fixture", Issuer: issuer.URL, JWKSURL: issuer.URL, Enabled: true, Audiences: []string{clientID, "other-middle-tier"}, Algorithms: []string{"EdDSA"}, MaxLifetimeSeconds: 600, DisabledKeyIDs: []string{}, Mappings: []workload.Mapping{{Name: "users", Match: map[string]string{}, ProjectID: project, LimitTemplate: "worker", RouteGroups: []string{}, AllowedToolsets: []string{f.toolsetID}, AllowedAgents: []string{}, Scopes: []string{"tools"}, EndUserClaim: &userClaim}}}, idem("entra-issuer"), 201)
	f.h.want(f.owner, "POST", "/api/v1/workload-issuers", workload.Config{Name: "Other trusted workload", Issuer: issuer.URL + "/other", JWKSURL: issuer.URL, Enabled: true, Audiences: []string{clientID}, Algorithms: []string{"EdDSA"}, MaxLifetimeSeconds: 600, DisabledKeyIDs: []string{}, Mappings: []workload.Mapping{{Name: "users", Match: map[string]string{}, ProjectID: project, LimitTemplate: "worker", RouteGroups: []string{}, AllowedToolsets: []string{f.toolsetID}, AllowedAgents: []string{}, Scopes: []string{"tools"}, EndUserClaim: &userClaim}}}, idem("other-workload-issuer"), 201)
	var redirected atomic.Int64
	var redirectToken atomic.Bool
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1); w.WriteHeader(http.StatusNoContent) }))
	defer foreign.Close()
	var exchanges atomic.Int64
	var refreshes atomic.Int64
	tokenEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ParseForm() != nil || r.Form.Get("client_id") != clientID || r.Form.Get("client_secret") != "operator-client-secret" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if redirectToken.Load() {
			http.Redirect(w, r, foreign.URL, http.StatusTemporaryRedirect)
			return
		}
		subject := func() string {
			if r.Form.Get("grant_type") == "refresh_token" {
				refreshes.Add(1)
				return strings.TrimPrefix(r.Form.Get("refresh_token"), "obo-refresh-")
			}
			if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || r.Form.Get("requested_token_use") != "on_behalf_of" || r.Form.Get("scope") != "downstream-fixture/tools.read" {
				return ""
			}
			claims := &entraExchangeClaims{}
			if _, err := jwt.ParseWithClaims(r.Form.Get("assertion"), claims, func(*jwt.Token) (any, error) { return public, nil }, jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithIssuer(issuer.URL), jwt.WithAudience(clientID)); err != nil {
				return ""
			}
			exchanges.Add(1)
			return claims.ObjectID
		}()
		if subject == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Access  string `json:"access_token"`
			Refresh string `json:"refresh_token"`
			Kind    string `json:"token_type"`
			Expires int    `json:"expires_in"`
		}{"obo-access-" + subject, entraRefreshToken(subject), "Bearer", 3600})
	}))
	defer tokenEndpoint.Close()
	conflict := struct {
		entraTestRegistration
		Delegation *routing.MCPDelegation `json:"delegation"`
	}{entraTestRegistration: entraTestRegistration{Name: mcpExecString(t, current, "name"), Endpoint: mcpExecString(t, current, "endpoint"), Enabled: true, OAuth: entraTestOAuth{ClientID: clientID, Audience: "downstream-fixture", Scopes: []string{"downstream-fixture/tools.read"}, TokenEndpoint: tokenEndpoint.URL, EntraOBO: entraTestProfile{Issuer: issuer.URL}}, EntraClientSecret: "operator-client-secret"}, Delegation: &routing.MCPDelegation{Issuer: "https://delegation.invalid", Audience: "other-resource"}}
	f.h.want(f.owner, "PUT", "/api/v1/mcp-servers/"+f.serverID, conflict, withMatch(current, idem("entra-conflicting-authorization")), http.StatusUnprocessableEntity)
	updated := f.h.want(f.owner, "PUT", "/api/v1/mcp-servers/"+f.serverID, entraTestRegistration{Name: mcpExecString(t, current, "name"), Endpoint: mcpExecString(t, current, "endpoint"), Enabled: true, OAuth: entraTestOAuth{ClientID: clientID, Audience: "downstream-fixture", Scopes: []string{"downstream-fixture/tools.read"}, TokenEndpoint: tokenEndpoint.URL, EntraOBO: entraTestProfile{Issuer: issuer.URL}}, EntraClientSecret: "operator-client-secret"}, withMatch(current, idem("entra-registration")), 200)
	publication := f.h.want(f.owner, "GET", "/api/v1/toolsets/"+f.toolsetID, nil, nil, 200)
	tool := agentcontract.Tool{ServerID: f.serverID, RevisionID: mcpExecString(t, updated, "revision_id"), CatalogDigest: mcpExecString(t, mcpExecObject(t, updated, "catalog"), "digest"), Name: "identity", Alias: f.alias}
	revised := f.h.want(f.owner, "PUT", "/api/v1/toolsets/"+f.toolsetID, personalOAuthToolsetInput{ProjectID: project, Name: "OAuth tools", Toolset: agentcontract.Toolset{Tools: []agentcontract.Tool{tool}}}, withMatch(publication, idem("entra-toolset")), 200)
	f.h.want(f.owner, "POST", "/api/v1/toolsets/"+f.toolsetID+"/publish", nil, withMatch(revised, idem("entra-publish")), 200)
	handler := f.consumer(t)
	signer, signerErr := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: private}, (&jose.SignerOptions{}).WithHeader("kid", "entra"))
	if signerErr != nil {
		t.Fatal(signerErr)
	}
	now := time.Now()
	claims := entraTestClaims{Issuer: issuer.URL, Audience: clientID, Subject: "service-principal", ObjectID: "not-a-user", IdentityType: "app", IssuedAt: now.Unix(), Expires: now.Add(5 * time.Minute).Unix()}
	signed, signErr := signer.Sign(mustJSON(claims))
	if signErr != nil {
		t.Fatal(signErr)
	}
	bearer, encodeErr := signed.CompactSerialize()
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	assertPersonalCall(t, handler, f, mcpPersonalKey{Secret: bearer}, "entra-app-only", http.StatusForbidden, "")
	if exchanges.Load() != 0 {
		t.Fatal("app-only assertion reached the token endpoint")
	}
	for _, rejected := range []struct{ name, issuer, audience, scope, kind string }{
		{"generic-workload", issuer.URL, clientID, "", ""},
		{"application-with-scope", issuer.URL, clientID, "delegated.read", "app"},
		{"wrong-audience", issuer.URL, "other-middle-tier", "delegated.read", ""},
		{"wrong-issuer", issuer.URL + "/other", clientID, "delegated.read", ""},
	} {
		t.Run(rejected.name, func(t *testing.T) {
			claim := entraTestClaims{Issuer: rejected.issuer, Audience: rejected.audience, Subject: "rejected-user", ObjectID: "rejected-user", Scopes: rejected.scope, IdentityType: rejected.kind, IssuedAt: now.Unix(), Expires: now.Add(5 * time.Minute).Unix()}
			assertPersonalCall(t, handler, f, mcpPersonalKey{Secret: signEntraAssertion(t, signer, claim)}, "entra-"+rejected.name, http.StatusForbidden, "")
		})
	}
	assertPersonalCall(t, handler, f, f.key, "entra-api-key", http.StatusForbidden, "")
	labelRequest := httptest.NewRequest(http.MethodPost, "/v1/mcp/tools/call", bytes.NewReader(mustJSON(struct {
		ToolsetID string          `json:"toolset_id"`
		Name      string          `json:"name"`
		CallID    string          `json:"call_id"`
		Arguments json.RawMessage `json:"arguments"`
	}{f.toolsetID, f.alias, "entra-label", json.RawMessage(`{}`)})))
	labelRequest.Header.Set("Authorization", "Bearer "+f.key.Secret)
	labelRequest.Header.Set("Content-Type", "application/json")
	labelRequest.Header.Set("X-OLP-End-User", "user-one")
	labelResponse := httptest.NewRecorder()
	handler.ServeHTTP(labelResponse, labelRequest)
	if labelResponse.Code != http.StatusForbidden {
		t.Fatal("caller label established Entra identity")
	}
	if exchanges.Load() != 0 || calls.Load() != 0 {
		t.Fatal("untrusted or application identity reached exchange or dispatch")
	}
	userClaims := entraTestClaims{Issuer: issuer.URL, Audience: clientID, Subject: "user-one", ObjectID: "user-one", Scopes: "delegated.read", IssuedAt: now.Unix(), Expires: now.Add(5 * time.Minute).Unix()}
	user := mcpPersonalKey{Secret: signEntraAssertion(t, signer, userClaims)}
	assertPersonalCall(t, handler, f, user, "entra-user-json", http.StatusOK, "user-one")
	if exchanges.Load() != 1 {
		t.Fatalf("unexpected OBO exchanges: %d", exchanges.Load())
	}
	assertPersonalCall(t, handler, f, user, "entra-user-reuse", http.StatusOK, "user-one")
	if exchanges.Load() != 1 {
		t.Fatal("active OBO grant was exchanged again")
	}
	if _, err := f.h.Pool.Exec(t.Context(), "UPDATE olp.mcp_user_grants SET expires_at=now()-interval '1 second' WHERE server_id=$1", f.serverID); err != nil {
		t.Fatal(err)
	}
	assertPersonalCall(t, handler, f, user, "entra-user-refresh", http.StatusOK, "user-one")
	if refreshes.Load() != 1 || exchanges.Load() != 1 {
		t.Fatal("OBO grant did not reuse the shared refresh lifecycle")
	}
	session := f.session(t, handler, user)
	initializePersonalSession(t, handler, user, session)
	params := func(id string) json.RawMessage {
		return mustJSON(struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Meta      struct {
				CallID string `json:"olp.call_id"`
			} `json:"_meta"`
		}{Name: f.alias, Arguments: json.RawMessage(`{}`), Meta: struct {
			CallID string `json:"olp.call_id"`
		}{id}})
	}
	reply := personalRPC(handler, user, "/mcp", session, "tools/call", params("entra-user-mcp"))
	if reply.Code != http.StatusOK || !strings.Contains(reply.Body.String(), "user-one") {
		t.Fatalf("MCP OBO call failed: %d %s", reply.Code, reply.Body.String())
	}
	secondClaims := entraTestClaims{Issuer: issuer.URL, Audience: clientID, Subject: userClaims.Subject, ObjectID: "user-two", Scopes: "delegated.read", IssuedAt: now.Unix(), Expires: now.Add(5 * time.Minute).Unix()}
	second := mcpPersonalKey{Secret: signEntraAssertion(t, signer, secondClaims)}
	assertPersonalCall(t, handler, f, second, "entra-user-json", http.StatusOK, "user-two")
	beforeCalls := calls.Load()
	crossed := personalRPC(handler, second, "/mcp", session, "tools/call", params("entra-wrong-user"))
	if crossed.Code != http.StatusNotFound || exchanges.Load() != 2 || calls.Load() != beforeCalls {
		t.Fatal("another user adopted a session or caused an exchange")
	}
	assertPersonalCall(t, handler, f, second, "entra-second-user", http.StatusOK, "user-two")
	if exchanges.Load() != 2 {
		t.Fatal("a second user did not receive a separate grant")
	}
	if _, err := f.h.Pool.Exec(t.Context(), "UPDATE olp.mcp_user_grants SET expires_at=now()-interval '1 second' WHERE server_id=$1 AND refresh_secret_id IS NULL", f.serverID); err != nil {
		t.Fatal(err)
	}
	assertPersonalCall(t, handler, f, second, "entra-renew-assertion", http.StatusOK, "user-two")
	if exchanges.Load() != 3 {
		t.Fatal("a grant without refresh token did not renew using the verified assertion")
	}
	results := make(chan struct {
		subject string
		status  int
		body    string
	}, 2)
	for _, subject := range []string{"user-three", "user-four"} {
		claim := entraTestClaims{Issuer: issuer.URL, Audience: clientID, Subject: subject, ObjectID: subject, Scopes: "delegated.read", IssuedAt: now.Unix(), Expires: now.Add(5 * time.Minute).Unix()}
		key := mcpPersonalKey{Secret: signEntraAssertion(t, signer, claim)}
		go func() {
			status, body := personalCall(handler, f, key, "entra-concurrent-"+subject)
			results <- struct {
				subject string
				status  int
				body    string
			}{subject, status, body}
		}()
	}
	for range 2 {
		result := <-results
		var body struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal([]byte(result.body), &body) != nil || result.status != http.StatusOK || len(body.Content) != 1 || body.Content[0].Text != result.subject {
			t.Fatal("concurrent OBO users received another user's result")
		}
	}
	standing := f.h.want(f.owner, "GET", "/api/v1/mcp-user-grants", nil, nil, http.StatusOK)
	attempts := f.h.want(f.owner, "GET", "/api/v1/mcp-tool-calls", nil, nil, http.StatusOK)
	exported := f.h.want(f.owner, "GET", "/api/v1/configuration/export", nil, nil, http.StatusOK)
	for _, encoded := range []string{string(mustJSON(standing)), string(mustJSON(attempts)), string(mustJSON(exported)), string(mustJSON(updated))} {
		for _, secret := range []string{user.Secret, second.Secret, "operator-client-secret", "obo-access-", "obo-refresh-", "user-one", "user-two"} {
			if strings.Contains(encoded, secret) {
				t.Fatal("identity or token material escaped into durable operator output")
			}
		}
	}
	var document configuration.Document
	if err := json.Unmarshal(mustJSON(mcpExecObject(t, exported, "document")), &document); err != nil {
		t.Fatal(err)
	}
	f.h.want(f.owner, "POST", "/api/v1/configuration/apply", struct {
		Document configuration.Document `json:"document"`
	}{document}, idem("entra-metadata-apply"), http.StatusOK)
	if err := f.h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	var records struct {
		Items []mcp.ToolCall `json:"items"`
	}
	if err := json.Unmarshal(mustJSON(attempts), &records); err != nil {
		t.Fatal(err)
	}
	for _, record := range records.Items {
		assertEntraDecision(t, record, record.Status == "succeeded")
	}
	var listed struct {
		Items []mcpgrants.Standing `json:"items"`
	}
	if err := json.Unmarshal(mustJSON(standing), &listed); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 64; i++ {
		_ = f.session(t, handler, []mcpPersonalKey{user, second}[i%2])
	}
	for _, key := range []mcpPersonalKey{user, second} {
		blocked := personalRPC(handler, key, "/mcp?toolset_id="+f.toolsetID, "", "initialize", mustJSON(mcpsdk.InitializeParams{ProtocolVersion: "2025-11-25", ClientInfo: &mcpsdk.Implementation{Name: "capacity", Version: "1"}, Capabilities: &mcpsdk.ClientCapabilities{}}))
		if blocked.Code != http.StatusTooManyRequests {
			t.Fatalf("per-individual sessions escaped global principal cap: %d %s", blocked.Code, blocked.Body.String())
		}
	}
	authority, authErr := f.h.Runtime.Authenticate(second.Secret)
	if authErr != nil {
		t.Fatal(authErr)
	}
	index := slices.IndexFunc(listed.Items, func(item mcpgrants.Standing) bool { return item.Binding.Identity.ID == authority.EndUserDigest })
	if index < 0 || listed.Items[index].GrantID == nil {
		t.Fatal("OBO grant is absent from standing")
	}
	f.h.want(f.owner, "DELETE", "/api/v1/mcp-user-grants/"+*listed.Items[index].GrantID, nil, nil, http.StatusNoContent)
	beforeRevoke := calls.Load()
	beforeExchange := exchanges.Load()
	assertPersonalCall(t, handler, f, second, "entra-revoked", http.StatusForbidden, "")
	if calls.Load() != beforeRevoke || exchanges.Load() != beforeExchange {
		t.Fatal("revoked OBO grant was recreated or dispatched")
	}
	redirectToken.Store(true)
	redirectClaims := entraTestClaims{Issuer: issuer.URL, Audience: clientID, Subject: "redirect-user", ObjectID: "redirect-user", Scopes: "delegated.read", IssuedAt: now.Unix(), Expires: now.Add(5 * time.Minute).Unix()}
	assertPersonalCall(t, handler, f, mcpPersonalKey{Secret: signEntraAssertion(t, signer, redirectClaims)}, "entra-redirect", http.StatusForbidden, "")
	if redirected.Load() != 0 || calls.Load() != beforeRevoke {
		t.Fatal("redirected token exchange forwarded assertion or dispatched")
	}
	refused := f.h.want(f.owner, "GET", "/api/v1/mcp-tool-calls", nil, nil, http.StatusOK)
	var refusedRecords struct {
		Items []mcp.ToolCall `json:"items"`
	}
	if err := json.Unmarshal(mustJSON(refused), &refusedRecords); err != nil {
		t.Fatal(err)
	}
	for _, record := range refusedRecords.Items {
		assertEntraDecision(t, record, record.Status == "succeeded")
	}

	redirectToken.Store(false)
	exerciseEntraCredentialBoundary(t, f, handler, entraTestOAuth{ClientID: clientID, Audience: "downstream-fixture", Scopes: []string{"downstream-fixture/tools.read"}, TokenEndpoint: tokenEndpoint.URL, EntraOBO: entraTestProfile{Issuer: issuer.URL}}, user)
}

type entraExchangeClaims struct {
	jwt.RegisteredClaims
	ObjectID string `json:"oid"`
}

func signEntraAssertion(t *testing.T, signer jose.Signer, claims entraTestClaims) string {
	t.Helper()
	signed, err := signer.Sign(mustJSON(claims))
	if err != nil {
		t.Fatal(err)
	}
	token, encodeErr := signed.CompactSerialize()
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	return token
}

func initializePersonalSession(t *testing.T, handler http.Handler, key mcpPersonalKey, session string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	request.Header.Set("Authorization", "Bearer "+key.Secret)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Mcp-Protocol-Version", "2025-11-25")
	request.Header.Set("Mcp-Session-Id", session)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("initialization failed: %d %s", response.Code, response.Body.String())
	}
}

func entraRefreshToken(subject string) string {
	if subject == "user-two" {
		return ""
	}
	return "obo-refresh-" + subject
}

func assertEntraDecision(t *testing.T, record mcp.ToolCall, succeeded bool) {
	t.Helper()
	index := slices.IndexFunc(record.IdentityDecisions, func(decision mcp.IdentityDecision) bool { return decision.Hook == mcp.EntraOBOHook })
	if index < 0 {
		t.Fatal("Entra hook standing was not recorded")
	}
	want := func() mcp.IdentityOutcome {
		if succeeded {
			return mcp.IdentityPassed
		}
		return mcp.IdentityDenied
	}()
	if record.IdentityDecisions[index].Outcome != want {
		t.Fatalf("Entra decision = %s, want %s", record.IdentityDecisions[index].Outcome, want)
	}
}

func exerciseEntraCredentialBoundary(t *testing.T, f personalOAuthFixture, handler http.Handler, profile entraTestOAuth, user mcpPersonalKey) {
	t.Helper()
	var oldSecretRequests atomic.Int64
	var replacementRequests atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Form.Get("client_secret") == "operator-client-secret" {
			oldSecretRequests.Add(1)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.Form.Get("client_secret") != "replacement-client-secret" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		replacementRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Access  string `json:"access_token"`
			Kind    string `json:"token_type"`
			Expires int    `json:"expires_in"`
		}{"obo-access-user-one", "Bearer", 3600})
	}))
	defer destination.Close()
	for _, changed := range []struct {
		name    string
		profile entraTestOAuth
	}{
		{"token-endpoint", entraBoundaryProfile(profile, profile.ClientID, profile.EntraOBO.Issuer, destination.URL)},
		{"client", entraBoundaryProfile(profile, "another-middle-tier", profile.EntraOBO.Issuer, profile.TokenEndpoint)},
		{"issuer", entraBoundaryProfile(profile, profile.ClientID, profile.EntraOBO.Issuer+"/other", profile.TokenEndpoint)},
	} {
		current := f.h.want(f.owner, "GET", "/api/v1/mcp-servers/"+f.serverID, nil, nil, http.StatusOK)
		request := entraTestRegistration{Name: mcpExecString(t, current, "name"), Endpoint: mcpExecString(t, current, "endpoint"), Enabled: true, OAuth: changed.profile}
		f.h.want(f.owner, "PUT", "/api/v1/mcp-servers/"+f.serverID, request, withMatch(current, idem("entra-boundary-"+changed.name)), http.StatusUnprocessableEntity)
		exported := f.h.want(f.owner, "GET", "/api/v1/configuration/export", nil, nil, http.StatusOK)
		var document configuration.Document
		if err := json.Unmarshal(mustJSON(mcpExecObject(t, exported, "document")), &document); err != nil {
			t.Fatal(err)
		}
		name := mcpExecString(t, current, "name")
		entryIndex := slices.IndexFunc(document.MCPServers, func(entry configuration.MCPServerEntry) bool { return entry.Name == name })
		if entryIndex < 0 {
			t.Fatal("registered server missing from export")
		}
		entry := document.MCPServers[entryIndex]
		promotion := configuration.Document{APIVersion: document.APIVersion, Projects: document.Projects, Providers: document.Providers, Routes: document.Routes, Pricing: document.Pricing, MCPServers: []configuration.MCPServerEntry{entraBoundaryEntry(entry, changed.profile)}}
		input := struct {
			Document configuration.Document `json:"document"`
		}{promotion}
		plan := f.h.want(f.owner, "POST", "/api/v1/configuration/plan", input, nil, http.StatusOK)
		var result struct {
			Conflicts []json.RawMessage `json:"conflicts"`
		}
		if err := json.Unmarshal(mustJSON(plan), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Conflicts) == 0 {
			t.Fatal("promotion concealed the required Entra credential replacement")
		}
		f.h.want(f.owner, "POST", "/api/v1/configuration/apply", input, idem("entra-promotion-boundary-"+changed.name), http.StatusUnprocessableEntity)
	}
	current := f.h.want(f.owner, "GET", "/api/v1/mcp-servers/"+f.serverID, nil, nil, http.StatusOK)
	retained := f.h.want(f.owner, "PUT", "/api/v1/mcp-servers/"+f.serverID, entraTestRegistration{Name: mcpExecString(t, current, "name") + " retained", Endpoint: mcpExecString(t, current, "endpoint"), Enabled: true, OAuth: profile}, withMatch(current, idem("entra-retained-secret")), http.StatusOK)
	republishEntraBoundary(t, f, mcpExecString(t, retained, "project_id"), mcpExecString(t, retained, "revision_id"), mcpExecString(t, mcpExecObject(t, retained, "catalog"), "digest"))
	if _, err := f.h.Pool.Exec(t.Context(), "UPDATE olp.mcp_user_grants SET expires_at=now()-interval '1 minute' WHERE server_id=$1 AND revoked_at IS NULL", f.serverID); err != nil {
		t.Fatal(err)
	}
	assertPersonalCall(t, handler, f, user, "entra-retained-secret", http.StatusOK, "user-one")
	replacementCurrent := f.h.want(f.owner, "GET", "/api/v1/mcp-servers/"+f.serverID, nil, nil, http.StatusOK)
	replaced := f.h.want(f.owner, "PUT", "/api/v1/mcp-servers/"+f.serverID, entraTestRegistration{Name: mcpExecString(t, replacementCurrent, "name"), Endpoint: mcpExecString(t, replacementCurrent, "endpoint"), Enabled: true, OAuth: entraBoundaryProfile(profile, profile.ClientID, profile.EntraOBO.Issuer, destination.URL), EntraClientSecret: "replacement-client-secret"}, withMatch(replacementCurrent, idem("entra-replaced-boundary")), http.StatusOK)
	republishEntraBoundary(t, f, mcpExecString(t, replaced, "project_id"), mcpExecString(t, replaced, "revision_id"), mcpExecString(t, mcpExecObject(t, replaced, "catalog"), "digest"))
	assertPersonalCall(t, handler, f, user, "entra-replaced-secret", http.StatusOK, "user-one")
	if oldSecretRequests.Load() != 0 || replacementRequests.Load() != 1 {
		t.Fatal("credential followed an unapproved boundary or replacement was unusable")
	}
}

func entraBoundaryProfile(previous entraTestOAuth, client, issuer, endpoint string) entraTestOAuth {
	return entraTestOAuth{ClientID: client, Audience: previous.Audience, Scopes: previous.Scopes, TokenEndpoint: endpoint, EntraOBO: entraTestProfile{Issuer: issuer}}
}

func entraBoundaryEntry(previous configuration.MCPServerEntry, profile entraTestOAuth) configuration.MCPServerEntry {
	return configuration.MCPServerEntry{Name: previous.Name, Project: previous.Project, Transport: previous.Transport, Endpoint: previous.Endpoint, Enabled: previous.Enabled, CatalogDigest: previous.CatalogDigest, UnitPrice: previous.UnitPrice, ToolPrices: previous.ToolPrices, CallsPerMinute: previous.CallsPerMinute, MaxConcurrency: previous.MaxConcurrency, PerKeyCallsPerMinute: previous.PerKeyCallsPerMinute, PerKeyMaxConcurrency: previous.PerKeyMaxConcurrency, OAuth: &routing.MCPOAuth{ClientID: profile.ClientID, Audience: profile.Audience, Scopes: profile.Scopes, TokenEndpoint: profile.TokenEndpoint, EntraOBO: &routing.MCPEntraOBO{Issuer: profile.EntraOBO.Issuer}}}
}

func republishEntraBoundary(t *testing.T, f personalOAuthFixture, project, revision, digest string) {
	t.Helper()
	publication := f.h.want(f.owner, "GET", "/api/v1/toolsets/"+f.toolsetID, nil, nil, http.StatusOK)
	tool := agentcontract.Tool{ServerID: f.serverID, RevisionID: revision, CatalogDigest: digest, Name: "identity", Alias: f.alias}
	updated := f.h.want(f.owner, "PUT", "/api/v1/toolsets/"+f.toolsetID, personalOAuthToolsetInput{ProjectID: project, Name: "OAuth tools", Toolset: agentcontract.Toolset{Tools: []agentcontract.Tool{tool}}}, withMatch(publication, idem("entra-boundary-repin-"+revision)), http.StatusOK)
	f.h.want(f.owner, "POST", "/api/v1/toolsets/"+f.toolsetID+"/publish", nil, withMatch(updated, idem("entra-boundary-publish-"+revision)), http.StatusOK)
	if err := f.h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestMCPMalformedResultsRetainDispatchedOutcome(t *testing.T) {
	replies := make(chan json.RawMessage, 1)
	fixture := newPersonalOAuthFixtureWithCall(t, nil, personalOAuthCallbacks{Result: func() json.RawMessage { return <-replies }})
	fixture.completePKCE(t, fixture.claim(t), testutil.OAuthIdentity{Subject: "malformed-user", Account: "malformed-user"})
	handler := fixture.consumer(t)
	for i, raw := range []json.RawMessage{json.RawMessage(`{}`), json.RawMessage(`{"content":[{}]}`), json.RawMessage(`{"content":[{"type":"resource","resource":{"uri":"https://resource.example.test","text":"must not vanish"}}]}`)} {
		replies <- raw
		status, body := personalCall(handler, fixture, fixture.key, fmt.Sprintf("malformed-result-%d", i))
		var response struct {
			Status    mcp.Status `json:"status"`
			AttemptID string     `json:"attempt_id"`
		}
		if json.Unmarshal([]byte(body), &response) != nil || status != http.StatusBadGateway || response.Status != mcp.StatusOutcomeUnknown {
			t.Fatalf("invalid result became clean success: %d %s", status, body)
		}
		attempt := fixture.h.want(fixture.owner, "GET", "/api/v1/mcp-tool-calls/"+response.AttemptID, nil, nil, http.StatusOK)
		var record mcp.ToolCall
		if json.Unmarshal(mustJSON(attempt), &record) != nil || record.Status != mcp.StatusOutcomeUnknown {
			t.Fatalf("invalid result lost durable dispatched outcome: %v", attempt)
		}
	}
}
