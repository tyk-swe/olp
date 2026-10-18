package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/routing"
)

func TestDecisionRefusesUnboundedTokenBudgetsBeforeDispatch(t *testing.T) {
	for _, scope := range []string{"key", "slot", "provider"} {
		t.Run(scope, func(t *testing.T) {
			limit := int64(10000)
			h := strictHarness(t, func(s *routing.Snapshot) {
				route := s.Routes[routeSlug]
				route.Operations = []string{"decision"}
				s.Routes[routeSlug] = route
				for id, p := range s.Providers {
					p.Kind, p.VendorID, p.ProfileID = "openai", "openai", "openai-decisions"
					p.Capabilities = []routing.Capability{{Model: p.Capabilities[0].Model, Operation: "decision", Surface: "openai", Mode: "unary"}}
					if scope == "slot" {
						p.Slots[0].TokensPerMinute = &limit
					}
					if scope == "provider" {
						p.Limits = &routing.Limits{TokensPerMinute: &limit}
					}
					s.Providers[id] = p
				}
			})
			if scope == "key" {
				key := h.rt.keys[fullKey]
				key.Policy.TokensPerMinute = &limit
				h.rt.keys[fullKey] = key
			}
			resp := h.do(t.Context(), http.MethodPost, "/v1/decisions", fullKey, []byte(`{"model":"`+routeSlug+`","input":"hello","questions":[{"type":"predicate","instructions":"Is this text?"}]}`), nil)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode < 400 || !strings.Contains(string(body), "unbounded_work_budget") || h.mock.count("a")+h.mock.count("b") != 0 {
				t.Fatalf("unbounded decision: %d %s", resp.StatusCode, body)
			}
		})
	}
}
