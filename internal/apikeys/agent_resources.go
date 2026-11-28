package apikeys

import (
	"context"
	"slices"

	"github.com/tyk-swe/olp/internal/domain/consumerscope"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/platform/database"
)

func addedKeyAgentResources(policy, previous keyauthority.KeyPolicy) keyauthority.KeyPolicy {
	policy.AllowedToolsets = slices.DeleteFunc(slices.Clone(policy.AllowedToolsets), func(id string) bool { return slices.Contains(previous.AllowedToolsets, id) })
	policy.AllowedAgents = slices.DeleteFunc(slices.Clone(policy.AllowedAgents), func(id string) bool { return slices.Contains(previous.AllowedAgents, id) })
	return policy
}

func checkKeyAgentResources(ctx context.Context, q database.Queryer, policy keyauthority.KeyPolicy, project *string) error {
	for field, ids := range map[string][]string{"allowed_toolsets": policy.AllowedToolsets, "allowed_agents": policy.AllowedAgents} {
		if len(ids) == 0 {
			continue
		}
		if project == nil {
			return problem.Invalid(field, "Consumer resources require an owning project.")
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
			return problem.Invalid(field, "Choose published resources in this key's project.")
		}
	}
	return nil
}

// ConsumerScopes returns a copy so configuration consumers cannot mutate the
// authoritative scope vocabulary. Scopes never imply access to a resource.
func ConsumerScopes() []string {
	return consumerscope.All()
}

func validateResourceAllowlist(field string, allowed []string) error {
	if len(allowed) > 100 {
		return problem.Invalid(field, "Use at most 100 unique resource UUIDs.")
	}
	seen := map[string]bool{}
	for _, raw := range allowed {
		if !ids.Canonical(raw) || seen[raw] {
			return problem.Invalid(field, "Use unique canonical resource UUIDs.")
		}
		seen[raw] = true
	}
	return nil
}
