package runtime

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/usage"
)

type TokenDemand struct {
	EstimatedInputTokens int64
	MaxOutputTokens      *int64
}

// Names are the names of the controls a request uses. A policy that requires
// parameters reads them and no other does, so they are a function: for a large
// request, listing them means decoding all of it.
type Names func() []string

// Listed is the Names of a list that is already at hand.
func Listed(names []string) Names { return func() []string { return names } }

func (n Names) list() []string {
	if n == nil {
		return nil
	}
	return n()
}

type SelectionOptions struct {
	KeyID       string
	Preferences *Preferences
	Parameters  Names
	Inputs      *usage.RoutingInputs
	TokenDemand *TokenDemand
	// Demand, when set, supplies the token demand of each target in place of
	// TokenDemand, for a request whose input counts differently on each model.
	Demand     func(Provider, Target) *TokenDemand
	Now        time.Time
	CheckSlots bool
	// CredentialEligibility excludes credential versions that may not serve;
	// a provider's ineligible network credential names its reason.
	CredentialEligibility func(credentialID string) Eligibility
	// UnconfinedPlugins is set where the deployment enables unconfined
	// plugins. Elsewhere, targets of providers whose plugin is unconfined
	// are ineligible.
	UnconfinedPlugins bool
	Accept            func(Provider, Target) error
	Effective         func(Provider, Target) (Names, *TokenDemand)
	// Supply reads the live capacity and spend state the plan needs in one
	// round trip, bounded by Context. The planner reads it at most once, and
	// only when the capacity strategy or a spend cap applies.
	Context context.Context
	Supply  SupplyReader
	// Unhealthy reports a provider that fleet health avoids. Its targets stay
	// eligible but move to the end of the attempt order.
	Unhealthy func(providerID string) bool
	// Features describe the request to route selectors, which are not
	// evaluated without them.
	Features *Features
	// Evaluate decides a selector's classifier or plugin predicate.
	Evaluate func(Selector) PredicateResult
	// Permitted reports whether the key may use a route a selector delegates
	// to.
	Permitted func(Route) bool
	// Mirror seeds shadow sampling; without it no shadow target is sampled.
	Mirror []byte
}
type Decision struct {
	Incompatibility       *Incompatibility    `json:"incompatibility,omitempty"`
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
	Selector              *string             `json:"selector,omitempty"`
	Shadow                bool                `json:"shadow,omitempty"`
	Unhealthy             bool                `json:"unhealthy,omitempty"`
	Headroom              *float64            `json:"headroom,omitempty"`
}

// Incompatibility describes an unsatisfied semantic obligation without retaining
// request values, native opaque material, or provider credentials.
type Incompatibility struct {
	Code        string `json:"code"`
	Field       string `json:"field,omitempty"`
	Requirement string `json:"requirement"`
	Message     string `json:"message"`
}

type incompatibilityError interface {
	Incompatibility() (code, field, requirement, message string)
}
type Plan struct {
	Attempts  []Attempt
	Decisions []Decision
	Policy    EffectivePolicy
	Budget    int
	// Selectors trace the route's selectors evaluated for the request.
	// Delegate names the route a matching selector hands the request to, in
	// which case the plan has no attempts of its own.
	Selectors []SelectorOutcome
	Delegate  string
	// Shadows are the sampled shadow targets eligible to mirror the request.
	Shadows []Attempt
	// Baseline is the route's most expensive eligible target when a selector
	// narrowed the route, against which usage reports measure the selector's
	// savings; nil without a narrowing selector or a known price.
	Baseline *Baseline
}

// Baseline names the target a request would have cost most on.
type Baseline struct {
	ProviderID    string
	UpstreamModel string
	VendorID      string
}

// Selected is the selector that matched, or empty when none did.
func (p Plan) Selected() string {
	for _, outcome := range p.Selectors {
		if outcome.Matched {
			return outcome.ID
		}
	}
	return ""
}

// Conditions names the fallback conditions a plan without attempts meets:
// context_window when a target's context cannot hold the request, and budget
// when a spend cap removed a target.
func (p Plan) Conditions() []string {
	if len(p.Attempts) > 0 || p.Delegate != "" {
		return nil
	}
	var out []string
	for _, d := range p.Decisions {
		if d.Reason == nil || d.Shadow {
			continue
		}
		condition := ""
		switch {
		case *d.Reason == "context_length_exceeded" || *d.Reason == "max_output_tokens_exceeded":
			condition = FallbackContextWindow
		case BudgetReason(*d.Reason):
			condition = FallbackBudget
		}
		if condition != "" && !slices.Contains(out, condition) {
			out = append(out, condition)
		}
	}
	return out
}

type rankedCandidate struct {
	slots      []Slot
	skipped    []skippedSlot
	attempt    Attempt
	decision   Decision
	order      int
	preference int
	shadow     bool
	// allowed marks a target that only the matching selector excluded.
	allowed bool
}

// PlanRequest evaluates targets, orders eligible candidates, and explains the
// credential attempts within the route's budget. Execution rechecks live
// credential authority and transient availability before dispatch.
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
	chosen := selectTargets(s, route, operation, options)
	plan.Selectors = chosen.trace
	if chosen.selector != nil && chosen.selector.Route != "" {
		plan.Delegate = chosen.selector.Route
		return plan, nil
	}
	rows, e := evaluateCandidates(s, route, operation, surface, mode, affinity, policy, chosen, options)
	if e != nil {
		return plan, e
	}
	capacity := policy.Strategy == "capacity"
	if query := supplyQuery(route, rows, s, capacity && options.CheckSlots); !query.empty() {
		var state *SupplyState
		if options.Supply != nil {
			state = options.Supply.ReadSupply(cmp.Or(options.Context, context.Background()), query)
		}
		applySupply(route, rows, s, state)
		for i := range rows {
			if capacity {
				OrderSlots(rows[i].slots, state)
			}
			rows[i].attempt.Slots = slotIDs(rows[i].slots)
		}
	}
	if chosen.selector != nil && len(chosen.selector.Tags) > 0 {
		plan.Baseline = baseline(rows, operation)
	}
	orderCandidates(rows, policy.Strategy, operation)
	plan.addCandidates(rows, options.CheckSlots)
	return plan, nil
}

// baseline is the most expensive known-price target that would serve the
// request but for the selector, or nil when no such price is known.
func baseline(rows []rankedCandidate, operation string) *Baseline {
	var most *rankedCandidate
	var price *big.Rat
	for i := range rows {
		row := &rows[i]
		if row.shadow || !row.decision.Eligible && !row.allowed {
			continue
		}
		if p := row.decision.Price.Scalar(operation); p != nil && (price == nil || p.Cmp(price) > 0) {
			most, price = row, p
		}
	}
	if most == nil {
		return nil
	}
	return &Baseline{ProviderID: most.attempt.ProviderID, UpstreamModel: most.attempt.UpstreamModel, VendorID: most.attempt.VendorID}
}

// evaluateCandidates preserves admission order: published eligibility and
// source constraints precede preparation, then effective request constraints.
func evaluateCandidates(s *Snapshot, route Route, operation, surface, mode string, affinity []byte, policy EffectivePolicy, chosen selection, options SelectionOptions) ([]rankedCandidate, error) {
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
		provider, exists := s.Providers[target.ProviderID]
		row := rankedCandidate{decision: Decision{TargetID: target.ID, ProviderID: target.ProviderID, UpstreamModel: target.ProviderModel, Priority: target.Priority, Strategy: policy.Strategy, Shadow: target.Shadow != nil}, order: len(policy.Preferences.Order), shadow: target.Shadow != nil}
		if chosen.selector != nil && !row.shadow {
			row.decision.Selector = &chosen.selector.ID
		}
		row.decision.Unhealthy = options.Unhealthy != nil && options.Unhealthy(target.ProviderID)
		if provider.VendorID != "" {
			row.decision.VendorID = &provider.VendorID
		}
		var metadata ModelMetadata
		// A model with no metadata is not decoded: the error that would build is
		// discarded, for every target of every request.
		if raw := provider.Models[target.ProviderModel]; len(raw) > 0 {
			_ = json.Unmarshal(raw, &metadata)
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
		row.decision.Price = options.Inputs.Price(provider.Kind, provider.ID, provider.VendorID, target.ProviderModel, operation, now)
		row.decision.Performance = options.Inputs.Metrics(provider.ID, target.ProviderModel, operation, mode, now)
		reason := ""
		switch {
		case !exists:
			reason = "target_unknown"
		case !provider.Enabled:
			reason = "provider_not_active"
		case provider.Plugin != nil && provider.Plugin.Unconfined() && !options.UnconfinedPlugins:
			reason = "plugin_unconfined_disabled"
		case !provider.Supports(target.ProviderModel, operation, surface, mode):
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

		if reason == "" && options.Accept != nil {
			if e := options.Accept(provider, target); e != nil {
				reason = "unsupported_request_semantics"
				var detail incompatibilityError
				if errors.As(e, &detail) {
					code, field, requirement, message := detail.Incompatibility()
					row.decision.Incompatibility = &Incompatibility{Code: code, Field: field, Requirement: requirement, Message: message}
					reason = code
				}
			}
		}
		if reason == "" && options.Effective != nil {
			parameters, demand := options.Effective(provider, target)
			reason = capacityReason(metadata, demand)
			if reason == "" {
				reason = constraintReason(policy, provider, metadata, row.decision.Price, parameters)
			}
			if demand != nil {
				input := demand.EstimatedInputTokens
				row.decision.EstimatedInputTokens = &input
				row.decision.RequestedOutputTokens = demand.MaxOutputTokens
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
	return rows, nil
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
		if order := cmp.Compare(a.order, b.order); order != 0 {
			return order
		}
		if order := cmp.Compare(a.preference, b.preference); order != 0 {
			return order
		}
		switch strategy {
		case "capacity":
			ah, bh := a.decision.Headroom, b.decision.Headroom
			if order := compareKnown(deref(ah), ah != nil, deref(bh), bh != nil); order != 0 {
				return order
			}
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
			if strategy == "throughput" {
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
				if strategy == "latency" && ap.LatencyMS != bp.LatencyMS {
					return cmp.Compare(ap.LatencyMS, bp.LatencyMS)
				}
				if strategy == "throughput" && *ap.Throughput != *bp.Throughput {
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

func constraintReason(policy EffectivePolicy, p Provider, m ModelMetadata, price *usage.RoutingPrice, parameters Names) string {
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

func unsupportedBudget() error { return &SelectionError{Code: "attempt_budget_increase_forbidden"} }
