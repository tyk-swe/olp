//go:build e2e

package e2e_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/tests/harness"
)

func TestPausedAPIKeyRefusesEverySecretUntilResume(t *testing.T) {
	t.Parallel()
	s := startService(t)
	owner := s.Owner()
	project := s.Want(owner, "POST", "/api/v1/projects", map[string]any{"name": "pause"}, harness.Idempotent(), http.StatusCreated)["id"].(string)
	provider := s.activeProvider(owner, "pause-provider", newVendor(t).URL, project)
	const route = "pause-chat"
	s.publishedRoute(owner, route, provider, project)
	created := s.Want(owner, "POST", "/api/v1/api-keys", map[string]any{
		"name": "pause", "scopes": []string{"inference"}, "project_id": project,
	}, harness.Idempotent(), http.StatusCreated)
	id := created["id"].(string)
	path := "/api/v1/api-keys/" + id
	detail := func() map[string]any { return s.Want(owner, "GET", path, nil, nil, http.StatusOK) }
	rotated := s.Want(owner, "POST", path+"/rotate", map[string]int{"overlap_seconds": 600}, harness.Idempotent(harness.ETag(detail())), http.StatusOK)

	request := map[string]any{"model": route, "messages": []any{map[string]any{"role": "user", "content": "hello"}}}
	call := func(secret string) (int, []byte) {
		return s.Do(&harness.Caller{Name: "test consumer", Token: secret}, "POST", "/v1/chat/completions", request, nil)
	}
	serves := func(secret string) func() (bool, string) {
		return func() (bool, string) {
			status, raw := call(secret)
			return status == http.StatusOK, describe(status, raw)
		}
	}
	refusesPaused := func(secret string) func() (bool, string) {
		return func() (bool, string) {
			status, raw := call(secret)
			return status == http.StatusForbidden && strings.Contains(string(raw), "api_key_paused"), describe(status, raw)
		}
	}
	s.Eventually("current secret serving", harness.StartupLimit, serves(rotated["secret"].(string)))
	s.Eventually("overlap secret serving", harness.StartupLimit, serves(created["secret"].(string)))

	paused := s.Want(owner, "POST", path+"/pause", nil, harness.Idempotent(harness.ETag(detail())), http.StatusOK)
	if paused["paused"] != true || paused["paused_at"] == nil {
		t.Fatalf("pause response did not report the pause: %+v", paused)
	}
	generation, ok := paused["runtime_generation"].(map[string]any)
	if !ok || generation["sequence"] == nil {
		t.Fatalf("pause response lost its authority generation: %+v", paused)
	}
	if got := detail(); got["paused_at"] == nil {
		t.Fatalf("key detail did not report the pause: %+v", got)
	}
	s.Eventually("pause rollout enforced across serving replicas", harness.StartupLimit, func() (bool, string) {
		rollout := s.Want(owner, "GET", path+"/pause-rollout", nil, nil, http.StatusOK)
		return rollout["enforced"] == true, "pause rollout not enforced"
	})
	for _, secret := range []string{rotated["secret"].(string), created["secret"].(string)} {
		if refused, got := refusesPaused(secret)(); !refused {
			t.Fatalf("secret served after the pause rollout reported enforcement: %s", got)
		}
	}

	replacement := s.Want(owner, "POST", path+"/rotate", map[string]int{"overlap_seconds": 0}, harness.Idempotent(harness.ETag(detail())), http.StatusOK)
	if got := detail(); got["paused_at"] == nil {
		t.Fatalf("rotation cleared the pause: %+v", got)
	}

	resumed := s.Want(owner, "POST", path+"/resume", nil, harness.Idempotent(harness.ETag(detail())), http.StatusOK)
	if resumed["paused"] != false {
		t.Fatalf("resume response did not clear the pause: %+v", resumed)
	}
	s.Eventually("newest secret serving after resume", harness.StartupLimit, serves(replacement["secret"].(string)))
	for _, retired := range []string{rotated["secret"].(string), created["secret"].(string)} {
		if status, raw := call(retired); status == http.StatusOK {
			t.Fatalf("retired secret served after resume: %s", describe(status, raw))
		}
	}

	s.Want(owner, "POST", path+"/pause", nil, harness.Idempotent(harness.ETag(detail())), http.StatusOK)
	s.Want(owner, "POST", path+"/revoke", nil, harness.Idempotent(harness.ETag(detail())), http.StatusOK)
	s.Want(owner, "POST", path+"/resume", nil, harness.Idempotent(harness.ETag(detail())), http.StatusConflict)
}
