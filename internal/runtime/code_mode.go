package runtime

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/codemode"
)

func compileCodeMode(ctx context.Context, tx pgx.Tx, s *Snapshot) error {
	rows, err := tx.Query(ctx, `SELECT r.slug,v.document FROM olp.code_routes r JOIN olp.code_route_revisions v ON v.id=r.latest_revision_id ORDER BY r.slug`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var slug string
		var document []byte
		var route codemode.Route
		if err = rows.Scan(&slug, &document); err != nil {
			rows.Close()
			return err
		}
		if err = json.Unmarshal(document, &route); err != nil {
			rows.Close()
			return err
		}
		if s.CodeRoutes == nil {
			s.CodeRoutes = map[string]codemode.Route{}
		}
		s.CodeRoutes[slug] = route
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	rows, err = tx.Query(ctx, `SELECT p.id::text,p.configuration FROM olp.providers p WHERE EXISTS(SELECT 1 FROM olp.code_accounts a WHERE a.provider_id=p.id) ORDER BY p.id::text`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var encoded []byte
		var config Configuration
		if err = rows.Scan(&id, &encoded); err != nil {
			return err
		}
		if err = json.Unmarshal(encoded, &config); err != nil {
			return err
		}
		if s.CodeConnections == nil {
			s.CodeConnections = map[string]Configuration{}
		}
		s.CodeConnections[id] = config
	}
	return rows.Err()
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
