//go:build e2e

package e2e_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/tests/harness"
	"github.com/tyk-swe/olp/tests/harness/vendor"
)

func TestCapturePolicyExpiryDoesNotInterruptInference(t *testing.T) {
	t.Parallel()
	s := startService(t)
	owner := s.Owner()
	project := s.Want(owner, "POST", "/api/v1/projects", map[string]any{"name": "capture"}, harness.Idempotent(), http.StatusCreated)["id"].(string)
	var stopAt atomic.Pointer[time.Time]
	arrived := make(chan time.Time, 1)
	upstream := captureVendor(&stopAt, arrived)
	t.Cleanup(upstream.Close)
	provider := s.activeProvider(owner, "capture-fixture", upstream.URL, project)
	slug := uniqueName("capture")
	s.publishedRoute(owner, slug, provider, project)
	activeKey := captureKey(s, owner, project, "active")
	crossingKey := captureKey(s, owner, project, "crossing")
	expiredKey := captureKey(s, owner, project, "expired")
	policy := createExpiringCapturePolicy(s, owner, project, upstream.URL+"/sink")
	const policies = "/api/v1/observability/capture-policies"
	path := policies + "/" + policy["id"].(string)
	unchanged := s.Want(owner, "PATCH", path, map[string]any{"sample_ratio": "1"}, harness.ETag(policy), http.StatusOK)
	if unchanged["expires_at"] != policy["expires_at"] {
		t.Fatalf("unrelated edit moved the deadline: %v -> %v", policy["expires_at"], unchanged["expires_at"])
	}
	deadline := time.Now().Add(8 * time.Second).UTC().Truncate(time.Millisecond)
	stopAt.Store(&deadline)
	shifted := deadline.In(time.FixedZone("fixture", 3600)).Format(time.RFC3339Nano)
	updated := s.Want(owner, "PATCH", path, map[string]any{"expires_at": shifted}, harness.ETag(unchanged), http.StatusOK)
	listed := s.Want(owner, "GET", policies, nil, nil, http.StatusOK)["items"].([]any)
	if listed[0].(map[string]any)["expires_at"] != deadline.Format(time.RFC3339Nano) {
		t.Fatalf("list did not expose the new UTC deadline: %v", listed)
	}
	var audited int
	if err := s.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.audit WHERE action='capture_policy.expiry.update' AND resource_id=$1`, policy["id"]).Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("expiry audit count = %d, error = %v", audited, err)
	}

	s.Eventually("capture active before expiry", time.Until(deadline.Add(-time.Second)), func() (bool, string) {
		if ok, detail := requestCaptureCompletion(s, slug, activeKey, "active"); !ok {
			return false, detail
		}
		return captureRecorded(s, activeKey, true)
	})
	time.Sleep(time.Until(deadline.Add(-time.Second)))
	if ok, detail := requestCaptureCompletion(s, slug, crossingKey, "hold across capture expiry"); !ok {
		t.Fatalf("crossing inference failed: %s", detail)
	}
	select {
	case started := <-arrived:
		if !started.Before(deadline) || time.Now().Before(deadline) {
			t.Fatalf("request did not cross the deadline: upstream start %v, deadline %v", started, deadline)
		}
	default:
		t.Fatal("crossing request did not reach the upstream fixture")
	}
	if ok, detail := requestCaptureCompletion(s, slug, expiredKey, "expired"); !ok {
		t.Fatalf("post-expiry inference failed: %s", detail)
	}
	for _, key := range []map[string]any{crossingKey, expiredKey} {
		s.Eventually(fmt.Sprintf("%s request settled without capture", key["name"]), 5*time.Second, func() (bool, string) {
			return captureRecorded(s, key, false)
		})
	}
	cleared := s.Want(owner, "PATCH", path, map[string]any{"expires_at": nil}, harness.ETag(updated), http.StatusOK)
	if expiry, present := cleared["expires_at"]; !present || expiry != nil {
		t.Fatalf("policy still has an expiry after clearing it: %v", cleared)
	}
}

func captureVendor(deadline *atomic.Pointer[time.Time], arrived chan<- time.Time) *httptest.Server {
	fixture := vendor.Handler()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sink" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if at := deadline.Load(); at != nil && bytes.Contains(body, []byte("hold across capture expiry")) {
			arrived <- time.Now()
			time.Sleep(time.Until(*at) + 10*time.Millisecond)
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		fixture.ServeHTTP(w, r)
	}))
}

func createExpiringCapturePolicy(s *service, owner *harness.Caller, project, destination string) map[string]any {
	sink := s.Want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
		"name": "capture", "type": "https", "destination": destination, "streams": []string{"requests"}, "format": "json",
	}, harness.Idempotent(), http.StatusCreated)
	const config = "/api/v1/observability/capture"
	current := s.Want(owner, "GET", config, nil, nil, http.StatusOK)
	s.Want(owner, "PATCH", config, map[string]any{"enabled": true}, harness.ETag(current), http.StatusOK)
	return s.Want(owner, "POST", "/api/v1/observability/capture-policies", map[string]any{
		"project_id": project, "sink": sink["export_sink_id"], "sample_ratio": "1", "include": []string{"input", "output"},
		"max_bytes": 65536, "expires_at": time.Now().Add(6 * time.Second).UTC().Format(time.RFC3339Nano),
	}, harness.Idempotent(), http.StatusCreated)
}

func captureKey(s *service, owner *harness.Caller, project, name string) map[string]any {
	return s.Want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": name, "scopes": []string{"inference"}, "project_id": project}, harness.Idempotent(), http.StatusCreated)
}

func requestCaptureCompletion(s *service, slug string, key map[string]any, content string) (bool, string) {
	s.T.Helper()
	consumer := &harness.Caller{Name: "capture fixture key", Token: key["secret"].(string)}
	status, raw := s.Do(consumer, "POST", "/v1/chat/completions", map[string]any{
		"model": slug, "messages": []map[string]string{{"role": "user", "content": content}},
	}, nil)
	return status == http.StatusOK && strings.Contains(string(raw), vendor.Answer), describe(status, raw)
}

func captureRecorded(s *service, key map[string]any, want bool) (bool, string) {
	var captured bool
	var status int
	err := s.Pool.QueryRow(s.T.Context(), `SELECT payload_captured,status_code FROM olp.requests
 WHERE api_key_id=$1 AND completed_at IS NOT NULL ORDER BY started_at DESC LIMIT 1`, key["id"]).Scan(&captured, &status)
	return err == nil && status == http.StatusOK && captured == want, fmt.Sprintf("status=%d captured=%t error=%v", status, captured, err)
}
