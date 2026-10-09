package runtime

import (
	"cmp"
	"context"
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
)

// The admission classes, most urgent first. Requests are normal unless their
// key or caller says otherwise; the gateway's own traffic is low.
const (
	PriorityCritical = "critical"
	PriorityHigh     = "high"
	PriorityNormal   = "normal"
	PriorityLow      = "low"
)

// Priorities are the admission classes API key policies name.
var Priorities = access.Priorities

// Supply holds the controls a connection or credential slot quota declares
// beyond its windows: spend caps, and per-priority shares of the quota that
// apply once its use crosses the saturation threshold.
type Supply struct {
	DailyCostLimit    *string `json:"daily_cost_limit,omitempty"`
	MonthlyCostLimit  *string `json:"monthly_cost_limit,omitempty"`
	PriorityShares    *Shares `json:"priority_shares,omitempty"`
	SaturationPercent *int64  `json:"saturation_percent,omitempty"`
}

// Shares are the percentages of a quota each priority keeps once the quota
// is saturated. They sum to 100.
type Shares struct {
	Critical int64 `json:"critical"`
	High     int64 `json:"high"`
	Normal   int64 `json:"normal"`
	Low      int64 `json:"low"`
}

// Of returns the share of one priority, or the normal share for an unknown
// class.
func (s Shares) Of(priority string) int64 {
	switch priority {
	case "critical":
		return s.Critical
	case "high":
		return s.High
	case "low":
		return s.Low
	}
	return s.Normal
}

// Costs returns the spend caps, or nil when neither is declared.
func (s Supply) Costs() *CostLimits {
	if s.DailyCostLimit == nil && s.MonthlyCostLimit == nil {
		return nil
	}
	return &CostLimits{DailyCostLimit: s.DailyCostLimit, MonthlyCostLimit: s.MonthlyCostLimit}
}

// Shared reports whether the quota divides itself among priorities.
func (s Supply) Shared() bool { return s.PriorityShares != nil }

func (s Supply) Validate(field string) error {
	if costs := s.Costs(); costs != nil {
		if err := costs.Validate(field); err != nil {
			return err
		}
	}
	if (s.PriorityShares == nil) != (s.SaturationPercent == nil) {
		return access.Invalid(field+".priority_shares", "Declare priority_shares and saturation_percent together.")
	}
	if s.PriorityShares == nil {
		return nil
	}
	shares := s.PriorityShares
	total := int64(0)
	for _, share := range []int64{shares.Critical, shares.High, shares.Normal, shares.Low} {
		if share < 0 || share > 100 {
			return access.Invalid(field+".priority_shares", "Use shares from 0 to 100 percent.")
		}
		total += share
	}
	if total != 100 {
		return access.Invalid(field+".priority_shares", "Use shares that sum to 100 percent.")
	}
	if *s.SaturationPercent < 1 || *s.SaturationPercent > 100 {
		return access.Invalid(field+".saturation_percent", "Use a saturation threshold from 1 to 100 percent.")
	}
	return nil
}

// SupplyQuery names the live state a plan needs: the headroom of credential
// slots a capacity plan orders, and the spend of capped connections, slots
// and the route.
type SupplyQuery struct {
	Slots []SupplySlot
	Caps  []SpendCap
}

// SupplySlot is a credential slot together with its connection, whose
// windows bound the slot's headroom too.
type SupplySlot struct {
	Provider *Provider
	Slot     Slot
}

// SpendCap is a cost-capped owner: a connection, credential slot or route.
type SpendCap struct {
	OwnerID string
	Limits  CostLimits
}

func (q SupplyQuery) empty() bool { return len(q.Slots) == 0 && len(q.Caps) == 0 }

// SupplyReader reads the live state a SupplyQuery asks for.
type SupplyReader interface {
	ReadSupply(ctx context.Context, query SupplyQuery) *SupplyState
}

// SupplyState is the live capacity and spend state the gateway reads for one
// request, in one round trip. An absent entry is unknown.
type SupplyState struct {
	// Headroom maps a credential slot ID to the smallest remaining fraction of
	// the request, token and concurrency windows of the slot and its connection.
	Headroom map[string]float64
	// Spend maps a capped owner ID to whether its caps still allow spending.
	Spend map[string]Spend
}

type Spend uint8

const (
	// SpendUnknown is a missing or malformed snapshot, which skips the owner.
	SpendUnknown Spend = iota
	SpendAvailable
	SpendExhausted
)

func (s *SupplyState) headroom(slotID string) (float64, bool) {
	if s == nil {
		return 0, false
	}
	fraction, ok := s.Headroom[slotID]
	return fraction, ok
}

// spendReason names why a capped owner may not spend, or "" when it may.
func (s *SupplyState) spendReason(ownerID, kind string) string {
	spend := SpendUnknown
	if s != nil {
		spend = s.Spend[ownerID]
	}
	switch spend {
	case SpendAvailable:
		return ""
	case SpendExhausted:
		return kind + "_budget_exhausted"
	}
	return kind + "_budget_unavailable"
}

// BudgetReason reports whether a plan reason is a supply-side spend cap, which
// can start a budget fallback.
func BudgetReason(reason string) bool {
	switch reason {
	case "connection_budget_exhausted", "slot_budget_exhausted", "route_budget_exhausted",
		"connection_budget_unavailable", "slot_budget_unavailable", "route_budget_unavailable":
		return true
	}
	return false
}

// supplyQuery collects what the plan needs to read: the slots of eligible
// targets when the strategy orders by capacity, and every applicable cap.
func supplyQuery(route Route, rows []rankedCandidate, s *Snapshot, capacity bool) SupplyQuery {
	var query SupplyQuery
	if route.Budget != nil && !route.CallerCostExempt {
		query.Caps = append(query.Caps, SpendCap{OwnerID: route.ID, Limits: *route.Budget})
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if !row.decision.Eligible {
			continue
		}
		provider, ok := s.Providers[row.attempt.ProviderID]
		if !ok {
			continue
		}
		if costs := provider.supply().Costs(); costs != nil && !route.CallerCostExempt && !seen[provider.ID] {
			seen[provider.ID] = true
			query.Caps = append(query.Caps, SpendCap{OwnerID: provider.ID, Limits: *costs})
		}
		for _, slot := range row.slots {
			if costs := slot.Costs(); costs != nil && !route.CallerCostExempt && !seen[slot.ID] {
				seen[slot.ID] = true
				query.Caps = append(query.Caps, SpendCap{OwnerID: slot.ID, Limits: *costs})
			}
			if capacity {
				query.Slots = append(query.Slots, SupplySlot{Provider: &provider, Slot: slot})
			}
		}
	}
	return query
}

func (p Provider) supply() Supply {
	if p.Limits == nil {
		return Supply{}
	}
	return p.Limits.Supply
}

// applySupply removes capped owners that may not spend and records each
// candidate's best known slot headroom. A route that may not spend removes
// every candidate.
func applySupply(route Route, rows []rankedCandidate, s *Snapshot, state *SupplyState) {
	routeReason := ""
	if route.Budget != nil && !route.CallerCostExempt {
		routeReason = state.spendReason(route.ID, "route")
	}
	for i := range rows {
		row := &rows[i]
		if !row.decision.Eligible {
			continue
		}
		reason := routeReason
		if !route.CallerCostExempt && reason == "" && s.Providers[row.attempt.ProviderID].supply().Costs() != nil {
			reason = state.spendReason(row.attempt.ProviderID, "connection")
		}
		if !route.CallerCostExempt && reason == "" && len(row.slots) > 0 {
			row.slots = slices.DeleteFunc(row.slots, func(slot Slot) bool {
				if slot.Costs() == nil {
					return false
				}
				if why := state.spendReason(slot.ID, "slot"); why != "" {
					row.skipped = append(row.skipped, skippedSlot{id: slot.ID, reason: why})
					return true
				}
				return false
			})
			if len(row.slots) == 0 {
				reason = noSlotReason(row.skipped)
			}
		}
		if reason != "" {
			row.decision.Eligible, row.decision.Reason = false, &reason
			continue
		}
		for _, slot := range row.slots {
			if fraction, ok := state.headroom(slot.ID); ok && (row.decision.Headroom == nil || fraction > *row.decision.Headroom) {
				row.decision.Headroom = &fraction
			}
		}
	}
}

// OrderSlots orders a target's credential slots for the capacity strategy:
// by slot priority, then by remaining headroom, with slots of unknown headroom
// after known ones. The sort is stable, so rendezvous order breaks ties.
func OrderSlots(slots []Slot, state *SupplyState) {
	slices.SortStableFunc(slots, func(a, b Slot) int {
		if order := cmp.Compare(a.Priority, b.Priority); order != 0 {
			return order
		}
		ah, aok := state.headroom(a.ID)
		bh, bok := state.headroom(b.ID)
		return compareKnown(ah, aok, bh, bok)
	})
}

// compareKnown orders known values before unknown ones and larger before
// smaller.
func compareKnown(a float64, aKnown bool, b float64, bKnown bool) int {
	switch {
	case aKnown != bKnown:
		if aKnown {
			return -1
		}
		return 1
	case !aKnown:
		return 0
	}
	return cmp.Compare(b, a)
}

// SupplyBudgets lists every spend cap the snapshot declares, for
// reconciliation to keep current: capped connections, capped credential slots
// and routes with a budget.
func (s *Snapshot) SupplyBudgets() []limits.SupplyBudget {
	var budgets []limits.SupplyBudget
	add := func(ownerID, kind string, costs *CostLimits) {
		if costs != nil {
			budgets = append(budgets, limits.SupplyBudget{OwnerID: ownerID, Kind: kind, DailyCostLimit: costs.DailyCostLimit, MonthlyCostLimit: costs.MonthlyCostLimit})
		}
	}
	for _, id := range slices.Sorted(maps.Keys(s.Providers)) {
		provider := s.Providers[id]
		add(provider.ID, "connection", provider.supply().Costs())
		for _, slot := range provider.Slots {
			add(slot.ID, "slot", slot.Supply.Costs())
		}
	}
	for _, slug := range slices.Sorted(maps.Keys(s.Routes)) {
		route := s.Routes[slug]
		add(route.ID, "route", route.Budget)
	}
	return budgets
}

func slotIDs(slots []Slot) []string {
	ids := make([]string, len(slots))
	for i, slot := range slots {
		ids[i] = slot.ID
	}
	return ids
}

// ArrangeSlots keeps the slots an attempt's plan left it, in the plan's order.
func ArrangeSlots(slots []Slot, order []string) []Slot {
	arranged := make([]Slot, 0, len(order))
	for _, id := range order {
		if i := slices.IndexFunc(slots, func(slot Slot) bool { return slot.ID == id }); i >= 0 {
			arranged = append(arranged, slots[i])
		}
	}
	return arranged
}
