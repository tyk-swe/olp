package identity

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
	"github.com/tyk-swe/olp/internal/workload"
)

const (
	placeholderProject = "00000000-0000-0000-0000-000000000001"
	selectIssuer       = "SELECT id::text,document,etag::text FROM olp.workload_issuers WHERE issuer=$1"
	unknownProject     = "Every mapping must reference a declared or existing project."
)

type Prepared struct{ issuers []preparedIssuer }

type preparedIssuer struct {
	entry contract.ConfigurationWorkloadIssuer
	etag  string
}

type storedIssuer struct {
	id, etag string
	config   workload.Config
}

type catalog struct{ templates, groups map[string]bool }

func parseWorkload(fragment nullable.Nullable[[]contract.ConfigurationWorkloadIssuer]) ([]contract.ConfigurationWorkloadIssuer, error) {
	entries, _ := promotion.Present(fragment)
	if len(entries) > 64 {
		return nil, access.Invalid("workload_issuers", "Use at most 64 issuers.")
	}
	if !unique(entries, func(issuer contract.ConfigurationWorkloadIssuer) string { return issuer.Issuer }) {
		return nil, access.Invalid("workload_issuers", "Issuer URLs must be unique.")
	}
	issuers := mapped(entries, trimmedProjects)
	if err := firstError(issuers, validIssuer); err != nil {
		return nil, err
	}
	return issuers, nil
}

func trimmedProjects(issuer contract.ConfigurationWorkloadIssuer) contract.ConfigurationWorkloadIssuer {
	return contract.ConfigurationWorkloadIssuer{
		Algorithms: issuer.Algorithms, Audiences: issuer.Audiences, DisabledKeyIds: issuer.DisabledKeyIds, Enabled: issuer.Enabled,
		Issuer: issuer.Issuer, JwksUrl: issuer.JwksUrl, MaxLifetimeSeconds: issuer.MaxLifetimeSeconds, Name: issuer.Name,
		Mappings: mapped(issuer.Mappings, func(m contract.ConfigurationWorkloadMapping) contract.ConfigurationWorkloadMapping {
			return contract.ConfigurationWorkloadMapping{
				AgentPrincipal: m.AgentPrincipal, AllowProviderState: m.AllowProviderState, AllowedAgents: m.AllowedAgents,
				AllowedToolsets: m.AllowedToolsets, EndUserClaim: m.EndUserClaim, LimitTemplate: m.LimitTemplate, Match: m.Match,
				Name: m.Name, Project: strings.TrimSpace(m.Project), RouteGroups: m.RouteGroups, Scopes: m.Scopes,
			}
		}),
	}
}

func validIssuer(issuer contract.ConfigurationWorkloadIssuer) error {
	if err := firstError(issuer.Mappings, func(m contract.ConfigurationWorkloadMapping) error {
		return access.ValidText("workload_issuers", m.Project, 100)
	}); err != nil {
		return err
	}
	return access.ValidateWorkloadConfig(configOf(issuer, func(string) string { return placeholderProject }))
}

func readWorkload(ctx context.Context, q access.Queryer, directory project.Directory) ([]contract.ConfigurationWorkloadIssuer, error) {
	rows, err := q.Query(ctx, "SELECT document FROM olp.workload_issuers ORDER BY issuer")
	if err != nil {
		return nil, err
	}
	configs, err := pgx.CollectRows(rows, pgx.RowTo[workload.Config])
	if err != nil {
		return nil, err
	}
	return collected(configs, func(config workload.Config) (contract.ConfigurationWorkloadIssuer, error) {
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
	return collected(d.issuers, func(issuer contract.ConfigurationWorkloadIssuer) (promotion.Item, error) {
		if detail, found := issuerProblem(issuer, directory, catalogs, d.declared); found {
			return promotion.Conflict(contract.ConfigurationPlanItemKindWorkloadIssuer, issuer.Issuer, detail), nil
		}
		return planIssuer(ctx, q, issuer, configOf(issuer, func(name string) string {
			return cmp.Or(lookup(directory, name), placeholderProject)
		}))
	})
}

func planIssuer(ctx context.Context, q access.Queryer, issuer contract.ConfigurationWorkloadIssuer, config workload.Config) (promotion.Item, error) {
	current, exists, err := readIssuer(ctx, q, selectIssuer, issuer.Issuer)
	if err != nil {
		return promotion.Item{}, err
	}
	if !exists {
		return issuerItem(issuer, contract.ConfigurationPlanItemActionCreate), nil
	}
	bindings := access.ValidateWorkloadBindings(ctx, q, current.id, config)
	if problem, ok := errors.AsType[*access.Problem](bindings); ok {
		return promotion.Conflict(contract.ConfigurationPlanItemKindWorkloadIssuer, issuer.Issuer, problem.Detail), nil
	}
	if bindings != nil {
		return promotion.Item{}, bindings
	}
	return issuerItem(issuer, action(true, sameConfig(current.config, config))), nil
}

func issuerItem(issuer contract.ConfigurationWorkloadIssuer, act contract.ConfigurationPlanItemAction) promotion.Item {
	return promotion.Item{
		Kind:   contract.ConfigurationPlanItemKindWorkloadIssuer,
		Key:    issuer.Issuer,
		Action: act,
		Detail: "Issuer trust and claim mappings; destination workload principals are preserved.",
	}
}

func issuerProblem(issuer contract.ConfigurationWorkloadIssuer, directory project.Directory, catalogs map[string]catalog, declared map[string]contract.ConfigurationProjectEntry) (string, bool) {
	for _, mapping := range issuer.Mappings {
		if detail, found := mappingProblem(issuer.Enabled, mapping, directory, catalogs, declared); found {
			return detail, true
		}
	}
	return "", false
}

func mappingProblem(enabled bool, mapping contract.ConfigurationWorkloadMapping, directory project.Directory, catalogs map[string]catalog, declared map[string]contract.ConfigurationProjectEntry) (string, bool) {
	id, exists := directory.ID(mapping.Project)
	entry, isDeclared := declared[folded(mapping.Project)]
	if !exists && !isDeclared {
		return unknownProject, true
	}
	if !enabled {
		return "", false
	}
	available := effective(catalogs[id], entry, isDeclared)
	if !available.templates[mapping.LimitTemplate] {
		return "A mapping template is absent from its project.", true
	}
	if slices.ContainsFunc(mapping.RouteGroups, func(group string) bool { return !available.groups[group] }) {
		return "A mapping route group is absent from its project.", true
	}
	return "", false
}

func effective(stored catalog, entry contract.ConfigurationProjectEntry, declared bool) catalog {
	if !declared {
		return stored
	}
	groups, _ := promotion.Present(entry.RouteGroups)
	templates, ok := promotion.Present(entry.LimitTemplates)
	if !ok {
		return catalog{templates: stored.templates, groups: keys(groups)}
	}
	return catalog{templates: keys(templates), groups: keys(groups)}
}

func readCatalogs(ctx context.Context, q access.Queryer) (map[string]catalog, error) {
	rows, err := q.Query(ctx, "SELECT id::text,limit_templates,route_groups FROM olp.projects")
	if err != nil {
		return nil, err
	}
	type row struct {
		id        string
		templates contract.LimitTemplates
		groups    contract.RouteGroups
	}
	projects, err := pgx.CollectRows(rows, func(scanned pgx.CollectableRow) (row, error) {
		var r row
		err := scanned.Scan(&r.id, &r.templates, &r.groups)
		return r, err
	})
	if err != nil {
		return nil, err
	}
	return indexed(projects, func(r row) (string, catalog) {
		return r.id, catalog{templates: keys(r.templates), groups: keys(r.groups)}
	}), nil
}

func readIssuer(ctx context.Context, q access.Queryer, query, issuer string) (storedIssuer, bool, error) {
	var current storedIssuer
	err := q.QueryRow(ctx, query, issuer).Scan(&current.id, &current.config, &current.etag)
	if errors.Is(err, pgx.ErrNoRows) {
		return storedIssuer{}, false, nil
	}
	return current, err == nil, err
}

func (d Desired) NeedsPrepare() bool { return len(d.issuers) > 0 }

func Prepare(ctx context.Context, d Desired, deps Deps) (Prepared, error) {
	issuers, err := collected(d.issuers, func(issuer contract.ConfigurationWorkloadIssuer) (preparedIssuer, error) {
		return prepareIssuer(ctx, deps, issuer)
	})
	return Prepared{issuers: issuers}, err
}

func prepareIssuer(ctx context.Context, deps Deps, issuer contract.ConfigurationWorkloadIssuer) (preparedIssuer, error) {
	current, exists, err := readIssuer(ctx, deps.Pool, selectIssuer, issuer.Issuer)
	if err != nil {
		return preparedIssuer{}, err
	}
	prepared := preparedIssuer{entry: issuer, etag: current.etag}
	if !issuer.Enabled || exists && current.config.Enabled && current.config.JWKSURL == issuer.JwksUrl {
		return prepared, nil
	}
	if _, err := workload.Fetch(ctx, deps.Identity, issuer.JwksUrl); err != nil {
		return preparedIssuer{}, access.Fail(422, "workload_keys_unavailable", "The declared public signing keys could not be verified through identity egress.")
	}
	return prepared, nil
}

func WorkloadApplier(prepared Prepared, deps Deps) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, p access.Principal) error {
		if len(prepared.issuers) == 0 {
			return nil
		}
		directory, err := project.ReadDirectory(ctx, tx)
		if err != nil {
			return err
		}
		return firstError(prepared.issuers, func(issuer preparedIssuer) error {
			return applyIssuer(ctx, tx, p, deps, directory, issuer)
		})
	}
}

func applyIssuer(ctx context.Context, tx pgx.Tx, p access.Principal, deps Deps, directory project.Directory, issuer preparedIssuer) error {
	current, exists, err := readIssuer(ctx, tx, selectIssuer+" FOR UPDATE", issuer.entry.Issuer)
	if err != nil {
		return err
	}
	if current.etag != issuer.etag {
		return access.Fail(409, "configuration_changed", "Workload trust changed during endpoint validation; plan and retry.")
	}
	if slices.ContainsFunc(issuer.entry.Mappings, func(m contract.ConfigurationWorkloadMapping) bool { return lookup(directory, m.Project) == "" }) {
		return access.Invalid("workload_issuers", unknownProject)
	}
	config := configOf(issuer.entry, func(name string) string { return lookup(directory, name) })
	if exists && sameConfig(current.config, config) {
		return nil
	}
	if _, err := deps.Access.StoreWorkloadIssuer(ctx, tx, p, issuerID(current, exists), config, !exists); err != nil {
		return err
	}
	return nil
}

func issuerID(current storedIssuer, exists bool) string {
	if exists {
		return current.id
	}
	return access.NewID()
}

func lookup(directory project.Directory, name string) string {
	id, _ := directory.ID(name)
	return id
}
