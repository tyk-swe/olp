package client

import (
	"slices"

	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/code/vendor"
)

type SubscriptionOffer struct {
	Adapter code.Adapter
	Models  []string
}

type PaidOffer struct {
	Model     string
	Protocol  code.Protocol
	Streaming bool
}

type Offers struct {
	Subscriptions []SubscriptionOffer
	Paid          []PaidOffer
}

func (o Offers) IsEmpty() bool {
	return len(o.Subscriptions) == 0 && len(o.Paid) == 0
}

func (o Offers) adapters(route code.Route, model string) []code.Adapter {
	resolved, err := route.ResolveModel(code.Operation{Model: model})
	if err != nil {
		return nil
	}
	if _, paid := route.PaidTargets[resolved.Model]; paid {
		return nil
	}
	var adapters []code.Adapter
	for _, offer := range o.Subscriptions {
		if slices.Contains(offer.Models, resolved.Model) && !slices.Contains(adapters, offer.Adapter) {
			adapters = append(adapters, offer.Adapter)
		}
	}
	slices.SortFunc(adapters, vendor.Compare)
	return adapters
}

func (o Offers) through(route code.Route, client string, models []string) (map[string]code.Adapter, bool) {
	paid := func(model string) bool {
		_, selected := o.paidProtocol(route, model, client)
		return selected
	}
	if slices.ContainsFunc(models, paid) && slices.ContainsFunc(models, func(model string) bool { return !paid(model) }) {
		return nil, false
	}
	chosen := make([]code.Adapter, len(models))
	var choose func(i int) bool
	choose = func(i int) bool {
		if i == len(models) {
			return o.reaches(route, client, models, chosen)
		}
		if _, isPaid := o.paidProtocol(route, models[i], client); isPaid {
			return choose(i + 1)
		}
		for _, adapter := range o.adapters(route, models[i]) {
			if declaration, _ := vendor.Lookup(adapter); routeReaches(route, declaration, client, models[i]) {
				if chosen[i] = adapter; choose(i + 1) {
					return true
				}
			}
		}
		return false
	}
	if !choose(0) {
		return nil, false
	}
	through := map[string]code.Adapter{}
	for i, model := range models {
		through[model] = chosen[i]
	}
	return through, true
}

func (o Offers) reaches(route code.Route, client string, models []string, through []code.Adapter) bool {
	var alike []SubscriptionOffer
	for _, offer := range o.Subscriptions {
		adapter, _ := vendor.Lookup(offer.Adapter)
		kind := SubscriptionOffer{Adapter: offer.Adapter}
		for i, model := range models {
			resolved, err := route.ResolveModel(code.Operation{Model: model})
			if err != nil {
				return false
			}
			sender, _ := vendor.Lookup(through[i])
			protocol, reachable := vendor.RouteClientProtocol(route, adapter, client, resolved.Model)
			if sent, ok := vendor.RouteClientProtocol(route, sender, client, resolved.Model); ok && reachable && sent == protocol && slices.Contains(offer.Models, resolved.Model) {
				kind.Models = append(kind.Models, model)
			}
		}
		if len(kind.Models) != 0 && !slices.ContainsFunc(alike, func(a SubscriptionOffer) bool {
			return a.Adapter == kind.Adapter && slices.Equal(a.Models, kind.Models)
		}) {
			alike = append(alike, kind)
		}
	}
	unpaid := slices.DeleteFunc(slices.Clone(models), func(model string) bool {
		_, paid := o.paidProtocol(route, model, client)
		return paid
	})
	return joinsTree(alike, nil, unpaid)
}

func joinsTree(offers, tree []SubscriptionOffer, models []string) bool {
	for i, model := range models {
		rest := slices.Concat(models[:i], models[i+1:])
		if slices.ContainsFunc(tree, func(a SubscriptionOffer) bool { return slices.Contains(a.Models, model) }) {
			if !joinsTree(offers, tree, rest) {
				return false
			}
			continue
		}
		joined := false
		for _, offer := range offers {
			if slices.Contains(offer.Models, model) && !slices.ContainsFunc(tree, func(a SubscriptionOffer) bool { return a.Adapter == offer.Adapter }) {
				if !joinsTree(offers, append(slices.Clone(tree), offer), rest) {
					return false
				}
				joined = true
			}
		}
		if !joined {
			return false
		}
	}
	return true
}

func routeReaches(route code.Route, adapter vendor.Vendor, client, model string) bool {
	resolved, err := route.ResolveModel(code.Operation{Model: model})
	if err != nil {
		return false
	}
	_, ok := vendor.RouteClientProtocol(route, adapter, client, resolved.Model)
	return ok
}
