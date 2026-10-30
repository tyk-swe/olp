//go:build integration && codecli

package integration_test

import (
	"bytes"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
)

func TestCodeQualificationGenericAffinityPreservesAuthority(t *testing.T) {
	f := newCodeFleet(t)
	second := f.h.want(f.owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Second coding key", "project_id": f.project, "scopes": []string{"inference"}, "allowed_routes": []string{"qualification"}}, idem("generic-second-key"), 201)
	foreignProject := createProject(f.h, f.owner, "Other coding project")
	foreign := f.h.want(f.owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Other project key", "project_id": foreignProject, "scopes": []string{"inference"}, "allowed_routes": []string{"qualification"}}, idem("generic-foreign-key"), 201)
	f.h.want(f.owner, "PUT", "/api/v1/code/pools/"+f.pool["id"].(string), f.poolInput([]string{f.keyID, second["id"].(string)}), etagHeader(f.pool), 200)
	if err := f.gateways[1].Stop(8 * time.Second); err != nil {
		t.Fatal(err)
	}
	f.gateways[1] = f.in.replica("generic-keys")
	request := func(key string, body []byte, status int) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.gateways[1].PublicOrigin+"/code/qualification/responses", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Olp-Code-Affinity", "same-client-affinity")
		response, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != status {
			t.Fatalf("generic authority request returned %d body %s error %v", response.StatusCode, data, err)
		}
	}
	request(f.key, codeBody(), http.StatusOK)
	continuation := []byte(`{"model":"gpt-5.4","previous_response_id":"resp-controlled","input":[]}`)
	request(second["secret"].(string), continuation, http.StatusConflict)
	if len(f.peer.Requests()) != 1 {
		t.Fatal("another key reused a generic response reference")
	}
	request(second["secret"].(string), codeBody(), http.StatusOK)
	request(f.key, continuation, http.StatusOK)
	bindings := codePublicDecode[[]codemode.Binding](t, f.list("bindings"))
	if len(bindings) != 2 || bindings[0].RootID == bindings[1].RootID || bindings[0].APIKeyID == bindings[1].APIKeyID || bindings[0].Conversation != bindings[1].Conversation {
		t.Fatal("equal generic affinity escaped API-key isolation")
	}
	request(foreign["secret"].(string), codeBody(), http.StatusForbidden)
	if len(f.peer.Requests()) != 3 {
		t.Fatal("another project's generic affinity reached upstream")
	}
}

func TestCodeQualificationDurableResponseReferencesAndGeneratedConfiguration(t *testing.T) {
	f := newCodeFleet(t)
	path := "/api/v1/code/routes/" + f.route["id"].(string) + "/client-config?gateway_url=" + f.gateways[0].PublicOrigin
	configuration := f.h.want(f.owner, "GET", path, nil, nil, 200)
	if configuration["base_url"] != f.gateways[0].PublicOrigin+"/code/qualification" ||
		!bytes.Contains([]byte(configuration["configuration"].(string)), []byte("OLP_API_KEY")) {
		t.Fatal("configuration lost public route or OLP-only authentication")
	}
	f.success(t, 0, "reference-root", "")
	if err := f.gateways[0].Stop(8 * time.Second); err != nil {
		t.Fatal(err)
	}
	f.gateways[0] = f.in.replica("reference-restarted")
	body := []byte(`{"model":"gpt-5.4","stream":true,"previous_response_id":"resp-controlled","input":[]}`)
	for _, conversation := range []string{"reference-root", "reference-child"} {
		response, data, err := f.request(t.Context(), 0, conversation, "", body)
		if err != nil || response.StatusCode != 200 {
			t.Fatalf("durable reference refused after restart: %v %s", err, data)
		}
	}
	bindings := codePublicDecode[[]codemode.Binding](t, f.list("bindings"))
	if len(bindings) != 2 || bindings[0].RootID != bindings[1].RootID || bindings[0].AccountID != bindings[1].AccountID {
		t.Fatal("response continuation did not inherit the durable tree")
	}
	missing := []byte(`{"model":"gpt-5.4","previous_response_id":"never-observed","input":[]}`)
	before := len(f.peer.Requests())
	response, data, err := f.request(t.Context(), 1, "orphan-reference", "", missing)
	if err != nil || response.StatusCode != 409 || !bytes.Contains(data, []byte("code_parent_unresolved")) || len(f.peer.Requests()) != before {
		t.Fatal("unknown reference reached upstream")
	}
	f.h.want(f.owner, "POST", "/api/v1/code/bindings/"+bindings[0].ID+"/retire", nil, idem("reference-retirement"), 200)
	response, _, err = f.request(t.Context(), 1, "retired-reference", "", body)
	if err != nil || response.StatusCode != 410 || len(f.peer.Requests()) != before {
		t.Fatal("reference revived a retired tree")
	}
	f.success(t, 1, "new-reference-root", "")
	before = len(f.peer.Requests())
	response, data, err = f.request(t.Context(), 1, "new-reference-root", "", body)
	if err != nil || response.StatusCode != 409 || !bytes.Contains(data, []byte("code_parent_unresolved")) || len(f.peer.Requests()) != before {
		t.Fatal("ambiguous upstream response ID authorized a continuation")
	}
}
