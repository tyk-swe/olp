//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/gateway"
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

func TestGeminiLiveBudgetRejectsBeforeDispatch(t *testing.T) {
	h := newAccessHarness(t)
	provider := newGeminiLifecycleProvider(t)
	owner := h.owner()
	slug, _, _ := provisionGeminiLifecycle(t, h, owner, "gemini-live", provider)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Live budget", "scopes": []string{"inference"}, "allowed_routes": []string{slug}, "tokens_per_minute": 100}, idem(uuid.NewString()), 201)
	h.refresh()
	before := len(provider.captured())
	address := "ws" + strings.TrimPrefix(h.HTTP.URL, "http") + "/gemini/ws/" + connectors.GeminiLiveMethod + "?key=" + url.QueryEscape(key["secret"].(string))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, address, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()
	if err := client.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(`{"setup":{"model":"models/%s"}}`, slug))); err != nil {
		t.Fatal(err)
	}
	_, _, err = client.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation || len(provider.captured()) != before {
		t.Fatalf("Live budget dispatched: %v calls %d -> %d", err, before, len(provider.captured()))
	}
}

func TestGeminiLiveReauthorizationRefusesNewConsumptionBudgets(t *testing.T) {
	for _, scope := range []string{"key", "group", "key route"} {
		t.Run(scope, func(t *testing.T) {
			h := newAccessHarness(t)
			client := limClient(t)
			h.Gateway.Admission = gateway.NewAdmission(limLimiter(t, client, limNamespace(t, client, "live-budget-refresh")), nil, slog.New(slog.DiscardHandler))
			provider := newGeminiLifecycleProvider(t)
			owner := h.owner()
			slug, secret, _ := provisionGeminiLifecycle(t, h, owner, "gemini-live", provider,
				map[string]any{"requests_per_minute": 1000, "max_concurrency": 1})
			ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
			defer cancel()
			address := "ws" + strings.TrimPrefix(h.HTTP.URL, "http") + "/gemini/ws/" + connectors.GeminiLiveMethod + "?key=" + url.QueryEscape(secret)
			conn, _, err := websocket.Dial(ctx, address, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			if err := conn.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(`{"setup":{"model":"models/%s"}}`, slug))); err != nil {
				t.Fatal(err)
			}
			if _, frame, err := conn.Read(ctx); err != nil || !strings.Contains(string(frame), "setupComplete") {
				t.Fatalf("request/concurrency-only Live session refused: %s %v", frame, err)
			}
			authority, err := h.authority(secret)
			if err != nil {
				t.Fatal(err)
			}
			fields := map[string]any{"weekly_cost_limit": "1"}
			if scope == "group" {
				group := h.want(owner, "POST", "/api/v1/budget-groups", map[string]any{"name": "live", "weekly_cost_limit": "1"}, idem(uuid.NewString()), 201)
				fields = map[string]any{"budget_group_id": group["id"]}
			} else if scope == "key route" {
				fields = map[string]any{"route_limits": map[string]any{slug: map[string]any{"weekly_cost_limit": "1"}}}
			}
			path := "/api/v1/api-keys/" + authority.ID
			key := h.want(owner, "GET", path, nil, nil, 200)
			h.want(owner, "PATCH", path, fields, etagHeader(key), 200)
			h.refresh()
			before := len(provider.captured())
			_, _, err = conn.Read(ctx)
			if websocket.CloseStatus(err) != websocket.StatusPolicyViolation || len(provider.captured()) != before {
				t.Fatalf("Live session continued with new %s budget: %v", scope, err)
			}
		})
	}
}
