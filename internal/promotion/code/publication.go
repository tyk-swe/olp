package code

import (
	"context"
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/access"
	domain "github.com/tyk-swe/olp/internal/code"
	coderoute "github.com/tyk-swe/olp/internal/code/route"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/provider"
	"github.com/tyk-swe/olp/internal/routing"
)

type Publication struct {
	Policy          Policy
	Connections     map[string]Connection
	PaidConnections map[string]PaidConnection
}

type frozen struct {
	Policy      coderoute.Input
	Connections map[string]routing.Configuration
	Paid        map[string]routing.Provider
}

func parsePublication(e contract.ConfigurationCodePublication, names promotion.Names, deps provider.Deps) (Publication, error) {
	connections, err := promotion.TryMapValues(e.Connections, parseConnection)
	if err != nil {
		return Publication{}, err
	}
	paid, err := promotion.TryMapValues(promotion.ValueOf(e.PaidConnections), func(_ string, c contract.ConfigurationCodePaidConnection) (PaidConnection, error) {
		return parsePaidConnection(c, names, deps)
	})
	if err != nil {
		return Publication{}, err
	}
	return Publication{Policy: policyOf(e.Policy), Connections: connections, PaidConnections: paid}, nil
}

func renderPublication(p *Publication) (*contract.ConfigurationCodePublication, error) {
	if p == nil {
		return nil, nil
	}
	policy, err := renderPolicy(p.Policy)
	if err != nil {
		return nil, err
	}
	connections, err := promotion.TryMapValues(p.Connections, renderConnection)
	if err != nil {
		return nil, err
	}
	paid, err := promotion.TryMapValues(p.PaidConnections, func(_ string, c PaidConnection) (contract.ConfigurationCodePaidConnection, error) { return c.render() })
	if err != nil {
		return nil, err
	}
	return &contract.ConfigurationCodePublication{Policy: policy, Connections: connections, PaidConnections: &paid}, nil
}

func (p Publication) resolve(ctx context.Context, q access.Queryer, slug, projectID string, in scope) (*frozen, error) {
	pinned, err := promotion.TryMap(slices.Sorted(maps.Keys(p.Connections)), func(name string) (pin, error) {
		return p.Connections[name].resolve(ctx, q, name, projectID, p.Policy.Pool, in)
	})
	if err != nil {
		return nil, err
	}
	policy, err := p.Policy.resolve(ctx, q, slug, projectID, in)
	if err != nil {
		return nil, err
	}
	paid, err := resolvePaid(ctx, q, p.PaidConnections, policy, in.bindings, in.deps)
	if err != nil {
		return nil, err
	}
	if _, err := coderoute.PaidProviders(ctx, q, paidRoute(policy)); err != nil {
		return nil, err
	}
	connections := promotion.Associate(pinned, func(c pin) (string, routing.Configuration) { return c.provider, c.configuration })
	return &frozen{Policy: policy, Connections: connections, Paid: paid}, nil
}

func (f *frozen) same(document *domain.Route, connections map[string]routing.Configuration, paid map[string]routing.Provider) (bool, error) {
	if f == nil || document == nil {
		return f == nil && document == nil, nil
	}
	return same(frozen{Policy: inputOf(*document), Connections: connections, Paid: paid}, *f)
}

func portablePublication(ctx context.Context, q access.Queryer, document *domain.Route, connections map[string]routing.Configuration, paid map[string]routing.Provider, slots map[string]PaidTarget) (*Publication, error) {
	if document == nil {
		return nil, nil
	}
	policy, err := portablePolicy(*document, slots)
	if err != nil {
		return nil, err
	}
	portable, err := portablePaid(ctx, q, paid)
	if err != nil {
		return nil, err
	}
	portableConns, err := promotion.TryMapValues(connections, func(_ string, cfg routing.Configuration) (Connection, error) {
		return portableConnection(cfg)
	})
	if err != nil {
		return nil, err
	}
	return &Publication{Policy: policy, Connections: portableConns, PaidConnections: portable}, nil
}
