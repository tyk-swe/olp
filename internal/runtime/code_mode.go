package runtime

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/routing"
)

func compileCodeMode(ctx context.Context, tx pgx.Tx, s *routing.Snapshot) error {
	rows, err := tx.Query(ctx, `SELECT r.slug,v.document,v.connections FROM olp.code_routes r JOIN olp.code_route_revisions v ON v.id=r.latest_revision_id ORDER BY r.slug`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var slug string
		var document, encoded []byte
		var connections map[string]routing.Configuration
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
			s.CodeConnections = map[string]routing.Configuration{}
		}
		for id, config := range connections {
			s.CodeConnections[route.RevisionID+":"+id] = config
		}
	}
	return rows.Err()
}
