//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/secrets"
)

type identifiedCodeRuntime struct {
	*codeForwardRuntime
	auth *secrets.AuthKey
}

func (r identifiedCodeRuntime) EndUserDigest(project *string, identifier string) string {
	return access.DigestEndUser(r.auth, project, identifier)
}

func TestCodeEndUsersRemainDigestOnlyThroughAccountingAndDiagnostics(t *testing.T) {
	f := newCodeFixture(t)
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-OLP-End-User") != "" {
			t.Error("end-user header reached provider")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_%d\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n", calls.Add(1))
	}))
	defer upstream.Close()
	server, _, _ := codeForwardServer(t, f, upstream.URL)
	rt := f.h.Gateway.Runtime.(*codeForwardRuntime)
	rt.authority.Policy.AllowedRoutes = nil
	rt.authority.Policy.AllowedRouteGroups = []string{"subscription"}
	groups := access.RouteGroups{"subscription": {"coding"}}
	if err := rt.authority.BindRouteGroups(groups); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE olp.projects SET route_groups=$2 WHERE id=$1`, f.project, groups.JSON())
	source := "header"
	rt.authority.Policy.EndUserSource = &source
	rt.authority.Policy.RequiredAttributionKeys = []string{"team"}
	rt.authority.Policy.AttributionDefaults = map[string]string{"team": "core"}
	rt.authority.ProjectAttributionPolicy = attribution.Policy{Defaults: map[string]string{"env": "prod"}}
	labels, _ := json.Marshal(rt.authority.ProjectAttributionPolicy)
	f.exec(t, `UPDATE olp.projects SET attribution_policy=$2 WHERE id=$1`, rt.authority.ProjectID, labels)
	policy, err := json.Marshal(rt.authority.Policy)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE olp.api_keys SET policy=$2 WHERE id=$1`, f.key, policy)
	f.h.Gateway.Runtime = identifiedCodeRuntime{rt, f.h.Server.Auth}
	identifiers := []string{"private-code-customer-one", "private-code-customer-two"}
	request := func(identifier, model string, thread int, want int) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"model": model, "input": "hello", "stream": true})
		r, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/code/coding/responses", strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer olp-code-fixture")
		r.Header.Set("Thread-Id", fmt.Sprintf("thread-%d", thread))
		r.Header.Set("X-OLP-End-User", identifier)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != want {
			t.Fatalf("status %d: %s: %v", resp.StatusCode, data, err)
		}
	}
	for i, identifier := range identifiers {
		request(identifier, "native-model", i, 200)
	}
	codeForwardAwait(t, f, "settled", 2)
	request(identifiers[0], "denied-model", 3, 403)
	if calls.Load() != 2 {
		t.Fatal("refused request dispatched")
	}
	for i, identifier := range identifiers {
		digest := access.DigestEndUser(f.h.Server.Auth, &f.project, identifier)
		for _, collection := range []string{"attempts", "refusals"} {
			path := "/api/v1/code/" + collection + "?project_id=" + f.project + "&end_user_digest=" + digest
			items := f.h.want(f.owner, "GET", path, nil, nil, 200)["items"].([]any)
			want := 1
			if collection == "refusals" && i == 1 {
				want = 0
			}
			if len(items) != want {
				t.Fatalf("%s customer %d: %v", collection, i, items)
			}
			if collection == "attempts" && len(items) > 0 {
				labels := items[0].(map[string]any)["attribution"].(map[string]any)
				if labels["team"] != "core" || labels["env"] != "prod" {
					t.Fatalf("code attribution: %v", labels)
				}
			}
			if len(items) > 0 && items[0].(map[string]any)["end_user_digest"] != digest {
				t.Fatal("identity lost")
			}
		}
		for _, table := range []string{"code_attempts", "code_refusals", "code_bindings"} {
			var leaked bool
			if err := f.h.Pool.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM olp."+table+" x WHERE to_jsonb(x)::text LIKE '%' || $1 || '%')", identifier).Scan(&leaked); err != nil || leaked {
				t.Fatalf("raw identity in %s: %v", table, err)
			}
		}
	}
	for _, collection := range []string{"attempts", "refusals"} {
		path := "/api/v1/code/" + collection
		f.h.want(f.owner, "GET", path+"?end_user_digest=raw-user", nil, nil, 422)
		if items := f.h.want(f.owner, "GET", path+"?end_user_digest=unidentified", nil, nil, 200)["items"].([]any); len(items) != 0 {
			t.Fatal("identified rows matched unidentified filter")
		}
	}
	invalid := f.input("invalid-identity", "", nil)
	invalid.EndUserDigest = identifiers[0]
	if _, err := f.store.Admit(t.Context(), invalid); err == nil {
		t.Fatal("ledger accepted a raw identifier")
	}
	if err := f.store.RecordRefusal(t.Context(), f.route, f.key, identifiers[0], "code_permission_denied"); err == nil {
		t.Fatal("refusal ledger accepted a raw identifier")
	}
	// The cached gateway still grants this group. Durable admission must consult
	// current project membership before dispatching another generation.
	f.exec(t, `UPDATE olp.projects SET route_groups='{}' WHERE id=$1`, f.project)
	request(identifiers[0], "native-model", 9, 403)
	if calls.Load() != 2 {
		t.Fatal("removed subscription group dispatched")
	}

}
