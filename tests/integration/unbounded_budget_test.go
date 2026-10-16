//go:build integration

package integration_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestStrictBatchBudgetRejectsBeforeDispatch(t *testing.T) {
	for _, scope := range []string{"key tokens", "key weekly", "group tokens", "group weekly", "provider tokens", "provider cost", "slot tokens", "slot cost"} {
		t.Run(scope, func(t *testing.T) {
			fixture := newOpenAIFixture(t, "")
			h := newAccessHarness(t)
			owner, detail, slug, _ := provisionOpenAIWith(t, h, fixture.URL,
				[]any{map[string]any{"operation": "batch", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "embeddings", "surface": "openai", "mode": "unary"}}, []string{"batch", "embeddings"},
				map[string]any{"fidelity": map[string]any{"mode": "strict"}}, map[string]any{"profile_id": "azure-legacy-chat", "profile_revision": "1"})
			key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "batch budget", "scopes": []string{"inference"}, "allowed_routes": []string{slug}, "allow_provider_state": true}, idem(uuid.NewString()), 201)
			secret := key["secret"].(string)
			h.refresh()
			input := fmt.Sprintf(`{"custom_id":"one","method":"POST","url":"/v1/embeddings","body":{"model":%q,"input":[1,2,3]}}`+"\n", vendorModel)
			status, file := h.uploadTestFile(slug, secret, input)
			if status != http.StatusOK {
				t.Fatalf("upload: %d %v", status, file)
			}
			fields := map[string]any{"tokens_per_minute": 100}
			if strings.HasSuffix(scope, "weekly") {
				fields = map[string]any{"weekly_cost_limit": "1"}
			} else if strings.HasSuffix(scope, "cost") {
				fields = map[string]any{"monthly_cost_limit": "1"}
			}
			if strings.HasPrefix(scope, "provider") || strings.HasPrefix(scope, "slot") {
				path := "/api/v1/providers/" + detail["id"].(string)
				detail = h.want(owner, "GET", path, nil, nil, 200)
				if strings.HasPrefix(scope, "provider") {
					configuration := detail["configuration"].(map[string]any)
					options, _ := configuration["options"].(map[string]any)
					if options == nil {
						options = map[string]any{}
						configuration["options"] = options
					}
					options["limits"] = fields
					h.want(owner, "PATCH", path, map[string]any{"name": "batch limited", "configuration": configuration}, etagHeader(detail), 200)
				} else {
					slots := h.want(owner, "GET", path+"/credential-slots", nil, nil, 200)
					slot := slots["items"].([]any)[0].(map[string]any)
					fields["name"] = "default"
					h.want(owner, "PUT", path+"/credential-slots/"+slot["id"].(string), map[string]any{"slot": fields}, withMatch(slots, idem(uuid.NewString())), 200)
				}
				detail = h.want(owner, "GET", path, nil, nil, 200)
				h.want(owner, "POST", path+"/activate", nil, withMatch(detail, idem(uuid.NewString())), 200)
			} else {
				if strings.HasPrefix(scope, "group") {
					fields["name"] = "batch shared"
					group := h.want(owner, "POST", "/api/v1/budget-groups", fields, idem(uuid.NewString()), 201)
					fields = map[string]any{"budget_group_id": group["id"]}
				}
				path := "/api/v1/api-keys/" + key["id"].(string)
				record := h.want(owner, "GET", path, nil, nil, 200)
				h.want(owner, "PATCH", path, fields, etagHeader(record), 200)
			}
			h.refresh()
			before := fixture.dials.Load()
			body := fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/embeddings","completion_window":"24h"}`, file["id"])
			status, raw, _ := h.gatewayRaw(http.MethodPost, "/v1/batches", secret, strings.NewReader(body), map[string]string{"Content-Type": "application/json"})
			if status != http.StatusBadRequest || !strings.Contains(string(raw), "unbounded_work_budget") || fixture.dials.Load() != before {
				t.Fatalf("batch admission: %d %s calls %d -> %d", status, raw, before, fixture.dials.Load())
			}
		})
	}
}
