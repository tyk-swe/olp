package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
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

// codeRevision is what a published revision's frozen connections serve.
type codeRevision struct {
	adapters  []codemode.Adapter
	providers map[string][]string
}

// CodeAdapters returns the adapters of a published route's frozen
// connections in table order. A route's pool may mix them.
func (s *Snapshot) CodeAdapters(route codemode.Route) []codemode.Adapter {
	return s.codeRevisions[route.RevisionID].adapters
}

// CodeProviders returns the providers of a published route's frozen
// connections whose adapter serves a client path, ordered by ID. The returned
// slice is shared and must not be modified.
func (s *Snapshot) CodeProviders(route codemode.Route, path string) []string {
	return s.codeRevisions[route.RevisionID].providers[path]
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
	// Connections no adapter accepts serve nothing.
	s.codeRevisions = make(map[string]codeRevision)
	for key, c := range s.CodeConnections {
		vendor, ok := codeadapter.ForConnection(c.Kind, c.AuthMode, c.ProfileID)
		if !ok {
			continue
		}
		id, provider, _ := strings.Cut(key, ":")
		revision := s.codeRevisions[id]
		if !slices.Contains(revision.adapters, vendor.Adapter) {
			revision.adapters = append(revision.adapters, vendor.Adapter)
		}
		if revision.providers == nil {
			revision.providers = make(map[string][]string, len(vendor.Paths))
		}
		for path := range vendor.Paths {
			revision.providers[path] = append(revision.providers[path], provider)
		}
		s.codeRevisions[id] = revision
	}
	for _, revision := range s.codeRevisions {
		slices.SortFunc(revision.adapters, codeadapter.Compare)
		for _, providers := range revision.providers {
			slices.Sort(providers)
		}
	}
	return nil
}
