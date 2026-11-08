package guardrail

import (
	"context"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
)

type Scope interface{ scope() }

type Installation struct{}

type Organization struct{ Name string }

type Project struct{ Name string }

type Route struct{ Slug string }

type APIKey struct{ Project, Name string }

type Toolset struct{ Name string }

func (Installation) scope() {}
func (Organization) scope() {}
func (Project) scope()      {}
func (Route) scope()        {}
func (APIKey) scope()       {}
func (Toolset) scope()      {}

type PolicyRef struct{ Project, Name string }

type Attachment struct {
	Scope  Scope
	Policy PolicyRef
	Order  int
}

const invalidAttachment = "Choose a declared policy and a valid additive attachment."

type described struct {
	kind          contract.ConfigurationGuardrailAttachmentScope
	name, project string
}

func describe(scope Scope) described {
	switch s := scope.(type) {
	case Installation:
		return described{kind: contract.ConfigurationGuardrailAttachmentScopeInstallation}
	case Organization:
		return described{kind: contract.ConfigurationGuardrailAttachmentScopeOrganization, name: s.Name}
	case Project:
		return described{kind: contract.ConfigurationGuardrailAttachmentScopeProject, name: s.Name}
	case Route:
		return described{kind: contract.ConfigurationGuardrailAttachmentScopeRoute, name: s.Slug}
	case APIKey:
		return described{kind: contract.ConfigurationGuardrailAttachmentScopeApiKey, name: s.Name, project: s.Project}
	case Toolset:
		return described{kind: contract.ConfigurationGuardrailAttachmentScopeToolset, name: s.Name}
	}
	return described{}
}

func parseScope(kind contract.ConfigurationGuardrailAttachmentScope, name, project string) (Scope, bool) {
	switch kind {
	case contract.ConfigurationGuardrailAttachmentScopeInstallation:
		return Installation{}, true
	case contract.ConfigurationGuardrailAttachmentScopeOrganization:
		return Organization{name}, true
	case contract.ConfigurationGuardrailAttachmentScopeProject:
		return Project{name}, true
	case contract.ConfigurationGuardrailAttachmentScopeRoute:
		return Route{name}, true
	case contract.ConfigurationGuardrailAttachmentScopeApiKey:
		return APIKey{project, name}, true
	case contract.ConfigurationGuardrailAttachmentScopeToolset:
		return Toolset{name}, true
	}
	return nil, false
}

func parseAttachments(entries []contract.ConfigurationGuardrailAttachment, policies catalog) ([]Attachment, error) {
	return collect(entries, func(a contract.ConfigurationGuardrailAttachment) (Attachment, error) {
		scope, valid := parseScope(a.Scope, dereference(a.Name), dereference(a.Project))
		_, declared := policies.engines[keyOf(a.PolicyProject, a.Policy)]
		if !valid || !declared || a.Order < 0 {
			return Attachment{}, access.Invalid("guardrail_attachments", invalidAttachment)
		}
		return Attachment{Scope: scope, Policy: PolicyRef{a.PolicyProject, a.Policy}, Order: a.Order}, nil
	})
}

func renderAttachment(a Attachment) contract.ConfigurationGuardrailAttachment {
	d := describe(a.Scope)
	return contract.ConfigurationGuardrailAttachment{Scope: d.kind, Name: filled(d.name), Project: filled(d.project), Policy: a.Policy.Name, PolicyProject: a.Policy.Project, Order: a.Order}
}

func filled(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func planAttachments(desired, current []Attachment, changed map[portable]bool) []promotion.Item {
	return mapped(desired, func(a Attachment) promotion.Item {
		d := describe(a.Scope)
		slot := string(d.kind) + "/" + d.name
		index := slices.IndexFunc(current, func(old Attachment) bool { return sameSlot(old, a) })
		const detail = "Add this policy without removing inherited guardrails."
		policy := keyOf(a.Policy.Project, a.Policy.Name)
		switch {
		case index < 0:
			return item(contract.ConfigurationPlanItemKindGuardrailAttachment, slot+"/"+a.Policy.Name, contract.ConfigurationPlanItemActionCreate, detail)
		case keyOf(current[index].Policy.Project, current[index].Policy.Name) != policy:
			return promotion.Conflict(contract.ConfigurationPlanItemKindGuardrailAttachment, slot, "An additive attachment cannot replace a different policy at this position.")
		case changed[policy]:
			return item(contract.ConfigurationPlanItemKindGuardrailAttachment, slot+"/"+a.Policy.Name, contract.ConfigurationPlanItemActionRevision, detail)
		}
		return item(contract.ConfigurationPlanItemKindGuardrailAttachment, slot+"/"+a.Policy.Name, contract.ConfigurationPlanItemActionReuse, detail)
	})
}

func sameSlot(a, b Attachment) bool {
	x, y := describe(a.Scope), describe(b.Scope)
	_, toolset := a.Scope.(Toolset)
	return x.kind == y.kind && strings.EqualFold(x.name, y.name) && strings.EqualFold(x.project, y.project) && a.Order == b.Order &&
		(!toolset || strings.EqualFold(a.Policy.Project, b.Policy.Project))
}

func PolicyApplier(desired Section) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, p access.Principal) error {
		if err := each(desired.Policies, func(policy Policy) error { return applyPolicy(ctx, tx, p, policy) }); err != nil {
			return err
		}
		return each(kept(desired.Attachments, func(a Attachment) bool { return !isToolset(a) }), func(a Attachment) error { return applyAttachment(ctx, tx, p, a) })
	}
}

func ToolsetApplier(desired Section) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, p access.Principal) error {
		return each(kept(desired.Attachments, isToolset), func(a Attachment) error { return applyAttachment(ctx, tx, p, a) })
	}
}

func isToolset(a Attachment) bool {
	_, toolset := a.Scope.(Toolset)
	return toolset
}

type destination interface{ destination() }

type published struct{ id *string }

type draft struct{ id string }

func (published) destination() {}
func (draft) destination()     {}

func applyAttachment(ctx context.Context, tx pgx.Tx, p access.Principal, a Attachment) error {
	project, err := projectID(ctx, tx, a.Policy.Project)
	if err != nil {
		return err
	}
	var revision string
	if err := tx.QueryRow(ctx, "SELECT active_revision_id::text FROM olp.guardrail_policies WHERE project_id IS NOT DISTINCT FROM $1::uuid AND lower(name)=lower($2) AND retired_at IS NULL", project, a.Policy.Name).Scan(&revision); err != nil {
		return err
	}
	target, err := resolveScope(ctx, tx, p, a.Scope, project)
	if err != nil {
		return err
	}
	if err := p.Project(project, access.Change); err != nil {
		return err
	}
	switch t := target.(type) {
	case draft:
		return guardrails.StageDraftAttachment(ctx, tx, p.UserID(), t.id, revision, a.Order)
	case published:
		placed := guardrails.Attachment{Scope: string(describe(a.Scope).kind), ScopeID: installationOr(t.id), PolicyRevisionID: revision, Order: a.Order}
		if err := guardrails.AuthorizeAttachment(ctx, tx, p, placed, access.Change); err != nil {
			return err
		}
		return guardrails.PlaceAttachment(ctx, tx, p.UserID(), placed)
	}
	return access.Invalid("guardrail_attachments", invalidAttachment)
}

func resolveScope(ctx context.Context, tx pgx.Tx, p access.Principal, scope Scope, project *string) (destination, error) {
	switch s := scope.(type) {
	case Installation:
		if !p.AllProjects {
			return nil, access.Forbidden()
		}
		return published{}, nil
	case Organization:
		var id string
		if err := tx.QueryRow(ctx, "SELECT id::text FROM olp.organizations WHERE lower(name)=lower($1)", s.Name).Scan(&id); err != nil {
			return nil, err
		}
		return published{&id}, p.Organization(id, access.Change)
	case Project:
		id, err := projectID(ctx, tx, s.Name)
		return published{id}, err
	case Toolset:
		id, err := optional(ctx, tx, scanText, "SELECT id::text FROM olp.agent_publications WHERE project_id IS NOT DISTINCT FROM $1::uuid AND kind='toolset' AND lower(name)=lower($2) AND NOT retired", project, s.Name)
		if err == nil && id == nil {
			return nil, access.Invalid("guardrail_attachments", "Toolset and policy must belong to the same project.")
		}
		return published{id}, err
	case Route:
		return resolveRoute(ctx, tx, s.Slug, project)
	case APIKey:
		return resolveKey(ctx, tx, s, project)
	}
	return nil, access.Invalid("guardrail_attachments", invalidAttachment)
}

func resolveRoute(ctx context.Context, tx pgx.Tx, slug string, project *string) (destination, error) {
	staged, err := optional(ctx, tx, scanText, "SELECT id::text FROM olp.route_drafts WHERE slug=$1 AND project_id IS NOT DISTINCT FROM $2::uuid ORDER BY updated_at DESC LIMIT 1", slug, project)
	if err != nil {
		return nil, err
	}
	if staged != nil {
		return draft{*staged}, nil
	}
	var id string
	var owner *string
	if err := tx.QueryRow(ctx, "SELECT id::text,project_id::text FROM olp.routes WHERE slug=$1", slug).Scan(&id, &owner); err != nil {
		return nil, err
	}
	if !sameProject(owner, project) {
		return nil, access.Invalid("guardrail_attachments", "Route and policy must belong to the same project.")
	}
	return published{&id}, nil
}

func resolveKey(ctx context.Context, tx pgx.Tx, key APIKey, project *string) (destination, error) {
	owner, err := projectID(ctx, tx, key.Project)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, "SELECT id::text FROM olp.api_keys WHERE project_id IS NOT DISTINCT FROM $1::uuid AND name=$2 AND revoked_at IS NULL", owner, key.Name)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	if len(ids) != 1 || !sameProject(owner, project) {
		return nil, access.Invalid("guardrail_attachments", "API key name must resolve uniquely in the policy project.")
	}
	return published{&ids[0]}, nil
}

func sameProject(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
