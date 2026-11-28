package attachment

import (
	"cmp"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/domain/guardrailpolicy"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/guardrails"
)

var invalidScope = problem.Invalid("scope", "Choose installation, organization, project, route, toolset or api-key.")

func parsePlacement(in guardrails.Attachment) (guardrails.Attachment, error) {
	if in.ID != "" || in.Order < 0 {
		return guardrails.Attachment{}, problem.Invalid("attachment", "The server assigns identities; position must be nonnegative.")
	}
	if !in.Scope.Valid() {
		return guardrails.Attachment{}, invalidScope
	}
	revision, err := ids.Parse(in.PolicyRevisionID)
	if err != nil {
		return guardrails.Attachment{}, problem.Invalid("policy_revision_id", "Choose an existing policy revision.")
	}
	scopeID, err := scopeIdentity(in.Scope, in.ScopeID)
	if err != nil {
		return guardrails.Attachment{}, err
	}
	return guardrails.Attachment{Scope: in.Scope, ScopeID: scopeID, PolicyRevisionID: revision, Order: in.Order}, nil
}

func scopeIdentity(scope guardrailpolicy.Scope, id string) (string, error) {
	if scope == guardrailpolicy.InstallationScope {
		return guardrails.InstallationProject, nil
	}
	parsed, err := ids.Parse(id)
	if err != nil {
		return "", problem.Invalid("scope_id", "Choose an existing scope.")
	}
	return parsed, nil
}

type target struct {
	guardrails.Attachment
	owner *string
	found bool
}

func (t target) owned() bool {
	return t.Scope == guardrailpolicy.RouteScope || t.Scope == guardrailpolicy.KeyScope || t.Scope == guardrailpolicy.ToolsetScope
}

var changeOperation = map[guardrailpolicy.Scope]authz.Operation{
	guardrailpolicy.InstallationScope: authz.Settings,
	guardrailpolicy.OrganizationScope: authz.ManageOrganization,
	guardrailpolicy.KeyScope:          authz.Keys,
}

func CanView(p authz.Principal, scope guardrailpolicy.Scope, id string, owner *string) bool {
	return authorize(p, target{Attachment: guardrails.Attachment{Scope: scope, ScopeID: id}, owner: owner, found: true}, authz.View) == nil
}

func authorize(p authz.Principal, t target, need authz.Need) error {
	if need == authz.Change {
		if err := p.Authorize(cmp.Or(changeOperation[t.Scope], authz.Configure)); err != nil {
			return err
		}
	}
	switch {
	case t.Scope == guardrailpolicy.InstallationScope && !p.AllProjects:
		return problem.Forbidden()
	case t.Scope == guardrailpolicy.InstallationScope:
		return p.Project(nil, need)
	case t.Scope == guardrailpolicy.OrganizationScope:
		return p.Organization(t.ScopeID, need)
	case t.Scope == guardrailpolicy.ProjectScope:
		return p.Project(&t.ScopeID, need)
	case !t.found:
		return pgx.ErrNoRows
	default:
		return p.Project(t.owner, need)
	}
}
