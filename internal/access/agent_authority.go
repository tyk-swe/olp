package access

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"
)

func addedKeyAgentResources(policy, previous KeyPolicy) KeyPolicy {
	policy.AllowedToolsets = slices.DeleteFunc(slices.Clone(policy.AllowedToolsets), func(id string) bool { return slices.Contains(previous.AllowedToolsets, id) })
	policy.AllowedAgents = slices.DeleteFunc(slices.Clone(policy.AllowedAgents), func(id string) bool { return slices.Contains(previous.AllowedAgents, id) })
	return policy
}

func checkKeyAgentResources(ctx context.Context, q Queryer, policy KeyPolicy, project *string) error {
	for field, ids := range map[string][]string{"allowed_toolsets": policy.AllowedToolsets, "allowed_agents": policy.AllowedAgents} {
		if len(ids) == 0 {
			continue
		}
		if project == nil {
			return Invalid(field, "Consumer resources require an owning project.")
		}
		kind := "toolset"
		if field == "allowed_agents" {
			kind = "agent"
		}
		var count int
		if err := q.QueryRow(ctx, `SELECT count(*) FROM olp.agent_publications WHERE id=ANY($1::uuid[]) AND project_id=$2 AND kind=$3 AND NOT retired AND latest_revision_id IS NOT NULL`, ids, *project, kind).Scan(&count); err != nil {
			return err
		}
		if count != len(ids) {
			return Invalid(field, "Choose published resources in this key's project.")
		}
	}
	return nil
}

// ConsumerScopes returns a copy so configuration consumers cannot mutate the
// authoritative scope vocabulary. Scopes never imply access to a resource.
func ConsumerScopes() []string {
	return []string{"inference", "models_read", "guardrails", "tools", "agents", "traces_write", "delegate"}
}

func validateResourceAllowlist(field string, ids []string) error {
	if len(ids) > 100 {
		return Invalid(field, "Use at most 100 unique resource UUIDs.")
	}
	seen := map[string]bool{}
	for _, raw := range ids {
		id, err := uuid.Parse(raw)
		if err != nil || id == uuid.Nil || id.String() != raw || seen[raw] {
			return Invalid(field, "Use unique canonical resource UUIDs.")
		}
		seen[raw] = true
	}
	return nil
}

// AllowsResource is deliberately separate from route authorization. A route
// group, inference scope, or installation-wide key cannot grant project tools.
// The caller also checks the resource's current publication and revocation.
func (a Authority) AllowsResource(scope, id, project string, now time.Time) bool {
	if a.ProjectID == nil || *a.ProjectID != project || a.RevokedAt != nil || a.ExpiresAt != nil && !a.ExpiresAt.After(now) || !slices.Contains(a.Policy.Scopes, scope) {
		return false
	}
	switch scope {
	case "tools":
		return slices.Contains(a.Policy.AllowedToolsets, id)
	case "agents":
		return slices.Contains(a.Policy.AllowedAgents, id)
	case "traces_write":
		return id == ""
	default:
		return false
	}
}
