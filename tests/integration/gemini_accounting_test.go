//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func pendingGeminiGeneration(t *testing.T, h *accessHarness, slug, key string) (string, string, string) {
	t.Helper()
	response, raw := geminiPublic(t, h, http.MethodPost, "/gemini/v1beta/interactions", key,
		[]byte(fmt.Sprintf(`{"model":%q,"input":"durable accounting","background":true}`, slug)))
	var accepted struct{ ID, Status string }
	if response.StatusCode != http.StatusOK || json.Unmarshal(raw, &accepted) != nil || accepted.Status != "in_progress" {
		t.Fatalf("pending generation: %d %s", response.StatusCode, raw)
	}
	var original, owner string
	err := h.Pool.QueryRow(t.Context(), `SELECT metadata->'pending_usage'->>'request_id',api_key_id::text
 FROM olp.provider_resources WHERE id=$1`, strings.ReplaceAll(strings.TrimPrefix(accepted.ID, "interaction_"), "_", "")).Scan(&original, &owner)
	if err != nil || original == "" {
		t.Fatalf("accepted generation has no durable original request: %q %v", original, err)
	}
	return accepted.ID, original, owner
}

func assertGeminiOriginalUsage(t *testing.T, h *accessHarness, original string, complete bool, input, output int64) {
	t.Helper()
	var count int
	var observed, settled bool
	var in, out int64
	err := h.Pool.QueryRow(t.Context(), `SELECT count(*),bool_and(usage_observed),bool_and(usage_complete),
 COALESCE(sum(input_tokens),0),COALESCE(sum(output_tokens),0) FROM olp.attempt_usage_facts WHERE request_id=$1`, original).Scan(&count, &observed, &settled, &in, &out)
	if err != nil || count != 1 || settled != complete || observed != (complete || input > 0 || output > 0) || in != input || out != output {
		t.Fatalf("original usage count=%d observed=%v complete=%v tokens=%d/%d err=%v", count, observed, settled, in, out, err)
	}
}

// Replay the actual forward migration over already-owned templates in this
// uniquely created scratch database, rather than hand-building its projection.

// Removal of the sole template authority removes every projected subject.
