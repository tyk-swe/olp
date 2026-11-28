//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/retrieval"
	"github.com/tyk-swe/olp/tests/harness"
)

func decodeRetrievalBinding(t *testing.T, body []byte) retrieval.Binding {
	t.Helper()
	var binding retrieval.Binding
	if err := json.Unmarshal(body, &binding); err != nil {
		t.Fatalf("decode binding %s: %v", body, err)
	}
	return binding
}

func decodeRetrievalPage(t *testing.T, body []byte) []retrieval.Binding {
	t.Helper()
	var page struct {
		Items []retrieval.Binding `json:"items"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode list %s: %v", body, err)
	}
	return page.Items
}

func decodeRetrievalRevision(t *testing.T, body []byte) (int64, retrieval.Input) {
	t.Helper()
	var revision struct {
		Revision   int64           `json:"revision"`
		Definition retrieval.Input `json:"definition"`
	}
	if err := json.Unmarshal(body, &revision); err != nil {
		t.Fatalf("decode revision %s: %v", body, err)
	}
	return revision.Revision, revision.Definition
}

func TestRetrievalBindingHTTPLifecycle(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool, _ := harness.Database(t)
	f := newUsageFixture(t, pool)

	const kbResource = "KB12345678"
	const routeSlug = "kb-search"
	bedrockProvider := uuid.NewString()
	targetID := uuid.NewString()
	otherProject := uuid.NewString()
	otherKey := uuid.NewString()
	managerID := uuid.NewString()
	outsiderID := uuid.NewString()

	mustExec(t, pool, `INSERT INTO olp.providers (id,name,kind,state,configuration,etag,slots_etag,created_by,project_id)
		VALUES ($1::uuid,'bedrock-kb','bedrock','active','{}',$1::uuid,$1::uuid,$2,$3)`, bedrockProvider, f.ownerID, f.projectID)
	targetsJSON := fmt.Sprintf(`[{"id":%q,"provider_id":%q,"provider_model":%q}]`, targetID, bedrockProvider, kbResource)
	mustExec(t, pool, `INSERT INTO olp.route_drafts (id,slug,state,operations,overall_timeout_ms,max_attempts,targets,etag,created_by,project_id,fidelity)
		VALUES ($1::uuid,$2,'draft','["retrieval"]'::jsonb,5000,1,$3::jsonb,$1::uuid,$4,$5,'{"mode":"strict"}'::jsonb)`,
		uuid.NewString(), routeSlug, targetsJSON, f.ownerID, f.projectID)
	mustExec(t, pool, `INSERT INTO olp.projects (id,name,etag,created_by) VALUES ($1::uuid,'other-project',$1::uuid,$2)`, otherProject, f.ownerID)
	mustExec(t, pool, `INSERT INTO olp.api_keys (id,lookup_id,digest,name,created_by,policy,etag,project_id)
		VALUES ($1::uuid,$1::text,decode(repeat('00',32),'hex'),'Other key',$2,'{}',$1::uuid,$3)`, otherKey, f.ownerID, otherProject)
	for _, user := range []struct{ id, project string }{{managerID, f.projectID}, {outsiderID, otherProject}} {
		mustExec(t, pool, `INSERT INTO olp.users (id,email,display_name,role,access_scope,etag)
			VALUES ($1::uuid,$1::text||'@example.test','Manager','operator','assigned',$1::uuid)`, user.id)
		mustExec(t, pool, `INSERT INTO olp.project_members (project_id,user_id,role,added_by) VALUES ($1,$2,'manager',$3)`,
			user.project, user.id, f.ownerID)
	}

	register := func(mux *http.ServeMux, a *access.Server) {
		retrieval.NewAPI(a).Register(mux)
	}
	manager := managementHTTPAs(t, pool, managerID, register)
	outsider := managementHTTPAs(t, pool, outsiderID, register)
	authority := func() (string, int64) {
		t.Helper()
		var id string
		var seq int64
		if err := pool.QueryRow(ctx, `SELECT authority_id::text,authority_sequence FROM olp.installation WHERE singleton`).Scan(&id, &seq); err != nil {
			t.Fatal(err)
		}
		return id, seq
	}

	payload := func(name, project string, enabled bool, grants []string, fields retrieval.Fields) string {
		raw, err := json.Marshal(retrieval.Input{
			ProjectID: project, Name: name, Route: routeSlug, TargetID: targetID,
			Backend: "bedrock_knowledge_base", Resource: kbResource,
			Fields: fields, KeyGrants: grants, Enabled: enabled,
		})
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	expectStatus(t, "", manager.call(t, "POST", "/api/v1/retrieval-bindings", "", payload("bad-key", f.projectID, true, []string{otherKey}, retrieval.Fields{})), http.StatusUnprocessableEntity)
	expectStatus(t, "", manager.call(t, "POST", "/api/v1/retrieval-bindings", "", payload("bad-fields", f.projectID, true, []string{f.keyID}, retrieval.Fields{Text: "content"})), http.StatusUnprocessableEntity)
	expectStatus(t, "", outsider.call(t, "POST", "/api/v1/retrieval-bindings", "", payload("outsider", f.projectID, true, []string{f.keyID}, retrieval.Fields{})), http.StatusNotFound)

	const createKey = "retrieval-create-1"
	idBeforeCreate, seqBeforeCreate := authority()
	createBody := payload("Primary KB", f.projectID, false, []string{f.keyID, f.keyID}, retrieval.Fields{Metadata: map[string]string{"tenant": "tenant_id"}})
	created := expectStatus(t, "", manager.call(t, "POST", "/api/v1/retrieval-bindings", "", createBody, createKey), http.StatusCreated)
	binding := decodeRetrievalBinding(t, created.body)
	if created.location != "/api/v1/retrieval-bindings/"+binding.ID || binding.ProviderID != bedrockProvider || binding.Revision != 1 || len(binding.KeyGrants) != 1 || binding.KeyGrants[0] != f.keyID {
		t.Fatalf("unexpected created binding: loc=%q binding=%+v", created.location, binding)
	}
	if id, seq := authority(); id == idBeforeCreate || seq <= seqBeforeCreate {
		t.Fatalf("authority did not advance on create: id %s -> %s, sequence %d -> %d", idBeforeCreate, id, seqBeforeCreate, seq)
	}

	replayed := expectStatus(t, "", manager.call(t, "POST", "/api/v1/retrieval-bindings", "", createBody, createKey), http.StatusCreated)
	if replayed.etag != created.etag || decodeRetrievalBinding(t, replayed.body).ID != binding.ID {
		t.Fatalf("idempotent replay diverged: %+v vs %+v", replayed, created)
	}
	expectStatus(t, "", manager.call(t, "POST", "/api/v1/retrieval-bindings", "", payload("Different KB", f.projectID, false, []string{f.keyID}, retrieval.Fields{}), createKey), http.StatusConflict)
	expectStatus(t, "", manager.call(t, "POST", "/api/v1/retrieval-bindings", "", createBody), http.StatusConflict)

	listed := expectStatus(t, "", manager.call(t, "GET", "/api/v1/retrieval-bindings", "", ""), http.StatusOK)
	if !slices.ContainsFunc(decodeRetrievalPage(t, listed.body), func(b retrieval.Binding) bool { return b.ID == binding.ID }) {
		t.Fatalf("manager list omitted binding %s: %s", binding.ID, listed.body)
	}
	outsiderList := expectStatus(t, "", outsider.call(t, "GET", "/api/v1/retrieval-bindings", "", ""), http.StatusOK)
	if slices.ContainsFunc(decodeRetrievalPage(t, outsiderList.body), func(b retrieval.Binding) bool { return b.ID == binding.ID }) {
		t.Fatalf("outsider list exposed foreign binding: %s", outsiderList.body)
	}
	path := "/api/v1/retrieval-bindings/" + binding.ID
	got := expectStatus(t, "", manager.call(t, "GET", path, "", ""), http.StatusOK)
	if got.etag != created.etag {
		t.Fatalf("GET ETag %q != created %q", got.etag, created.etag)
	}
	expectStatus(t, "", outsider.call(t, "GET", path, "", ""), http.StatusNotFound)

	updateBody := payload("Primary KB v2", f.projectID, true, []string{f.keyID}, retrieval.Fields{Metadata: map[string]string{"tenant": "tenant_id"}})
	expectStatus(t, "", manager.call(t, "PUT", path, "", updateBody), http.StatusPreconditionRequired)
	expectStatus(t, "", manager.call(t, "PUT", path, `"`+uuid.NewString()+`"`, updateBody), http.StatusPreconditionFailed)
	expectStatus(t, "", manager.call(t, "PUT", path, created.etag, payload("Moved", otherProject, true, nil, retrieval.Fields{})), http.StatusUnprocessableEntity)
	expectStatus(t, "", outsider.call(t, "PUT", path, created.etag, updateBody), http.StatusNotFound)

	const updateKey = "retrieval-update-1"
	_, seqBeforeUpdate := authority()
	updated := expectStatus(t, "", manager.call(t, "PUT", path, created.etag, updateBody, updateKey), http.StatusOK)
	bindingV2 := decodeRetrievalBinding(t, updated.body)
	if bindingV2.Revision != 2 || !bindingV2.Enabled || bindingV2.Name != "Primary KB v2" || updated.etag == created.etag {
		t.Fatalf("unexpected updated binding: etag=%q binding=%+v", updated.etag, bindingV2)
	}
	if _, seq := authority(); seq <= seqBeforeUpdate {
		t.Fatalf("authority sequence did not advance on update")
	}
	replayedUpdate := expectStatus(t, "", manager.call(t, "PUT", path, created.etag, updateBody, updateKey), http.StatusOK)
	if replayedUpdate.etag != updated.etag || decodeRetrievalBinding(t, replayedUpdate.body).RevisionID != bindingV2.RevisionID {
		t.Fatalf("idempotent update replay diverged")
	}

	rev1 := expectStatus(t, "", manager.call(t, "GET", path+"/revisions/"+binding.RevisionID, "", ""), http.StatusOK)
	rev2 := expectStatus(t, "", manager.call(t, "GET", path+"/revisions/"+bindingV2.RevisionID, "", ""), http.StatusOK)
	if n, definition := decodeRetrievalRevision(t, rev1.body); n != 1 || definition.Name != "Primary KB" {
		t.Fatalf("unexpected first revision: revision=%d definition=%+v", n, definition)
	}
	if n, definition := decodeRetrievalRevision(t, rev2.body); n != 2 || definition.Name != "Primary KB v2" {
		t.Fatalf("unexpected second revision: revision=%d definition=%+v", n, definition)
	}
	expectStatus(t, "", outsider.call(t, "GET", path+"/revisions/"+binding.RevisionID, "", ""), http.StatusNotFound)

	_, seqBeforeRetire := authority()
	expectStatus(t, "", manager.call(t, "DELETE", path, created.etag, ""), http.StatusPreconditionFailed)
	expectStatus(t, "", manager.call(t, "DELETE", path, updated.etag, ""), http.StatusNoContent)
	if _, seq := authority(); seq <= seqBeforeRetire {
		t.Fatalf("authority sequence did not advance on retire")
	}
	afterRetireList := expectStatus(t, "", manager.call(t, "GET", "/api/v1/retrieval-bindings", "", ""), http.StatusOK)
	if slices.ContainsFunc(decodeRetrievalPage(t, afterRetireList.body), func(b retrieval.Binding) bool { return b.ID == binding.ID }) {
		t.Fatalf("retired binding still returned in list: %s", afterRetireList.body)
	}
	retiredDetail := expectStatus(t, "", manager.call(t, "GET", path, "", ""), http.StatusOK)
	expectStatus(t, "", manager.call(t, "PUT", path, retiredDetail.etag, updateBody), http.StatusConflict)

	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM olp.audit WHERE resource_type='retrieval_binding' AND resource_id=$1 AND action IN ('retrieval_binding.create','retrieval_binding.update','retrieval_binding.retire')`, binding.ID).Scan(&auditCount); err != nil || auditCount != 3 {
		t.Fatalf("retrieval binding audit rows = %d, want 3 (%v)", auditCount, err)
	}
}
