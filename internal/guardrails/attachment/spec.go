package attachment

import (
	"cmp"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/guardrails"
)

var invalidScope = access.Invalid("scope", "Choose installation, organization, project, route, toolset or api-key.")

func parsePlacement(in guardrails.Attachment) (guardrails.Attachment, error) {
	if in.ID != "" || in.Order < 0 {
		return guardrails.Attachment{}, access.Invalid("attachment", "The server assigns identities; position must be nonnegative.")
	}
	if !in.Scope.Valid() {
		return guardrails.Attachment{}, invalidScope
	}
	revision, err := access.ParseUUID(in.PolicyRevisionID)
	if err != nil {
		return guardrails.Attachment{}, access.Invalid("policy_revision_id", "Choose an existing policy revision.")
	}
	scopeID, err := scopeIdentity(in.Scope, in.ScopeID)
	if err != nil {
		return guardrails.Attachment{}, err
	}
	return guardrails.Attachment{Scope: in.Scope, ScopeID: scopeID, PolicyRevisionID: revision, Order: in.Order}, nil
}

func scopeIdentity(scope guardrails.Scope, id string) (string, error) {
	if scope == guardrails.InstallationScope {
		return guardrails.InstallationProject, nil
	}
	parsed, err := access.ParseUUID(id)
	if err != nil {
		return "", access.Invalid("scope_id", "Choose an existing scope.")
	}
	return parsed, nil
}

type target struct {
	guardrails.Attachment
	owner *string
	found bool
}

func (t target) owned() bool {
	return t.Scope == guardrails.RouteScope || t.Scope == guardrails.KeyScope || t.Scope == guardrails.ToolsetScope
}

var changeOperation = map[guardrails.Scope]access.Operation{
	guardrails.InstallationScope: access.Settings,
	guardrails.OrganizationScope: access.ManageOrganization,
	guardrails.KeyScope:          access.Keys,
}

func authorize(p access.Principal, t target, need access.Need) error {
	if need == access.Change {
		if err := p.Authorize(cmp.Or(changeOperation[t.Scope], access.Configure)); err != nil {
			return err
		}
	}
	switch {
	case t.Scope == guardrails.InstallationScope && !p.AllProjects:
		return access.Forbidden()
	case t.Scope == guardrails.InstallationScope:
		return p.Project(nil, need)
	case t.Scope == guardrails.OrganizationScope:
		return p.Organization(t.ScopeID, need)
	case t.Scope == guardrails.ProjectScope:
		return p.Project(&t.ScopeID, need)
	case !t.found:
		return pgx.ErrNoRows
	default:
		return p.Project(t.owner, need)
	}
}
