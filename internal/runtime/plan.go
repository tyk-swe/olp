package runtime

import (
	"cmp"
	"encoding/json"
	"math"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/usage"
)

type TokenDemand struct {
	EstimatedInputTokens int64
	MaxOutputTokens      *int64
}

type SelectionOptions struct {
	KeyID             string
	Preferences       *Preferences
	Parameters        []string
	Inputs            *usage.RoutingInputs
	TokenDemand       *TokenDemand
	Now               time.Time
	CheckSlots        bool
	CredentialRevoked func(string) bool
	Accept            func(Provider, Target) error
}
type Decision struct {
	TargetID              string              `json:"target_id"`
	ProviderID            string              `json:"provider_id"`
	UpstreamModel         string              `json:"upstream_model"`
	Eligible              bool                `json:"eligible"`
	Priority              int                 `json:"priority"`
	Strategy              string              `json:"strategy"`
	Attempt               *int                `json:"attempt"`
	CredentialSlotID      *string             `json:"credential_slot_id"`
	Reason                *string             `json:"reason"`
	Price                 *usage.RoutingPrice `json:"price"`
	Performance           *usage.Performance  `json:"performance"`
	VendorID              *string             `json:"vendor_id"`
	MetadataObservedAt    *time.Time          `json:"metadata_observed_at"`
	EstimatedInputTokens  *int64              `json:"estimated_input_tokens"`
	RequestedOutputTokens *int64              `json:"requested_output_tokens"`
	ContextLength         *int64              `json:"context_length"`
	MaxOutputTokens       *int64              `json:"max_output_tokens"`
}
type Plan struct {
	Attempts  []Attempt
	Decisions []Decision
	Policy    EffectivePolicy
	Budget    int
}
type rankedCandidate struct {
	slots      []Slot
	attempt    Attempt
	decision   Decision
	order      int
	preference int
}

func PlanRequest(s *Snapshot, slug, operation, surface, mode string, affinity []byte, options SelectionOptions) (Plan, error) {
	plan := Plan{Attempts: []Attempt{}, Decisions: []Decision{}}
	route, ok := s.Routes[slug]
	if !ok {
		return plan, &SelectionError{Code: RouteNotFound}
	}
	if !slices.Contains(route.Operations, operation) {
		return plan, &SelectionError{Code: OperationNotSupported}
	}
	policy, e := ResolvePolicy(s.InstallationPolicy, route.Policy, s.KeyPolicies[options.KeyID], options.Preferences)
	if e != nil {
		return plan, e
	}
	plan.Policy = policy
	if options.Preferences != nil && options.Preferences.MaxAttempts != nil && *options.Preferences.MaxAttempts > route.MaxAttempts {
		return plan, unsupportedBudget()
	}
	plan.Budget = policy.Preferences.Budget(route.MaxAttempts)
	now := options.Now
	if now.IsZero() {
		now = time.Now()
	}
	routeID, e := uuid.Parse(route.RoutingID)
	if e != nil {
		return plan, &SelectionError{Code: NoEligibleTargets}
	}
	rows := make([]rankedCandidate, 0, len(route.Targets))
	for _, target := range route.Targets {
		provider, exists := s.Providers[target.ProviderID]
		row := rankedCandidate{decision: Decision{TargetID: target.ID, ProviderID: target.ProviderID, UpstreamModel: target.ProviderModel, Priority: target.Priority, Strategy: policy.Strategy}, order: len(policy.Preferences.Order)}
		if provider.VendorID != "" {
			row.decision.VendorID = &provider.VendorID
		}
		var metadata ModelMetadata
		_ = json.Unmarshal(provider.Models[target.ProviderModel], &metadata)
		row.decision.MetadataObservedAt = metadata.ObservedAt
		row.decision.ContextLength = metadata.ContextLength
		row.decision.MaxOutputTokens = metadata.MaxOutputTokens
		if options.TokenDemand != nil {
			input := options.TokenDemand.EstimatedInputTokens
			row.decision.EstimatedInputTokens = &input
			row.decision.RequestedOutputTokens = options.TokenDemand.MaxOutputTokens
		}
		row.decision.Price = options.Inputs.Price(provider.Kind, provider.ID, provider.VendorID, target.ProviderModel, operation, now)
		row.decision.Performance = options.Inputs.Metrics(provider.ID, target.ProviderModel, operation, mode, now)
		reason := ""
		switch {
		case !exists:
			reason = "target_unknown"
		case !provider.Enabled:
			reason = "provider_not_active"
		case !provider.Supports(target.ProviderModel, operation, surface, mode):
			reason = "capability_not_certified"
		}
		if reason == "" {
			reason = capacityReason(metadata, options.TokenDemand)
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
			row.slots = SelectSlots(provider, target.ProviderModel, route, options.KeyID, operation, surface, mode, affinity)
			if options.CredentialRevoked != nil {
				row.slots = slices.DeleteFunc(row.slots, func(slot Slot) bool { return slot.CredentialID != nil && options.CredentialRevoked(*slot.CredentialID) })
			}
			if len(row.slots) == 0 {
				reason = "no_eligible_credentials"
			}
		}

		if reason == "" && options.Accept != nil {
			if e := options.Accept(provider, target); e != nil {
				reason = "unsupported_request_semantics"
			}
		}
		targetID, e := uuid.Parse(target.RoutingID)
		if e != nil {
			reason = "target_unknown"
		}
		if reason != "" {
			row.decision.Reason = &reason
		} else {
			row.decision.Eligible = true
		}
		row.attempt = Attempt{TargetID: target.ID, ProviderID: target.ProviderID, ProviderRevisionID: provider.RevisionID, ProviderKind: provider.Kind, UpstreamModel: target.ProviderModel, Timeout: time.Duration(target.Timeout) * time.Millisecond, Priority: target.Priority, Score: Score(routeID, targetID, target.Weight, operation, surface, mode, affinity), Strategy: policy.Strategy, PolicyDigest: policy.Digest, Price: row.decision.Price, Performance: row.decision.Performance, VendorID: provider.VendorID}
		perf := row.decision.Performance
		if policy.Strategy == "latency" && policy.Preferences.PreferredMaxLatencyMS != nil && (perf == nil || perf.LatencyMS > float64(*policy.Preferences.PreferredMaxLatencyMS)) {
			row.preference = 1
		}
		if policy.Strategy == "throughput" && policy.Preferences.PreferredMinThroughput != nil && (perf == nil || perf.Throughput == nil || *perf.Throughput < float64(*policy.Preferences.PreferredMinThroughput)) {
			row.preference = 1
		}
		rows = append(rows, row)
	}
	slices.SortStableFunc(rows, func(a, b rankedCandidate) int {
		if a.decision.Eligible != b.decision.Eligible {
			if a.decision.Eligible {
				return -1
			}
			return 1
		}
		if order := cmp.Compare(a.attempt.Priority, b.attempt.Priority); order != 0 {
			return order
		}
		if order := cmp.Compare(a.order, b.order); order != 0 {
			return order
		}
		if order := cmp.Compare(a.preference, b.preference); order != 0 {
			return order
		}
		switch policy.Strategy {
		case "price":
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
		case "latency", "throughput":
			ap, bp := a.decision.Performance, b.decision.Performance
			if policy.Strategy == "throughput" {
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
				if policy.Strategy == "latency" && ap.LatencyMS != bp.LatencyMS {
					return cmp.Compare(ap.LatencyMS, bp.LatencyMS)
				}
				if policy.Strategy == "throughput" && *ap.Throughput != *bp.Throughput {
					return cmp.Compare(*bp.Throughput, *ap.Throughput)
				}
			}
		}
		return cmp.Compare(b.attempt.Score, a.attempt.Score)
	})
	ordinal := 0
	for _, row := range rows {
		if !row.decision.Eligible {
			plan.Decisions = append(plan.Decisions, row.decision)
			continue
		}
		plan.Attempts = append(plan.Attempts, row.attempt)
		count := len(row.slots)
		if !options.CheckSlots {
			count = 1
		}
		for i := 0; i < count; i++ {
			decision := row.decision
			if options.CheckSlots {
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
	}

	return plan, nil
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

func constraintReason(policy EffectivePolicy, p Provider, m ModelMetadata, price *usage.RoutingPrice, parameters []string) string {
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
		if required(c.RequireParameters) && len(parameters) > 0 {
			if m.SupportedParameters == nil {
				return "parameters_unknown"
			}
			for _, name := range parameters {
				if !slices.Contains(*m.SupportedParameters, name) {
					return "parameter_not_supported"
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
func unsupportedBudget() error { return &SelectionError{Code: "attempt_budget_increase_forbidden"} }
