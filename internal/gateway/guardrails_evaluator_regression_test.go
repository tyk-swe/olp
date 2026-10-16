package gateway

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/routing"
)

func modelGuardrailHarness(t *testing.T, operation string) (*harness, guardrails.Revision) {
	t.Helper()
	h := newHarness(t, Config{})
	h.republish(func(s *routing.Snapshot, _, b routing.Provider) {
		original := s.Routes[routeSlug]
		triage(s, b, routing.Predicate{})
		s.Routes[routeSlug] = original
		route := s.Routes[classifierSlug]
		if operation != "generation" {
			b.Capabilities = []routing.Capability{{Model: modelB, Operation: operation, Surface: "native", Mode: "unary"}}
			b.ProfileID, b.ProfileRevision = "tei-classification", "1"
			if operation == "moderation" {
				b.Kind = "openai"
				b.ProfileID = "openai-moderation"
			}
			s.Providers[b.ID] = b
			route.Operations = []string{operation}
			route.Fidelity = routing.RouteFidelity{Mode: routing.FidelityStrict}
		}
		s.Routes[classifierSlug] = route
	})
	installTestGuardrail(t, h, guardrails.Input, guardrails.Block, "model", `{"route":"team-classifier","rule":{"pointer":"/unsafe","equals":true,"category":"model.unsafe"}}`)
	h.mock.set("b", completion(modelB, `{"unsafe":true}`))
	return h, h.rt.release.Snapshot.Guardrails.Definitions["revision"]
}

func TestModelGuardrailKeylessAdmissionReservesSystemBudgets(t *testing.T) {
	for _, scope := range []string{"installation", "organization", "project"} {
		for _, exhausted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/exhausted=%t", scope, exhausted), func(t *testing.T) {
				h, definition := modelGuardrailHarness(t, "generation")
				authority := access.Authority{InstallationID: uuid.NewString(), OrganizationID: costText(uuid.NewString()), ProjectID: costText(uuid.NewString())}
				budget := &access.BudgetPolicy{DailyCostLimit: costText("1")}
				switch scope {
				case "installation":
					authority.InstallationBudget = budget
				case "organization":
					authority.OrganizationBudget = budget
				case "project":
					authority.ProjectBudget = budget
				}
				h.gateway.Runtime = budgetRuntime{fakeRuntime: h.rt, authority: authority}
				reservations := make(chan bool, 1)
				h.gateway.Admission = newAdmission(t, accountingStreamFunc(func(_ context.Context, args ...string) (any, error) {
					if len(args) == 13 {
						reservations <- true
						if exhausted {
							return []any{int64(1), int64(0), "daily_cost", int64(1000), int64(20_000), int64(24_000)}, nil
						}
						return costAllowed, nil
					}
					if len(args) == 8 {
						return costSettled, nil
					}
					return released, nil
				}))
				parent := &call{request: request{id: uuid.NewString(), minted: true, release: h.rt.release}, actor: "playground", authorize: func(*routing.Route) *Error { return nil }}
				ctx := context.WithValue(t.Context(), guardrailCallerKey{}, parent)
				_, err := h.gateway.modelGuardrail(ctx, definition, guardrails.Evaluation{Phase: guardrails.Input, Segments: []guardrails.Segment{{ID: "s", Text: "hello"}}})
				if len(reservations) != 1 {
					t.Fatal("evaluator did not reserve the system budget")
				}
				if exhausted {
					var failure *Error
					if !errors.As(err, &failure) || failure.Code != "budget_exhausted" || failure.Param == nil || *failure.Param != "budget."+scope || h.mock.count("b") != 0 {
						t.Fatalf("evaluator bypassed exhausted %s budget: %v calls=%d", scope, err, h.mock.count("b"))
					}
				} else if err != nil || h.mock.count("b") != 1 {
					t.Fatalf("available system budget refused: %v calls=%d", err, h.mock.count("b"))
				}
			})
		}
	}
}
