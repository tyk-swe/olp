package mcpserver

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/mcp/registration"
	"github.com/tyk-swe/olp/internal/mcp/upstream"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
)

type credential struct {
	id       *string
	replaced bool
}

func Applier(servers []Server, bindings promotion.Bindings, prepared Prepared, keys Keys) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, p access.Principal) error {
		if len(servers) == 0 {
			return nil
		}
		directory, err := project.ReadDirectory(ctx, tx)
		if err != nil {
			return err
		}
		changed, err := promotion.TryMap(servers, func(server Server) (bool, error) {
			return apply(ctx, tx, p, directory, bindings, prepared, keys, server)
		})
		if err != nil || !slices.Contains(changed, true) {
			return err
		}
		_, err = access.AdvanceAuthority(ctx, tx)
		return err
	}
}

func apply(ctx context.Context, tx pgx.Tx, p access.Principal, directory project.Directory, bindings promotion.Bindings, prepared Prepared, keys Keys, server Server) (bool, error) {
	projectID, ok := directory.ID(server.Project)
	if !ok {
		return false, fmt.Errorf("mcp server project %q is missing", server.Project)
	}
	if err := p.Project(&projectID, access.Change); err != nil {
		return false, err
	}
	existing, err := find(ctx, tx, projectID, server)
	if err != nil {
		return false, err
	}
	certified, ok := prepared.certifications[server.key()]
	if !ok {
		return false, access.Invalid("mcp_servers", "Prepare certified registrations before applying.")
	}
	if etagOf(existing) != certified.observed {
		return false, access.Fail(412, "mcp_registration_changed", "The destination MCP registration changed during certification.")
	}
	switch outcome := certified.outcome.(type) {
	case limited:
		return true, registration.UpdateLimits(ctx, tx, existing.ID, server.Limits, access.NewID())
	case recertified:
		return true, save(ctx, tx, p, bindings, keys, projectID, existing, server, outcome.catalog)
	}
	return false, nil
}

func save(ctx context.Context, tx pgx.Tx, p access.Principal, bindings promotion.Bindings, keys Keys, projectID string, existing *registration.Definition, server Server, catalog *upstream.Catalog) error {
	flat := flatten(server.Transport)
	bearer, err := credentialOf(ctx, tx, bindings, keys, flat, existing)
	if err != nil {
		return err
	}
	retainEntra := registration.CanRetainEntraClientSecret(existing, flat.oauth)
	next := registration.Definition{
		EntraClientSecretID: retained(existing, retainEntra, func(d registration.Definition) *string { return d.EntraClientSecretID }),
		DelegationKeyID:     retained(existing, flat.delegation != nil, func(d registration.Definition) *string { return d.DelegationKeyID }),
		Delegation:          flat.delegation, OAuth: flat.oauth, Stdio: flat.stdio,
		ProjectID: projectID, Name: server.Name, Transport: string(flat.transport), Endpoint: flat.endpoint, Enabled: server.Enabled,
		Catalog: catalog, UnitPrice: server.UnitPrice, ToolPrices: server.ToolPrices, CredentialID: bearer.id,
		CallsPerMinute: server.Limits.CallsPerMinute, MaxConcurrency: server.Limits.MaxConcurrency,
		PerKeyCallsPerMinute: server.Limits.PerKeyCallsPerMinute, PerKeyMaxConcurrency: server.Limits.PerKeyMaxConcurrency,
	}
	if _, err := registration.Save(ctx, tx, p.UserID(), existing, next); err != nil {
		return err
	}
	return registration.DeleteReplacedCredentials(ctx, tx, existing, registration.Replaced{Credential: bearer.replaced, Delegation: flat.delegation == nil, EntraClient: !retainEntra})
}

func credentialOf(ctx context.Context, tx pgx.Tx, bindings promotion.Bindings, keys Keys, flat wire, existing *registration.Definition) (credential, error) {
	if flat.bearer == nil {
		return credential{replaced: true}, nil
	}
	sealed, bound := bindings[*flat.bearer].(promotion.Sealed)
	if !bound {
		return credential{id: retained(existing, true, func(d registration.Definition) *string { return d.CredentialID })}, nil
	}
	id := access.NewID()
	if err := keys.store(ctx, tx, id, sealed.Secret); err != nil {
		return credential{}, err
	}
	return credential{id: &id, replaced: true}, nil
}

func retained(existing *registration.Definition, keep bool, id func(registration.Definition) *string) *string {
	if existing == nil || !keep {
		return nil
	}
	return id(*existing)
}

func etagOf(existing *registration.Definition) string {
	if existing == nil {
		return ""
	}
	return existing.ETag
}
