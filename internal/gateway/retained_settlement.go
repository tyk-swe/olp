package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

func (s *Server) prepareRetainedAccounting(x *call, p *pin, route *routing.Route, r *resources.Resource, e retainedapi.Endpoint, source oif.Document) *Error {
	if !e.Metered {
		return nil
	}
	models := []string{p.model}
	var metadata struct {
		AuthorizedModels []string `json:"authorized_models"`
	}
	_ = json.Unmarshal(r.Metadata, &metadata)
	if len(metadata.AuthorizedModels) > 0 {
		models = metadata.AuthorizedModels
	}
	templates := map[string]*usage.Event{}
	for _, model := range models {
		cp := *p
		cp.model = model
		cp.attempt = p.attempt
		cp.attempt.UpstreamModel = model
		operation := e.Operation
		if operation == "eval" {
			operation = "generation"
		}
		price := x.routingInputs.Price(p.provider.Kind, p.provider.ID, p.provider.VendorID, model, operation, s.now())
		if price == nil || !retainedPriceCovers(price, e.Operation, r.Metadata) {
			return serverError(503, "pricing_unavailable", "Every retained billing component and model needs a published price before acceptance.")
		}
		if e.Operation == "eval" && !retainedEvalPricingSupported(price, x.attemptCardRevisionForRoute(p.provider.ID, route, x.request.startedAt), p.provider.Kind, p.provider.VendorID, model) {
			return policyUnavailable("unsupported_metered_operation", "Evaluation totals cannot establish per-invocation input tiers; publish prices and rate cards without those tiers.")
		}
		template, err := s.batchSettlementTemplate(x, &cp, e.Surface, operation, s.now())
		if err != nil {
			return serverError(503, "pricing_unavailable", "Retained accounting could not be pinned.")
		}
		template.Origin = usage.OriginRetainedSettlement
		template.Attempts[0].Routing.Budgets = append(template.Attempts[0].Routing.Budgets, p.provider.ID, p.slot.ID, route.ID)
		templates[model] = template
	}
	r.RetainedAccounting = &resources.RetainedAccounting{Operation: e.Operation, Templates: templates, TokenConsuming: e.Operation == "fine_tuning" || e.Operation == "eval"}
	return nil
}

func retainedEvalPricingSupported(price *usage.RoutingPrice, card *usage.RoutingCardRevision, kind, vendor, model string) bool {
	if price != nil && len(price.Tiers) > 0 {
		return false
	}
	explicit := card.Explicit(kind, vendor, model, "generation")
	return explicit == nil || len(explicit.Tiers) == 0
}

func (s *Server) observeRetainedSettlement(ctx context.Context, r *resources.Resource, e retainedapi.Endpoint, parsed oif.Document) error {
	observation, err := resources.ObserveRetained(r, e, parsed, s.now())
	if err != nil {
		return err
	}
	return s.Resources.RecordRetainedObservation(ctx, r, observation, nil)
}

func (s *Server) fenceRetainedMutation(ctx context.Context, r *http.Request, x *call, e retainedapi.Endpoint, resource *resources.Resource, body []byte) (func(), *Error) {
	noop := func() {}
	if !e.Metered || (e.Action != "update" && e.Action != "patch") {
		return noop, nil
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 128 {
		return noop, invalidRequest("invalid_idempotency_key", "Idempotency-Key of at most 128 bytes is required for a paid mutation.", nil)
	}
	payload := append([]byte(e.Method+":"+e.Pattern+":"+resource.ID+":"+r.URL.RawQuery+":"), body...)
	digest := sha256.Sum256(payload)
	err := s.Resources.PrepareRetainedMutation(ctx, resource, e.Operation+":"+e.Action, key, digest[:])
	if err != nil {
		if errors.Is(err, resources.ErrSubmissionConflict) {
			return noop, serverError(409, "idempotency_conflict", "This mutation identity already exists or conflicts with its original request.")
		}
		return noop, serverError(503, "provider_state_unavailable", "The paid mutation could not be fenced.")
	}
	return func() {
		commit, cancel := resourceCommitContext(ctx)
		defer cancel()
		_ = s.Resources.UnknownRetainedMutation(commit, resource.UUID, key, e.Operation+":"+e.Action)
	}, nil
}

func (s *Server) retainedUnboundedLimits(x *call, p *pin) *Error {
	if x.route == nil {
		return serverError(503, "provider_state_unavailable", "A retained operation requires a pinned route.")
	}
	if x.operationName() == "fine_tuning" || x.operationName() == "eval" {
		return s.unboundedPinLimits(x.authority, p, x.route)
	}
	monetary := func(provider *routing.Provider, slot *routing.Slot, route *routing.Route) bool {
		return route != nil && route.Budget != nil && (route.Budget.DailyCostLimit != nil || route.Budget.MonthlyCostLimit != nil) || provider != nil && provider.Limits != nil && provider.Limits.Costs() != nil || slot != nil && slot.Costs() != nil
	}
	capped := x.authority.CostBudgetedForRoute(x.route.Slug) || monetary(&p.provider, &p.slot, x.route)
	if s.Runtime != nil {
		if release := s.Runtime.Release(); release != nil {
			var route *routing.Route
			if current, ok := release.Snapshot.Routes[x.route.Slug]; ok {
				route = &current
			}
			if provider, ok := release.Snapshot.Providers[p.provider.ID]; ok {
				var slot *routing.Slot
				for _, candidate := range provider.Slots {
					if candidate.ID == p.slot.ID {
						v := candidate
						slot = &v
					}
				}
				capped = capped || monetary(&provider, slot, route)
			} else {
				capped = capped || monetary(nil, nil, route)
			}
		}
	}
	if capped {
		return invalidRequest("unbounded_work_budget", "This retained operation cannot bound future monetary usage under an applicable budget.", nil)
	}
	return nil
}

func retainedPriceCovers(price *usage.RoutingPrice, operation string, metadata []byte) bool {
	switch operation {
	case "fine_tuning":
		return price.TrainingPerMillion != nil
	case "vector_store":
		return price.StoragePerGiBDay != nil
	case "context_cache":
		return price.CacheStoragePerMillionTokenHour != nil
	case "eval":
		return price.InputPerMillion != nil && price.OutputPerMillion != nil
	case "container":
		var meta struct {
			Tier string `json:"memory_tier"`
		}
		_ = json.Unmarshal(metadata, &meta)
		if meta.Tier == "" {
			return false
		}
		_, ok := price.ContainerPerSession[meta.Tier]
		return ok
	}
	return false
}
