//go:build e2e

package e2e_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/tyk-swe/olp/tests/harness"
)

func TestCapturePolicyExpiryLifecycle(t *testing.T) {
	t.Parallel()
	s := startService(t)
	owner := s.Owner()
	project := s.Want(owner, "POST", "/api/v1/projects", map[string]any{"name": "capture"}, harness.Idempotent(), http.StatusCreated)["id"].(string)
	sink := s.Want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
		"name": "capture", "type": "https", "destination": "https://sinks.example.com/capture", "streams": []string{"requests"}, "format": "json",
	}, harness.Idempotent(), http.StatusCreated)
	const config = "/api/v1/observability/capture"
	s.Want(owner, "PATCH", config, map[string]any{"enabled": true}, harness.ETag(s.Want(owner, "GET", config, nil, nil, http.StatusOK)), http.StatusOK)
	const policies = "/api/v1/observability/capture-policies"
	policy := s.Want(owner, "POST", policies, map[string]any{
		"project_id": project, "sink": sink["export_sink_id"], "sample_ratio": "1", "include": []string{"input", "output"},
		"max_bytes": 65536, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
	}, harness.Idempotent(), http.StatusCreated)
	path := policies + "/" + policy["id"].(string)

	unchanged := s.Want(owner, "PATCH", path, map[string]any{"sample_ratio": "1"}, harness.ETag(policy), http.StatusOK)
	if unchanged["expires_at"] != policy["expires_at"] {
		t.Fatalf("unrelated edit moved the deadline: %v -> %v", policy["expires_at"], unchanged["expires_at"])
	}
	deadline := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Millisecond)
	shifted := deadline.In(time.FixedZone("fixture", 3600)).Format(time.RFC3339Nano)
	widened := s.Want(owner, "PATCH", path, map[string]any{"include": []string{"input"}, "expires_at": shifted}, harness.ETag(unchanged), http.StatusOK)
	listed := s.Want(owner, "GET", policies, nil, nil, http.StatusOK)["items"].([]any)
	if widened["expires_at"] != deadline.Format(time.RFC3339Nano) || listed[0].(map[string]any)["expires_at"] != widened["expires_at"] {
		t.Fatalf("API did not return the new deadline in UTC: update %v, list %v", widened["expires_at"], listed)
	}
	resent := s.Want(owner, "PATCH", path, map[string]any{"expires_at": shifted}, harness.ETag(widened), http.StatusOK)
	cleared := s.Want(owner, "PATCH", path, map[string]any{"expires_at": nil}, harness.ETag(resent), http.StatusOK)
	if expiry, present := cleared["expires_at"]; !present || expiry != nil {
		t.Fatalf("policy still has an expiry after clearing it: %v", cleared)
	}

	for action, want := range map[string]int{"capture_policy.update": 4, "capture_policy.expiry.update": 2} {
		var audited int
		if err := s.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.audit WHERE action=$1 AND resource_id=$2 AND project_id=$3`, action, policy["id"], project).Scan(&audited); err != nil || audited != want {
			t.Fatalf("%s audit rows = %d, want %d, error = %v", action, audited, want, err)
		}
	}
}
