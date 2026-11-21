//go:build e2e

package e2e_test

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/apikeys"
	"github.com/tyk-swe/olp/tests/harness"
	"github.com/tyk-swe/olp/tests/harness/vendor"
)

func TestOverlapRecordingFailureDoesNotInterruptAdmittedInference(t *testing.T) {
	t.Parallel()
	s := startService(t)
	owner := s.Owner()
	project := s.Want(owner, "POST", "/api/v1/projects", map[string]any{"name": "overlap"}, harness.Idempotent(), http.StatusCreated)["id"].(string)
	provider := s.activeProvider(owner, "overlap-provider", newVendor(t).URL, project)
	const route = "overlap-chat"
	s.publishedRoute(owner, route, provider, project)
	old := s.Want(owner, "POST", "/api/v1/api-keys", map[string]any{
		"name": "overlap", "scopes": []string{"inference"}, "project_id": project,
	}, harness.Idempotent(), http.StatusCreated)
	id, lookup := old["id"].(string), old["lookup_id"].(string)
	path := "/api/v1/api-keys/" + id
	rotate := func(prev map[string]any, seconds int) map[string]any {
		return s.Want(owner, "POST", path+"/rotate", map[string]int{"overlap_seconds": seconds}, harness.Idempotent(harness.ETag(prev)), http.StatusOK)
	}
	middle := rotate(old, 180)
	current := rotate(middle, 180)

	overlaps := func() []any {
		return s.Want(owner, "GET", path, nil, nil, http.StatusOK)["active_overlaps"].([]any)
	}
	evidence := func(lookupID string) map[string]any {
		t.Helper()
		for _, raw := range overlaps() {
			if row := raw.(map[string]any); row["lookup_id"] == lookupID {
				return row
			}
		}
		t.Fatal("active overlap missing from key detail")
		return nil
	}
	if row := evidence(lookup); row["observation_status"] != "ok" || row["last_observed_use"] != nil || row["last_recording_error_at"] != nil {
		t.Fatalf("unused overlap did not report healthy empty evidence: %+v", row)
	}
	request := map[string]any{"model": route, "messages": []any{map[string]any{"role": "user", "content": "hello"}}}
	call := func(secret string) (int, []byte) {
		return s.Do(&harness.Caller{Name: "test consumer", Token: secret}, "POST", "/v1/chat/completions", request, nil)
	}
	s.Eventually("current secret serving the published route", harness.StartupLimit, func() (bool, string) {
		status, raw := call(current["secret"].(string))
		return status == http.StatusOK, describe(status, raw)
	})
	if status, raw := call("olp_" + lookup + "_wrong"); status != http.StatusUnauthorized {
		t.Fatal(describe(status, raw))
	}

	exec := func(sql string) {
		t.Helper()
		if _, err := s.Pool.Exec(t.Context(), sql); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE FUNCTION olp.test_overlap_failure() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN RAISE EXCEPTION 'injected observation persistence failure'; END $$;
 CREATE TRIGGER test_overlap_failure BEFORE UPDATE OF last_observed_use ON olp.api_key_overlaps
 FOR EACH ROW EXECUTE FUNCTION olp.test_overlap_failure()`)
	serveOld := func() {
		t.Helper()
		if status, body := call(old["secret"].(string)); status != http.StatusOK || !strings.Contains(string(body), vendor.Answer) {
			t.Fatal(describe(status, body))
		}
	}
	serveOld()
	s.Eventually("failed observation visible on key detail", 5*time.Second, func() (bool, string) {
		return evidence(lookup)["observation_status"] == "degraded", "observation status not degraded"
	})
	if row := evidence(lookup); row["last_observed_use"] != nil || row["last_recording_error_at"] == nil {
		t.Fatalf("failed observation advanced the timestamp or lost its warning: %+v", row)
	}
	exec("DROP TRIGGER test_overlap_failure ON olp.api_key_overlaps")
	serveOld()
	s.Eventually("successful observation visible on key detail", 5*time.Second, func() (bool, string) {
		return evidence(lookup)["last_observed_use"] != nil, "observation is missing"
	})
	row := evidence(lookup)
	if row["observation_status"] != "degraded" || len(row) != 5 {
		t.Fatalf("overlap lost its warning or exposed extra data: %+v", row)
	}
	if other := evidence(middle["lookup_id"].(string)); other["last_observed_use"] != nil || other["observation_status"] != "ok" {
		t.Fatalf("another version's evidence changed: %+v", other)
	}
	assertOverlapIsolationAndRetirement(t, s, owner, path, current, row)
}

func assertOverlapIsolationAndRetirement(t *testing.T, s *service, owner *harness.Caller, path string, current, row map[string]any) {
	t.Helper()
	other := s.Want(owner, "POST", "/api/v1/projects", map[string]any{"name": "other"}, harness.Idempotent(), http.StatusCreated)["id"].(string)
	token := s.Want(owner, "POST", "/api/v1/management-tokens", map[string]any{
		"name": "other-project", "scopes": []string{"read"}, "project_ids": []string{other},
		"expires_at": time.Now().Add(time.Hour),
	}, harness.Idempotent(), http.StatusCreated)
	caller := &harness.Caller{Name: "other project", Token: token["secret"].(string)}
	s.Want(caller, "GET", path, nil, nil, http.StatusNotFound)

	recorder := apikeys.NewOverlapRecorder(s.Pool, slog.Default())
	id, lookup := current["id"].(string), row["lookup_id"].(string)
	observed, err := time.Parse(time.RFC3339Nano, row["last_observed_use"].(string))
	if err != nil {
		t.Fatal(err)
	}
	expires, err := time.Parse(time.RFC3339Nano, row["expires_at"].(string))
	if err != nil {
		t.Fatal(err)
	}
	overlap := keyauthority.SecretOverlap{LookupID: lookup, ExpiresAt: expires}
	recorder.Observe(id, overlap, observed.Add(-time.Second))
	recorder.Flush(t.Context())
	detail := s.Want(owner, "GET", path, nil, nil, http.StatusOK)
	for _, raw := range detail["active_overlaps"].([]any) {
		if item := raw.(map[string]any); item["lookup_id"] == lookup && item["last_observed_use"] != row["last_observed_use"] {
			t.Fatalf("an older observation rewound overlap evidence: %+v", item)
		}
	}

	recorder.Observe(id, overlap, observed.Add(time.Second))
	s.Want(owner, "POST", path+"/rotate", map[string]int{"overlap_seconds": 0}, harness.Idempotent(harness.ETag(current)), http.StatusOK)
	recorder.Flush(t.Context())
	if got := s.Want(owner, "GET", path, nil, nil, http.StatusOK)["active_overlaps"].([]any); len(got) != 0 {
		t.Fatalf("a delayed observation restored a retired overlap: %+v", got)
	}
}
