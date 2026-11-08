package mcpserver

import (
	"context"
	"maps"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/mcp/registration"
	"github.com/tyk-swe/olp/internal/mcp/upstream"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
	"github.com/tyk-swe/olp/internal/routing"
)

type Deps struct {
	Keys       Keys
	Egress     *egress.Policy
	Unconfined *plugins.Unconfined
}

type Prepared struct{ certifications map[key]certification }

type certification struct {
	observed string
	outcome  outcome
}

type outcome interface{ outcome() }

type reused struct{}

type limited struct{}

type recertified struct{ catalog *upstream.Catalog }

func (reused) outcome()      {}
func (limited) outcome()     {}
func (recertified) outcome() {}

func Prepare(ctx context.Context, pool access.Queryer, p access.Principal, servers []Server, bindings promotion.Bindings, deps Deps) (Prepared, error) {
	if len(servers) == 0 {
		return Prepared{}, nil
	}
	directory, err := project.ReadDirectory(ctx, pool)
	if err != nil {
		return Prepared{}, err
	}
	active, err := registration.ListActive(ctx, pool)
	if err != nil {
		return Prepared{}, err
	}
	certifications, err := promotion.TryMap(servers, func(server Server) (certification, error) {
		return prepare(ctx, pool, p, bindings, deps, current(active, directory, server), server)
	})
	if err != nil {
		return Prepared{}, err
	}
	return Prepared{certifications: maps.Collect(func(yield func(key, certification) bool) {
		for index, server := range servers {
			if !yield(server.key(), certifications[index]) {
				return
			}
		}
	})}, nil
}

func prepare(ctx context.Context, pool access.Queryer, p access.Principal, bindings promotion.Bindings, deps Deps, existing *registration.Definition, server Server) (certification, error) {
	if existing != nil {
		projectID := existing.ProjectID
		if err := p.Project(&projectID, access.Change); err != nil {
			return certification{}, err
		}
	}
	if unretainedEntra(flatten(server.Transport), existing) {
		return certification{}, access.Invalid("mcp_servers", entraDetail)
	}
	if existing == nil {
		catalog, err := certify(ctx, pool, bindings, deps, existing, server)
		return certification{outcome: recertified{catalog: catalog}}, err
	}
	same, err := matches(ctx, pool, bindings, deps.Keys, *existing, server)
	if err != nil || same {
		return certification{observed: existing.ETag, outcome: reused{}}, err
	}
	limitsOnly, err := matches(ctx, pool, bindings, deps.Keys, *existing, withLimits(server, routing.ToolLimits{
		CallsPerMinute: existing.CallsPerMinute, MaxConcurrency: existing.MaxConcurrency,
		PerKeyCallsPerMinute: existing.PerKeyCallsPerMinute, PerKeyMaxConcurrency: existing.PerKeyMaxConcurrency,
	}))
	if err != nil || limitsOnly {
		return certification{observed: existing.ETag, outcome: limited{}}, err
	}
	catalog, err := certify(ctx, pool, bindings, deps, existing, server)
	return certification{observed: existing.ETag, outcome: recertified{catalog: catalog}}, err
}

func certify(ctx context.Context, pool access.Queryer, bindings promotion.Bindings, deps Deps, existing *registration.Definition, server Server) (*upstream.Catalog, error) {
	flat := flatten(server.Transport)
	material, err := materialOf(ctx, pool, bindings, deps.Keys, flat, existing)
	if err != nil {
		return nil, err
	}
	defer clear(material)
	catalog, err := upstream.CertifyOptions(ctx, deps.Egress, flat.endpoint, material, upstream.TransportOptions{
		Transport: flat.transport, Stdio: flat.stdio, Executables: registration.NewExecutables(deps.Unconfined, pool),
	})
	if err != nil || catalog.Digest != server.CatalogDigest {
		return nil, access.Fail(422, "mcp_catalog_changed", "Certification must match the artifact's reviewed tool catalog digest.")
	}
	return catalog, nil
}

func materialOf(ctx context.Context, pool access.Queryer, bindings promotion.Bindings, keys Keys, flat wire, existing *registration.Definition) ([]byte, error) {
	if flat.bearer == nil {
		return nil, nil
	}
	if sealed, bound := bindings[*flat.bearer].(promotion.Sealed); bound {
		return []byte(sealed.Secret), nil
	}
	if existing == nil || existing.CredentialID == nil {
		return nil, access.Invalid("secret_bindings", "Supply the destination MCP bearer binding.")
	}
	if flat.endpoint != existing.Endpoint {
		return nil, access.Invalid("secret_bindings", "Supply a new MCP bearer binding or remove credential_ref when changing the endpoint.")
	}
	return keys.read(ctx, pool, *existing.CredentialID)
}

func withLimits(server Server, limits routing.ToolLimits) Server {
	return Server{
		Project: server.Project, Name: server.Name, Transport: server.Transport, Enabled: server.Enabled,
		CatalogDigest: server.CatalogDigest, UnitPrice: server.UnitPrice, ToolPrices: server.ToolPrices, Limits: limits,
	}
}
