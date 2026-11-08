package retrievalbinding

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
	"github.com/tyk-swe/olp/internal/retrieval"
)

var errProjectAbsent = errors.New("retrieval binding project is absent")

const targetQuery = `SELECT DISTINCT t->>'id' FROM (
 SELECT d.targets,0 AS priority FROM olp.route_drafts d WHERE d.slug=$1 AND d.project_id=$2 AND d.state='draft'
 UNION ALL SELECT v.targets,1 AS priority FROM olp.routes r JOIN olp.route_revisions v ON v.id=r.latest_revision_id WHERE r.slug=$1 AND r.project_id=$2 AND r.state='active'
 ) r CROSS JOIN LATERAL jsonb_array_elements(r.targets) t JOIN olp.providers p ON p.id=(t->>'provider_id')::uuid
 WHERE lower(p.name)=lower($3) AND t->>'provider_model'=$4 AND r.priority=(SELECT min(priority) FROM (
 SELECT 0 AS priority FROM olp.route_drafts d WHERE d.slug=$1 AND d.project_id=$2 AND d.state='draft'
 UNION ALL SELECT 1 FROM olp.routes r WHERE r.slug=$1 AND r.project_id=$2 AND r.state='active') priorities)`

const activeKeysQuery = "SELECT id::text FROM olp.api_keys WHERE project_id=$1 AND name=$2 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>now())"

func resolve(ctx context.Context, q access.Queryer, directory project.Directory, binding Binding) (retrieval.Input, error) {
	projectID, ok := directory.ID(binding.Project)
	if !ok {
		return retrieval.Input{}, errProjectAbsent
	}
	targets, err := promotion.Rows(ctx, q, pgx.RowTo[string], targetQuery, binding.Route, projectID, binding.TargetProvider, binding.Resource)
	if err != nil {
		return retrieval.Input{}, err
	}
	if len(targets) != 1 {
		return retrieval.Input{}, access.Invalid("retrieval_bindings.target_provider", "The destination route must contain exactly one matching provider/resource target.")
	}
	keys, err := keyIDs(ctx, q, projectID, binding.KeyGrants)
	if err != nil {
		return retrieval.Input{}, err
	}
	return retrieval.Input{
		ProjectID: projectID,
		Name:      binding.Name,
		Route:     binding.Route,
		TargetID:  targets[0],
		Backend:   binding.Backend,
		Resource:  binding.Resource,
		Fields:    binding.Fields,
		KeyGrants: append([]string{}, slices.Sorted(slices.Values(keys))...),
		Enabled:   binding.Enabled,
	}, nil
}

func keyIDs(ctx context.Context, q access.Queryer, projectID string, names []string) ([]string, error) {
	return promotion.TryMap(names, func(name string) (string, error) {
		ids, err := promotion.Rows(ctx, q, pgx.RowTo[string], activeKeysQuery, projectID, name)
		if err != nil {
			return "", err
		}
		if len(ids) != 1 {
			return "", access.Invalid("retrieval_bindings.key_grants", "Each destination grant must resolve to exactly one active API key with the exact project-local name.")
		}
		return ids[0], nil
	})
}

func activeKeyCount(ctx context.Context, q access.Queryer, projectID, name string) (int, error) {
	ids, err := promotion.Rows(ctx, q, pgx.RowTo[string], activeKeysQuery, projectID, name)
	return len(ids), err
}
