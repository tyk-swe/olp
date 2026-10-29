package runtime

import (
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

type delegationConsentRequest struct {
	AgentID       string   `json:"agent_id"`
	APIKeyID      string   `json:"api_key_id"`
	AllowedRoutes []string `json:"allowed_routes"`
	TTLSeconds    int64    `json:"ttl_seconds,omitempty"`
}

func (s *DelegationManagement) Register(mux *http.ServeMux) {
	s.Access.Route(mux, "POST /api/v1/delegation-consents", s.create)
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
	bounds := DelegationBounds{Scopes: []string{"inference"}, AllowedRoutes: input.AllowedRoutes, TTLSeconds: input.TTLSeconds}
	if !validDelegationBounds(bounds) {
		return access.Reply{}, access.Invalid("allowed_routes", "Choose valid routes and a duration of at most 15 minutes.")
	}
	target, err := s.Manager.memberConsentTarget(r.Context(), tx, agentID, keyID)
	if err != nil {
		return access.Reply{}, err
	}
	if err := principal.Project(target.key.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	if target.key.ProjectID == nil || target.agent.ProjectID == nil || *target.key.ProjectID != *target.agent.ProjectID || !slices.Contains(target.key.Policy.Scopes, "delegate") || !delegationRoutesAllowed(target.agent, target.key, bounds.AllowedRoutes, time.Now()) {
		return access.Reply{}, access.Forbidden()
	}
	code := "olpc_" + secrets.Token()
	expires := boundDelegationExpiry(time.Now().Add(delegationTTL(bounds.TTLSeconds)), target.key.ExpiresAt)
	if _, err := tx.Exec(r.Context(), `INSERT INTO olp.agent_delegations(token_digest,consent_digest,consenting_user_id,project_id,api_key_id,agent_id,issuer_revision,allowed_routes,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, s.Manager.auth.Digest(secrets.DelegationTokenDigest, "unissued:"+code), s.Manager.auth.Digest(secrets.DelegationConsentDigest, code), principal.ID, *target.key.ProjectID, keyID, agentID, target.revision, bounds.AllowedRoutes, expires); err != nil {
		return access.Reply{}, err
	}
	if err := access.Audit(r.Context(), tx, r, principal.Actor(), "agent_delegation.consent", "api_key", keyID, "success"); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, access.Reply{Status: http.StatusCreated, Body: map[string]any{"consent": code, "expires_at": expires}})
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
		if principal.ID != agentID || principal.WorkloadIssuerID == nil || principal.RevokedAt != nil {
			continue
		}
		for _, issuer := range state.workloadIssuers {
			if issuer.ID != *principal.WorkloadIssuerID || !issuer.Config.Enabled {
				continue
			}
			for _, mapping := range issuer.Config.Mappings {
				if mapping.Name != principal.WorkloadMapping || !mapping.AgentPrincipal {
					continue
				}
				agent, exists := issuer.bindings[mapping.Name]
				if exists {
					return memberConsentTarget{agent: agent, key: key, revision: issuer.Revision}, nil
				}
			}
		}
	}
	return memberConsentTarget{}, access.Fail(http.StatusNotFound, "not_found", "The resource was not found.")
}
