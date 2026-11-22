package routing

import (
	"cmp"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/usage/rates"
)

// evaluateCandidates preserves admission order: published eligibility and
// source constraints precede preparation, then effective request constraints.
func evaluateCandidates(s selectionSource, route Route, operation, surface, mode string, affinity []byte, policy EffectivePolicy, chosen selection, options Options) ([]rankedCandidate, error) {
	now := options.Now
	if now.IsZero() {
		now = time.Now()
	}
	routeID, e := uuid.Parse(route.RoutingID)
	if e != nil {
		return nil, &SelectionError{Code: NoEligibleTargets}
	}
	rows := make([]rankedCandidate, 0, len(route.Targets))
	for _, target := range route.Targets {
		rows = append(rows, evaluateCandidate(s, route, routeID, target, operation, surface, mode, affinity, policy, chosen, options, now))
	}
	return rows, nil
}

func evaluateCandidate(s selectionSource, route Route, routeID uuid.UUID, target Target, operation, surface, mode string, affinity []byte, policy EffectivePolicy, chosen selection, options Options, now time.Time) rankedCandidate {
	provider, exists := s.lookupProvider(target.ProviderID)
	row := rankedCandidate{provider: provider, decision: Decision{TargetID: target.ID, ProviderID: target.ProviderID, UpstreamModel: target.ProviderModel, Priority: target.Priority, Strategy: policy.Strategy, Shadow: target.Shadow != nil}, order: len(policy.Preferences.Order), shadow: target.Shadow != nil}
	if chosen.selector != nil && !row.shadow {
		row.decision.Selector = &chosen.selector.ID
	}
	row.decision.Unhealthy = options.Unhealthy != nil && options.Unhealthy(target.ProviderID)
	if provider.VendorID != "" {
		row.decision.VendorID = &provider.VendorID
	}
	metadata, _ := provider.Metadata(target.ProviderModel)
	connectionRegion := provider.CloudRegion
	if connectionRegion == "" && metadata.Region != nil {
		connectionRegion = *metadata.Region
	}
	if options.Region != "" && connectionRegion != options.Region {
		row.locality = 1
	}
	row.decision.MetadataObservedAt = metadata.ObservedAt
	row.decision.ContextLength = metadata.ContextLength
	row.decision.MaxOutputTokens = metadata.MaxOutputTokens
	demand := options.TokenDemand
	if options.Demand != nil {
		demand = options.Demand(provider, target)
	}
	if demand != nil {
		input := demand.EstimatedInputTokens
		row.decision.EstimatedInputTokens = &input
		row.decision.RequestedOutputTokens = demand.MaxOutputTokens
	}
	row.decision.Price = options.Inputs.Price(string(provider.Kind), provider.ID, provider.VendorID, target.ProviderModel, operation, now)
	row.attempt.Discount = options.Inputs.Discount(string(provider.Kind), provider.ID, provider.VendorID, now)
	row.decision.MaxCost = copyPointer(policy.MaxCost)
	if options.EstimateCost && options.WorstCost != nil {
		row.decision.EstimatedCost = estimatedCost(provider, row.decision.Price, row.attempt.Discount, demand, now, options.WorstCost, target)
	}
	row.decision.Performance = options.Inputs.Metrics(provider.ID, target.ProviderModel, operation, mode, now)
	reason := qualifyCandidate(&row, route, provider, exists, target, metadata, demand, operation, surface, mode, affinity, policy, chosen, options, now)
	targetID, e := uuid.Parse(target.RoutingID)
	if e != nil {
		reason = "target_unknown"
	}
	if reason != "" {
		row.decision.Reason = &reason
	} else {
		row.decision.Eligible = true
	}
	row.attempt = Attempt{TargetID: target.ID, ProviderID: target.ProviderID, ProviderRevisionID: provider.RevisionID, ProviderKind: string(provider.Kind), UpstreamModel: target.ProviderModel, Timeout: time.Duration(target.Timeout) * time.Millisecond, Priority: target.Priority, Score: Score(routeID, targetID, target.Weight, operation, surface, mode, affinity), Strategy: policy.Strategy, PolicyDigest: policy.Digest, Price: row.decision.Price, Discount: row.attempt.Discount, Performance: row.decision.Performance, VendorID: provider.VendorID}
	perf := row.decision.Performance
	if policy.Strategy == strategyLatency && policy.Preferences.PreferredMaxLatencyMS != nil && (perf == nil || perf.LatencyMS > float64(*policy.Preferences.PreferredMaxLatencyMS)) {
		row.preference = 1
	}
	if policy.Strategy == strategyThroughput && policy.Preferences.PreferredMinThroughput != nil && (perf == nil || perf.Throughput == nil || *perf.Throughput < float64(*policy.Preferences.PreferredMinThroughput)) {
		row.preference = 1
	}
	return row
}

func qualifyCandidate(row *rankedCandidate, route Route, provider Provider, exists bool, target Target, metadata ModelMetadata, demand *TokenDemand, operation, surface, mode string, affinity []byte, policy EffectivePolicy, chosen selection, options Options, now time.Time) string {
	reason := ""
	switch {
	case !exists:
		reason = "target_unknown"
	case !provider.Enabled:
		reason = "provider_not_active"
	case provider.Plugin != nil && provider.Plugin.Unconfined() && !options.UnconfinedPlugins:
		reason = "plugin_unconfined_disabled"
	case !(route.IsPassthrough() && operation == PassthroughOperation) && !provider.Supports(target.ProviderModel, operation, surface, mode):
		reason = "capability_not_certified"
	case row.shadow && (options.Mirror == nil || !Sampled(target.ID, options.Mirror, target.Shadow.SampleRate)):
		reason = "shadow_not_sampled"
	case !row.shadow && !chosen.admits(target):
		reason = "selector_excluded"
	}
	if reason == "selector_excluded" {
		row.allowed = capacityReason(metadata, demand) == "" && constraintReason(policy, provider, metadata, row.decision.Price, options.Parameters) == ""
	}
	if reason == "" {
		reason = capacityReason(metadata, demand)
	}
	if reason == "" {
		reason = constraintReason(policy, provider, metadata, row.decision.Price, options.Parameters)
	}
	for index, selector := range policy.Preferences.Order {
		if selectorMatches(selector, provider) {
			row.order = index
			break
		}
	}
	if reason == "" && policy.Preferences.AllowFallbacks != nil && !*policy.Preferences.AllowFallbacks && len(policy.Preferences.Order) > 0 && row.order == len(policy.Preferences.Order) {
		reason = "outside_preferred_order"
	}
	if reason == "" && options.CheckSlots {
		if provider.Network != nil && provider.Network.CredentialID != "" && options.CredentialEligibility != nil {
			if eligibility := options.CredentialEligibility(provider.Network.CredentialID); eligibility != Eligible {
				reason = "network_credential_" + string(eligibility)
			}
		}
	}
	if reason == "" && options.CheckSlots {
		row.slots, row.skipped = eligibleSlots(SelectSlots(provider, target.ProviderModel, route, options.KeyID, operation, surface, mode, affinity), options.CredentialEligibility)
		if len(row.slots) == 0 {
			reason = noSlotReason(row.skipped)
		}
	}
	if reason == "" && options.Prepare != nil {
		reason = applyPreparedCandidate(row, provider, target, metadata, policy, options, now)
	}
	if reason == "" && options.WorstCost != nil && (policy.MaxCost != nil || policy.Strategy == strategyBoundedCost && operation == "generation") {
		if bound, ok := options.WorstCost(provider, target, row.decision.Price, row.attempt.Discount); ok {
			row.bound = &bound
		}
	}
	if reason == "" && policy.MaxCost != nil && options.WorstCost != nil && provider.CommitmentAt(now) == nil {
		if row.bound == nil || !lessOrEqual(strptr(row.bound.Cost.String()), policy.MaxCost) {
			reason = "cost_limit_exceeded"
		}
	}
	return reason
}

func applyPreparedCandidate(row *rankedCandidate, provider Provider, target Target, metadata ModelMetadata, policy EffectivePolicy, options Options, now time.Time) string {
	prepared, err := options.Prepare(provider, target)
	if err != nil {
		reason := "unsupported_request_semantics"
		var detail incompatibilityError
		if errors.As(err, &detail) {
			code, field, requirement, message := detail.Incompatibility()
			row.decision.Incompatibility = &Incompatibility{Code: code, Field: field, Requirement: requirement, Message: message}
			reason = code
		}
		return reason
	}
	if prepared == nil {
		return ""
	}
	reason := capacityReason(metadata, prepared.Demand)
	if reason == "" {
		reason = constraintReason(policy, provider, metadata, row.decision.Price, prepared.Parameters)
	}
	if prepared.Demand != nil {
		input := prepared.Demand.EstimatedInputTokens
		row.decision.EstimatedInputTokens = &input
		row.decision.RequestedOutputTokens = prepared.Demand.MaxOutputTokens
		if options.EstimateCost && options.WorstCost != nil {
			row.decision.EstimatedCost = estimatedCost(provider, row.decision.Price, row.attempt.Discount, prepared.Demand, now, options.WorstCost, target)
		}
	}
	return reason
}

func orderCandidates(rows []rankedCandidate, strategy, operation string) {
	slices.SortStableFunc(rows, func(a, b rankedCandidate) int {
		if a.decision.Eligible != b.decision.Eligible {
			if a.decision.Eligible {
				return -1
			}
			return 1
		}
		if a.decision.Unhealthy != b.decision.Unhealthy {
			if b.decision.Unhealthy {
				return -1
			}
			return 1
		}
		if order := cmp.Compare(a.attempt.Priority, b.attempt.Priority); order != 0 {
			return order
		}
		if order := cmp.Compare(a.locality, b.locality); order != 0 {
			return order
		}
		if order := cmp.Compare(a.order, b.order); order != 0 {
			return order
		}
		if order := cmp.Compare(a.preference, b.preference); order != 0 {
			return order
		}
		switch strategy {
		case strategyCapacity:
			ah, bh := a.decision.Headroom, b.decision.Headroom
			if order := compareKnown(deref(ah), ah != nil, deref(bh), bh != nil); order != 0 {
				return order
			}
		case strategyBoundedCost:
			if order := compareBoundedCost(a, b); order != 0 {
				return order
			}
		case strategyPrice:
			ap, bp := a.decision.Price.Scalar(operation), b.decision.Price.Scalar(operation)
			if (ap == nil) != (bp == nil) {
				if ap != nil {
					return -1
				}
				return 1
			}
			if ap != nil && ap.Cmp(bp) != 0 {
				return ap.Cmp(bp)
			}
		case strategyLatency, strategyThroughput:
			ap, bp := a.decision.Performance, b.decision.Performance
			if strategy == strategyThroughput {
				if ap != nil && ap.Throughput == nil {
					ap = nil
				}
				if bp != nil && bp.Throughput == nil {
					bp = nil
				}
			}
			if (ap == nil) != (bp == nil) {
				if ap != nil {
					return -1
				}
				return 1
			}
			if ap != nil {
				if strategy == strategyLatency && ap.LatencyMS != bp.LatencyMS {
					return cmp.Compare(ap.LatencyMS, bp.LatencyMS)
				}
				if strategy == strategyThroughput && *ap.Throughput != *bp.Throughput {
					return cmp.Compare(*bp.Throughput, *ap.Throughput)
				}
			}
		}
		return cmp.Compare(b.attempt.Score, a.attempt.Score)
	})
}

// addCandidates retains every eligible target for execution. Preview ordinals
// account for credential slots; excluded credentials never spend the budget.
func (plan *Plan) addCandidates(rows []rankedCandidate, checkSlots bool) {
	ordinal := 0
	for _, row := range rows {
		if row.shadow {
			if row.decision.Eligible {
				plan.Shadows = append(plan.Shadows, row.attempt)
			}
			plan.Decisions = append(plan.Decisions, row.decision)
			continue
		}
		if !row.decision.Eligible {
			plan.Decisions = append(plan.Decisions, row.decision)
			continue
		}
		plan.Attempts = append(plan.Attempts, row.attempt)
		count := len(row.slots)
		if !checkSlots {
			count = 1
		}
		for i := 0; i < count; i++ {
			decision := row.decision
			if checkSlots {
				id := row.slots[i].ID
				decision.CredentialSlotID = &id
			}
			ordinal++
			if ordinal <= plan.Budget {
				n := ordinal
				decision.Attempt = &n
			} else {
				decision.Eligible = false
				reason := "attempt_budget_exhausted"
				decision.Reason = &reason
			}
			plan.Decisions = append(plan.Decisions, decision)
		}
		for _, slot := range row.skipped {
			decision := row.decision
			decision.Eligible = false
			decision.CredentialSlotID, decision.Reason = &slot.id, &slot.reason
			plan.Decisions = append(plan.Decisions, decision)
		}
	}
}

// skippedSlot is a credential slot planning skips because its credential
// version may not serve, with the plan reason that names why.
type skippedSlot struct {
	id, reason string
}

// eligibleSlots keeps a target's selected slots whose credential versions may
// serve and returns the rest as skipped, with their reasons. Skipped slots
// never spend the attempt budget.
func eligibleSlots(slots []Slot, eligibility func(string) Eligibility) ([]Slot, []skippedSlot) {
	if eligibility == nil {
		return slots, nil
	}
	var skipped []skippedSlot
	slots = slices.DeleteFunc(slots, func(slot Slot) bool {
		if slot.CredentialID == nil {
			return false
		}
		if e := eligibility(*slot.CredentialID); e != Eligible {
			skipped = append(skipped, skippedSlot{id: slot.ID, reason: "credential_" + string(e)})
			return true
		}
		return false
	})
	return slots, skipped
}

// noSlotReason names why a target has no credential slot to try: the reason
// every skipped slot shares, such as credential_lapsed, or
// no_eligible_credentials.
func noSlotReason(skipped []skippedSlot) string {
	if len(skipped) == 0 || slices.ContainsFunc(skipped, func(slot skippedSlot) bool { return slot.reason != skipped[0].reason }) {
		return "no_eligible_credentials"
	}
	return skipped[0].reason
}

func capacityReason(m ModelMetadata, demand *TokenDemand) string {
	if demand == nil {
		return ""
	}
	if m.ContextLength != nil {
		if demand.EstimatedInputTokens > *m.ContextLength {
			return "context_length_exceeded"
		}
		if demand.MaxOutputTokens != nil && saturatingSum(demand.EstimatedInputTokens, *demand.MaxOutputTokens) > *m.ContextLength {
			return "context_length_exceeded"
		}
	}
	if m.MaxOutputTokens != nil && demand.MaxOutputTokens != nil && *demand.MaxOutputTokens > *m.MaxOutputTokens {
		return "max_output_tokens_exceeded"
	}
	return ""
}

func saturatingSum(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

func constraintReason(policy EffectivePolicy, p Provider, m ModelMetadata, price *rates.RoutingPrice, parameters Names) string {
	for _, c := range policy.Constraints {
		if c.Only != nil && !anySelector(c.Only, p) {
			return "provider_not_allowed"
		}
		if anySelector(c.Ignore, p) {
			return "provider_ignored"
		}
		if !factAllowed(c.Regions, m.Region) {
			return "region_not_allowed"
		}
		if !factAllowed(c.Quantizations, m.Quantization) {
			return "quantization_not_allowed"
		}
		if required(c.DenyDataCollection) && (m.DataCollection == nil || *m.DataCollection || m.Source == nil || m.ObservedAt == nil) {
			return "data_collection_not_allowed"
		}
		if required(c.RequireZeroDataRetention) && (m.ZeroDataRetention == nil || !*m.ZeroDataRetention || m.Source == nil || m.ObservedAt == nil) {
			return "zero_data_retention_required"
		}
		if required(c.RequireParameters) {
			if names := parameters.list(); len(names) > 0 {
				if m.SupportedParameters == nil {
					return "parameters_unknown"
				}
				for _, name := range names {
					if !slices.Contains(*m.SupportedParameters, name) {
						return "parameter_not_supported"
					}
				}
			}
		}
		if len(c.MaxPrice) > 0 && string(c.MaxPrice) != "null" {
			var ceiling PriceCeiling
			_ = json.Unmarshal(c.MaxPrice, &ceiling)
			if price == nil || !lessOrEqual(price.InputPerMillion, ceiling.Input) || !lessOrEqual(price.OutputPerMillion, ceiling.Output) || !lessOrEqual(price.UnitPrice, ceiling.Unit) {
				return "price_ceiling_exceeded_or_unknown"
			}
		}
	}
	return ""
}

func deref(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}
