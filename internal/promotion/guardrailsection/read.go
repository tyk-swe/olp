package guardrail

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion/project"
)

type named struct{ id, name string }

type revisionRow struct {
	named
	active bool
}

type draftRow struct {
	slug, revision string
	position       int
}

type legacyRow struct {
	slug, project string
	policy        json.RawMessage
	overallMS     int64
}

type slot struct {
	slug  string
	order int
}

func readStored(ctx context.Context, q access.Queryer) (Section, error) {
	active, err := query(ctx, q, func(row pgx.CollectableRow) (Guardrail, error) {
		var g Guardrail
		err := row.Scan(&g.Name, &g.Project, &g.Type, &g.Configuration, &g.TimeoutMS, &g.OnError)
		return g, err
	}, "SELECT g.name,COALESCE(p.name,''),g.type,v.configuration,v.timeout_ms,v.on_error FROM olp.guardrails g LEFT JOIN olp.projects p ON p.id=g.project_id JOIN olp.guardrail_revisions v ON v.id=g.active_revision_id WHERE g.retired_at IS NULL AND g.managed_route_revision_id IS NULL")
	if err != nil {
		return Section{}, err
	}
	registry, err := guardrails.ReadRegistry(ctx, q)
	if err != nil {
		return Section{}, err
	}
	platform, err := readPlatform(ctx, q, registry)
	if err != nil {
		return Section{}, err
	}
	definitions, err := collect(slices.Concat(active, platform.Guardrails), func(g Guardrail) (Guardrail, error) {
		configuration, err := rewriteCredentials(g.Type, g.Configuration, credentialName(ctx, q))
		return Guardrail{Project: g.Project, Name: g.Name, Type: g.Type, Configuration: configuration, TimeoutMS: g.TimeoutMS, OnError: g.OnError}, err
	})
	return Section{Guardrails: definitions, Policies: platform.Policies, Attachments: platform.Attachments}, err
}

func readPlatform(ctx context.Context, q access.Queryer, registry *guardrails.Registry) (Section, error) {
	if registry == nil {
		return Section{}, nil
	}
	directory, err := project.ReadDirectory(ctx, q)
	if err != nil {
		return Section{}, err
	}
	projectName := func(id string) string { name, _ := directory.Name(id); return name }
	definitionNames, err := query(ctx, q, scanNamed, "SELECT active_revision_id::text,name FROM olp.guardrails WHERE retired_at IS NULL AND managed_route_revision_id IS NULL AND active_revision_id IS NOT NULL")
	if err != nil {
		return Section{}, err
	}
	revisions, err := query(ctx, q, func(row pgx.CollectableRow) (revisionRow, error) {
		var r revisionRow
		err := row.Scan(&r.id, &r.name, &r.active)
		return r, err
	}, `SELECT v.id::text,p.name,(v.id=p.active_revision_id) FROM olp.guardrail_policies p JOIN olp.guardrail_policy_revisions v ON v.policy_id=p.id
 WHERE p.managed_route_revision_id IS NULL AND (v.id=p.active_revision_id OR v.id IN(SELECT policy_revision_id FROM olp.guardrail_attachments UNION SELECT policy_revision_id FROM olp.guardrail_draft_attachments))`)
	if err != nil {
		return Section{}, err
	}
	policyNames, err := collect(revisions, func(r revisionRow) (named, error) { return policyName(registry, r) })
	if err != nil {
		return Section{}, err
	}
	policies := indexed(policyNames, func(n named) (string, string) { return n.id, n.name })
	ids := slices.Sorted(maps.Keys(policies))
	active := indexed(definitionNames, func(n named) (string, string) { return n.id, n.name })
	pinned := indexed(kept(slices.Concat(mapped(ids, func(id string) []string {
		return mapped(registry.Policies[id].Entries, func(e guardrails.Entry) string { return e.RevisionID })
	})...), func(revision string) bool { _, ok := active[revision]; return !ok }), func(revision string) (string, string) {
		return revision, pinnedName(registry.Definitions[revision])
	})
	definitionName := func(revision string) string { return cmp.Or(active[revision], pinned[revision]) }
	reference := func(revision string) PolicyRef {
		return PolicyRef{Project: projectName(registry.Policies[revision].ProjectID), Name: policies[revision]}
	}
	attachments, err := collect(kept(registry.Attachments, func(a guardrails.Attachment) bool { _, ok := policies[a.PolicyRevisionID]; return ok }), func(a guardrails.Attachment) (Attachment, error) {
		return readAttachment(ctx, q, projectName, reference(a.PolicyRevisionID), a)
	})
	if err != nil {
		return Section{}, err
	}
	staged, err := readDrafts(ctx, q, policies, reference)
	if err != nil {
		return Section{}, err
	}
	return Section{
		Guardrails: mapped(slices.Sorted(maps.Keys(pinned)), func(revision string) Guardrail {
			d := registry.Definitions[revision]
			return Guardrail{Project: projectName(d.ProjectID), Name: pinned[revision], Type: d.Type, Configuration: d.Configuration, TimeoutMS: d.TimeoutMS, OnError: d.OnError}
		}),
		Policies: mapped(ids, func(id string) Policy {
			p := registry.Policies[id]
			return Policy{Project: projectName(p.ProjectID), Name: policies[id], Entries: mapped(p.Entries, func(e guardrails.Entry) Entry {
				return Entry{Guardrail: definitionName(e.RevisionID), Phase: e.Phase, Action: e.Action, Monitor: e.Monitor, Mandatory: e.Mandatory}
			})}
		}),
		Attachments: slices.Concat(kept(attachments, func(a Attachment) bool { _, replaced := staged[routeSlot(a)]; return !replaced }), slices.Collect(maps.Values(staged))),
	}, nil
}

func policyName(registry *guardrails.Registry, r revisionRow) (named, error) {
	if r.active {
		return r.named, nil
	}
	type portableRule struct {
		Type                   string
		Config                 json.RawMessage
		Timeout                int64
		OnError, Phase, Action string
		Monitor, Mandatory     bool
	}
	content, err := json.Marshal(mapped(registry.Policies[r.id].Entries, func(e guardrails.Entry) portableRule {
		d := registry.Definitions[e.RevisionID]
		return portableRule{d.Type, d.Configuration, d.TimeoutMS, d.OnError, e.Phase, e.Action, e.Monitor, e.Mandatory}
	}))
	if err != nil {
		return named{}, err
	}
	sum := sha256.Sum256(content)
	return named{r.id, bounded(r.name + ".revision." + hex.EncodeToString(sum[:6]))}, nil
}

func pinnedName(d guardrails.Revision) string {
	sum := sha256.Sum256(slices.Concat([]byte(d.Type+d.OnError+strconv.FormatInt(d.TimeoutMS, 10)), d.Configuration))
	return bounded(d.Name + ".revision." + hex.EncodeToString(sum[:6]))
}

func readAttachment(ctx context.Context, q access.Queryer, projectName func(string) string, policy PolicyRef, a guardrails.Attachment) (Attachment, error) {
	name, owner, err := scopeName(ctx, q, projectName, a)
	if err != nil {
		return Attachment{}, err
	}
	scope, _ := parseScope(contract.ConfigurationGuardrailAttachmentScope(a.Scope), name, owner)
	return Attachment{Scope: scope, Policy: policy, Order: a.Order}, nil
}

func scopeName(ctx context.Context, q access.Queryer, projectName func(string) string, a guardrails.Attachment) (string, string, error) {
	text := func(sql string) (string, string, error) {
		name, err := scanText(q.QueryRow(ctx, sql, a.ScopeID))
		return name, "", err
	}
	switch contract.ConfigurationGuardrailAttachmentScope(a.Scope) {
	case contract.ConfigurationGuardrailAttachmentScopeProject:
		return projectName(a.ScopeID), "", nil
	case contract.ConfigurationGuardrailAttachmentScopeOrganization:
		return text("SELECT name FROM olp.organizations WHERE id=$1")
	case contract.ConfigurationGuardrailAttachmentScopeRoute:
		return text("SELECT slug FROM olp.routes WHERE id=$1")
	case contract.ConfigurationGuardrailAttachmentScopeToolset:
		return text("SELECT name FROM olp.agent_publications WHERE id=$1 AND kind='toolset'")
	case contract.ConfigurationGuardrailAttachmentScopeApiKey:
		var name, owner string
		err := q.QueryRow(ctx, "SELECT name,COALESCE(project_id::text,'installation') FROM olp.api_keys WHERE id=$1", a.ScopeID).Scan(&name, &owner)
		return name, projectName(owner), err
	}
	return "", "", nil
}

func readDrafts(ctx context.Context, q access.Queryer, policies map[string]string, reference func(string) PolicyRef) (map[slot]Attachment, error) {
	drafts, err := query(ctx, q, func(row pgx.CollectableRow) (draftRow, error) {
		var d draftRow
		err := row.Scan(&d.slug, &d.revision, &d.position)
		return d, err
	}, "SELECT d.slug,a.policy_revision_id::text,a.position FROM olp.guardrail_draft_attachments a JOIN olp.route_drafts d ON d.id=a.draft_id ORDER BY d.updated_at,a.position")
	if err != nil {
		return nil, err
	}
	staged, err := collect(drafts, func(d draftRow) (Attachment, error) {
		if _, ok := policies[d.revision]; !ok {
			return Attachment{}, access.Invalid("guardrails", "A staged policy is unavailable.")
		}
		return Attachment{Scope: Route{d.slug}, Policy: reference(d.revision), Order: d.position}, nil
	})
	return indexed(staged, func(a Attachment) (slot, Attachment) { return routeSlot(a), a }), err
}

func routeSlot(a Attachment) slot {
	route, ok := a.Scope.(Route)
	if !ok {
		return slot{order: -1}
	}
	return slot{route.Slug, a.Order}
}

func readLegacyRoutes(ctx context.Context, q access.Queryer) ([]Section, error) {
	routes, err := query(ctx, q, func(row pgx.CollectableRow) (legacyRow, error) {
		var r legacyRow
		err := row.Scan(&r.slug, &r.project, &r.policy, &r.overallMS)
		return r, err
	}, "SELECT r.slug,COALESCE(pr.name,''),v.content_policy,v.overall_timeout_ms FROM olp.routes r JOIN olp.route_revisions v ON v.id=r.latest_revision_id LEFT JOIN olp.projects pr ON pr.id=r.project_id WHERE v.content_policy IS NOT NULL AND v.content_policy<>'null'::jsonb ORDER BY r.slug")
	if err != nil {
		return nil, err
	}
	return collect(routes, func(r legacyRow) (Section, error) {
		c, err := legacyRoute(r.slug, r.project, r.policy, r.overallMS)
		if err != nil {
			return Section{}, err
		}
		return parseConverted(c)
	})
}

func query[T any](ctx context.Context, q access.Queryer, scan pgx.RowToFunc[T], sql string, args ...any) ([]T, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scan)
}

func scanNamed(row pgx.CollectableRow) (named, error) {
	var n named
	err := row.Scan(&n.id, &n.name)
	return n, err
}
