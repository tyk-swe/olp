//go:build integration

package integration_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/tests/harness"
)

func providerAgent(n, base string) string {
	if base != "" {
		return `{"id":"agent-` + providerOwned + n + `","description":"helper","base_agent":"agent-` + providerOwned + base + `"}`
	}
	return `{"id":"agent-` + providerOwned + n + `","description":"helper"}`
}

func providerAgentPath(n string) string { return "/agents/agent-" + providerOwned + n }

func agentObjects(issues string) map[string]string {
	replies := map[string]string{"POST /agents": providerAgent(issues, "")}
	for _, n := range []string{"1", "2", "3", "4", "5"} {
		replies["GET "+providerAgentPath(n)], replies["DELETE "+providerAgentPath(n)] = providerAgent(n, ""), `{}`
	}
	return replies
}

func TestAgentResourcesBindTheirBaseAgentPageNewestFirstAndKeepOrForgetWhatEachProviderAnswerDecides(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	provider := &storeProvider{issues: "1", objects: agentObjects, scripted: map[string]string{}, statuses: map[string]int{}}
	upstream := &retainedUpstream{replies: provider.replies, status: provider.status}
	sink := &terminals{}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: "agents", model: "agents-model", kind: "gemini", surface: "gemini", profile: "gemini-agents", version: "/v1beta", secure: true,
		fidelity: routing.FidelityStrict, operations: []string{"agent_resource"}, upstream: upstream, sink: sink,
	})
	g.key(t, "owner")
	g.key(t, "stranger")
	g.key(t, "stateless")
	stateless := g.runtime.keys["stateless"]
	stateless.Policy.AllowProviderState = false
	g.runtime.keys["stateless"] = stateless
	type agentReply struct {
		ID            string
		BaseAgent     string `json:"base_agent"`
		Agents        []struct{ ID string }
		NextPageToken string `json:"next_page_token"`
	}
	ask := func(key, method, path, body, submission string, status int, reason string, classes ...string) (http.Header, agentReply) {
		t.Helper()
		before := len(sink.all())
		contentType := ""
		if body != "" {
			contentType = "application/json"
		}
		header, raw := g.send(t, method, path, map[string]string{"x-goog-api-key": key, "Content-Type": contentType, "Idempotency-Key": submission}, body, status)
		var reply struct {
			agentReply
			Error struct{ Details []struct{ Reason string } }
		}
		if err := json.Unmarshal(raw, &reply); err != nil {
			t.Fatalf("%s %s answered %s: %v", method, path, raw, err)
		}
		got := ""
		if len(reply.Error.Details) > 0 {
			got = reply.Error.Details[0].Reason
		}
		if got != reason {
			t.Fatalf("%s %s answered %s, want reason %q", method, path, raw, reason)
		}
		seen := sink.all()
		if len(seen) != before+1 {
			t.Fatalf("%s %s recorded %d requests, want one", method, path, len(seen)-before)
		}
		recorded := make([]string, 0, len(seen[before].Attempts))
		for _, attempt := range seen[before].Attempts {
			recorded = append(recorded, attempt.Class)
		}
		if !slices.Equal(recorded, classes) {
			t.Fatalf("%s %s recorded attempts %v, want %v", method, path, recorded, classes)
		}
		return header, reply.agentReply
	}
	kept := func(id string) string {
		t.Helper()
		var state string
		if err := pool.QueryRow(t.Context(), `SELECT state FROM olp.provider_resources WHERE 'agent_resource_'||replace(id::text,'-','')=$1`, id).Scan(&state); err != nil {
			t.Fatalf("read kept agent %s: %v", id, err)
		}
		return state
	}
	listed := func(reply agentReply) []string {
		ids := make([]string, 0, len(reply.Agents))
		for _, agent := range reply.Agents {
			ids = append(ids, agent.ID)
		}
		return ids
	}
	const agents, helper = "/gemini/v1beta/agents", `{"description":"helper"}`
	rejection, outage, lost := `{"error":{"code":400,"message":"The provider refused.","status":"INVALID_ARGUMENT"}}`, `{"error":{"code":500,"message":"The provider broke.","status":"INTERNAL"}}`, `{"error":{"code":404,"message":"The provider lost it.","status":"NOT_FOUND"}}`

	for _, refused := range []struct {
		key, method, path, body, submission string
		status                              int
		reason                              string
	}{
		{"owner", "POST", agents, helper, "", http.StatusBadRequest, "missing_required_parameter"},
		{"stateless", "POST", agents, helper, "refused", http.StatusForbidden, "provider_state_forbidden"},
		{"owner", "POST", agents, `{"name":"helper"}`, "refused", http.StatusBadRequest, "unsupported_field"},
		{"owner", "POST", agents, `{"base_environment":{"env":{"TOKEN":{"credential":"secret"}}}}`, "refused", http.StatusUnprocessableEntity, "unsupported_stateful_reference"},
		{"owner", "POST", agents, `[]`, "refused", http.StatusBadRequest, "invalid_request"},
		{"owner", "GET", agents + "?filter=helper", "", "", http.StatusBadRequest, "unsupported_parameter"},
		{"owner", "GET", agents + "?page_size=0", "", "", http.StatusBadRequest, "invalid_parameter"},
		{"owner", "GET", agents + "?page_token=agent_resource_unknown", "", "", http.StatusBadRequest, "invalid_parameter"},
	} {
		header, _ := ask(refused.key, refused.method, refused.path, refused.body, refused.submission, refused.status, refused.reason)
		if refused.method == "POST" && refused.key == "owner" && header.Get("X-Should-Retry") != "false" {
			t.Fatalf("refused creation %s answered retry hint %q", refused.body, header.Get("X-Should-Retry"))
		}
	}
	if sent := upstream.sent("POST /agents"); len(sent) != 0 {
		t.Fatalf("refused requests reached the provider: %+v", sent)
	}

	header, first := ask("owner", "POST", agents, `{"description":"helper","base_agent":""}`, "first", http.StatusOK, "", "success")
	sent := upstream.sent("POST /agents")
	if !strings.HasPrefix(first.ID, "agent_resource_") || header.Get("X-OLP-Resource-ID") != first.ID || len(sent) != 1 || sent[0].key != "listed-secret" || sent[0].body != helper || kept(first.ID) != "active" {
		t.Fatalf("creation answered %+v with %q, sent %+v and is kept as %q", first, header.Get("X-OLP-Resource-ID"), sent, kept(first.ID))
	}
	header, _ = ask("owner", "POST", agents, `{"description":"helper","base_agent":""}`, "first", http.StatusConflict, "submission_exists")
	ask("owner", "POST", agents, `{"description":"another"}`, "first", http.StatusConflict, "idempotency_conflict")
	if header.Get("X-OLP-Resource-ID") != first.ID || len(upstream.sent("POST /agents")) != 1 {
		t.Fatalf("a repeated creation named %q and reached the provider %d times", header.Get("X-OLP-Resource-ID"), len(upstream.sent("POST /agents")))
	}

	provider.answers("POST /agents", 0, providerAgent("2", "1"))
	provider.answers("GET "+providerAgentPath("2"), 0, providerAgent("2", "1"))
	based := `{"description":"child","base_agent":"` + first.ID + `"}`
	ask("stranger", "POST", agents, based, "derived", http.StatusBadRequest, "invalid_resource_reference")
	_, derived := ask("owner", "POST", agents, based, "derived", http.StatusOK, "", "success")
	sent = upstream.sent("POST /agents")
	if derived.BaseAgent != first.ID || len(sent) != 2 || sent[1].body != `{"description":"child","base_agent":"agent-`+providerOwned+`1"}` {
		t.Fatalf("an agent built on another answered %+v and sent %+v", derived, sent)
	}
	if _, read := ask("owner", "GET", agents+"/"+derived.ID, "", "", http.StatusOK, "", "success"); read.ID != derived.ID || read.BaseAgent != first.ID {
		t.Fatalf("reading the derived agent answered %+v", read)
	}
	ask("stranger", "GET", agents+"/"+derived.ID, "", "", http.StatusNotFound, "not_found")
	ask("stranger", "DELETE", agents+"/"+derived.ID, "", "", http.StatusNotFound, "not_found")

	_, page := ask("owner", "GET", agents, "", "", http.StatusOK, "", "success", "success")
	if !slices.Equal(listed(page), []string{derived.ID, first.ID}) || page.NextPageToken != "" {
		t.Fatalf("the list answered %+v, want the newest agent first", page)
	}
	_, page = ask("owner", "GET", agents+"?page_size=1", "", "", http.StatusOK, "", "success")
	if !slices.Equal(listed(page), []string{derived.ID}) || page.NextPageToken != derived.ID {
		t.Fatalf("the first page answered %+v", page)
	}
	_, page = ask("owner", "GET", agents+"?page_size=1&page_token="+page.NextPageToken, "", "", http.StatusOK, "", "success")
	if !slices.Equal(listed(page), []string{first.ID}) || page.NextPageToken != "" {
		t.Fatalf("the second page answered %+v", page)
	}
	if _, page = ask("stranger", "GET", agents, "", "", http.StatusOK, ""); len(page.Agents) != 0 {
		t.Fatalf("another key's list answered %+v", page)
	}

	for _, broken := range []struct {
		name, body, reason, class string
		status, want              int
	}{
		{"another agent", providerAgent("2", ""), "provider_protocol_error", "protocol", 0, http.StatusBadGateway},
		{"an unknown base agent", providerAgent("1", "5"), "provider_protocol_error", "protocol", 0, http.StatusBadGateway},
		{"an outage", outage, "provider_resource_error", "upstream_server", http.StatusInternalServerError, http.StatusInternalServerError},
	} {
		provider.answers("GET "+providerAgentPath("1"), broken.status, broken.body)
		ask("owner", "GET", agents+"/"+first.ID, "", "", broken.want, broken.reason, broken.class)
		ask("owner", "GET", agents, "", "", broken.want, broken.reason, "success", broken.class)
		if kept(first.ID) != "active" {
			t.Fatalf("a read answered with %s left the agent %q", broken.name, kept(first.ID))
		}
	}
	provider.recovers("GET " + providerAgentPath("1"))
	provider.answers("DELETE "+providerAgentPath("1"), http.StatusInternalServerError, outage)
	ask("owner", "DELETE", agents+"/"+first.ID, "", "", http.StatusInternalServerError, "provider_resource_error", "upstream_server")
	if kept(first.ID) != "active" {
		t.Fatalf("a deletion the provider failed left the agent %q", kept(first.ID))
	}

	provider.answers("POST /agents", http.StatusBadRequest, rejection)
	header, _ = ask("owner", "POST", agents, helper, "refused", http.StatusBadRequest, "provider_resource_error", "upstream_client")
	refused := header.Get("X-OLP-Resource-ID")
	if state := kept(refused); state != "not_accepted" {
		t.Fatalf("a refused agent creation is kept as %q, want not_accepted", state)
	}
	provider.answers("POST /agents", http.StatusInternalServerError, outage)
	header, _ = ask("owner", "POST", agents, helper, "unknown", http.StatusInternalServerError, "provider_resource_error", "ambiguous")
	unknown := header.Get("X-OLP-Resource-ID")
	provider.answers("POST /agents", 0, `{"description":"helper"}`)
	header, _ = ask("owner", "POST", agents, helper, "unnamed", http.StatusBadGateway, "provider_protocol_error", "protocol")
	unnamed := header.Get("X-OLP-Resource-ID")
	provider.recovers("POST /agents")
	before := len(upstream.sent("POST /agents"))
	for submission, id := range map[string]string{"refused": refused, "unknown": unknown, "unnamed": unnamed} {
		header, _ = ask("owner", "POST", agents, helper, submission, http.StatusConflict, "submission_exists")
		if id == "" || header.Get("X-OLP-Resource-ID") != id || header.Get("X-Should-Retry") != "false" {
			t.Fatalf("repeating creation %q named %q, want %q", submission, header.Get("X-OLP-Resource-ID"), id)
		}
		ask("owner", "GET", agents+"/"+id, "", "", http.StatusConflict, "submission_unknown")
		ask("owner", "DELETE", agents+"/"+id, "", "", http.StatusConflict, "submission_unknown")
	}
	if len(upstream.sent("POST /agents")) != before || kept(unknown) != "submission_unknown" || kept(unnamed) != "submission_unknown" {
		t.Fatalf("unsettled creations reached the provider %d more times and are kept as %q and %q", len(upstream.sent("POST /agents"))-before, kept(unknown), kept(unnamed))
	}

	provider.answers("POST /agents", 0, providerAgent("3", "5"))
	header, _ = ask("owner", "POST", agents, helper, "orphan", http.StatusBadGateway, "provider_protocol_error", "protocol")
	orphan := header.Get("X-OLP-Resource-ID")
	if kept(orphan) != "active" {
		t.Fatalf("an agent the provider created on an unknown base is kept as %q, so its owner could not delete it", kept(orphan))
	}
	ask("owner", "DELETE", agents+"/"+orphan, "", "", http.StatusOK, "", "success")
	ask("owner", "GET", agents+"/"+orphan, "", "", http.StatusNotFound, "not_found")
	if len(upstream.sent("DELETE "+providerAgentPath("3"))) != 1 || kept(orphan) != "deleted" {
		t.Fatalf("deleting the agent reached the provider %d times and left it %q", len(upstream.sent("DELETE "+providerAgentPath("3"))), kept(orphan))
	}

	provider.answers("GET "+providerAgentPath("1"), http.StatusNotFound, lost)
	if _, page = ask("owner", "GET", agents, "", "", http.StatusOK, "", "success", "upstream_client"); !slices.Equal(listed(page), []string{derived.ID}) || kept(first.ID) != "deleted" {
		t.Fatalf("a list past an agent the provider lost answered %+v and left it %q", page, kept(first.ID))
	}
	ask("owner", "GET", agents+"/"+first.ID, "", "", http.StatusNotFound, "not_found")
	provider.answers("GET "+providerAgentPath("2"), http.StatusNotFound, lost)
	ask("owner", "GET", agents+"/"+derived.ID, "", "", http.StatusNotFound, "not_found", "upstream_client")
	if _, page = ask("owner", "GET", agents, "", "", http.StatusOK, ""); len(page.Agents) != 0 || kept(derived.ID) != "deleted" {
		t.Fatalf("after the provider lost every agent the list answered %+v and the last is %q", page, kept(derived.ID))
	}
}
