package guardrail

import (
	"context"
	"fmt"
	"slices"
	"strings"

	guardrailattachment "github.com/tyk-swe/olp/internal/guardrails/attachment"

	guardrailpolicy "github.com/tyk-swe/olp/internal/guardrails/policy"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agents/publications"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/projects"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
	"github.com/tyk-swe/olp/internal/routes"
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
	return promotion.TryMap(entries, func(a contract.ConfigurationGuardrailAttachment) (Attachment, error) {
		scope, valid := parseScope(a.Scope, promotion.ValueOf(a.Name), promotion.ValueOf(a.Project))
		_, declared := policies.engines[keyOf(a.PolicyProject, a.Policy)]
		if !valid || !declared || a.Order < 0 {
			return Attachment{}, access.Invalid("guardrail_attachments", invalidAttachment)
		}
		return Attachment{Scope: scope, Policy: PolicyRef{a.PolicyProject, a.Policy}, Order: a.Order}, nil
	})
}

func renderAttachment(a Attachment) contract.ConfigurationGuardrailAttachment {
	d := describe(a.Scope)
	return contract.ConfigurationGuardrailAttachment{Scope: d.kind, Name: promotion.NonZero(d.name), Project: promotion.NonZero(d.project), Policy: a.Policy.Name, PolicyProject: a.Policy.Project, Order: a.Order}
}

func planAttachments(desired, current []Attachment, changed map[portable]bool) []promotion.Item {
	return promotion.Map(desired, func(a Attachment) promotion.Item {
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
		directory, err := project.ReadDirectory(ctx, tx)
		if err != nil {
			return err
		}
		if err := promotion.Every(desired.Policies, func(policy Policy) error { return applyPolicy(ctx, tx, p, directory, policy) }); err != nil {
			return err
		}
		return promotion.Every(promotion.Filter(desired.Attachments, func(a Attachment) bool { return !isToolset(a) }), func(a Attachment) error { return applyAttachment(ctx, tx, p, directory, a) })
	}
}

func ToolsetApplier(desired Section) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, p access.Principal) error {
		directory, err := project.ReadDirectory(ctx, tx)
		if err != nil {
			return err
		}
		return promotion.Every(promotion.Filter(desired.Attachments, isToolset), func(a Attachment) error { return applyAttachment(ctx, tx, p, directory, a) })
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

func applyAttachment(ctx context.Context, tx pgx.Tx, p access.Principal, directory promotion.Directory, a Attachment) error {
	project, err := projectID(directory, a.Policy.Project)
	if err != nil {
		return err
	}
	revision, err := activeRevision(ctx, tx, project, a.Policy.Name)
	if err != nil {
		return err
	}
	target, err := resolveScope(ctx, tx, p, directory, a.Scope, project)
	if err != nil {
		return err
	}
	if err := p.Project(project, access.Change); err != nil {
		return err
	}
	switch t := target.(type) {
	case draft:
		return guardrailattachment.Stage(ctx, tx, p.UserID(), t.id, revision, a.Order)
	case published:
		placed := guardrails.Attachment{Scope: guardrails.Scope(describe(a.Scope).kind), ScopeID: guardrails.ProjectOf(t.id), PolicyRevisionID: revision, Order: a.Order}
		if err := guardrailattachment.Authorize(ctx, tx, p, placed, access.Change); err != nil {
			return err
		}
		return guardrailattachment.Place(ctx, tx, p.UserID(), placed)
	}
	return access.Invalid("guardrail_attachments", invalidAttachment)
}

func resolveScope(ctx context.Context, tx pgx.Tx, p access.Principal, directory promotion.Directory, scope Scope, project *string) (destination, error) {
	switch s := scope.(type) {
	case Installation:
		if !p.AllProjects {
			return nil, access.Forbidden()
		}
		return published{}, nil
	case Organization:
		id, found, err := projects.OrganizationIDByName(ctx, tx, s.Name)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, promotion.UnknownOrganization(s.Name)
		}
		return published{&id}, p.Organization(id, access.Change)
	case Project:
		id, err := projectID(directory, s.Name)
		return published{id}, err
	case Toolset:
		return resolveToolset(ctx, tx, s.Name, project)
	case Route:
		return resolveRoute(ctx, tx, s.Slug, project)
	case APIKey:
		return resolveKey(ctx, tx, directory, s, project)
	}
	return nil, access.Invalid("guardrail_attachments", invalidAttachment)
}

func resolveRoute(ctx context.Context, tx pgx.Tx, slug string, project *string) (destination, error) {
	staged, found, err := routes.FindLatestDraftID(ctx, tx, slug, project)
	if err != nil {
		return nil, err
	}
	if found {
		return draft{staged}, nil
	}
	route, err := routes.RouteBySlug(ctx, tx, slug)
	if err != nil {
		return nil, err
	}
	if !sameProject(route.ProjectID, project) {
		return nil, access.Invalid("guardrail_attachments", "Route and policy must belong to the same project.")
	}
	return published{&route.RouteID}, nil
}

func resolveKey(ctx context.Context, tx pgx.Tx, directory promotion.Directory, key APIKey, project *string) (destination, error) {
	owner, err := projectID(directory, key.Project)
	if err != nil {
		return nil, err
	}
	keys, err := access.ReadKeysNamed(ctx, tx, owner, key.Name)
	if err != nil {
		return nil, err
	}
	if len(keys) != 1 || !sameProject(owner, project) {
		return nil, access.Invalid("guardrail_attachments", "API key name must resolve uniquely in the policy project.")
	}
	return published{&keys[0].ID}, nil
}

func sameProject(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func resolveToolset(ctx context.Context, tx pgx.Tx, name string, project *string) (destination, error) {
	if project == nil {
		return nil, toolsetOutsideProject()
	}
	id, found, err := publications.ActiveID(ctx, tx, *project, "toolset", name)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, toolsetOutsideProject()
	}
	return published{&id}, nil
}

func toolsetOutsideProject() error {
	return access.Invalid("guardrail_attachments", "Toolset and policy must belong to the same project.")
}

func activeRevision(ctx context.Context, q access.Queryer, project *string, name string) (string, error) {
	current, err := guardrailpolicy.FindLatest(ctx, q, project, name)
	if err != nil {
		return "", err
	}
	if current == nil || current.ActiveRevisionID == nil {
		return "", fmt.Errorf("guardrail policy %q has no active revision: %w", name, pgx.ErrNoRows)
	}
	return *current.ActiveRevisionID, nil
}
