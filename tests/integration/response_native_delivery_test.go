//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/usage"
)

func TestResponseNativeTerminalSurvivesOriginalWriterFailure(t *testing.T) {
	for _, fidelity := range []string{"strict", "transformed"} {
		for _, phase := range []string{"completed", "zero", "missing", "failed"} {
			t.Run(fidelity+"/"+phase, func(t *testing.T) {
				f := newResponseRefusalFixture(t, fidelity)
				input, output, cache := int64(17), int64(9), int64(3)
				kind := "completed"
				if phase == "zero" {
					input, output, cache = 0, 0, 0
				} else if phase == "failed" {
					kind = "failed"
				}
				wire := responseRefusalWire(t, kind, input, output, cache, false)
				if phase == "missing" {
					wire = strings.Replace(wire, `,"usage":{"input_tokens":17,"output_tokens":9,"total_tokens":26,"input_tokens_details":{"cached_tokens":3}}`, "", 1)
					if strings.Contains(wire, `"usage"`) {
						t.Fatal("missing-usage fixture did not remove usage")
					}
				}
				f.reply.Store(wire)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				writer := &deferredDeliveryGate{header: http.Header{}, entered: make(chan struct{}), release: make(chan struct{}), fail: true}
				request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/responses", strings.NewReader(
					`{"model":"`+f.slug+`","input":"delivery failure","store":true,"background":true,"stream":true,"max_output_tokens":256}`))
				request.Header.Set("Authorization", "Bearer "+f.key)
				request.Header.Set("Content-Type", "application/json")
				done := make(chan struct{})
				t.Cleanup(func() {
					cancel()
					select {
					case <-writer.release:
					default:
						close(writer.release)
					}
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("original delivery did not finish after cleanup")
					}
				})
				go func() {
					defer close(done)
					f.h.HTTP.Config.Handler.ServeHTTP(writer, request)
				}()
				select {
				case <-writer.entered:
				case <-done:
					t.Fatal("original request ended before native payload delivery")
				case <-time.After(15 * time.Second):
					t.Fatal("native terminal result did not reach delivery")
				}
				cancel()
				close(writer.release)
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("failed delivery did not finish")
				}
				event := f.sink.last()
				if f.posts.Load() != 1 || f.sink.count() != 1 || len(event.Attempts) != 1 || event.ErrorClass != "client_cancelled" {
					t.Fatalf("delivery failure repeated or changed original client outcome: %+v posts=%d", event, f.posts.Load())
				}
				fact := event.Attempts[0]
				if fidelity == "strict" && fact.Interaction == nil {
					t.Fatal("strict response omitted interaction evidence")
				}
				if fact.Interaction != nil && (fact.Interaction.UpstreamState != usage.UpstreamTerminal || fact.Interaction.ClientState == usage.ClientTerminal) {
					t.Fatalf("known native outcome lost to original delivery cancellation: %+v", fact.Interaction)
				}
				health := f.h.Gateway.Health().ProviderHealth(time.Hour)[f.provider]
				if health.Attempts != 1 || health.RateLimits != 0 || health.TransportErrors != 0 {
					t.Fatalf("one native attempt acquired other provider outcomes: %+v", health)
				}
				if phase == "failed" {
					if fact.Class != "upstream_server" || health.ServerErrors != 1 || health.Successes != 0 {
						t.Fatalf("real native failure lost to delivery: class=%s health=%+v", fact.Class, health)
					}
				} else if fact.Class != "success" || health.Successes != 1 || health.ServerErrors != 0 {
					t.Fatalf("valid native completion lost to delivery: class=%s health=%+v", fact.Class, health)
				}
				if phase == "missing" {
					if fact.Usage != nil || fact.UsageObserved || fact.UsageComplete || !fact.BillingUncertain {
						t.Fatalf("missing native consumption became known: %+v", fact)
					}
				} else if fact.Usage == nil || fact.Usage.InputTokens != input || fact.Usage.OutputTokens != output || fact.Usage.TotalTokens != input+output ||
					fact.Usage.CachedInputTokens == nil || *fact.Usage.CachedInputTokens != cache || !fact.UsageObserved || !fact.UsageComplete || fact.BillingUncertain {
					t.Fatalf("native consumption changed after delivery: %+v", fact)
				}
			})
		}
	}
}
