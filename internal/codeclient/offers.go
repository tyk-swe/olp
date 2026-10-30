package codeclient

import (
	"slices"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
)

// Offers is what the accounts a route's requests can reach serve.
type Offers []Offer

// Offer is the adapter and models of one or more accounts.
type Offer struct {
	PaidProtocol codemode.Protocol
	Adapter      codemode.Adapter
	Models       []string
}

// adapters returns the adapters whose accounts serve model, in table order.
func (o Offers) adapters(route codemode.Route, model string) []codemode.Adapter {
	resolved, err := route.ResolveModel(codemode.Operation{Model: model})
	if err != nil {
		return nil
	}
	if _, paid := route.PaidTargets[resolved.Model]; paid {
		return nil
	}
	var adapters []codemode.Adapter
	for _, offer := range o {
		if offer.PaidProtocol == "" && slices.Contains(offer.Models, resolved.Model) && !slices.Contains(adapters, offer.Adapter) {
			adapters = append(adapters, offer.Adapter)
		}
	}
	slices.SortFunc(adapters, codeadapter.Compare)
	return adapters
}

// through returns the adapter a client sends each model through: the first
// choice, in table order, with which one conversation reaches them all.
func (o Offers) through(route codemode.Route, client string, models []string) (map[string]codemode.Adapter, bool) {
	chosen := make([]codemode.Adapter, len(models))
	var choose func(i int) bool
	choose = func(i int) bool {
		if i == len(models) {
			return o.reaches(route, client, models, chosen)
		}
		if _, paid := o.paidProtocol(route, models[i], client); paid {
			return choose(i + 1)
		}
		for _, adapter := range o.adapters(route, models[i]) {
			if declaration, _ := codeadapter.Lookup(adapter); routeReaches(route, declaration, client, models[i]) {
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
	through := map[string]codemode.Adapter{}
	for i, model := range models {
		through[model] = chosen[i]
	}
	return through, true
}

// reaches reports whether one conversation reaches every model, sent through
// its adapter, whatever order it first uses them in and whichever account
// admission pins for each. The client sends a model through an adapter on one
// protocol, and an account serves it when its adapter serves the model on that
// protocol; another account joins the conversation only with an adapter it
// does not use yet.
func (o Offers) reaches(route codemode.Route, client string, models []string, through []codemode.Adapter) bool {
	// Accounts with one adapter serving the same of these models are alike.
	var alike Offers
	for _, offer := range o {
		adapter, _ := codeadapter.Lookup(offer.Adapter)
		kind := Offer{Adapter: offer.Adapter}
		for i, model := range models {
			resolved, err := route.ResolveModel(codemode.Operation{Model: model})
			if err != nil {
				return false
			}
			sender, _ := codeadapter.Lookup(through[i])
			protocol, reachable := codeadapter.RouteClientProtocol(route, adapter, client, resolved.Model)
			if sent, ok := codeadapter.RouteClientProtocol(route, sender, client, resolved.Model); ok && reachable && sent == protocol && slices.Contains(offer.Models, resolved.Model) {
				kind.Models = append(kind.Models, model)
			}
		}
		if len(kind.Models) != 0 && !slices.ContainsFunc(alike, func(a Offer) bool { return a.Adapter == kind.Adapter && slices.Equal(a.Models, kind.Models) }) {
			alike = append(alike, kind)
		}
	}
	return alike.joins(nil, slices.DeleteFunc(slices.Clone(models), func(model string) bool { _, paid := o.paidProtocol(route, model, client); return paid }))
}

// joins reports whether a conversation using tree reaches every model.
func (o Offers) joins(tree Offers, models []string) bool {
	for i, model := range models {
		rest := slices.Concat(models[:i], models[i+1:])
		if slices.ContainsFunc(tree, func(a Offer) bool { return slices.Contains(a.Models, model) }) {
			if !o.joins(tree, rest) {
				return false
			}
			continue
		}
		joined := false
		for _, offer := range o {
			if slices.Contains(offer.Models, model) && !slices.ContainsFunc(tree, func(a Offer) bool { return a.Adapter == offer.Adapter }) {
				if !o.joins(append(slices.Clone(tree), offer), rest) {
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

func routeReaches(route codemode.Route, adapter codeadapter.Vendor, client, model string) bool {
	resolved, err := route.ResolveModel(codemode.Operation{Model: model})
	if err != nil {
		return false
	}
	_, ok := codeadapter.RouteClientProtocol(route, adapter, client, resolved.Model)
	return ok
}
