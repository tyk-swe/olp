//go:build integration

package gateway

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestIntegrationBedrockRequestsEnforceAndSettleSupplyCaps(t *testing.T) {
	for _, operation := range []string{"converse", "converse-stream", "invoke", "invoke-with-response-stream"} {
		for _, outcome := range []string{"admitted", "cap refused", "local failure", "upstream failure"} {
			t.Run(operation+"/"+outcome, func(t *testing.T) {
				limiter := mediaLimiter(t)
				h := newHarness(t, Config{})
				identifyHarness(h, "header")
				h.gateway.Admission = NewAdmission(limiter, nil, h.gateway.log)
				snapshot := h.rt.release.Snapshot
				route := snapshot.Routes[routeSlug]
				route.Targets = route.Targets[:1]
				provider := snapshot.Providers[route.Targets[0].ProviderID]
				family, name, mode := wire.Bedrock, "generation", "unary"
				profile, model := "bedrock-converse", "anthropic.claude-3-5-sonnet-v2:0"
				if strings.HasPrefix(operation, "invoke") {
					family, name, profile = wire.BedrockInvoke, "bedrock_invoke", "bedrock-invoke"
				}
				if strings.Contains(operation, "stream") {
					mode = "streaming"
				}
				provider.Kind, provider.ProfileID, provider.ProfileRevision = "bedrock", profile, connectors.ProfileRevision
				provider.AuthMode, provider.CloudRegion, provider.Endpoint = "static", "us-east-1", h.upstream.URL+"/a"
				provider.Capabilities = []routing.Capability{{Model: model, Operation: name, Surface: "bedrock", Mode: mode}}
				route.Operations, route.Targets[0].ProviderModel = []string{name}, model
				route.Budget = &routing.CostLimits{DailyCostLimit: costText("1")}
				provider.Limits = &routing.Limits{Supply: routing.Supply{DailyCostLimit: costText("1")}}
				provider.Slots[0].DailyCostLimit = costText("1")
				if outcome == "cap refused" {
					// The route's hold must be refunded if the next cap refuses.
					provider.Limits.DailyCostLimit = costText("0.000001")
				}
				if outcome == "local failure" {
					provider.Endpoint = "http://192.0.2.1"
				}
				snapshot.Providers[provider.ID], snapshot.Routes[route.Slug] = provider, route
				var err error
				h.rt.release, err = runtime.NewRelease(uuid.NewString(), 8, snapshot, map[string][]byte{
					h.credA: []byte(`{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789"}`),
				})
				if err != nil {
					t.Fatal(err)
				}
				owners := []string{route.ID, provider.ID, provider.Slots[0].ID}
				for _, owner := range owners {
					installBudgetBalance(t, limiter, owner)
				}
				h.rt.inputs = &usage.RoutingInputs{RefreshedAt: time.Now(), Prices: []usage.RoutingPrice{{Price: usage.Price{
					ProviderKind: "bedrock", Model: model, Operation: name, InputPerMillion: costText("1"), OutputPerMillion: costText("10"),
				}}}}
				h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get(endUserHeader) != "" {
						t.Error("Bedrock forwarded end-user header")
					}
					if outcome == "upstream failure" {
						status(http.StatusServiceUnavailable, `{"message":"unavailable"}`)(w, r)
						return
					}
					if mode == "streaming" {
						w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
						encoder := eventstream.NewEncoder()
						message := bedrockMetadata(`{"usage":{"inputTokens":3,"outputTokens":2,"totalTokens":5}}`)
						if name == "bedrock_invoke" {
							message = bedrockChunk(t, `{"type":"message_stop","amazon-bedrock-invocationMetrics":{"inputTokenCount":3,"outputTokenCount":2}}`)
						} else if err := encoder.Encode(w, *bedrockEvent(t, "messageStop", map[string]string{"stopReason": "end_turn"})); err != nil {
							t.Error(err)
							return
						}
						if err := encoder.Encode(w, *message); err != nil {
							t.Error(err)
						}
						return
					}
					body := `{"output":{"message":{"role":"assistant","content":[{"text":"hello"}]}},"stopReason":"end_turn","usage":{"inputTokens":3,"outputTokens":2,"totalTokens":5}}`
					if name == "bedrock_invoke" {
						body = `{"id":"msg_1","type":"message","role":"assistant","model":"` + model + `","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
					}
					status(http.StatusOK, body)(w, r)
				})
				request := httptest.NewRequest(http.MethodPost, "/bedrock/model/"+routeSlug+"/"+operation, strings.NewReader(`{"messages":[{"role":"user","content":[{"text":"hello"}]}]}`))
				request.SetPathValue("model", routeSlug)
				request.Header.Set("X-OLP-API-Key", fullKey)
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set(endUserHeader, "bedrock-user")
				response := httptest.NewRecorder()
				h.gateway.bedrockServe(response, request, family, name, mode, operation)
				calls, wantStatus, wantCost := 0, http.StatusBadGateway, "0"
				switch outcome {
				case "admitted":
					calls, wantStatus, wantCost = 1, http.StatusOK, "0.000023"
					assertEndUserEnvelope(t, h.sink.last(t), (endUserRuntime{h.rt}).EndUserDigest(nil, "bedrock-user"), "bedrock-user")
					event := accountingEvent(h.sink.last(t))
					if event == nil || len(event.Attempts) != 1 || event.Attempts[0].Routing == nil || !slices.Equal(event.Attempts[0].Routing.Budgets, owners) {
						t.Fatalf("Bedrock accounting lost supply budget owners: %+v", event)
					}
				case "cap refused":
					wantStatus = http.StatusServiceUnavailable
				case "upstream failure":
					calls, wantStatus = 1, http.StatusServiceUnavailable
				}
				if response.Code != wantStatus || h.mock.count("a") != calls {
					t.Fatalf("Bedrock request: %d %s calls=%d; want status=%d calls=%d", response.Code, response.Body, h.mock.count("a"), wantStatus, calls)
				}
				for _, owner := range owners {
					if reserved, err := limiter.Reserved(t.Context(), owner); err != nil || reserved != wantCost {
						t.Fatalf("settled Bedrock reservation for %s = %s, %v; want %s", owner, reserved, err, wantCost)
					}
				}
			})
		}
	}
}
