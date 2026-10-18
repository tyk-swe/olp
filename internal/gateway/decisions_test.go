package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/guardrails"
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

func TestDecisionDelegationKeepsNamedRouteBudget(t *testing.T) {
	h := strictHarness(t, func(s *routing.Snapshot) {
		route := s.Routes[routeSlug]
		route.Operations = []string{"decision"}
		route.Fidelity.Mode = routing.FidelityTransformed
		s.Routes[routeSlug] = route
		var backup routing.Provider
		for id, p := range s.Providers {
			p.Kind, p.VendorID, p.ProfileID = "openai", "openai", "openai-decisions"
			p.Capabilities = []routing.Capability{{Model: p.Capabilities[0].Model, Operation: "decision", Surface: "openai", Mode: "unary"}}
			s.Providers[id] = p
			if p.Name == "b" {
				backup = p
			}
		}
		splitRoutes(s, backup, routing.Behavior{Selectors: []routing.Selector{{ID: "delegate", Route: backupSlug}}})
	})
	key := h.rt.keys[fullKey]
	key.Policy.RouteLimits = access.RouteLimits{routeSlug: {TokensPerMinute: new(int64(1000))}}
	h.rt.keys[fullKey] = key
	resp := h.do(t.Context(), http.MethodPost, "/v1/decisions", fullKey, []byte(`{"model":"`+routeSlug+`","input":"hello","questions":[{"type":"predicate","instructions":"Is this text?"}]}`), nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 400 || !strings.Contains(string(body), "unbounded_work_budget") || h.mock.count("a")+h.mock.count("b") != 0 {
		t.Fatalf("named-route cap bypassed: %d %s", resp.StatusCode, body)
	}
}

func TestDecisionIgnoresCappedSlotsOutsideRequestScope(t *testing.T) {
	for _, scope := range []string{"model", "route", "key"} {
		t.Run(scope, func(t *testing.T) {
			h := strictHarness(t, func(s *routing.Snapshot) {
				route := s.Routes[routeSlug]
				route.Operations = []string{"decision"}
				s.Routes[routeSlug] = route
				for id, p := range s.Providers {
					p.Kind, p.VendorID, p.ProfileID = "openai", "openai", "openai-decisions"
					p.Capabilities = []routing.Capability{{Model: p.Capabilities[0].Model, Operation: "decision", Surface: "openai", Mode: "unary"}}
					capped := p.Slots[0]
					capped.ID = uuid.NewString()
					capped.TokensPerMinute = new(int64(10000))
					switch scope {
					case "model":
						capped.AllowedModels = []string{"other-model"}
					case "route":
						capped.AllowedRoutes = []string{"other-route"}
					case "key":
						capped.AllowedAPIKeys = []string{uuid.NewString()}
					}
					p.Slots = append(p.Slots, capped)
					s.Providers[id] = p
				}
			})
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"model":"`+modelA+`","answers":[{"type":"predicate","probability":0.75}],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`)
			})
			resp := h.do(t.Context(), http.MethodPost, "/v1/decisions", fullKey, []byte(`{"model":"`+routeSlug+`","input":"hello","questions":[{"type":"predicate","instructions":"Is this text?"}]}`), nil)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || h.mock.count("a") != 1 {
				t.Fatalf("unrelated capped slot: %d %s", resp.StatusCode, body)
			}
		})
	}
}

func TestDecisionRefusesMutatingGuardrailsBeforeDispatch(t *testing.T) {
	for _, tc := range []struct{ phase, action string }{
		{guardrails.Input, guardrails.Redact},
		{guardrails.Input, guardrails.Mask},
		{guardrails.Output, guardrails.Redact},
		{guardrails.OutputStream, guardrails.Redact},
	} {
		t.Run(tc.phase+"/"+tc.action, func(t *testing.T) {
			h := strictHarness(t, func(s *routing.Snapshot) {
				route := s.Routes[routeSlug]
				route.Fidelity.Mode = routing.FidelityTransformed
				route.Operations = []string{"decision"}
				s.Routes[routeSlug] = route
				for id, p := range s.Providers {
					p.Kind, p.VendorID, p.ProfileID = "openai", "openai", "openai-decisions"
					p.Capabilities = []routing.Capability{{Model: p.Capabilities[0].Model, Operation: "decision", Surface: "openai", Mode: "unary"}}
					s.Providers[id] = p
				}
			})
			installTestGuardrail(t, h, tc.phase, tc.action, "builtin.regex", `{"rules":[{"id":"secret","pattern":"secret"}]}`)
			resp := h.do(t.Context(), http.MethodPost, "/v1/decisions", fullKey, []byte(`{"model":"`+routeSlug+`","input":"secret","questions":[{"type":"predicate","instructions":"Is this text?"}]}`), nil)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), "guardrail_surface_unavailable") || h.mock.count("a")+h.mock.count("b") != 0 {
				t.Fatalf("mutating decision guardrail: %d %s", resp.StatusCode, body)
			}
		})
	}
}
