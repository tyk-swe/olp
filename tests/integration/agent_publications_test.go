//go:build integration

package integration_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

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
