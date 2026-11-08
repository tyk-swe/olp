package retrievalbinding

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/promotion/project"
	"github.com/tyk-swe/olp/internal/retrieval"
)

const keyNameQuery = "SELECT name FROM olp.api_keys WHERE id=$1 AND project_id=$2 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>now())"

func Read(ctx context.Context, q access.Queryer, directory project.Directory) ([]Binding, error) {
	stored, err := retrieval.List(ctx, q)
	if err != nil {
		return nil, err
	}
	return collect(stored, func(binding retrieval.Binding) (Binding, error) {
		return exported(ctx, q, directory, binding)
	})
}

func exported(ctx context.Context, q access.Queryer, directory project.Directory, binding retrieval.Binding) (Binding, error) {
	projectName, ok := directory.Name(binding.ProjectID)
	if !ok {
		return Binding{}, pgx.ErrNoRows
	}
	var provider string
	if err := q.QueryRow(ctx, "SELECT name FROM olp.providers WHERE id=$1", binding.ProviderID).Scan(&provider); err != nil {
		return Binding{}, err
	}
	keys, err := collect(binding.KeyGrants, func(id string) (string, error) {
		var name string
		err := q.QueryRow(ctx, keyNameQuery, id, binding.ProjectID).Scan(&name)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", access.Invalid("retrieval_bindings.key_grants", "Remove inactive API-key grants before exporting retrieval bindings.")
		}
		return name, err
	})
	if err != nil {
		return Binding{}, err
	}
	return Binding{
		Project:        projectName,
		Name:           binding.Name,
		Route:          binding.Route,
		TargetProvider: provider,
		Backend:        binding.Backend,
		Resource:       binding.Resource,
		Fields:         binding.Fields,
		KeyGrants:      keys,
		Enabled:        binding.Enabled,
	}, nil
}
