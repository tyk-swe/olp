//go:build integration

package integration_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/gateway"
)

func TestBudgetGroupIdentitySurvivesInFlightUnassignmentAndDeletion(t *testing.T) {
	h := newAccessHarness(t)
	c := limClient(t)
	h.Gateway.Admission = gateway.NewAdmission(limLimiter(t, c, limNamespace(t, c, "group-lifetime")), nil, slog.New(slog.DiscardHandler))
	owner := h.owner()
	group := h.want(owner, "POST", "/api/v1/budget-groups", map[string]any{"name": "inflight group", "requests_per_minute": 100}, idem(uuid.NewString()), 201)
	provider := newGeminiLifecycleProvider(t)
	provider.terminalUsage.Store(true)
	slug, key, _ := provisionGeminiLifecycle(t, h, owner, "gemini-interactions", provider, map[string]any{"budget_group_id": group["id"]})
	var keyID string
	if err := h.Pool.QueryRow(t.Context(), "SELECT id::text FROM olp.api_keys WHERE budget_group_id=$1", group["id"]).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	gate := &geminiReplyGate{method: http.MethodPost, suffix: "/interactions", reached: make(chan struct{}), release: make(chan struct{})}
	provider.replyGate.Store(gate)
	t.Cleanup(func() {
		select {
		case <-gate.release:
		default:
			close(gate.release)
		}
	})
	type reply struct {
		status int
		body   []byte
	}
	result := make(chan reply, 1)
	go func() {
		response, body := geminiPublic(t, h, http.MethodPost, "/gemini/v1beta/interactions", key, []byte(`{"model":"`+slug+`","input":"retained group","background":true}`))
		result <- reply{response.StatusCode, body}
	}()
	select {
	case <-gate.reached:
	case early := <-result:
		t.Fatalf("generation refused before dispatch: %d %s", early.status, early.body)
	case <-time.After(15 * time.Second):
		t.Fatal("native generation never reached provider")
	}
	currentKey := h.want(owner, "GET", "/api/v1/api-keys/"+keyID, nil, nil, 200)
	h.want(owner, "PATCH", "/api/v1/api-keys/"+keyID, map[string]any{"budget_group_id": nil}, withMatch(currentKey, idem(uuid.NewString())), 200)
	h.refresh()
	groupPath := "/api/v1/budget-groups/" + group["id"].(string)
	h.want(owner, "DELETE", groupPath, nil, withMatch(group, idem(uuid.NewString())), 204)
	close(gate.release)
	var accepted reply
	select {
	case accepted = <-result:
	case <-time.After(15 * time.Second):
		t.Fatal("accepted generation did not return")
	}
	if accepted.status != http.StatusOK {
		t.Fatalf("accepted generation: %d %s", accepted.status, accepted.body)
	}
	var native struct{ ID string }
	if err := json.Unmarshal(accepted.body, &native); err != nil || native.ID == "" {
		t.Fatalf("native accepted result: %s %v", accepted.body, err)
	}
	var originalGroup, original string
	if err := h.Pool.QueryRow(t.Context(), `SELECT metadata->'pending_usage'->>'budget_group_id',metadata->'pending_usage'->>'request_id' FROM olp.provider_resources WHERE id=$1`, strings.ReplaceAll(strings.TrimPrefix(native.ID, "interaction_"), "_", "")).Scan(&originalGroup, &original); err != nil || originalGroup != group["id"] {
		t.Fatalf("lost original group: %q %v", originalGroup, err)
	}
	response, body := geminiPublic(t, h, http.MethodGet, "/gemini/v1beta/interactions/"+native.ID, key, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("terminal generation: %d %s", response.StatusCode, body)
	}
	assertGeminiOriginalUsage(t, h, original, true, 5, 4)
	var storedGroup string
	if err := h.Pool.QueryRow(t.Context(), `SELECT budget_group_id::text FROM olp.attempt_usage_facts WHERE request_id=$1`, original).Scan(&storedGroup); err != nil || storedGroup != group["id"] {
		t.Fatalf("late original group receipt: %q %v", storedGroup, err)
	}
	h.want(owner, "GET", groupPath, nil, nil, 404)
	h.want(owner, "PATCH", groupPath, map[string]any{"requests_per_minute": 200}, withMatch(group, nil), 404)
	h.want(owner, "PATCH", "/api/v1/api-keys/"+keyID, map[string]any{"budget_group_id": group["id"]}, withMatch(h.want(owner, "GET", "/api/v1/api-keys/"+keyID, nil, nil, 200), idem(uuid.NewString())), 404)
	var retiredWithoutPolicy bool
	if err := h.Pool.QueryRow(t.Context(), `SELECT retired_at IS NOT NULL AND limit_template IS NULL AND requests_per_minute IS NULL AND tokens_per_minute IS NULL AND max_concurrency IS NULL AND daily_cost_limit IS NULL AND weekly_cost_limit IS NULL AND monthly_cost_limit IS NULL FROM olp.budget_groups WHERE id=$1`, group["id"]).Scan(&retiredWithoutPolicy); err != nil || !retiredWithoutPolicy {
		t.Fatalf("deleted group retained policy: %v %v", retiredWithoutPolicy, err)
	}
	// A reused visible name creates a distinct accounting identity.
	replacement := h.want(owner, "POST", "/api/v1/budget-groups", map[string]any{"name": "inflight group", "requests_per_minute": 100}, idem(uuid.NewString()), 201)
	if replacement["id"] == group["id"] {
		t.Fatal("replacement reused retired accounting identity")
	}
}

func TestBudgetGroupRetirementPreservesDependencyRefusalsAndReplay(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	group := h.want(owner, "POST", "/api/v1/budget-groups", map[string]any{"name": "dependent group", "requests_per_minute": 100}, idem(uuid.NewString()), 201)
	path := "/api/v1/budget-groups/" + group["id"].(string)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "group member", "budget_group_id": group["id"]}, idem(uuid.NewString()), 201)
	h.want(owner, "DELETE", path, nil, withMatch(group, idem(uuid.NewString())), 409)
	h.want(owner, "PATCH", "/api/v1/api-keys/"+key["id"].(string), map[string]any{"budget_group_id": nil}, withMatch(h.want(owner, "GET", "/api/v1/api-keys/"+key["id"].(string), nil, nil, 200), idem(uuid.NewString())), 200)
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.budget_group_cost_windows(budget_group_id,window_kind,window_id,accrued,unpriced_attempts) VALUES($1,'day',0,0,0)`, group["id"]); err != nil {
		t.Fatal(err)
	}
	h.want(owner, "DELETE", path, nil, withMatch(group, idem(uuid.NewString())), 409)
	if _, err := h.Pool.Exec(t.Context(), "DELETE FROM olp.budget_group_cost_windows WHERE budget_group_id=$1", group["id"]); err != nil {
		t.Fatal(err)
	}
	headers := withMatch(group, idem(uuid.NewString()))
	h.want(owner, "DELETE", path, nil, headers, 204)
	h.want(owner, "DELETE", path, nil, headers, 204)
	h.want(owner, "DELETE", path, nil, withMatch(group, idem(uuid.NewString())), 404)
	h.want(owner, "GET", path, nil, nil, 404)
}
