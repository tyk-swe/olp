//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/resources"
)

func researchFixture(t *testing.T) *codingPlanFixture {
	t.Helper()
	f := newCodingPlanFixture(t, "zai-coding", codeplans.ZAIProfile, "research-vendor-key")
	f.mix(t, "inference-vendor-key")
	f.h.Gateway.CodeResearch = &resources.CodeStore{Pool: f.h.Pool}
	f.h.Gateway.ResearchAuth = f.h.Gateway.CodeAuthorizer.(*providers.CodeAuthorizer)
	f.publishResearch(t, []string{"search", "reader", "zread"})
	f.peer.mu.Lock()
	f.peer.respond = func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/mcp/") {
			codingPlanResponse(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer research-vendor-key" || r.URL.RawQuery != "" {
			t.Errorf("upstream authorization or query: %v %s", r.Header, r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "DELETE" {
			w.WriteHeader(204)
			return
		}
		if r.Method == "GET" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "id: event-1\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progressToken\":1,\"progress\":1}}\n\n")
			return
		}
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string `json:"name"`
				Arguments struct {
					Query string `json:"query"`
				} `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if message.Method != "initialize" && r.Header.Get("Mcp-Session-Id") != "native-session" {
			t.Errorf("upstream session not translated: %v", r.Header)
		}
		switch message.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "native-session")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-11-25","serverInfo":{"name":"controlled","version":"1"},"capabilities":{"tools":{},"resources":{}}}}`, message.ID)
		case "tools/list":
			tools := []map[string]any{}
			for _, name := range []string{"webSearchPrime", "webReader", "search_doc", "get_repo_structure", "read_file", "vision"} {
				tools = append(tools, map[string]any{"name": name, "inputSchema": map[string]any{"type": "object"}, "outputSchema": map[string]any{"type": "object"}})
			}
			data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(message.ID), "result": map[string]any{"tools": tools}})
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "id: listing\nevent: message\ndata: %s\n\n", data)
		case "tools/call":
			switch message.Params.Arguments.Query {
			case "disconnect":
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close()
			case "limited":
				w.WriteHeader(429)
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"Research allowance exhausted"}}`)
			default:
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"https://example.com/source"}],"structuredContent":{"source":"https://example.com/source"}}}`, message.ID)
			}
		default:
			w.WriteHeader(202)
		}
	}
	f.peer.mu.Unlock()
	return f
}

func (f *codingPlanFixture) publishResearch(t *testing.T, services []string) {
	t.Helper()
	var tools any
	if len(services) != 0 {
		tools = map[string]any{"account_id": f.accounts[0]["id"], "services": services}
	}
	path := "/api/v1/code/routes/" + f.route["id"].(string)
	draft := f.h.want(f.owner, "PUT", path, map[string]any{"project_id": f.project, "slug": "qualification", "pool_id": f.pool["id"], "models": []string{"gpt-5.5"}, "enabled": true, "tools": tools}, etagHeader(f.route), 200)
	f.route = f.h.want(f.owner, "POST", path+"/publish", nil, withMatch(draft, idem("research-publish-"+draft["etag"].(string))), 200)
	f.h.refresh()
}

func (f *codingPlanFixture) research(t *testing.T, verb, service, key, session, body string, status int) (*http.Response, string) {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), verb, f.gateway.URL+"/code/qualification/mcp/"+service, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Mcp-Protocol-Version", "2025-11-25")
	if session != "" {
		r.Header.Set("Mcp-Session-Id", session)
	}
	response, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != status {
		t.Fatalf("%s %s: %d %s; want %d", verb, service, response.StatusCode, raw, status)
	}
	return response, string(raw)
}

const researchInit = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"fixture","version":"1"}}}`
const researchList = `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`

func TestCodeResearchGPTOnlyRouteAndNativeTransport(t *testing.T) {
	f := researchFixture(t)
	for service, tool := range map[string]string{"search": "webSearchPrime", "reader": "webReader", "zread": "read_file"} {
		response, raw := f.research(t, "POST", service, f.key, "", researchInit, 200)
		session := response.Header.Get("Mcp-Session-Id")
		if session == "" || session == "native-session" || strings.Contains(raw, `"resources"`) {
			t.Fatalf("unscoped session or capabilities: %v %s", response.Header, raw)
		}
		f.research(t, "POST", service, f.key, session, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, 202)
		_, raw = f.research(t, "POST", service, f.key, session, researchList, 200)
		if !strings.Contains(raw, `"name":"`+tool+`"`) || strings.Contains(raw, "vision") || !strings.Contains(raw, "outputSchema") {
			t.Fatalf("native discovery: %s", raw)
		}
		body := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"` + tool + `","arguments":{"query":"public docs"}}}`
		_, raw = f.research(t, "POST", service, f.key, session, body, 200)
		if !strings.Contains(raw, `"structuredContent":{"source":"https://example.com/source"}`) {
			t.Fatalf("native result: %s", raw)
		}
		f.research(t, "POST", service, f.key, session, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":3}}`, 202)
		_, raw = f.research(t, "GET", service, f.key, session, "", 200)
		if !strings.Contains(raw, "id:") || strings.Contains(raw, "id:event-1") || !strings.Contains(raw, "notifications/progress") {
			t.Fatalf("notification stream: %s", raw)
		}
		resume, _, _ := strings.Cut(strings.TrimPrefix(raw, "id:"), "\n")
		request, err := http.NewRequestWithContext(t.Context(), "GET", f.gateway.URL+"/code/qualification/mcp/"+service, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+f.key)
		request.Header.Set("Mcp-Session-Id", session)
		request.Header.Set("Last-Event-Id", resume)
		resumed, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resumed.Body)
		resumed.Body.Close()
		received := f.peer.received()
		if resumed.StatusCode != 200 || received[len(received)-1].Header.Get("Last-Event-Id") != "event-1" {
			t.Fatal("resume ID was not translated to its native value")
		}
		f.research(t, "DELETE", service, f.key, session, "", 204)
		f.research(t, "POST", service, f.key, session, researchList, 404)
	}
	var bindings, attempts int
	if err := f.h.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM olp.code_bindings),(SELECT count(*) FROM olp.code_attempts)`).Scan(&bindings, &attempts); err != nil || bindings != 0 || attempts != 0 {
		t.Fatalf("research created inference state: %d %d %v", bindings, attempts, err)
	}
	config := f.h.want(f.owner, "GET", "/api/v1/code/routes/"+f.route["id"].(string)+"/client-config?gateway_url=https%3A%2F%2Folp.example&client=opencode", nil, nil, 200)
	if !strings.Contains(config["configuration"].(string), "/mcp/search") || strings.Contains(config["configuration"].(string), "research-vendor-key") {
		t.Fatalf("generated research setup: %v", config)
	}
	// Stateless upstreams also work; GET JSON discovery must use the same filter.
	f.peer.mu.Lock()
	f.peer.respond = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Mcp-Session-Id") != "" {
			t.Error("forwarded an invented upstream session")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"webSearchPrime","inputSchema":{"type":"object"}},{"name":"vision"}]}}`)
			return
		}
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","serverInfo":{"name":"stateless","version":"1"},"capabilities":{"tools":{}}}}`)
	}
	f.peer.mu.Unlock()
	response, _ := f.research(t, "POST", "search", f.key, "", researchInit, 200)
	session := response.Header.Get("Mcp-Session-Id")
	if session == "" {
		t.Fatal("stateless upstream bypassed the OLP session fence")
	}
	_, raw := f.research(t, "GET", "search", f.key, session, "", 200)
	if strings.Contains(raw, "vision") || !strings.Contains(raw, "webSearchPrime") {
		t.Fatalf("unfiltered stateless discovery: %s", raw)
	}
	f.research(t, "GET", "search", f.key, "", "", 400)
	f.research(t, "DELETE", "search", f.key, session, "", 204)
	f.research(t, "GET", "search", f.key, session, "", 404)
}

func TestCodeResearchLiveAuthoritySessionsAndNoReplay(t *testing.T) {
	f := researchFixture(t)
	response, _ := f.research(t, "POST", "search", f.key, "", researchInit, 200)
	session := response.Header.Get("Mcp-Session-Id")
	key := f.h.want(f.owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Other research key", "project_id": f.project, "scopes": []string{"inference"}, "allowed_routes": []string{"qualification"}}, idem("other-research-key"), 201)
	f.h.refresh()
	otherID, other := key["id"].(string), key["secret"].(string)
	f.research(t, "POST", "search", other, "", researchList, 403)
	if _, err := f.h.Pool.Exec(t.Context(), `INSERT INTO olp.code_pool_keys(pool_id,api_key_id) VALUES($1,$2)`, f.pool["id"], otherID); err != nil {
		t.Fatal(err)
	}
	f.research(t, "POST", "search", other, session, researchList, 404)
	f.research(t, "POST", "reader", f.key, session, researchList, 404)
	before := len(f.peer.received())
	f.research(t, "POST", "search", f.key, session, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"vision"}}`, 403)
	if len(f.peer.received()) != before {
		t.Fatal("disallowed tool was dispatched")
	}
	call := func(query string) string {
		return `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"webSearchPrime","arguments":{"query":"` + query + `"}}}`
	}
	f.research(t, "POST", "search", f.key, session, call("disconnect"), 502)
	if len(f.peer.received()) != before+1 {
		t.Fatal("ambiguous dispatch was replayed")
	}
	f.research(t, "POST", "search", f.key, session, call("limited"), 429)
	f.research(t, "POST", "search", f.key, session, call("docs"), 200)
	// A tool-specific allowance error must leave ordinary inference usable.
	response, raw := f.post(t, "v1/responses", http.Header{"Authorization": {"Bearer " + f.key}, "X-Opencode-Session-Id": {"ses_research"}}, `{"model":"gpt-5.5","store":false}`)
	if response.StatusCode != 200 {
		t.Fatalf("research failure affected inference: %d %s", response.StatusCode, raw)
	}
	accountID := f.accounts[0]["id"].(string)
	if _, err := f.h.Pool.Exec(t.Context(), `DELETE FROM olp.code_pool_accounts WHERE pool_id=$1 AND account_id=$2`, f.pool["id"], accountID); err != nil {
		t.Fatal(err)
	}
	f.research(t, "POST", "search", f.key, session, researchList, 403)
	if _, err := f.h.Pool.Exec(t.Context(), `INSERT INTO olp.code_pool_accounts(pool_id,account_id) VALUES($1,$2)`, f.pool["id"], accountID); err != nil {
		t.Fatal(err)
	}
	// A second store instance resolves the same session, without process affinity.
	f.h.Gateway.CodeResearch = &resources.CodeStore{Pool: f.h.Pool}
	f.research(t, "POST", "search", f.key, session, researchList, 200)
	credential := f.accounts[0]["credential_id"].(string)
	if _, err := f.h.Pool.Exec(t.Context(), `UPDATE olp.provider_grants SET generation=generation+1 WHERE credential_id=$1`, credential); err != nil {
		t.Fatal(err)
	}
	f.research(t, "POST", "search", f.key, session, researchList, 404)
	f.publishResearch(t, []string{"search"})
	f.research(t, "POST", "reader", f.key, "", researchInit, 404)
	f.research(t, "POST", "search", f.key, session, researchList, 404)
	response, _ = f.research(t, "POST", "search", f.key, "", researchInit, 200)
	session = response.Header.Get("Mcp-Session-Id")
	if _, err := f.h.Pool.Exec(t.Context(), `UPDATE olp.api_keys SET revoked_at=now() WHERE id=$1`, f.keyID); err != nil {
		t.Fatal(err)
	}
	f.research(t, "POST", "search", f.key, session, researchList, 403)
}

func TestCodeResearchValidationAndGrantRevocation(t *testing.T) {
	f := researchFixture(t)
	path := "/api/v1/code/routes/" + f.route["id"].(string)
	input := func(account any, services []string) map[string]any {
		return map[string]any{"project_id": f.project, "slug": "qualification", "pool_id": f.pool["id"], "models": []string{"gpt-5.5"}, "enabled": true, "tools": map[string]any{"account_id": account, "services": services}}
	}
	for _, services := range [][]string{{}, {"search", "search"}, {"vision"}} {
		f.h.want(f.owner, "PUT", path, input(f.accounts[0]["id"], services), etagHeader(f.route), 422)
	}
	// OpenCode Go is enrolled in the same pool but cannot serve Z.ai research.
	var other string
	if err := f.h.Pool.QueryRow(t.Context(), `SELECT a.id::text FROM olp.code_accounts a JOIN olp.providers p ON p.id=a.provider_id WHERE p.configuration->>'profile_id'=$1`, codeplans.OpenCodeGoProfile).Scan(&other); err != nil {
		t.Fatal(err)
	}
	f.h.want(f.owner, "PUT", path, input(other, []string{"search"}), etagHeader(f.route), 422)
	accountID := f.accounts[0]["id"].(string)
	if _, err := f.h.Pool.Exec(t.Context(), `DELETE FROM olp.code_pool_accounts WHERE pool_id=$1 AND account_id=$2`, f.pool["id"], accountID); err != nil {
		t.Fatal(err)
	}
	f.h.want(f.owner, "PUT", path, input(accountID, []string{"search"}), etagHeader(f.route), 422)
	f.h.want(f.owner, "POST", path+"/publish", nil, withMatch(f.route, idem("research-removed-account")), 422)
	if _, err := f.h.Pool.Exec(t.Context(), `INSERT INTO olp.code_pool_accounts(pool_id,account_id) VALUES($1,$2)`, f.pool["id"], accountID); err != nil {
		t.Fatal(err)
	}
	response, _ := f.research(t, "POST", "reader", f.key, "", researchInit, 200)
	session := response.Header.Get("Mcp-Session-Id")
	// A draft edit does not change the published service binding.
	f.h.want(f.owner, "PUT", path, input(accountID, []string{"search"}), etagHeader(f.route), 200)
	f.research(t, "POST", "reader", f.key, session, researchList, 200)
	// Live request/concurrency policy cannot bypass an unavailable limiter.
	if _, err := f.h.Pool.Exec(t.Context(), `UPDATE olp.api_keys SET policy=policy||'{"max_concurrency":1}'::jsonb WHERE id=$1`, f.keyID); err != nil {
		t.Fatal(err)
	}
	f.research(t, "POST", "reader", f.key, session, researchList, 503)
	if _, err := f.h.Pool.Exec(t.Context(), `UPDATE olp.api_keys SET policy=policy-'max_concurrency' WHERE id=$1`, f.keyID); err != nil {
		t.Fatal(err)
	}
	credential := f.accounts[0]["credential_id"].(string)
	if _, err := f.h.Pool.Exec(t.Context(), `UPDATE olp.provider_grants SET lapsed_at=now() WHERE credential_id=$1`, credential); err != nil {
		t.Fatal(err)
	}
	f.research(t, "POST", "reader", f.key, session, researchList, 403)
	if _, err := f.h.Pool.Exec(t.Context(), `UPDATE olp.provider_grants SET lapsed_at=NULL WHERE credential_id=$1`, credential); err != nil {
		t.Fatal(err)
	}
	f.research(t, "POST", "reader", f.key, session, researchList, 200)
	if _, err := f.h.Pool.Exec(t.Context(), `UPDATE olp.provider_credentials SET revoked_at=now() WHERE id=$1`, credential); err != nil {
		t.Fatal(err)
	}
	f.research(t, "POST", "reader", f.key, session, researchList, 403)
}
