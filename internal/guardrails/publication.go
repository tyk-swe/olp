package guardrails

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
)

const revisionColumns = "v.id::text,g.id::text,COALESCE(g.project_id::text,'installation'),g.name,g.type,v.configuration,v.timeout_ms,v.on_error"

const legacyRuleColumn = "CASE WHEN g.managed_route_revision_id IS NOT NULL THEN v.configuration->'rules'->0->>'id' ELSE '' END"

type NamedPolicyRevision struct {
	ID     string
	Name   string
	Active bool
}

func (d *Revision) columns() []any {
	return []any{&d.ID, &d.GuardrailID, &d.ProjectID, &d.Name, &d.Type, &d.Configuration, &d.TimeoutMS, &d.OnError}
}

func scanRevision(row pgx.CollectableRow) (Revision, error) {
	var d Revision
	return d, row.Scan(d.columns()...)
}

func scanPublishedRevision(row pgx.CollectableRow) (Revision, error) {
	var d Revision
	return d, row.Scan(append(d.columns(), &d.LegacyRuleID)...)
}

func collect[T any](ctx context.Context, q access.Queryer, sql string, scan pgx.RowToFunc[T], args ...any) ([]T, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scan)
}

func index[T, V any](values []T, entry func(T) (string, V)) map[string]V {
	out := make(map[string]V, len(values))
	for _, value := range values {
		key, v := entry(value)
		out[key] = v
	}
	return out
}

func revisionEntry(d Revision) (string, Revision) { return d.ID, d }

func ReadRevisions(ctx context.Context, q access.Queryer, ids []string) (map[string]Revision, error) {
	revisions, err := collect(ctx, q, "SELECT "+revisionColumns+" FROM olp.guardrails g JOIN olp.guardrail_revisions v ON v.guardrail_id=g.id WHERE v.id=ANY($1::uuid[])", scanRevision, ids)
	if err != nil {
		return nil, err
	}
	return index(revisions, revisionEntry), nil
}

func ReadUnmanagedActiveRevisions(ctx context.Context, q access.Queryer) ([]Revision, error) {
	return collect(ctx, q, "SELECT "+revisionColumns+" FROM olp.guardrails g JOIN olp.guardrail_revisions v ON v.id=g.active_revision_id WHERE g.retired_at IS NULL AND g.managed_route_revision_id IS NULL", scanRevision)
}

func ReadUnmanagedPolicyRevisions(ctx context.Context, q access.Queryer) ([]NamedPolicyRevision, error) {
	return collect(ctx, q, `SELECT v.id::text,p.name,(v.id=p.active_revision_id) FROM olp.guardrail_policies p JOIN olp.guardrail_policy_revisions v ON v.policy_id=p.id
 WHERE p.managed_route_revision_id IS NULL AND ((p.retired_at IS NULL AND v.id=p.active_revision_id) OR v.id IN(SELECT policy_revision_id FROM olp.guardrail_attachments UNION SELECT policy_revision_id FROM olp.guardrail_draft_attachments))`, pgx.RowToStructByPos[NamedPolicyRevision])
}

func ReadRegistry(ctx context.Context, q access.Queryer) (*Registry, error) {
	var unactivated bool
	if err := q.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM olp.guardrail_attachments a LEFT JOIN olp.guardrail_policy_revision_activations v ON v.revision_id=a.policy_revision_id WHERE v.revision_id IS NULL
 UNION ALL
 SELECT 1 FROM olp.guardrail_draft_attachments d LEFT JOIN olp.guardrail_policy_revision_activations v ON v.revision_id=d.policy_revision_id WHERE v.revision_id IS NULL
 UNION ALL
 SELECT 1 FROM olp.guardrail_policy_revisions p CROSS JOIN LATERAL jsonb_array_elements(p.entries) e LEFT JOIN olp.guardrail_revision_activations a ON a.revision_id=(e->>'revision_id')::uuid
 WHERE a.revision_id IS NULL AND (p.id IN(SELECT active_revision_id FROM olp.guardrail_policies) OR p.id IN(SELECT policy_revision_id FROM olp.guardrail_attachments UNION SELECT policy_revision_id FROM olp.guardrail_draft_attachments))
 )`).Scan(&unactivated); err != nil {
		return nil, err
	}
	if unactivated {
		return nil, errors.New("activate referenced guardrail and policy revisions before publication")
	}
	definitions, err := collect(ctx, q, "SELECT "+revisionColumns+","+legacyRuleColumn+` FROM olp.guardrails g JOIN olp.guardrail_revisions v ON v.guardrail_id=g.id
 WHERE v.id=g.active_revision_id OR v.id IN (SELECT (entry->>'revision_id')::uuid FROM olp.guardrail_policy_revisions p,
 jsonb_array_elements(p.entries) entry WHERE p.id IN (SELECT active_revision_id FROM olp.guardrail_policies UNION SELECT policy_revision_id FROM olp.guardrail_attachments UNION SELECT policy_revision_id FROM olp.guardrail_draft_attachments)
 OR p.policy_id IN (SELECT id FROM olp.guardrail_policies WHERE managed_route_revision_id IN (SELECT latest_revision_id FROM olp.routes)))`, scanPublishedRevision)
	if err != nil {
		return nil, err
	}
	if err = requirePublishable(ctx, q, definitions); err != nil {
		return nil, err
	}
	policies, err := collect(ctx, q, `SELECT v.id::text,p.id::text,COALESCE(p.project_id::text,'installation'),p.name,v.revision,v.entries FROM olp.guardrail_policies p JOIN olp.guardrail_policy_revisions v ON v.policy_id=p.id
 WHERE v.id=p.active_revision_id OR v.id IN (SELECT policy_revision_id FROM olp.guardrail_attachments UNION SELECT policy_revision_id FROM olp.guardrail_draft_attachments)`, pgx.RowToStructByPos[PolicyRevision])
	if err != nil {
		return nil, err
	}
	attachments, err := collect(ctx, q, "SELECT id::text,scope,COALESCE(scope_id::text,'installation'),policy_revision_id::text,position FROM olp.guardrail_attachments ORDER BY position,id", pgx.RowToStructByPos[Attachment])
	if err != nil {
		return nil, err
	}
	// Converted route policies follow the route revision, rather than a mutable
	// attachment, so publishing or retiring a route cannot revive an old rule.
	migrated, err := collect(ctx, q, "SELECT p.id::text,'route',r.id::text,p.active_revision_id::text,0 FROM olp.guardrail_policies p JOIN olp.routes r ON r.latest_revision_id=p.managed_route_revision_id WHERE r.state='active'", pgx.RowToStructByPos[Attachment])
	if err != nil {
		return nil, err
	}
	organizations, err := collect(ctx, q, "SELECT id::text,organization_id::text FROM olp.projects WHERE organization_id IS NOT NULL", pgx.RowToStructByPos[projectOrganization])
	if err != nil {
		return nil, err
	}
	window, err := streamWindow(ctx, q)
	if err != nil {
		return nil, err
	}
	if len(definitions) == 0 && len(policies) == 0 && len(attachments) == 0 && len(migrated) == 0 {
		return nil, nil
	}
	r := &Registry{
		Definitions:          index(definitions, revisionEntry),
		Policies:             index(policies, func(p PolicyRevision) (string, PolicyRevision) { return p.ID, p }),
		Attachments:          slices.Concat(attachments, migrated),
		ProjectOrganizations: index(organizations, func(o projectOrganization) (string, string) { return o.Project, o.Organization }),
		MigratedRoutes:       index(migrated, func(a Attachment) (string, bool) { return a.ScopeID, true }),
		WindowBytes:          window,
	}
	return r, r.Validate()
}

type projectOrganization struct{ Project, Organization string }

func requirePublishable(ctx context.Context, q access.Queryer, definitions []Revision) error {
	var credentials, owners, digests, plugins []string
	for _, d := range definitions {
		spec, err := ParseRevision(d)
		if err != nil {
			return err
		}
		for _, id := range spec.References().IDs() {
			credentials, owners = append(credentials, id), append(owners, d.ProjectID)
		}
		if plugin, ok := spec.(PluginSpec); ok {
			digests, plugins = append(digests, plugin.Version), append(plugins, plugin.ID)
		}
	}
	if len(credentials) == 0 && len(digests) == 0 {
		return nil
	}
	var stray, unapproved bool
	if err := q.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM unnest($1::text[],$2::text[]) r(id,owner) WHERE NOT EXISTS(SELECT 1 FROM olp.guardrail_credentials c WHERE c.id::text=r.id AND COALESCE(c.project_id::text,'installation')=r.owner)),
 EXISTS(SELECT 1 FROM unnest($3::text[],$4::text[]) r(digest,name) WHERE NOT EXISTS(SELECT 1 FROM olp.plugins p WHERE p.digest=r.digest AND p.approved_at IS NOT NULL AND p.module IS NOT NULL AND p.manifest->>'name'=r.name AND p.manifest->>'guardrail'='true'))`,
		credentials, owners, digests, plugins).Scan(&stray, &unapproved); err != nil {
		return err
	}
	if stray {
		return errors.New("guardrail credential boundary mismatch")
	}
	if unapproved {
		return errors.New("guardrail requires its approved confined plugin build")
	}
	return nil
}

func streamWindow(ctx context.Context, q access.Queryer) (int, error) {
	var raw string
	if err := q.QueryRow(ctx, "SELECT COALESCE((SELECT value FROM olp.settings WHERE key='guardrails.stream_window_bytes'),'65536')").Scan(&raw); err != nil {
		return 0, err
	}
	var number int
	if json.Unmarshal([]byte(raw), &number) == nil {
		return number, nil
	}
	if parsed, err := strconv.Atoi(raw); err == nil {
		return parsed, nil
	}
	return 0, nil
}
