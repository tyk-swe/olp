package retrievalbinding

import (
	"context"
	"fmt"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/retrieval"
)

func Read(ctx context.Context, q access.Queryer, directory project.Directory) ([]Binding, error) {
	stored, err := retrieval.List(ctx, q)
	if err != nil {
		return nil, err
	}
	named, err := providers.ReadProviderNames(ctx, q)
	if err != nil {
		return nil, err
	}
	providerNames := promotion.Associate(named, func(p providers.ProviderName) (string, string) { return p.ID, p.Name })
	return promotion.TryMap(stored, func(binding retrieval.Binding) (Binding, error) {
		return exported(ctx, q, directory, providerNames, binding)
	})
}

func exported(ctx context.Context, q access.Queryer, directory project.Directory, providerNames map[string]string, binding retrieval.Binding) (Binding, error) {
	projectName, ok := directory.Name(binding.ProjectID)
	if !ok {
		return Binding{}, fmt.Errorf("retrieval binding %s belongs to an unknown project", binding.ID)
	}
	provider, ok := providerNames[binding.ProviderID]
	if !ok {
		return Binding{}, fmt.Errorf("retrieval binding %s targets an unknown provider", binding.ID)
	}
	keys, err := promotion.TryMap(binding.KeyGrants, func(id string) (string, error) {
		name, active, err := access.ActiveKeyName(ctx, q, id, binding.ProjectID)
		if err != nil {
			return "", err
		}
		if !active {
			return "", access.Invalid("retrieval_bindings.key_grants", "Remove inactive API-key grants before exporting retrieval bindings.")
		}
		return name, nil
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
