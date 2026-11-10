//go:build e2e

package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/tests/harness/vendor"
)

// TestPublishedRouteServesAndSettlesUsage takes one request from publication
// through the gateway to durable usage, across the Valkey stream and the
// consumer that writes PostgreSQL.
func TestPublishedRouteServesAndSettlesUsage(t *testing.T) {
	t.Parallel()
	s := startService(t)
	owner := s.owner()
	project := s.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "journey"}, idempotent(), http.StatusCreated)["id"].(string)
	provider := s.activeProvider(owner, "journey-provider", newVendor(t).URL, project)
	slug := uniqueName("journey")
	s.publishedRoute(owner, slug, provider, project)
	key := s.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "journey", "scopes": []string{"inference"}, "project_id": project}, idempotent(), http.StatusCreated)
	consumer := &caller{name: "journey key", token: key["secret"].(string)}
	request := map[string]any{"model": slug, "messages": []any{map[string]any{"role": "user", "content": "hello"}}}

	s.eventually("the published route serving a request", 20*time.Second, func() (bool, string) {
		status, raw := s.do(consumer, "POST", "/v1/chat/completions", request, nil)
		if status != http.StatusOK {
			return false, describe(status, raw)
		}
		var completion struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(raw, &completion); err != nil {
			t.Fatalf("invalid completion: %v: %s", err, raw)
		}
		if len(completion.Choices) != 1 || !strings.Contains(completion.Choices[0].Message.Content, vendor.Answer) {
			t.Fatalf("unexpected completion: %s", raw)
		}
		return true, ""
	})

	s.eventually("usage reaching durable storage", 20*time.Second, func() (bool, string) {
		var attempts, input, output int64
		err := s.pool.QueryRow(t.Context(), `SELECT count(*),coalesce(sum(input_tokens),0),coalesce(sum(output_tokens),0) FROM olp.attempt_usage_facts`).Scan(&attempts, &input, &output)
		if err != nil {
			t.Fatal(err)
		}
		return attempts == 1 && input == vendor.InputTokens && output == vendor.OutputTokens,
			fmt.Sprintf("attempts=%d input=%d output=%d", attempts, input, output)
	})

	t.Run("simulation_requires_all_predicates", func(t *testing.T) {
		classifier := &routing.ClassifierPredicate{Route: slug, Labels: []string{"complex"}, TimeoutMS: 500}
		plugin := &routing.PluginPredicate{Digest: strings.Repeat("a", 64)}
		draft := s.want(owner, "POST", "/api/v1/route-drafts", map[string]any{
			"slug": uniqueName("simulation"), "operations": []string{"generation"}, "overall_timeout_ms": 5000, "max_attempts": 1,
			"fidelity": map[string]any{"mode": "transformed"}, "project_id": project,
			"targets": []any{map[string]any{"provider_id": provider["id"], "provider_model": vendor.Model, "priority": 0, "weight": 1, "timeout_ms": 2000, "tags": []string{"selected"}}},
			"selectors": []routing.Selector{
				{ID: "both", Tags: []string{"selected"}, When: routing.Predicate{Classifier: classifier, Plugin: plugin}},
				{ID: "plugin", Tags: []string{"selected"}, When: routing.Predicate{Plugin: plugin}},
				{ID: "classifier", Tags: []string{"selected"}, When: routing.Predicate{Classifier: classifier}},
			},
		}, idempotent(), http.StatusCreated)
		result := s.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/simulate", map[string]any{
			"operation": "generation", "surface": "openai", "mode": "unary", "seed": "predicates",
			"classifier_labels": map[string]string{"both": "complex", "classifier": "complex"},
		}, nil, http.StatusOK)
		selectors, ok := result["selectors"].([]any)
		if !ok || len(selectors) != 3 {
			t.Fatalf("simulation selectors = %v, want both plugin predicates skipped before classifier", result["selectors"])
		}
		for i, id := range []string{"both", "plugin", "classifier"} {
			selector := selectors[i].(map[string]any)
			want := "plugin_not_simulated"
			if id == "classifier" {
				want = "matched"
			}
			if selector["id"] != id || selector["matched"] != (id == "classifier") || selector["outcome"] != want {
				t.Fatalf("selector %s = %v, want %s", id, selector, want)
			}
		}
	})
}

func TestInstallationNotificationDeleteRequiresSettings(t *testing.T) {
	t.Parallel()
	s := startService(t)
	owner := s.owner()
	ownerID := s.want(owner, "GET", "/api/v1/profile", nil, nil, http.StatusOK)["id"].(string)
	destination := uuid.NewString()
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO olp.notification_destinations(id,name,url,etag,created_by)
 VALUES($1,'installation',$2,$1,$3)`, destination, "https://hooks.example.com/olp", ownerID); err != nil {
		t.Fatal(err)
	}
	token := s.want(owner, "POST", "/api/v1/management-tokens", map[string]any{
		"name": "keys only", "scopes": []string{"keys"}, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}, idempotent(), http.StatusCreated)
	path := "/api/v1/notifications/destinations/" + destination
	match := map[string]string{"If-Match": `"` + destination + `"`}
	for _, c := range []*caller{s.invite(owner, "developer@example.com", access.RoleDeveloper), {name: "keys token", token: token["secret"].(string)}} {
		if status, raw := s.do(c, "DELETE", path, nil, idempotent(match)); status != http.StatusForbidden {
			t.Errorf("%s deleted an installation destination: %s", c.name, describe(status, raw))
		}
	}
	s.want(owner, "DELETE", path, nil, idempotent(match), http.StatusNoContent)
}
