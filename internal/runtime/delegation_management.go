package runtime

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secrets"
)

type DelegationManagement struct {
	Access  *access.Server
	Manager *Manager
}

type delegationOption struct {
	ID              string   `json:"id"`
	AllowedRoutes   []string `json:"allowed_routes"`
	AllowedToolsets []string `json:"allowed_toolsets"`
	AllowedAgents   []string `json:"allowed_agents"`
}

type delegationConsentRequest struct {
	AgentID         string   `json:"agent_id"`
	APIKeyID        string   `json:"api_key_id"`
	Scopes          []string `json:"scopes,omitempty"`
	AllowedRoutes   []string `json:"allowed_routes"`
	AllowedToolsets []string `json:"allowed_toolsets,omitempty"`
	AllowedAgents   []string `json:"allowed_agents,omitempty"`
	TTLSeconds      int64    `json:"ttl_seconds,omitempty"`
}

func (s *DelegationManagement) Register(mux *http.ServeMux) {
	s.Access.Route(mux, "POST /api/v1/delegation-consents", s.create)
	s.Access.Route(mux, "GET /api/v1/delegation-options", s.options)
}

func (s *DelegationManagement) options(r *http.Request, _ access.Principal) (access.Reply, error) {
	keyID, err := access.ParseUUID(r.URL.Query().Get("api_key_id"))
	if err != nil {
		return access.Reply{}, err
	}
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	principal, err := s.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	var projectID *string
	if err := tx.QueryRow(r.Context(), "SELECT project_id::text FROM olp.api_keys WHERE id=$1 AND workload_issuer_id IS NULL", keyID).Scan(&projectID); err != nil {
		if err == pgx.ErrNoRows {
			return access.Reply{}, access.Fail(http.StatusNotFound, "not_found", "The resource was not found.")
		}
		return access.Reply{}, err
	}
	if projectID == nil {
		return access.Reply{}, access.Forbidden()
	}
	if err := principal.Project(projectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	rows, err := tx.Query(r.Context(), "SELECT slug FROM olp.routes WHERE project_id=$1 AND state='active' ORDER BY slug", *projectID)
	if err != nil {
		return access.Reply{}, err
	}
	routes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return access.Reply{}, err
	}
	toolsetRows, err := tx.Query(r.Context(), "SELECT id::text FROM olp.agent_publications WHERE project_id=$1 AND kind='toolset' AND NOT retired AND latest_revision_id IS NOT NULL ORDER BY id", *projectID)
	if err != nil {
		return access.Reply{}, err
	}
	toolsets, err := pgx.CollectRows(toolsetRows, pgx.RowTo[string])
	if err != nil {
		return access.Reply{}, err
	}
	agentRows, err := tx.Query(r.Context(), "SELECT id::text FROM olp.agent_publications WHERE project_id=$1 AND kind='agent' AND NOT retired AND latest_revision_id IS NOT NULL ORDER BY id", *projectID)
	if err != nil {
		return access.Reply{}, err
	}
	agents, err := pgx.CollectRows(agentRows, pgx.RowTo[string])
	if err != nil {
		return access.Reply{}, err
	}
	var current string
	if err := tx.QueryRow(r.Context(), "SELECT authority_id::text FROM olp.installation WHERE singleton").Scan(&current); err != nil {
		return access.Reply{}, err
	}
	s.Manager.mu.RLock()
	defer s.Manager.mu.RUnlock()
	state := s.Manager.authority
	if !state.loaded || state.id != current || time.Since(state.readAt) > AuthorityStaleAfter {
		return access.Reply{}, access.Fail(http.StatusServiceUnavailable, "authority_unavailable", "Delegation is unavailable.")
	}
	key, ok := state.agentPrincipals[keyID]
	if !ok || key.WorkloadIssuerID != nil || key.ProjectID == nil || *key.ProjectID != *projectID || !slices.Contains(key.Policy.Scopes, "delegate") {
		return access.Reply{}, access.Forbidden()
	}
	now := time.Now()
	options := slices.SortedFunc(func(yield func(delegationOption) bool) {
		for _, registered := range state.workloadPrincipals {
			if registered.ProjectID == nil || *registered.ProjectID != *projectID {
				continue
			}
			agent, _, ok := state.memberConsentAgent(registered)
			if !ok {
				continue
			}
			allowed := slices.Collect(func(yieldRoute func(string) bool) {
				for _, route := range routes {
					if delegationResourcesAllowed(agent, key, []string{route}, nil, nil, now) && !yieldRoute(route) {
						return
					}
				}
			})
			allowedToolsets := slices.Collect(func(yieldToolset func(string) bool) {
				for _, toolset := range toolsets {
					if delegationResourcesAllowed(agent, key, nil, []string{toolset}, nil, now) && !yieldToolset(toolset) {
						return
					}
				}
			})
			allowedAgents := slices.Collect(func(yieldAgent func(string) bool) {
				for _, target := range agents {
					if delegationResourcesAllowed(agent, key, nil, nil, []string{target}, now) && !yieldAgent(target) {
						return
					}
				}
			})
			if (len(allowed) != 0 || len(allowedToolsets) != 0 || len(allowedAgents) != 0) && !yield(delegationOption{ID: registered.ID, AllowedRoutes: append([]string{}, allowed...), AllowedToolsets: append([]string{}, allowedToolsets...), AllowedAgents: append([]string{}, allowedAgents...)}) {
				return
			}
		}
	}, func(left, right delegationOption) int { return cmp.Compare(left.ID, right.ID) })
	return access.Commit(r, tx, access.Reply{Status: http.StatusOK, Body: struct {
		Agents []delegationOption `json:"agents"`
	}{Agents: append([]delegationOption{}, options...)}})
}

func (s *DelegationManagement) create(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input delegationConsentRequest
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	keyID, err := access.ParseUUID(input.APIKeyID)
	if err != nil {
		return access.Reply{}, err
	}
	agentID, err := access.ParseUUID(input.AgentID)
	if err != nil {
		return access.Reply{}, err
	}
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	principal, err := s.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	if err := s.Access.ConsumeRecent(r, tx, principal, "agent_delegation", keyID); err != nil {
		return access.Reply{}, err
	}
	bounds := DelegationBounds{Scopes: consentScopes(input.Scopes), AllowedRoutes: input.AllowedRoutes, AllowedToolsets: input.AllowedToolsets, AllowedAgents: input.AllowedAgents, TTLSeconds: input.TTLSeconds}
	if !validDelegationBounds(bounds) {
		return access.Reply{}, access.Invalid("scopes", "Choose valid scopes and resources and a duration of at most 15 minutes.")
	}
	target, err := s.Manager.memberConsentTarget(r.Context(), tx, agentID, keyID)
	if err != nil {
		return access.Reply{}, err
	}
	if err := principal.Project(target.key.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	if target.key.ProjectID == nil || target.agent.ProjectID == nil || *target.key.ProjectID != *target.agent.ProjectID || !slices.Contains(target.key.Policy.Scopes, "delegate") || !delegationResourcesAllowed(target.agent, target.key, bounds.AllowedRoutes, bounds.AllowedToolsets, bounds.AllowedAgents, time.Now()) {
		return access.Reply{}, access.Forbidden()
	}
	code := "olpc_" + secrets.Token()
	expires := boundDelegationExpiry(time.Now().Add(delegationTTL(bounds.TTLSeconds)), target.key.ExpiresAt)
	if _, err := tx.Exec(r.Context(), `INSERT INTO olp.agent_delegations(token_digest,consent_digest,consenting_user_id,project_id,api_key_id,agent_id,issuer_revision,scopes,allowed_routes,allowed_toolsets,expires_at,allowed_agents) VALUES($1,$2,$3,$4,$5,$6,$7,$8,COALESCE($9,ARRAY[]::text[]),COALESCE($10,ARRAY[]::text[]),$11,COALESCE($12,ARRAY[]::text[]))`, s.Manager.auth.Digest(secrets.DelegationTokenDigest, "unissued:"+code), s.Manager.auth.Digest(secrets.DelegationConsentDigest, code), principal.ID, *target.key.ProjectID, keyID, agentID, target.revision, bounds.Scopes, bounds.AllowedRoutes, bounds.AllowedToolsets, expires, bounds.AllowedAgents); err != nil {
		return access.Reply{}, err
	}
	if err := access.Audit(r.Context(), tx, r, principal.Actor(), "agent_delegation.consent", "api_key", keyID, "success"); err != nil {
		return access.Reply{}, err
	}
	pair, _ := s.Manager.delegationPair(*target.key.ProjectID, keyID, agentID, &principal.ID)
	return access.Commit(r, tx, access.Reply{Status: http.StatusCreated, Body: map[string]any{"consent": code, "expires_at": expires, "delegation_pair_digest": pair}})
}

func consentScopes(scopes []string) []string {
	if len(scopes) == 0 {
		return []string{"inference"}
	}
	return scopes
}

type memberConsentTarget struct {
	agent    access.Authority
	key      access.Authority
	revision string
}

func (m *Manager) memberConsentTarget(ctx context.Context, tx pgx.Tx, agentID, keyID string) (memberConsentTarget, error) {
	var current string
	if err := tx.QueryRow(ctx, "SELECT authority_id::text FROM olp.installation WHERE singleton").Scan(&current); err != nil {
		return memberConsentTarget{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	state := m.authority
	if !state.loaded || state.id != current || time.Since(state.readAt) > AuthorityStaleAfter {
		return memberConsentTarget{}, access.Fail(http.StatusServiceUnavailable, "authority_unavailable", "Delegation is unavailable.")
	}
	key, ok := state.agentPrincipals[keyID]
	if !ok || key.WorkloadIssuerID != nil {
		return memberConsentTarget{}, access.Fail(http.StatusNotFound, "not_found", "The resource was not found.")
	}
	for _, principal := range state.workloadPrincipals {
		if principal.ID != agentID {
			continue
		}
		agent, revision, exists := state.memberConsentAgent(principal)
		if exists {
			return memberConsentTarget{agent: agent, key: key, revision: revision}, nil
		}
	}
	return memberConsentTarget{}, access.Fail(http.StatusNotFound, "not_found", "The resource was not found.")
}

func (state authorityState) memberConsentAgent(principal access.Authority) (access.Authority, string, bool) {
	if principal.WorkloadIssuerID == nil || principal.RevokedAt != nil || principal.ProjectID == nil {
		return access.Authority{}, "", false
	}
	for _, issuer := range state.workloadIssuers {
		if issuer.ID != *principal.WorkloadIssuerID || !issuer.Config.Enabled {
			continue
		}
		for _, mapping := range issuer.Config.Mappings {
			if mapping.Name != principal.WorkloadMapping || !mapping.AgentPrincipal || mapping.ProjectID != *principal.ProjectID {
				continue
			}
			agent, exists := issuer.bindings[mapping.Name]
			if exists {
				return agent, issuer.Revision, true
			}
		}
	}
	return access.Authority{}, "", false
}
