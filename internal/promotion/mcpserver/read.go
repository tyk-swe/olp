package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/mcp/registration"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/routing"
)

func Read(ctx context.Context, q access.Queryer, directory promotion.Directory) ([]Server, error) {
	active, err := registration.ListActive(ctx, q)
	if err != nil {
		return nil, err
	}
	return promotion.TryMap(active, func(definition registration.Definition) (Server, error) {
		name, ok := directory.Name(definition.ProjectID)
		if !ok {
			return Server{}, fmt.Errorf("mcp server %s belongs to an unknown project", definition.ID)
		}
		return fromDefinition(definition, name), nil
	})
}

func fromDefinition(definition registration.Definition, projectName string) Server {
	ref := refOf(keyOf(projectName, definition.Name))
	return Server{
		Project: projectName, Name: definition.Name, Enabled: definition.Enabled, CatalogDigest: definition.Catalog.Digest,
		Transport: transportOf(wire{
			transport: routing.MCPTransport(definition.Transport), endpoint: definition.Endpoint, stdio: definition.Stdio,
			oauth: definition.OAuth, delegation: definition.Delegation, bearer: bearerOf(definition.HasCredential, ref),
		}),
		UnitPrice: definition.UnitPrice, ToolPrices: nonNil(definition.ToolPrices),
		Limits: routing.ToolLimits{
			CallsPerMinute: definition.CallsPerMinute, MaxConcurrency: definition.MaxConcurrency,
			PerKeyCallsPerMinute: definition.PerKeyCallsPerMinute, PerKeyMaxConcurrency: definition.PerKeyMaxConcurrency,
		},
	}
}

func current(active []registration.Definition, directory promotion.Directory, server Server) *registration.Definition {
	projectID, ok := directory.ID(server.Project)
	if !ok {
		return nil
	}
	for _, definition := range active {
		if definition.ProjectID == projectID && strings.EqualFold(definition.Name, server.Name) {
			return &definition
		}
	}
	return nil
}

func find(ctx context.Context, tx pgx.Tx, projectID string, server Server) (*registration.Definition, error) {
	definition, err := registration.FindActive(ctx, tx, projectID, server.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return definition, err
}
