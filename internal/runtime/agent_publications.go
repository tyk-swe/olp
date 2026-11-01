package runtime

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
)

func readAgentPublications(ctx context.Context, tx pgx.Tx) ([]agentcontract.Revision, error) {
	rows, err := tx.Query(ctx, `SELECT v.id::text,v.publication_id::text,v.project_id::text,v.kind,v.digest,v.definition,COALESCE(p.latest_revision_id=v.id,false),COALESCE((SELECT array_agg(label ORDER BY label) FROM olp.prompt_labels WHERE publication_id=p.id AND revision_id=v.id),'{}'::text[]) FROM olp.agent_publication_revisions v JOIN olp.agent_publications p ON p.id=v.publication_id WHERE NOT p.retired ORDER BY v.id LIMIT 8193`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []agentcontract.Revision
	for rows.Next() {
		var v agentcontract.Revision
		if err = rows.Scan(&v.ID, &v.PublicationID, &v.ProjectID, &v.Kind, &v.Digest, &v.Definition, &v.Current, &v.Labels); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if err = agentcontract.ValidateRevisions(out); err != nil {
		return nil, err
	}
	return out, nil
}

type agentResourceAuthority struct {
	ProjectID, Kind string
	Revisions       map[string]bool
}

func loadAgentResourceAuthority(ctx context.Context, tx pgx.Tx) (map[string]agentResourceAuthority, error) {
	rows, err := tx.Query(ctx, `SELECT publication_id::text,project_id::text,kind,id::text FROM olp.live_agent_publication_revisions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]agentResourceAuthority{}
	for rows.Next() {
		var id, project, kind, revision string
		if err = rows.Scan(&id, &project, &kind, &revision); err != nil {
			return nil, err
		}
		value, ok := out[id]
		if !ok {
			value = agentResourceAuthority{project, kind, map[string]bool{}}
		}
		value.Revisions[revision] = true
		out[id] = value
	}
	return out, rows.Err()
}

// AuthorizeAgentResource intersects fresh key policy with live publication
// authority. Long-running callers must invoke it before each outbound step and
// at least every PollInterval; stale authority is never accepted.
func (m *Manager) AuthorizeAgentResource(a access.Authority, scope, id, revision, project string) error {
	if a.TaskCapability != nil && (scope != "tools" || !slices.Contains(a.TaskCapability.ToolsetRevisions, revision)) {
		return access.Forbidden()
	}
	now := time.Now()
	if !a.AllowsResource(scope, id, project, now) {
		return access.Forbidden()
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.authority.loaded || now.Sub(m.authority.readAt) > AuthorityStaleAfter {
		return ErrStaleAuthority
	}
	if !m.authority.currentAgentPrincipal(a, scope, id, project, now) {
		return access.Forbidden()
	}
	resource, ok := m.authority.agentResources[id]
	if !ok || resource.ProjectID != project || !resource.Revisions[revision] || (scope == "tools" && resource.Kind != "toolset") || (scope == "agents" && resource.Kind != "agent") {
		return access.Forbidden()
	}
	return nil
}

func (state authorityState) currentAgentPrincipal(a access.Authority, scope, id, project string, now time.Time) bool {
	current, ok := state.agentPrincipals[a.ID]
	if a.WorkloadDigest != "" {
		principal, known := state.workloadPrincipals[a.WorkloadDigest]
		if !known || principal.ID != a.ID || principal.WorkloadIssuerID == nil || a.WorkloadIssuerID == nil || *principal.WorkloadIssuerID != *a.WorkloadIssuerID || principal.WorkloadMapping != a.WorkloadMapping || principal.ProjectID == nil || *principal.ProjectID != project {
			return false
		}
		ok = false
		for _, issuer := range state.workloadIssuers {
			if issuer.ID != *principal.WorkloadIssuerID {
				continue
			}
			// Verified claims stay valid only for the issuer revision that
			// authenticated them, as with durable code-mode continuations.
			if issuer.Config.Enabled && issuer.Revision == a.WorkloadRevision {
				current, ok = issuer.bindings[principal.WorkloadMapping]
				current.ID = principal.ID
				current.RevokedAt = principal.RevokedAt
			}
			break
		}
	}
	if !ok || current.ID != a.ID || !current.AllowsResource(scope, id, project, now) {
		return false
	}
	if a.DelegationAgentID == "" {
		return true
	}
	if a.DelegationAgentLimits == nil || a.DelegationAgentLimits.ID != a.DelegationAgentID || a.DelegationAgentLimits.WorkloadDigest == "" {
		return false
	}
	return state.currentAgentPrincipal(access.Authority{ID: a.DelegationAgentID, WorkloadIssuerID: a.DelegationAgentLimits.WorkloadIssuerID, WorkloadDigest: a.DelegationAgentLimits.WorkloadDigest, WorkloadRevision: a.DelegationAgentLimits.WorkloadRevision, WorkloadMapping: a.DelegationAgentLimits.WorkloadMapping}, scope, id, project, now)
}

type mcpToolAuthority struct {
	PublicationID string
	RevisionID    string
	ProjectID     string
	Servers       map[string]string
}

func loadMCPToolAuthority(ctx context.Context, tx pgx.Tx) (map[string]mcpToolAuthority, error) {
	rows, err := tx.Query(ctx, `SELECT p.id::text,v.id::text,v.project_id::text,
 COALESCE((SELECT jsonb_object_agg(s.id::text,s.latest_revision_id::text)
 FROM jsonb_array_elements(v.definition->'toolset'->'tools') t
 JOIN olp.mcp_servers s ON s.id=(t->>'server_id')::uuid
 WHERE s.enabled AND s.retired_at IS NULL AND s.project_id=v.project_id AND s.latest_revision_id=(t->>'revision_id')::uuid),'{}'::jsonb)
 FROM olp.agent_publication_revisions v JOIN olp.agent_publications p ON p.id=v.publication_id
 WHERE NOT p.retired AND v.kind='toolset'`)
	if err != nil {
		return nil, err
	}
	values, collectErr := pgx.CollectRows(rows, func(row pgx.CollectableRow) (mcpToolAuthority, error) {
		var value mcpToolAuthority
		scanErr := row.Scan(&value.PublicationID, &value.RevisionID, &value.ProjectID, &value.Servers)
		return value, scanErr
	})
	if collectErr != nil {
		return nil, collectErr
	}
	return maps.Collect(func(yield func(string, mcpToolAuthority) bool) {
		for _, value := range values {
			if !yield(value.RevisionID, value) {
				return
			}
		}
	}), nil
}
func (m *Manager) AuthorizeMCPTool(a access.Authority, id, revision, project, server, serverRevision string) error {
	if a.TaskCapability != nil && !slices.Contains(a.TaskCapability.ToolsetRevisions, revision) {
		return access.Forbidden()
	}
	now := time.Now()
	if !a.AllowsResource("tools", id, project, now) {
		return access.Forbidden()
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.authority.loaded || now.Sub(m.authority.readAt) > AuthorityStaleAfter {
		return ErrStaleAuthority
	}
	if !m.authority.currentAgentPrincipal(a, "tools", id, project, now) {
		return access.Forbidden()
	}
	toolset, ok := m.authority.mcpTools[revision]
	if !ok || toolset.PublicationID != id || toolset.ProjectID != project {
		return access.Forbidden()
	}
	if server != "" && toolset.Servers[server] != serverRevision {
		return access.Forbidden()
	}
	return nil
}
