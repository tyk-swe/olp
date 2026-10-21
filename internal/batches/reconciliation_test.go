package batches

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
)

func TestReconciliationExcludesMappedJobs(t *testing.T) {
	policy := &egress.Policy{}
	auth := connectors.NewAuth(policy)
	for _, adapter := range []string{"openai", "mistral", "together", "gemini"} {
		t.Run(adapter, func(t *testing.T) {
			items := make([]json.RawMessage, 0, 2)
			for _, id := range []string{"known", "lost"} {
				item := map[string]any{"id": id, "status": "validating", "endpoint": "/v1/chat/completions", "input_file_id": "input", "created_at": 1792497600}
				if adapter == "mistral" {
					item["status"] = "RUNNING"
				}
				if adapter == "together" {
					item["status"] = "IN_PROGRESS"
				}
				if adapter == "gemini" {
					item = map[string]any{"name": "batches/" + id, "metadata": map[string]any{"state": "BATCH_STATE_RUNNING", "createTime": "2026-10-20T12:00:00Z", "inputConfig": map[string]any{"fileName": "input"}}}
				}
				raw, _ := json.Marshal(item)
				items = append(items, raw)
			}
			var body []byte
			if adapter == "together" {
				body, _ = json.Marshal(items)
			} else if adapter == "gemini" {
				body, _ = json.Marshal(map[string]any{"operations": items})
			} else {
				body, _ = json.Marshal(map[string]any{"data": items})
			}
			target := &PinnedTarget{
				Config: connectors.Config{Kind: "openai", Endpoint: "https://api.example.test/v1", AuthMode: "api_key"}, Secret: []byte("key"),
				Client: &http.Client{Transport: cloudRoundTripper(func(req *http.Request) (*http.Response, error) {
					if adapter == "gemini" && strings.HasSuffix(req.URL.Path, "/lost") {
						return cloudResponse(req, http.StatusOK, "application/json", string(items[1])), nil
					}
					return cloudResponse(req, http.StatusOK, "application/json", string(body)), nil
				})},
				KnownBatch: func(_ context.Context, id string) (bool, error) { return id == "known", nil },
			}
			var reader Reader = &OpenAIReader{Auth: auth, Egress: policy}
			switch adapter {
			case "mistral":
				reader = NewMistralReader(auth, policy)
			case "together":
				reader = NewTogetherReader(auth, policy)
			case "gemini":
				reader = &GeminiReader{Auth: auth, Egress: policy}
			}
			settlement := Settlement{CreatedAt: time.Unix(1792497600, 0), Cursor: json.RawMessage(`{"submission":{"gemini_file":"input"}}`)}
			result, err := reader.Reconcile(t.Context(), settlement, target, "input", "/v1/chat/completions")
			if err != nil || len(result.Matches) != 1 || !strings.HasSuffix(result.Matches[0].ID, "lost") {
				t.Fatalf("reused input reconciliation=%+v err=%v", result, err)
			}
			lookupErr := errors.New("mapping lookup unavailable")
			target.KnownBatch = func(context.Context, string) (bool, error) { return false, lookupErr }
			if _, err := reader.Reconcile(t.Context(), settlement, target, "input", "/v1/chat/completions"); !errors.Is(err, lookupErr) {
				t.Fatalf("mapping lookup failure permitted adoption: %v", err)
			}
		})
	}
}

func TestSubmissionCreatorDeadline(t *testing.T) {
	created := time.Now().UTC().Add(-3 * time.Minute)
	deadline := created.Add(10 * time.Minute)
	cursor, _ := json.Marshal(map[string]any{"submission": map[string]any{"creator_deadline": deadline}})
	settlement := Settlement{CreatedAt: created, CloseAfter: created.Add(72 * time.Hour), Cursor: cursor}
	if got := submissionCreatorDeadline(settlement); !got.Equal(deadline) || !time.Now().Before(got) {
		t.Fatalf("long-running creator lost its recovery fence: %v", got)
	}
	settlement.Cursor = json.RawMessage(`{"submission":{}}`)
	if got := submissionCreatorDeadline(settlement); !got.Equal(settlement.CloseAfter) {
		t.Fatalf("legacy creator without deadline was guessed inactive: %v", got)
	}
}
