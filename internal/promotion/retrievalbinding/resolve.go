package retrievalbinding

import (
	"context"
	"errors"
	"slices"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/retrieval"
	"github.com/tyk-swe/olp/internal/routes"
)

var errProjectAbsent = errors.New("retrieval binding project is absent")

type installed struct {
	projects  promotion.Directory
	providers promotion.Directory
}

func readInstalled(ctx context.Context, q access.Queryer) (installed, error) {
	directory, err := project.ReadDirectory(ctx, q)
	if err != nil {
		return installed{}, err
	}
	names, err := providers.ReadProviderNames(ctx, q)
	if err != nil {
		return installed{}, err
	}
	return installed{projects: directory, providers: promotion.DirectoryOf(names)}, nil
}

func resolve(ctx context.Context, q access.Queryer, lookup installed, binding Binding) (retrieval.Input, error) {
	projectID, ok := lookup.projects.ID(binding.Project)
	if !ok {
		return retrieval.Input{}, errProjectAbsent
	}
	targets, err := targetIDs(ctx, q, lookup, binding, projectID)
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

func targetIDs(ctx context.Context, q access.Queryer, lookup installed, binding Binding, projectID string) ([]string, error) {
	providerID, ok := lookup.providers.ID(binding.TargetProvider)
	if !ok {
		return nil, nil
	}
	return routes.CurrentTargetIDs(ctx, q, binding.Route, projectID, providerID, binding.Resource)
}

func keyIDs(ctx context.Context, q access.Queryer, projectID string, names []string) ([]string, error) {
	return promotion.TryMap(names, func(name string) (string, error) {
		ids, err := usableKeys(ctx, q, projectID, name)
		if err != nil {
			return "", err
		}
		if len(ids) != 1 {
			return "", access.Invalid("retrieval_bindings.key_grants", "Each destination grant must resolve to exactly one active API key with the exact project-local name.")
		}
		return ids[0].ID, nil
	})
}

func usableKeys(ctx context.Context, q access.Queryer, projectID, name string) ([]access.NamedKey, error) {
	keys, err := access.ReadKeysNamed(ctx, q, &projectID, name)
	if err != nil {
		return nil, err
	}
	return promotion.Filter(keys, func(key access.NamedKey) bool { return !key.Expired }), nil
}
