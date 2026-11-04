//go:build service

package service_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestPublishedRouteServesAndSettlesUsage takes one request from publication
// through the gateway to durable usage, across the Valkey stream and the
// consumer that writes PostgreSQL.
func TestPublishedRouteServesAndSettlesUsage(t *testing.T) {
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
		if len(completion.Choices) != 1 || !strings.Contains(completion.Choices[0].Message.Content, vendorAnswer) {
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
		return attempts == 1 && input == vendorInputTokens && output == vendorOutputTokens,
			fmt.Sprintf("attempts=%d input=%d output=%d", attempts, input, output)
	})
}
