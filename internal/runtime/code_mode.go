package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
)

func compileCodeMode(ctx context.Context, tx pgx.Tx, s *Snapshot) error {
	rows, err := tx.Query(ctx, `SELECT r.slug,v.document,v.connections FROM olp.code_routes r JOIN olp.code_route_revisions v ON v.id=r.latest_revision_id ORDER BY r.slug`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var slug string
		var document, encoded []byte
		var connections map[string]Configuration
		var route codemode.Route
		if err = rows.Scan(&slug, &document, &encoded); err != nil {
			return err
		}
		if err = json.Unmarshal(document, &route); err != nil {
			return err
		}
		if s.CodeRoutes == nil {
			s.CodeRoutes = map[string]codemode.Route{}
		}
		s.CodeRoutes[slug] = route
		if err = json.Unmarshal(encoded, &connections); err != nil {
			return err
		}
		if s.CodeConnections == nil {
			s.CodeConnections = map[string]Configuration{}
		}
		for id, config := range connections {
			s.CodeConnections[route.RevisionID+":"+id] = config
		}
	}
	return rows.Err()
}

func (s *Snapshot) CodeConnection(route codemode.Route, providerID string) (Configuration, bool) {
	configuration, ok := s.CodeConnections[route.RevisionID+":"+providerID]
	return configuration, ok
}

// CodeAdapter returns the adapter of a published route, derived from the
// connections its revision froze. It is "" when they name no single adapter,
// such as a revision that mixed adapters before publication refused that.
func (s *Snapshot) CodeAdapter(route codemode.Route) codemode.Adapter {
	var connections []codeadapter.Connection
	for key, c := range s.CodeConnections {
		if revision, _, _ := strings.Cut(key, ":"); revision == route.RevisionID {
			connections = append(connections, codeadapter.Connection{Kind: c.Kind, AuthMode: c.AuthMode, ProfileID: c.ProfileID})
		}
	}
	adapter, err := codeadapter.Derive(connections)
	if err != nil {
		return ""
	}
	return adapter
}

func (s *Snapshot) validateCodeMode() error {
	for slug, route := range s.CodeRoutes {
		if route.Slug != slug || route.ID == "" || route.ProjectID == "" || route.PoolID == "" || route.RevisionID == "" || route.Revision < 1 || route.PublishedAt == nil {
			return fmt.Errorf("invalid code route %s", slug)
		}
		if err := codemode.ValidateModels(route.Models); err != nil {
			return err
		}
		if _, exists := s.Routes[slug]; exists {
			return fmt.Errorf("code route slug conflicts with ordinary route %s", slug)
		}
	}
	return nil
}
