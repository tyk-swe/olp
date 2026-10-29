//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/a2a"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/prompts"
	"github.com/tyk-swe/olp/internal/secrets"
)

func TestAgentPublicationsManagementAndConfiguration(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	org := h.want(owner, http.MethodPost, "/api/v1/organizations", map[string]any{"name": "Agent contracts"}, idem("rm4-org"), 201)
	project := h.want(owner, http.MethodPost, "/api/v1/organizations/"+org["id"].(string)+"/projects", map[string]any{"name": "Agent contracts"}, idem("rm4-project"), 201)["id"].(string)
	draft := map[string]any{"project_id": project, "name": "greeting", "prompt": map[string]any{"template": "Hello {{name}}", "variables": []any{"name"}}}
	created := h.want(owner, http.MethodPost, "/api/v1/prompts", draft, idem("rm4-create"), 201)
	replayed := h.want(owner, http.MethodPost, "/api/v1/prompts", draft, idem("rm4-create"), 201)
	if created["id"] != replayed["id"] {
		t.Fatal("creation retry produced another publication")
	}
	path := "/api/v1/prompts/" + created["id"].(string)
	headers := func(tag, key string) map[string]string {
		return map[string]string{"If-Match": `"` + tag + `"`, "Idempotency-Key": key}
	}
	published := h.want(owner, http.MethodPost, path+"/publish", nil, headers(created["etag"].(string), "rm4-publish"), 200)
	revision := published["revision_id"].(string)
	h.want(owner, http.MethodPut, path, draft, headers(created["etag"].(string), "rm4-stale"), 412)
	labeled := h.want(owner, http.MethodPut, path+"/labels/production", map[string]any{"revision_id": revision}, headers(published["etag"].(string), "rm4-label"), 200)
	read := h.want(owner, http.MethodGet, path+"/revisions/"+revision, nil, nil, 200)
	if read["digest"] == "" || len(read["labels"].([]any)) != 1 {
		t.Fatal("immutable revision lost digest or label")
	}
	changed := map[string]any{"project_id": project, "name": "greeting", "prompt": map[string]any{"template": "Goodbye {{name}}", "variables": []any{"name"}}}
	saved := h.want(owner, http.MethodPut, path, changed, headers(labeled["etag"].(string), "rm4-update"), 200)
	if saved["revision_id"] != revision {
		t.Fatal("editing a draft changed published execution")
	}
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Export keeps the mutable draft, installed revision and label distinct.
	exported := h.want(owner, http.MethodGet, "/api/v1/configuration/export", nil, nil, 200)
	document := exported["document"].(map[string]any)
	entries := document["agent_publications"].([]any)
	if len(entries) != 1 {
		t.Fatalf("exported publications: %v", entries)
	}
	e := entries[0].(map[string]any)
	if e["draft"].(map[string]any)["prompt"].(map[string]any)["template"] != "Goodbye {{name}}" || e["published"].(map[string]any)["prompt"].(map[string]any)["template"] != "Hello {{name}}" {
		t.Fatal("export merged draft and publication")
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "credential_id") {
		t.Fatal("export leaked destination secret identities")
	}
	// A nested revision is never authorized through a different publication.
	second := h.want(owner, http.MethodPost, "/api/v1/prompts", map[string]any{"project_id": project, "name": "other", "prompt": map[string]any{"template": "plain", "variables": []any{}}}, idem("rm4-other"), 201)
	h.want(owner, http.MethodGet, "/api/v1/prompts/"+second["id"].(string)+"/revisions/"+revision, nil, nil, 404)
	h.want(owner, http.MethodDelete, path, nil, headers(saved["etag"].(string), "rm4-retire"), 204)
	h.want(owner, http.MethodPost, path+"/publish", nil, headers(saved["etag"].(string), "rm4-retired-write"), 412)
	var events, unscoped int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*),count(*) FILTER (WHERE project_id IS DISTINCT FROM $2::uuid) FROM olp.audit WHERE resource_id=$1`, created["id"], project).Scan(&events, &unscoped); err != nil || events != 5 || unscoped != 0 {
		t.Fatalf("publication audits lost project ownership or included replays/failures: events=%d unscoped=%d error=%v", events, unscoped, err)
	}
}

func TestAgentPromptLabelWithoutCurrentRevision(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := h.want(owner, http.MethodPost, "/api/v1/projects", map[string]any{"name": "Label history"}, idem("label-project"), 201)["id"].(string)
	created := h.want(owner, http.MethodPost, "/api/v1/prompts", map[string]any{"project_id": project, "name": "greeting", "prompt": map[string]any{"template": "Hello", "variables": []any{}}}, idem("label-create"), 201)
	path := "/api/v1/prompts/" + created["id"].(string)
	published := h.want(owner, http.MethodPost, path+"/publish", nil, withMatch(created, idem("label-publish")), 200)
	revision := published["revision_id"].(string)
	h.want(owner, http.MethodPut, path+"/labels/production", map[string]any{"revision_id": revision}, withMatch(published, idem("label-production")), 200)
	exported := h.want(owner, http.MethodGet, "/api/v1/configuration/export", nil, nil, 200)
	document := exported["document"].(map[string]any)
	entry := document["agent_publications"].([]any)[0].(map[string]any)
	delete(entry, "published") // Keep immutable label history without a default publication.
	h.want(owner, http.MethodPost, "/api/v1/configuration/apply", map[string]any{"document": document}, idem("label-only-apply"), 200)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	read := h.want(owner, http.MethodGet, path+"/revisions/"+revision, nil, nil, 200)
	if read["current"] != false || len(read["labels"].([]any)) != 1 {
		t.Fatal("retained prompt revision lost its label or became current")
	}
}

func TestAgentPublicationKeyGrantsRemainVisible(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "Key grants")
	created := h.want(owner, http.MethodPost, "/api/v1/toolsets", map[string]any{"project_id": project, "name": "empty", "toolset": map[string]any{"tools": []any{}}}, idem("grants-toolset"), 201)
	published := h.want(owner, http.MethodPost, "/api/v1/toolsets/"+created["id"].(string)+"/publish", nil, withMatch(created, idem("grants-publish")), 200)
	key := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "Tool caller", "project_id": project, "scopes": []string{"tools"}, "allowed_toolsets": []string{created["id"].(string)}}, idem("grants-key"), 201)
	path := "/api/v1/api-keys/" + key["id"].(string)
	check := func(read map[string]any) {
		t.Helper()
		tools, toolsOK := read["allowed_toolsets"].([]any)
		agents, agentsOK := read["allowed_agents"].([]any)
		if !toolsOK || len(tools) != 1 || tools[0] != created["id"] || !agentsOK || len(agents) != 0 {
			t.Fatalf("resource grants missing from key response: %v", read)
		}
	}
	read := h.want(owner, http.MethodGet, path, nil, nil, 200)
	check(read)
	h.want(owner, http.MethodPatch, path, map[string]any{"name": "Renamed caller"}, withMatch(read, idem("grants-rename")), 200)
	check(h.want(owner, http.MethodGet, path, nil, nil, 200))
	h.want(owner, http.MethodDelete, "/api/v1/toolsets/"+created["id"].(string), nil, withMatch(published, idem("grants-retire")), 204)
	read = h.want(owner, http.MethodGet, path, nil, nil, 200)
	h.want(owner, http.MethodPatch, path, map[string]any{"name": "Historical grants", "allowed_toolsets": read["allowed_toolsets"]}, withMatch(read, idem("grants-retired-rename")), 200)
	check(h.want(owner, http.MethodGet, path, nil, nil, 200))
	for _, item := range h.want(owner, http.MethodGet, "/api/v1/api-keys", nil, nil, 200)["items"].([]any) {
		read := item.(map[string]any)
		if read["id"] == key["id"] {
			check(read)
			removed := h.want(owner, http.MethodPatch, path, map[string]any{"allowed_toolsets": []string{}}, withMatch(read, idem("grants-remove")), 200)
			h.want(owner, http.MethodPatch, path, map[string]any{"allowed_toolsets": []string{created["id"].(string)}}, withMatch(removed, idem("grants-readd-retired")), 422)
			return
		}
	}
	t.Fatal("created key missing from list")
}

func TestAgentApprovalConsumptionCommitsWithDispatch(t *testing.T) {
	f, store, project := debitFixture(t)
	server, serverRevision := acctID(t), acctID(t)
	digest := strings.Repeat("a", 64)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = tx.Exec(t.Context(), `INSERT INTO olp.mcp_servers(id,project_id,name,transport,endpoint,enabled,latest_revision_id,etag,created_by) VALUES($1,$2,'approved','streamable_http','https://tools.example.test/mcp',true,$3,$4,$5)`, server, project, serverRevision, acctID(t), f.User); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(t.Context(), `INSERT INTO olp.mcp_server_revisions(id,server_id,revision,endpoint,protocol_version,tools,digest,created_by) VALUES($1,$2,1,'https://tools.example.test/mcp','2025-11-25','[{"name":"lookup","input_schema":{"type":"object"}}]'::jsonb,$3,$4)`, serverRevision, server, digest, f.User); err != nil {
		t.Fatal(err)
	}
	tool := agentcontract.Tool{ServerID: server, RevisionID: serverRevision, CatalogDigest: digest, Name: "lookup", Alias: agentcontract.ToolAlias(server, "lookup")}
	p := &agents.Publication{Kind: "toolset", Draft: agentcontract.Draft{ProjectID: project, Name: "approved", Toolset: &agentcontract.Toolset{Tools: []agentcontract.Tool{tool}}}}
	if err = agents.SaveDraft(t.Context(), tx, f.User, nil, p, nil); err != nil {
		t.Fatal(err)
	}
	if err = agents.Publish(t.Context(), tx, f.User, p); err != nil {
		t.Fatal(err)
	}
	a := access.Authority{ID: f.Key, ProjectID: &project, Policy: access.KeyPolicy{Scopes: []string{"tools"}, AllowedToolsets: []string{p.ID}}}
	key := secrets.NewAuthKey(bytes.Repeat([]byte{1}, 32), project)
	run := acctID(t)
	if err = agents.StartRunTx(t.Context(), tx, key, a, run, "tools", p.ID, *p.RevisionID); err != nil {
		t.Fatal(err)
	}
	i := debitIntent(t, f, project)
	i.Kind = "tool"
	i.RunID = run
	i.StepID = acctID(t)
	i.Affinity.RevisionID = *p.RevisionID
	i.Affinity.ToolAlias = tool.Alias
	i.Affinity.InputDigest = strings.Repeat("b", 64)
	i.Affinity.TranscriptDigest = strings.Repeat("c", 64)
	if _, err = agents.PrepareStepTx(t.Context(), tx, store, a, i, 0, []limits.DebitOwner{{ID: f.Key, Kind: "key"}, {ID: f.Provider, Kind: "supply"}}); err != nil {
		t.Fatal(err)
	}
	var principal []byte
	if err = tx.QueryRow(t.Context(), `SELECT principal_digest FROM olp.agent_runs WHERE id=$1`, run).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	b := agents.ApprovalBinding{RunID: run, ToolsetID: p.ID, ToolsetRevisionID: *p.RevisionID, ToolAlias: tool.Alias, PrincipalDigest: hex.EncodeToString(principal), ArgumentDigest: i.Affinity.InputDigest, TranscriptDigest: i.Affinity.TranscriptDigest, MaxCost: "2", Currency: "USD"}
	approval, err := agents.CreateApprovalTx(t.Context(), tx, key, a, b, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	valkey, prefix, _ := acctKeyspace(t)
	store.Limiter, err = limits.New(valkey, strings.TrimSuffix(prefix, ":"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Admit(t.Context(), i.ID, i.ClaimID, []limits.Request{{CostOwnerID: f.Key}, {CostOwnerID: f.Provider}}); err != nil {
		t.Fatal(err)
	}
	consume := func(binding agents.ApprovalBinding, dispatch bool) error {
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			return err
		}
		defer tx.Rollback(t.Context())
		if err = agents.ConsumeApprovalTx(t.Context(), tx, key, a, approval, i.ID, binding); err != nil {
			return err
		}
		if dispatch {
			if err = limits.MarkDispatchedTx(t.Context(), tx, i.ID, i.ClaimID); err != nil {
				return err
			}
		}
		return tx.Commit(t.Context())
	}
	changed := b
	changed.ArgumentDigest = strings.Repeat("d", 64)
	if err = consume(changed, true); err == nil {
		t.Fatal("approval accepted changed arguments")
	}
	if err = consume(b, false); err == nil {
		t.Fatal("approval committed without its dispatch")
	}
	acctExec(t, f.Pool, `UPDATE olp.mcp_servers SET enabled=false WHERE id=$1`, server)
	if err = consume(b, true); err == nil {
		t.Fatal("approval bypassed live MCP revocation")
	}
	acctExec(t, f.Pool, `UPDATE olp.mcp_servers SET enabled=true WHERE id=$1`, server)
	if err = consume(b, true); err != nil {
		t.Fatal(err)
	}
	if err = consume(b, true); err == nil {
		t.Fatal("approval was reusable")
	}
}

func TestAgentPublicationImmutableRows(t *testing.T) {
	f, _, project := debitFixture(t)
	id, revision := acctID(t), acctID(t)
	d := agentcontract.Draft{ProjectID: project, Name: "pinned", Prompt: &prompts.Definition{Template: "operator template", Variables: []string{}}}
	body, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	acctExec(t, f.Pool, `INSERT INTO olp.agent_publications(id,project_id,kind,name,draft,etag,created_by) VALUES($1,$2,'prompt','pinned',$3,$4,$5)`, id, project, body, acctID(t), f.User)
	acctExec(t, f.Pool, `INSERT INTO olp.agent_publication_revisions(id,publication_id,project_id,kind,definition,digest,created_by) VALUES($1,$2,$3,'prompt',$4,$5,$6)`, revision, id, project, body, agentcontract.DraftDigest(d), f.User)
	if _, err = f.Pool.Exec(t.Context(), `UPDATE olp.agent_publication_revisions SET digest=$2 WHERE id=$1`, revision, strings.Repeat("a", 64)); err == nil {
		t.Fatal("published evidence was mutable")
	}
	if _, err = f.Pool.Exec(t.Context(), `DELETE FROM olp.agent_publication_revisions WHERE id=$1`, revision); err == nil {
		t.Fatal("published evidence could be deleted")
	}
	var text string
	if err = f.Pool.QueryRow(t.Context(), `SELECT definition::text FROM olp.agent_publication_revisions WHERE id=$1`, revision).Scan(&text); err != nil || !strings.Contains(text, "operator template") {
		t.Fatalf("immutable definition lost: %s %v", text, err)
	}
}

type a2aTestParams struct {
	ID      protocol.TaskID             `json:"id,omitempty"`
	Message *protocol.Message           `json:"message,omitempty"`
	Config  *protocol.SendMessageConfig `json:"configuration,omitempty"`
}

func TestA2AAgentCardUsesGatewayAuthenticationAndEndpoint(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "A2A discovery")
	const cardTemplate = `{"name":"Fixture agent","description":"A test agent","version":"1.0","supportedInterfaces":[{"url":"PINNED_ENDPOINT","protocolBinding":"JSONRPC","protocolVersion":"1.0"},{"url":"https://wrong.invalid/rpc","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],"capabilities":{"streaming":true,"pushNotifications":true},"defaultInputModes":["text"],"defaultOutputModes":["text"],"skills":[]}`
	cardFor := func(endpoint string) []byte {
		return bytes.ReplaceAll([]byte(cardTemplate), []byte("PINNED_ENDPOINT"), []byte(endpoint))
	}
	upstreamIDs := make(chan string, 1)
	var redirectCard, redirectRPC atomic.Bool
	var redirectHits atomic.Int32
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(redirectTarget.Close)
	agentHandler := a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(a2asrv.AgentExecutorFunc(func(ctx context.Context, execution *a2asrv.ExecutorContext) iter.Seq2[protocol.Event, error] {
		return func(yield func(protocol.Event, error) bool) {
			var dispatched int
			if err := h.Pool.QueryRow(ctx, `SELECT count(*) FROM olp.execution_debits WHERE kind='agent' AND state='dispatched'`).Scan(&dispatched); err != nil || dispatched != 1 {
				t.Errorf("send before durable debit: %d, %v", dispatched, err)
			}
			upstreamIDs <- string(execution.TaskID)
			if !yield(protocol.NewSubmittedTask(execution, execution.Message), nil) {
				return
			}
			if !yield(protocol.NewArtifactEvent(execution, protocol.NewTextPart("PRIVATE ARTIFACT")), nil) {
				return
			}
			yield(protocol.NewStatusUpdateEvent(execution, protocol.TaskStateCompleted, nil), nil)
		}
	})))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-secret" {
			t.Error("upstream credential missing")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/rpc" {
			if redirectRPC.Load() {
				http.Redirect(w, r, redirectTarget.URL, http.StatusTemporaryRedirect)
				return
			}
			agentHandler.ServeHTTP(w, r)
			return
		}
		if r.URL.Path != "/.well-known/agent-card.json" {
			t.Errorf("unexpected upstream request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if redirectCard.Load() {
			http.Redirect(w, r, redirectTarget.URL, http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cardFor("http://" + r.Host + "/rpc"))
	}))
	t.Cleanup(upstream.Close)
	card := cardFor(upstream.URL + "/rpc")
	var user string
	if err := h.Pool.QueryRow(t.Context(), `SELECT id::text FROM olp.users WHERE email='owner@example.com'`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	pricing := acctID(t)
	acctExec(t, h.Pool, `INSERT INTO olp.pricing_revisions(id,revision,effective_at,created_by) VALUES($1,1,now(),$2)`, pricing, user)
	digest := sha256.Sum256(card)
	for _, price := range []struct{ name, value string }{
		{name: "malformed", value: "not-a-number"},
		{name: "negative", value: "-0.01"},
		{name: "above-bound", value: "1.01"},
	} {
		problem := h.want(owner, http.MethodPost, "/api/v1/agents", map[string]any{
			"project_id": project, "name": "invalid " + price.name,
			"agent": map[string]any{"protocol": "a2a-1.0.0", "endpoint": upstream.URL + "/rpc", "card_digest": hex.EncodeToString(digest[:]), "toolset_revisions": []string{}, "route_revisions": []string{}, "pricing_revision_id": pricing, "max_cost": "1", "message_price": price.value},
		}, idem("a2a-invalid-price-"+price.name), http.StatusUnprocessableEntity)
		if !strings.Contains(fmt.Sprint(problem["detail"]), "message_price") {
			t.Fatalf("invalid price rejected for the wrong reason: %+v", problem)
		}
	}
	created := h.want(owner, http.MethodPost, "/api/v1/agents", map[string]any{
		"project_id": project, "name": "fixture", "credential": "upstream-secret",
		"agent": map[string]any{"protocol": "a2a-1.0.0", "endpoint": upstream.URL + "/rpc", "card_digest": hex.EncodeToString(digest[:]), "toolset_revisions": []string{}, "route_revisions": []string{}, "pricing_revision_id": pricing, "max_cost": "1", "message_price": "0.25"},
	}, idem("a2a-card-agent"), http.StatusCreated)
	agent := created["id"].(string)
	published := h.want(owner, http.MethodPost, "/api/v1/agents/"+agent+"/publish", nil, withMatch(created, idem("a2a-card-publish")), http.StatusOK)
	key := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "A2A caller", "project_id": project, "scopes": []string{"agents"}, "allowed_agents": []string{agent}}, idem("a2a-card-key"), http.StatusCreated)["secret"].(string)
	noScope := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "missing scope", "project_id": project, "scopes": []string{"tools"}, "allowed_agents": []string{agent}}, idem("a2a-no-scope"), http.StatusCreated)["secret"].(string)
	noAgent := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "missing agent", "project_id": project, "scopes": []string{"agents"}}, idem("a2a-no-agent"), http.StatusCreated)["secret"].(string)
	otherProject := createProject(h, owner, "A2A other project")
	otherProjectKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "other project", "project_id": otherProject, "scopes": []string{"agents"}}, idem("a2a-other-project"), http.StatusCreated)["secret"].(string)
	client, prefix, _ := acctKeyspace(t)
	limiter, err := limits.New(client, strings.TrimSuffix(prefix, ":"))
	if err != nil {
		t.Fatal(err)
	}
	h.A2A.Ledger = &a2a.Ledger{Pool: h.Pool, Store: &limits.DebitStore{Pool: h.Pool, Limiter: limiter}, Auth: h.AuthKey}
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	getCard := func(secret, agentID string) int {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, h.HTTP.URL+"/a2a/"+agentID+"/.well-known/agent-card.json", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+secret)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	if getCard(noScope, agent) == http.StatusOK || getCard(noAgent, agent) == http.StatusOK {
		t.Fatal("agent access succeeded without its scope and allowlist")
	}
	if got, missing := getCard(otherProjectKey, agent), getCard(otherProjectKey, uuid.NewString()); got != missing || got != http.StatusNotFound {
		t.Fatalf("cross-project agent differs from missing: %d, %d", got, missing)
	}
	r, err := http.NewRequest(http.MethodGet, h.HTTP.URL+"/a2a/"+agent+"/.well-known/agent-card.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+key)
	response, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("card request returned %d, want 200", response.StatusCode)
	}
	var result struct {
		SupportedInterfaces []struct {
			URL             string `json:"url"`
			ProtocolBinding string `json:"protocolBinding"`
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"supportedInterfaces"`
		Capabilities struct {
			Streaming         bool `json:"streaming"`
			PushNotifications bool `json:"pushNotifications"`
		} `json:"capabilities"`
		SecuritySchemes map[string]struct {
			HTTPAuth struct {
				Scheme string `json:"scheme"`
			} `json:"httpAuthSecurityScheme"`
		} `json:"securitySchemes"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if len(result.SupportedInterfaces) != 1 || result.SupportedInterfaces[0].URL != h.HTTP.URL+"/a2a/"+agent || result.SupportedInterfaces[0].ProtocolBinding != "JSONRPC" || result.Capabilities.Streaming || result.Capabilities.PushNotifications {
		t.Fatalf("card advertises an unimplemented or foreign interface: %+v", result)
	}
	if len(result.SecuritySchemes) != 1 {
		t.Fatalf("card must advertise gateway authentication: %+v", result.SecuritySchemes)
	}
	for _, scheme := range result.SecuritySchemes {
		if scheme.HTTPAuth.Scheme != "Bearer" {
			t.Fatalf("unexpected security scheme: %+v", scheme)
		}
	}
	message := func(id, text string) *protocol.Message {
		return &protocol.Message{ID: id, Role: protocol.MessageRoleUser, Parts: protocol.ContentParts{protocol.NewTextPart(text)}}
	}
	rpc := func(secret, method string, params a2aTestParams) map[string]any {
		t.Helper()
		body, err := json.Marshal(struct {
			JSONRPC string        `json:"jsonrpc"`
			ID      int           `json:"id"`
			Method  string        `json:"method"`
			Params  a2aTestParams `json:"params"`
		}{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequest(http.MethodPost, h.HTTP.URL+"/a2a/"+agent, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+secret)
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s returned %d", method, response.StatusCode)
		}
		var decoded map[string]any
		if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	invalidIDRequest, err := http.NewRequest(http.MethodPost, h.HTTP.URL+"/a2a/"+agent, bytes.NewBufferString(`{"jsonrpc":"2.0","id":{},"method":"GetTask","params":{"id":"missing"}}`))
	if err != nil {
		t.Fatal(err)
	}
	invalidIDRequest.Header.Set("Authorization", "Bearer "+key)
	invalidIDResponse, err := http.DefaultClient.Do(invalidIDRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer invalidIDResponse.Body.Close()
	var invalidIDResult struct {
		ID    json.RawMessage `json:"id"`
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(invalidIDResponse.Body).Decode(&invalidIDResult); err != nil || invalidIDResult.Error.Code != -32600 || string(invalidIDResult.ID) != "null" {
		t.Fatalf("invalid JSON-RPC ID was accepted: %+v, %v", invalidIDResult, err)
	}
	sent := rpc(key, "SendMessage", a2aTestParams{Message: message("fixture-message", "PRIVATE REQUEST")})
	sendResult, ok := sent["result"].(map[string]any)
	if !ok {
		t.Fatalf("send failed: %+v", sent)
	}
	task, ok := sendResult["task"].(map[string]any)
	if !ok {
		t.Fatalf("send did not return a task: %+v", sent)
	}
	upstreamID := <-upstreamIDs
	id, ok := task["id"].(string)
	if !ok || id == "" || id == upstreamID {
		t.Fatalf("send did not mask upstream ID: %+v", task)
	}
	read := rpc(key, "GetTask", a2aTestParams{ID: protocol.TaskID(id)})
	readTask, ok := read["result"].(map[string]any)
	if !ok || readTask["id"] != id || !strings.Contains(fmt.Sprint(readTask), "PRIVATE ARTIFACT") {
		t.Fatalf("task read failed: %+v", read)
	}
	if got := rpc(key, "SendMessage", a2aTestParams{Message: &protocol.Message{ID: "upstream-reference", Role: protocol.MessageRoleUser, Parts: protocol.ContentParts{protocol.NewTextPart("reference")}, ReferenceTasks: []protocol.TaskID{protocol.TaskID(upstreamID)}}}); got["error"] == nil {
		t.Fatalf("accepted upstream task reference: %+v", got)
	}
	if got := rpc(key, "SendMessage", a2aTestParams{Message: message("push-attempt", "push"), Config: &protocol.SendMessageConfig{PushConfig: &protocol.PushConfig{URL: "https://wrong.invalid"}}}); got["error"] == nil {
		t.Fatalf("accepted an unsupported push request: %+v", got)
	}
	second := rpc(key, "SendMessage", a2aTestParams{Message: message("second-fixture-message", "another message")})
	secondResult, ok := second["result"].(map[string]any)
	if !ok || secondResult["task"] == nil {
		t.Fatalf("second send failed: %+v", second)
	}
	<-upstreamIDs
	foreign := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "other caller", "project_id": project, "scopes": []string{"agents"}, "allowed_agents": []string{agent}}, idem("a2a-foreign-key"), http.StatusCreated)["secret"].(string)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	missing := rpc(foreign, "GetTask", a2aTestParams{ID: protocol.TaskID(uuid.NewString())})
	if got := rpc(foreign, "GetTask", a2aTestParams{ID: protocol.TaskID(id)}); !reflect.DeepEqual(got, missing) {
		t.Fatalf("foreign task differs from missing: %+v, %+v", got, missing)
	}
	if got := rpc(key, "GetTask", a2aTestParams{ID: protocol.TaskID(upstreamID)}); got["error"] == nil {
		t.Fatalf("accepted upstream task ID: %+v", got)
	}
	projectReadStatus := func(agentID, taskID string) int {
		t.Helper()
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "GetTask", "params": map[string]any{"id": taskID}})
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequest(http.MethodPost, h.HTTP.URL+"/a2a/"+agentID, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+otherProjectKey)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	if got, missing := projectReadStatus(agent, id), projectReadStatus(uuid.NewString(), uuid.NewString()); got != missing || got != http.StatusNotFound {
		t.Fatalf("cross-project task differs from missing: %d, %d", got, missing)
	}
	var settled, results int
	var cost string
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*),sum(d.observed_cost)::text,(SELECT count(*) FROM olp.execution_debit_results r JOIN olp.execution_debits x ON x.id=r.dispatch_id WHERE x.kind='agent') FROM olp.execution_debits d WHERE d.kind='agent' AND d.state='settled'`).Scan(&settled, &cost, &results); err != nil {
		t.Fatal(err)
	}
	if settled != 2 || !limits.AmountEqual(cost, "0.50") || results != settled {
		t.Fatalf("incorrect settlements: %d, %s, %d", settled, cost, results)
	}
	redirectRPC.Store(true)
	if got := rpc(key, "SendMessage", a2aTestParams{Message: message("redirect-message", "redirect")}); got["error"] == nil {
		t.Fatalf("redirected send succeeded: %+v", got)
	}
	redirectCard.Store(true)
	if got := getCard(key, agent); got != http.StatusBadGateway || redirectHits.Load() != 0 {
		t.Fatalf("redirected credential request: status %d, target calls %d", got, redirectHits.Load())
	}
	var unknown int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.execution_debits WHERE kind='agent' AND state='outcome_unknown'`).Scan(&unknown); err != nil || unknown != 1 {
		t.Fatalf("ambiguous send was not retained: %d, %v", unknown, err)
	}
	var stored string
	if err := h.Pool.QueryRow(t.Context(), `SELECT concat(coalesce((SELECT json_agg(t)::text FROM olp.a2a_tasks t),''),coalesce((SELECT json_agg(d)::text FROM olp.execution_debits d),''),coalesce((SELECT json_agg(r)::text FROM olp.execution_debit_results r),''),coalesce((SELECT json_agg(r)::text FROM olp.agent_runs r),''),coalesce((SELECT json_agg(s)::text FROM olp.agent_steps s),''))`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "PRIVATE REQUEST") || strings.Contains(stored, "PRIVATE ARTIFACT") {
		t.Fatal("message content persisted")
	}
	redirectRPC.Store(false)
	redirectCard.Store(false)
	updated := h.want(owner, http.MethodPut, "/api/v1/agents/"+agent, map[string]any{
		"project_id": project, "name": "fixture",
		"agent": map[string]any{"protocol": "a2a-1.0.0", "endpoint": upstream.URL + "/rpc", "card_digest": strings.Repeat("a", 64), "toolset_revisions": []string{}, "route_revisions": []string{}, "pricing_revision_id": pricing, "max_cost": "1", "message_price": "0.50"},
	}, withMatch(published, idem("a2a-update")), http.StatusOK)
	h.want(owner, http.MethodPost, "/api/v1/agents/"+agent+"/publish", nil, withMatch(updated, idem("a2a-republish")), http.StatusOK)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if getCard(key, agent) != http.StatusBadGateway {
		t.Fatal("new revision did not enforce its card digest")
	}
	pinned := rpc(key, "GetTask", a2aTestParams{ID: protocol.TaskID(id)})
	if task, ok := pinned["result"].(map[string]any); !ok || task["id"] != id || !strings.Contains(fmt.Sprint(task), "PRIVATE ARTIFACT") {
		t.Fatalf("task did not retain its published revision: %+v", pinned)
	}
}

func TestStructuredPromptPublicationValidatesAndExports(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := h.want(owner, http.MethodPost, "/api/v1/projects", map[string]any{"name": "Structured prompts"}, idem("structured-project"), http.StatusCreated)["id"].(string)
	definition := json.RawMessage(`{"messages":[{"role":"system","content":"Help {{name}}"},{"role":"user","content":"Hello {{name}}"}],"variable_schema":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false},"default_parameters":{"temperature":0.25,"max_output_tokens":32}}`)
	created := h.want(owner, http.MethodPost, "/api/v1/prompts", map[string]any{"project_id": project, "name": "structured", "prompt": definition}, idem("structured-create"), http.StatusCreated)
	path := "/api/v1/prompts/" + created["id"].(string)
	published := h.want(owner, http.MethodPost, path+"/publish", nil, withMatch(created, idem("structured-publish")), http.StatusOK)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	exported := h.want(owner, http.MethodGet, "/api/v1/configuration/export", nil, nil, http.StatusOK)
	entries := exported["document"].(map[string]any)["agent_publications"].([]any)
	if len(entries) != 1 {
		t.Fatalf("exported publications = %v", entries)
	}
	publishedPrompt := entries[0].(map[string]any)["published"].(map[string]any)["prompt"]
	encoded, err := json.Marshal(publishedPrompt)
	if err != nil {
		t.Fatal(err)
	}
	var original, roundTrip map[string]any
	if err := json.Unmarshal(definition, &original); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &roundTrip); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"messages", "variable_schema", "default_parameters"} {
		if !reflect.DeepEqual(original[field], roundTrip[field]) {
			t.Fatalf("export changed %s: original=%v exported=%v", field, original[field], roundTrip[field])
		}
	}
	if published["revision_id"] == nil {
		t.Fatal("publication did not create a revision")
	}
	for _, tc := range []struct{ name, definition string }{
		{"role", `{"messages":[{"role":"tool","content":"{{name}}"}],"variable_schema":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}}`},
		{"schema-type", `{"messages":[{"role":"user","content":"{{name}}"}],"variable_schema":{"type":"object","properties":{"name":{"type":"integer"}},"required":["name"]}}`},
		{"schema-extra", `{"messages":[{"role":"user","content":"{{name}}"}],"variable_schema":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":true}}`},
		{"defaults", `{"messages":[{"role":"user","content":"{{name}}"}],"variable_schema":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]},"default_parameters":{"temperature":3}}`},
		{"size", `{"messages":[{"role":"user","content":` + strconv.Quote(strings.Repeat("x", prompts.MaxTemplateBytes+1)) + `}],"variable_schema":{"type":"object","properties":{},"required":[]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.want(owner, http.MethodPost, "/api/v1/prompts", map[string]any{"project_id": project, "name": "invalid", "prompt": json.RawMessage(tc.definition)}, idem("structured-invalid-"+tc.name), http.StatusUnprocessableEntity)
		})
	}
}
