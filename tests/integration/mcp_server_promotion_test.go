//go:build integration

package integration_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

func TestMCPServerPromotionRecertifiesReviewedCatalogAndReusesPinnedDestination(t *testing.T) {
	const sourceSecret = "source-mcp-private"
	const destinationSecret = "destination-mcp-private"
	var description atomic.Value
	description.Store("Reviewed tool")
	var requests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if token := r.Header.Get("Authorization"); token != "Bearer "+sourceSecret && token != "Bearer "+destinationSecret {
			w.WriteHeader(401)
			return
		}
		var request struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		json.NewDecoder(r.Body).Decode(&request)
		if request.ID == 0 {
			w.WriteHeader(202)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var result any
		if request.Method == "initialize" {
			result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}}
		} else {
			result = map[string]any{"tools": []any{map[string]any{"name": "lookup", "description": description.Load(), "inputSchema": map[string]any{"type": "object"}}}}
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	defer upstream.Close()
	source := newAccessHarness(t)
	owner := source.owner()
	project := createProject(source, owner, "MCP portable")
	created := source.want(owner, "POST", "/api/v1/mcp-servers", map[string]any{"project_id": project, "name": "Lookup", "transport": "streamable_http", "endpoint": upstream.URL, "enabled": true, "credential": sourceSecret}, idem(uuid.NewString()), 201)
	exported := source.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	document := exported["document"].(map[string]any)
	entry := document["mcp_servers"].([]any)[0].(map[string]any)
	ref := entry["credential_ref"].(string)
	encoded := string(mustJSON(document))
	if strings.Contains(encoded, sourceSecret) || strings.Contains(encoded, created["id"].(string)) || entry["catalog_digest"] != created["catalog"].(map[string]any)["digest"] {
		t.Fatal("artifact leaked identity/material or omitted reviewed digest")
	}
	destination := newAccessHarness(t)
	destOwner := destination.owner()
	input := map[string]any{"document": document}
	plan := destination.want(destOwner, "POST", "/api/v1/configuration/plan", input, nil, 200)
	if len(plan["blockers"].([]any)) == 0 {
		t.Fatal("unbound destination credential did not block")
	}
	input["secret_bindings"] = map[string]any{ref: destinationSecret}
	description.Store("Changed upstream tool")
	destination.want(destOwner, "POST", "/api/v1/configuration/apply", input, idem(uuid.NewString()), 422)
	var count int
	if err := destination.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.mcp_servers").Scan(&count); err != nil || count != 0 {
		t.Fatal("changed catalog partially applied")
	}
	description.Store("Reviewed tool")
	applyHeaders := idem(uuid.NewString())
	destination.want(destOwner, "POST", "/api/v1/configuration/apply", input, applyHeaders, 200)
	servers := destination.want(destOwner, "GET", "/api/v1/mcp-servers", nil, nil, 200)["items"].([]any)
	if len(servers) != 1 {
		t.Fatal("destination registration missing")
	}
	id := servers[0].(map[string]any)["id"].(string)
	path := "/api/v1/mcp-servers/" + id
	registered := destination.want(destOwner, "GET", path, nil, nil, 200)
	etag := registered["etag"]
	before := requests.Load()
	description.Store("New unapproved upstream tool")
	plan = destination.want(destOwner, "POST", "/api/v1/configuration/plan", input, nil, 200)
	for _, action := range plan["actions"].([]any) {
		item := action.(map[string]any)
		if item["kind"] == "mcp_server" && item["action"] != "reuse" {
			t.Fatal("unchanged pinned registration was not reused")
		}
	}
	destination.want(destOwner, "POST", "/api/v1/configuration/apply", input, idem(uuid.NewString()), 200)
	after := destination.want(destOwner, "GET", path, nil, nil, 200)
	if after["etag"] != etag || requests.Load() != before {
		t.Fatal("reuse recertified or changed pinned history")
	}
	if !strings.Contains(string(mustJSON(after)), "Reviewed tool") || strings.Contains(string(mustJSON(after)), "New unapproved") {
		t.Fatal("reuse adopted live upstream drift")
	}
	updateHeaders := etagHeader(after)
	updateHeaders["Idempotency-Key"] = uuid.NewString()
	changed := destination.want(destOwner, "PUT", path, map[string]any{
		"name": "Lookup", "endpoint": upstream.URL, "enabled": false,
	}, updateHeaders, 200)
	before = requests.Load()
	destination.want(destOwner, "POST", "/api/v1/configuration/apply", input, applyHeaders, 200)
	unchanged := destination.want(destOwner, "GET", path, nil, nil, 200)
	if unchanged["etag"] != changed["etag"] || requests.Load() != before {
		t.Fatal("completed promotion replay recertified or overwrote a later registration")
	}
	var redirectedCalls atomic.Int64
	redirected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedCalls.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("promotion forwarded a saved credential to a changed endpoint")
		}
		w.WriteHeader(503)
	}))
	defer redirected.Close()
	entry["endpoint"] = redirected.URL
	delete(input, "secret_bindings")
	destination.want(destOwner, "POST", "/api/v1/configuration/apply", input, idem(uuid.NewString()), 422)
	if redirectedCalls.Load() != 0 {
		t.Fatal("promotion contacted a changed endpoint without a new credential binding")
	}
	entry["endpoint"] = upstream.URL
	input["secret_bindings"] = map[string]any{ref: destinationSecret}
	upstream.Close()
	destination.want(destOwner, "POST", "/api/v1/configuration/apply", input, applyHeaders, 200)
}
