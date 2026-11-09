package identity

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/projects"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
	"github.com/tyk-swe/olp/internal/workload"
)

const (
	placeholderProject = "00000000-0000-0000-0000-000000000001"
	unknownProject     = "Every mapping must reference a declared or existing project."
)

type Prepared struct{ etags map[string]string }

type preparedIssuer struct{ issuer, etag string }

type catalog struct{ templates, groups []string }

func parseWorkload(fragment nullable.Nullable[[]contract.ConfigurationWorkloadIssuer]) ([]contract.ConfigurationWorkloadIssuer, error) {
	entries := fragment.GetOrEmpty()
	if len(entries) > 64 {
		return nil, access.Invalid("workload_issuers", "Use at most 64 issuers.")
	}
	if _, repeated := promotion.FirstRepeat(entries, func(issuer contract.ConfigurationWorkloadIssuer) string { return issuer.Issuer }); repeated {
		return nil, access.Invalid("workload_issuers", "Issuer URLs must be unique.")
	}
	if err := promotion.Every(entries, validIssuer); err != nil {
		return nil, err
	}
	return entries, nil
}

func validIssuer(issuer contract.ConfigurationWorkloadIssuer) error {
	if err := promotion.Every(issuer.Mappings, func(m contract.ConfigurationWorkloadMapping) error {
		return access.ValidText("workload_issuers", strings.TrimSpace(m.Project), 100)
	}); err != nil {
		return err
	}
	config, err := configOf(issuer, func(string) (string, error) { return placeholderProject, nil })
	if err != nil {
		return err
	}
	return access.ValidateWorkloadConfig(config)
}

func readWorkload(ctx context.Context, q access.Queryer, directory promotion.Directory) ([]contract.ConfigurationWorkloadIssuer, error) {
	configs, err := access.ReadWorkloadIssuerDocuments(ctx, q)
	if err != nil {
		return nil, err
	}
	return promotion.TryMap(configs, func(config workload.Config) (contract.ConfigurationWorkloadIssuer, error) {
		return entryOf(config, directory)
	})
}

func renderWorkload(issuers []contract.ConfigurationWorkloadIssuer) []contract.ConfigurationWorkloadIssuer {
	return slices.SortedFunc(slices.Values(issuers), func(a, b contract.ConfigurationWorkloadIssuer) int {
		return cmp.Compare(a.Issuer, b.Issuer)
	})
}

func planWorkload(ctx context.Context, q access.Queryer, d Desired) ([]promotion.Item, error) {
	if len(d.issuers) == 0 {
		return nil, nil
	}
	directory, err := project.ReadDirectory(ctx, q)
	if err != nil {
		return nil, err
	}
	catalogs, err := readCatalogs(ctx, q)
	if err != nil {
		return nil, err
	}
	return promotion.TryMap(d.issuers, func(issuer contract.ConfigurationWorkloadIssuer) (promotion.Item, error) {
		if detail, found := issuerProblem(issuer, directory, catalogs, d.declared); found {
			return promotion.Conflict(contract.ConfigurationPlanItemKindWorkloadIssuer, issuer.Issuer, detail), nil
		}
		config, err := configOf(issuer, func(name string) (string, error) {
			id, ok := directory.ID(name)
			if !ok {
				return placeholderProject, nil
			}
			return id, nil
		})
		if err != nil {
			return promotion.Item{}, err
		}
		return planIssuer(ctx, q, issuer, config)
	})
}

func planIssuer(ctx context.Context, q access.Queryer, issuer contract.ConfigurationWorkloadIssuer, config workload.Config) (promotion.Item, error) {
	current, exists, err := access.FindWorkloadIssuer(ctx, q, issuer.Issuer)
	if err != nil {
		return promotion.Item{}, err
	}
	if !exists {
		return issuerItem(issuer, contract.ConfigurationPlanItemActionCreate), nil
	}
	if err := access.ValidateWorkloadBindings(ctx, q, current.ID, config); err != nil {
		return promotion.Item{}, err
	}
	return issuerItem(issuer, action(true, sameConfig(current.Config, config))), nil
}

func issuerItem(issuer contract.ConfigurationWorkloadIssuer, act contract.ConfigurationPlanItemAction) promotion.Item {
	return promotion.Item{
		Kind:   contract.ConfigurationPlanItemKindWorkloadIssuer,
		Key:    issuer.Issuer,
		Action: act,
		Detail: "Issuer trust and claim mappings; destination workload principals are preserved.",
	}
}

func issuerProblem(issuer contract.ConfigurationWorkloadIssuer, directory promotion.Directory, catalogs map[string]catalog, declared map[string]contract.ConfigurationProjectEntry) (string, bool) {
	for _, mapping := range issuer.Mappings {
		if detail, found := mappingProblem(issuer.Enabled, mapping, directory, catalogs, declared); found {
			return detail, true
		}
	}
	return "", false
}

func mappingProblem(enabled bool, mapping contract.ConfigurationWorkloadMapping, directory promotion.Directory, catalogs map[string]catalog, declared map[string]contract.ConfigurationProjectEntry) (string, bool) {
	id, exists := directory.ID(mapping.Project)
	entry, isDeclared := declared[promotion.Fold(mapping.Project)]
	if !exists && !isDeclared {
		return unknownProject, true
	}
	if !enabled {
		return "", false
	}
	available := effective(catalogs[id], entry, isDeclared)
	if !slices.Contains(available.templates, mapping.LimitTemplate) {
		return "A mapping template is absent from its project.", true
	}
	if slices.ContainsFunc(mapping.RouteGroups, func(group string) bool { return !slices.Contains(available.groups, group) }) {
		return "A mapping route group is absent from its project.", true
	}
	return "", false
}

func effective(stored catalog, entry contract.ConfigurationProjectEntry, declared bool) catalog {
	if !declared {
		return stored
	}
	groups := slices.Collect(maps.Keys(entry.RouteGroups.GetOrEmpty()))
	templates, ok := promotion.Present(entry.LimitTemplates)
	if !ok {
		return catalog{templates: stored.templates, groups: groups}
	}
	return catalog{templates: slices.Collect(maps.Keys(templates)), groups: groups}
}

func readCatalogs(ctx context.Context, q access.Queryer) (map[string]catalog, error) {
	stored, err := projects.ReadProjectPolicies(ctx, q)
	if err != nil {
		return nil, err
	}
	return promotion.Associate(stored, func(project projects.ProjectPolicies) (string, catalog) {
		return project.ID, catalog{
			templates: slices.Collect(maps.Keys(project.Policies.Templates)),
			groups:    slices.Collect(maps.Keys(project.Policies.RouteGroups)),
		}
	}), nil
}

func (d Desired) NeedsPrepare() bool { return len(d.issuers) > 0 }

func Prepare(ctx context.Context, desired Desired, principal access.Principal, deps Deps) (Prepared, error) {
	if err := Authorize(desired, principal); err != nil {
		return Prepared{}, err
	}
	issuers, err := promotion.TryMap(desired.issuers, func(issuer contract.ConfigurationWorkloadIssuer) (preparedIssuer, error) {
		return prepareIssuer(ctx, deps, issuer)
	})
	if err != nil {
		return Prepared{}, err
	}
	return Prepared{etags: promotion.Associate(issuers, func(p preparedIssuer) (string, string) { return p.issuer, p.etag })}, nil
}

func prepareIssuer(ctx context.Context, deps Deps, issuer contract.ConfigurationWorkloadIssuer) (preparedIssuer, error) {
	current, exists, err := access.FindWorkloadIssuer(ctx, deps.Access.Pool, issuer.Issuer)
	if err != nil {
		return preparedIssuer{}, err
	}
	prepared := preparedIssuer{issuer: issuer.Issuer, etag: current.ETag}
	if !issuer.Enabled || exists && current.Config.Enabled && current.Config.JWKSURL == issuer.JwksUrl {
		return prepared, nil
	}
	if _, err := workload.Fetch(ctx, deps.Identity, issuer.JwksUrl); err != nil {
		return preparedIssuer{}, access.Fail(422, "workload_keys_unavailable", "The declared public signing keys could not be verified through identity egress.")
	}
	return prepared, nil
}

func WorkloadApplier(desired Desired, prepared Prepared, deps Deps) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, p access.Principal) error {
		if len(desired.issuers) == 0 {
			return nil
		}
		directory, err := project.ReadDirectory(ctx, tx)
		if err != nil {
			return err
		}
		return promotion.Every(desired.issuers, func(issuer contract.ConfigurationWorkloadIssuer) error {
			etag, ok := prepared.etags[issuer.Issuer]
			return applyIssuer(ctx, tx, p, deps, directory, issuer, etag, ok)
		})
	}
}

func applyIssuer(ctx context.Context, tx pgx.Tx, p access.Principal, deps Deps, directory promotion.Directory, issuer contract.ConfigurationWorkloadIssuer, etag string, prepared bool) error {
	current, exists, err := access.LockWorkloadIssuer(ctx, tx, issuer.Issuer)
	if err != nil {
		return err
	}
	if !prepared || current.ETag != etag {
		return access.Fail(409, "configuration_changed", "Workload trust changed during endpoint validation; plan and retry.")
	}
	config, err := configOf(issuer, func(name string) (string, error) {
		id, ok := directory.ID(name)
		if !ok {
			return "", access.Invalid("workload_issuers", unknownProject)
		}
		return id, nil
	})
	if err != nil {
		return err
	}
	if exists && sameConfig(current.Config, config) {
		return nil
	}
	_, stored := deps.Access.StoreWorkloadIssuer(ctx, tx, p, issuerID(current, exists), config, !exists)
	return stored
}

func issuerID(current access.StoredWorkloadIssuer, exists bool) string {
	if exists {
		return current.ID
	}
	return access.NewID()
}
