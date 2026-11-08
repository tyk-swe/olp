package mcpserver

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/mcp/registration"
	"github.com/tyk-swe/olp/internal/promotion/project"
	"github.com/tyk-swe/olp/internal/routing"
)

func Read(ctx context.Context, q access.Queryer, directory project.Directory) ([]Server, error) {
	active, err := registration.ListActive(ctx, q)
	if err != nil {
		return nil, err
	}
	return collect(active, func(_ int, definition registration.Definition) (Server, error) {
		name, ok := directory.Name(definition.ProjectID)
		if !ok {
			return Server{}, pgx.ErrNoRows
		}
		return fromDefinition(definition, name)
	})
}

func fromDefinition(definition registration.Definition, projectName string) (Server, error) {
	ref, err := refOf(keyOf(projectName, definition.Name))
	if err != nil {
		return Server{}, err
	}
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
	}, nil
}

func current(active []registration.Definition, directory project.Directory, server Server) *registration.Definition {
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
