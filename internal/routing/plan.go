package routing

import (
	"cmp"
	"context"
	"math/big"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/usage/rates"
	"github.com/tyk-swe/olp/internal/usage/routinginputs"
)

type TokenDemand struct {
	EstimatedInputTokens int64
	MaxOutputTokens      *int64
	// CostInputTokens includes all billed prompts/candidates when context
	// demand describes only the largest individual prompt.
	CostInputTokens *int64
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

type CandidatePreparation struct {
	Parameters Names
	Demand     *TokenDemand
}

type PrepareCandidate func(Provider, Target) (*CandidatePreparation, error)

type Options struct {
	Region      string
	KeyID       string
	Preferences *Preferences
	Parameters  Names
	Inputs      *routinginputs.Inputs
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
	Prepare           PrepareCandidate
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
	// EstimateCost asks the plan to record each target's minimum and maximum
	// possible cost; only simulations set it, so live planning adds nothing.
	EstimateCost bool
	// WorstCost bounds one candidate's attempt cost — provider cost with the
	// attempt's pinned discount — or reports it cannot be bounded. It runs only
	// when an effective maximum-cost ceiling, cost estimation or the
	// bounded_cost strategy calls for it.
	WorstCost func(Provider, Target, *rates.RoutingPrice, *rates.RoutingDiscount) (CostBound, bool)
	// CallerCostExempt marks a request whose named route waives cost
	// accounting. The gateway bounds none of its attempts, so bounded_cost
	// keeps weighted order even where WorstCost prices a target.
	CallerCostExempt bool
}

// CostBound is one candidate's upper attempt cost. InputTokens and
// OutputTokens are the input estimate and reserved output allowance the bound
// priced, nil when it priced no tokens, as a per-request or media price does.
type CostBound struct {
	Cost         rates.Cost
	InputTokens  *int64
	OutputTokens *int64
}
type Decision struct {
	Incompatibility           *Incompatibility           `json:"incompatibility,omitempty"`
	TargetID                  string                     `json:"target_id"`
	ProviderID                string                     `json:"provider_id"`
	UpstreamModel             string                     `json:"upstream_model"`
	Eligible                  bool                       `json:"eligible"`
	Priority                  int                        `json:"priority"`
	Strategy                  string                     `json:"strategy"`
	Attempt                   *int                       `json:"attempt"`
	CredentialSlotID          *string                    `json:"credential_slot_id"`
	Reason                    *string                    `json:"reason"`
	Price                     *rates.RoutingPrice        `json:"price"`
	Performance               *routinginputs.Performance `json:"performance"`
	VendorID                  *string                    `json:"vendor_id"`
	MetadataObservedAt        *time.Time                 `json:"metadata_observed_at"`
	EstimatedInputTokens      *int64                     `json:"estimated_input_tokens"`
	RequestedOutputTokens     *int64                     `json:"requested_output_tokens"`
	ContextLength             *int64                     `json:"context_length"`
	MaxOutputTokens           *int64                     `json:"max_output_tokens"`
	Selector                  *string                    `json:"selector,omitempty"`
	Shadow                    bool                       `json:"shadow,omitempty"`
	Unhealthy                 bool                       `json:"unhealthy,omitempty"`
	Headroom                  *float64                   `json:"headroom,omitempty"`
	MaxCost                   *string                    `json:"max_cost"`
	BoundedCostFallbackReason *string                    `json:"bounded_cost_fallback_reason,omitempty"`
	// EstimatedCost is the attempt's cost bound pair for a simulation, null
	// when the target's price cannot bound or no demand is known.
	EstimatedCost *EstimatedCost `json:"estimated_cost"`
}

// EstimatedCost is the minimum and maximum one attempt on a target could
// cost: the floor prices the input estimate at the cheapest input rate; the
// bound prices the worst case the gateway would reserve. The token counts are
// the input estimate and reserved output allowance the maximum priced, null
// when it priced no tokens.
type EstimatedCost struct {
	Minimum              string `json:"minimum"`
	Maximum              string `json:"maximum"`
	Currency             string `json:"currency"`
	PricedInputTokens    *int64 `json:"priced_input_tokens"`
	ReservedOutputTokens *int64 `json:"reserved_output_tokens"`
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
	provider   Provider
	slots      []Slot
	skipped    []skippedSlot
	attempt    Attempt
	decision   Decision
	order      int
	preference int
	locality   int
	shadow     bool
	bound      *CostBound
	costRanked bool
	// allowed marks a target that only the matching selector excluded.
	allowed bool
}

// Select evaluates targets, orders eligible candidates, and explains the
// credential attempts within the route's budget. Execution rechecks live
// credential authority and transient availability before dispatch.
func Select(s selectionSource, slug, operation, surface, mode string, affinity []byte, options Options) (Plan, error) {
	plan := Plan{Attempts: []Attempt{}, Decisions: []Decision{}}
	route, ok := s.lookupRoute(slug)
	if !ok {
		return plan, &SelectionError{Code: RouteNotFound}
	}
	if !slices.Contains(route.Operations, operation) {
		return plan, &SelectionError{Code: OperationNotSupported}
	}
	installationPolicy, keyPolicy := s.selectionPolicies(options.KeyID)
	policy, e := ResolvePolicy(installationPolicy, route.Policy, keyPolicy, options.Preferences)
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
	capacity := policy.Strategy == strategyCapacity
	if query := supplyQuery(route, rows, capacity && options.CheckSlots); !query.empty() {
		var state *SupplyState
		if options.Supply != nil {
			state = options.Supply.ReadSupply(cmp.Or(options.Context, context.Background()), query)
		}
		applySupply(route, rows, state)
		for i := range rows {
			if capacity {
				OrderSlots(rows[i].slots, state)
			}
			if options.CheckSlots {
				rows[i].attempt.Slots = slotIDs(rows[i].slots)
			}
		}
	}
	if chosen.selector != nil && len(chosen.selector.Tags) > 0 {
		plan.Baseline = baseline(rows, operation)
	}
	applyBoundedCost(rows, policy.Strategy, operation, options.CallerCostExempt)
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

// estimatedCost is the bound pair a simulation reports for a target. A
// committed provider can only charge zero; a target without a demand or a
// boundable price reports nothing.
func estimatedCost(provider Provider, price *rates.RoutingPrice, discount *rates.RoutingDiscount, demand *TokenDemand, now time.Time, worst func(Provider, Target, *rates.RoutingPrice, *rates.RoutingDiscount) (CostBound, bool), target Target) *EstimatedCost {
	if price == nil || demand == nil {
		return nil
	}
	estimated := &EstimatedCost{Minimum: "0", Maximum: "0", Currency: price.Currency}
	if provider.CommitmentAt(now) != nil {
		return estimated
	}
	bound, ok := worst(provider, target, price, discount)
	if !ok {
		return nil
	}
	input := demand.EstimatedInputTokens
	if demand.CostInputTokens != nil {
		input = *demand.CostInputTokens
	}
	floor, ok := price.CostFloor(input, now, discount)
	if !ok {
		return nil
	}
	estimated.Minimum, estimated.Maximum = floor.String(), bound.Cost.String()
	estimated.PricedInputTokens, estimated.ReservedOutputTokens = bound.InputTokens, bound.OutputTokens
	return estimated
}

func strptr(s string) *string { return &s }

func unsupportedBudget() error { return &SelectionError{Code: "attempt_budget_increase_forbidden"} }
