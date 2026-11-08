package retrievalbinding

import (
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/retrieval"
)

type stage interface{ stage() }

type unstaged struct{}

type staged struct{ projectDeclared bool }

type misstaged struct{ detail string }

func (unstaged) stage()  {}
func (staged) stage()    {}
func (misstaged) stage() {}

func stageOf(document contract.ConfigurationDocument, names promotion.Names, binding Binding) stage {
	index := slices.IndexFunc(document.Routes, func(route contract.ConfigurationRouteEntry) bool { return route.Slug == binding.Route })
	if index < 0 {
		return unstaged{}
	}
	if !stagesTarget(document.Routes[index], binding) {
		return misstaged{detail: "The document must stage exactly one matching retrieval provider/resource target in the owning project."}
	}
	if slices.ContainsFunc(document.Providers, func(provider contract.ConfigurationProviderEntry) bool {
		return strings.EqualFold(provider.Name, binding.TargetProvider) && string(provider.Configuration.Kind) != retrieval.ProviderKind(binding.Backend)
	}) {
		return misstaged{detail: "The target provider kind does not match the retrieval backend."}
	}
	return staged{projectDeclared: names.Project(binding.Project)}
}

func stagesTarget(route contract.ConfigurationRouteEntry, binding Binding) bool {
	routeProject, scoped := promotion.Present(route.Project)
	matches := len(slices.DeleteFunc(slices.Clone(route.Targets), func(target contract.ConfigurationRouteTarget) bool {
		_, shadow := promotion.Present(target.Shadow)
		return !strings.EqualFold(target.Provider, binding.TargetProvider) || target.ProviderModel != binding.Resource || shadow
	}))
	retired := route.Retired != nil && *route.Retired
	return !retired && scoped && strings.EqualFold(routeProject, binding.Project) && slices.Contains(route.Operations, "retrieval") && matches == 1
}
