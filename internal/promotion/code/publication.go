package code

import (
	"context"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/provider"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/internal/routing"
)

type Publication struct {
	Policy          Policy
	Connections     map[string]Connection
	PaidConnections map[string]PaidConnection
}

type frozen struct {
	policy      routes.CodeRouteInput
	connections map[string]providers.Configuration
	paid        map[string]routing.Provider
}

func parsePublication(e contract.ConfigurationCodePublication, names promotion.Names, deps provider.Deps) (Publication, error) {
	connectionNames := sortedKeys(e.Connections)
	connections, err := promotion.TryMapIndex(connectionNames, func(_ int, name string) (Connection, error) { return parseConnection(name, e.Connections[name]) })
	if err != nil {
		return Publication{}, err
	}
	declared := orEmpty(e.PaidConnections)
	paidNames := sortedKeys(declared)
	paid, err := promotion.TryMapIndex(paidNames, func(_ int, name string) (PaidConnection, error) {
		return parsePaidConnection(declared[name], names, deps)
	})
	if err != nil {
		return Publication{}, err
	}
	return Publication{Policy: policyOf(e.Policy), Connections: keyed(connectionNames, connections), PaidConnections: keyed(paidNames, paid)}, nil
}

func renderPublication(p *Publication) (*contract.ConfigurationCodePublication, error) {
	if p == nil {
		return nil, nil
	}
	policy, err := renderPolicy(p.Policy)
	if err != nil {
		return nil, err
	}
	names := sortedKeys(p.Connections)
	connections, err := promotion.TryMapIndex(names, func(_ int, name string) (contract.ConfigurationCodeConnection, error) {
		return renderConnection(name, p.Connections[name])
	})
	if err != nil {
		return nil, err
	}
	paidNames := sortedKeys(p.PaidConnections)
	paid, err := promotion.TryMapIndex(paidNames, func(_ int, name string) (contract.ConfigurationCodePaidConnection, error) {
		return p.PaidConnections[name].render()
	})
	if err != nil {
		return nil, err
	}
	return &contract.ConfigurationCodePublication{Policy: policy, Connections: keyed(names, connections), PaidConnections: new(keyed(paidNames, paid))}, nil
}

func (p Publication) resolve(ctx context.Context, q access.Queryer, slug, projectID string, in scope) (*frozen, error) {
	pinned, err := promotion.TryMapIndex(sortedKeys(p.Connections), func(_ int, name string) (pin, error) {
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
	if _, err := routes.CodePaidProviders(ctx, q, paidRoute(policy)); err != nil {
		return nil, err
	}
	connections := keyed(promotion.Map(pinned, func(c pin) string { return c.provider }), promotion.Map(pinned, func(c pin) providers.Configuration { return c.configuration }))
	return &frozen{policy: policy, connections: connections, paid: paid}, nil
}

func (f *frozen) same(document *codemode.Route, connections map[string]providers.Configuration, paid map[string]routing.Provider) (bool, error) {
	if f == nil || document == nil {
		return f == nil && document == nil, nil
	}
	return same([]any{inputOf(*document), connections, paid}, []any{f.policy, f.connections, f.paid})
}

func portablePublication(ctx context.Context, q access.Queryer, document *codemode.Route, connections map[string]providers.Configuration, paid map[string]routing.Provider, slots map[string]PaidTarget) (*Publication, error) {
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
	return &Publication{Policy: policy, Connections: mappedValues(connections, portableConnection), PaidConnections: portable}, nil
}
