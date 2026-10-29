package runtime

import (
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/secrets"
)

var ErrDelegationBounds = errors.New("invalid delegation bounds")

type DelegationBounds struct {
	Scopes          []string `json:"scopes"`
	AllowedRoutes   []string `json:"allowed_routes"`
	AllowedToolsets []string `json:"allowed_toolsets,omitempty"`
	AllowedAgents   []string `json:"allowed_agents,omitempty"`
	TTLSeconds      int64    `json:"ttl_seconds,omitempty"`
}

type DelegationGrant struct {
	Token                string    `json:"token"`
	ExpiresAt            time.Time `json:"expires_at"`
	DelegationPairDigest string    `json:"delegation_pair_digest"`
}

func (m *Manager) CreateDelegation(ctx context.Context, agent, key access.Authority, bounds DelegationBounds) (DelegationGrant, error) {
	if !agent.AgentPrincipal || agent.WorkloadIssuerID == nil || key.WorkloadIssuerID != nil || agent.ProjectID == nil || key.ProjectID == nil || *agent.ProjectID != *key.ProjectID || !slices.Contains(key.Policy.Scopes, "delegate") {
		return DelegationGrant{}, access.Forbidden()
	}
	if agent.SnapshotID == "" || agent.SnapshotID != key.SnapshotID {
		return DelegationGrant{}, ErrStaleAuthority
	}
	if !validDelegationBounds(bounds) {
		return DelegationGrant{}, ErrDelegationBounds
	}
	now := time.Now()
	if !delegationResourcesAllowed(agent, key, bounds.AllowedRoutes, bounds.AllowedToolsets, bounds.AllowedAgents, now) {
		return DelegationGrant{}, access.Forbidden()
	}
	expiry := boundDelegationExpiry(boundDelegationExpiry(now.Add(delegationTTL(bounds.TTLSeconds)), agent.ExpiresAt), key.ExpiresAt)
	token := "olpd_" + secrets.Token()
	tx, err := m.delegationTransaction(ctx, agent.SnapshotID)
	if err != nil {
		return DelegationGrant{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO olp.agent_delegations(token_digest,project_id,api_key_id,agent_id,issuer_revision,allowed_routes,expires_at,scopes,allowed_toolsets,allowed_agents) VALUES($1,$2,$3,$4,$5,COALESCE($6,'{}'::text[]),$7,$8,COALESCE($9,'{}'::text[]),COALESCE($10,'{}'::text[]))`, m.auth.Digest(secrets.DelegationTokenDigest, token), *key.ProjectID, key.ID, agent.ID, agent.WorkloadRevision, bounds.AllowedRoutes, expiry, bounds.Scopes, bounds.AllowedToolsets, bounds.AllowedAgents); err != nil {
		return DelegationGrant{}, ErrStaleAuthority
	}
	if err := tx.Commit(ctx); err != nil {
		return DelegationGrant{}, ErrStaleAuthority
	}
	pair, _ := m.delegationPair(*key.ProjectID, key.ID, agent.ID, nil)
	return DelegationGrant{Token: token, ExpiresAt: expiry, DelegationPairDigest: pair}, nil
}

func validDelegationBounds(bounds DelegationBounds) bool {
	if len(bounds.Scopes) == 0 || len(bounds.Scopes) > 3 || len(bounds.AllowedRoutes) > 100 || len(bounds.AllowedToolsets) > 100 || len(bounds.AllowedAgents) > 100 || bounds.TTLSeconds < 0 || bounds.TTLSeconds > 900 {
		return false
	}
	for index, scope := range bounds.Scopes {
		if !slices.Contains([]string{"inference", "tools", "agents"}, scope) || slices.Contains(bounds.Scopes[:index], scope) {
			return false
		}
	}
	if slices.Contains(bounds.Scopes, "inference") != (len(bounds.AllowedRoutes) > 0) || slices.Contains(bounds.Scopes, "tools") != (len(bounds.AllowedToolsets) > 0) || slices.Contains(bounds.Scopes, "agents") != (len(bounds.AllowedAgents) > 0) {
		return false
	}
	for index, route := range bounds.AllowedRoutes {
		if !access.RouteSlug.MatchString(route) || slices.Contains(bounds.AllowedRoutes[:index], route) {
			return false
		}
	}
	for index, raw := range bounds.AllowedToolsets {
		id, err := uuid.Parse(raw)
		if err != nil || id == uuid.Nil || id.String() != raw || slices.Contains(bounds.AllowedToolsets[:index], raw) {
			return false
		}
	}
	for index, raw := range bounds.AllowedAgents {
		id, err := uuid.Parse(raw)
		if err != nil || id == uuid.Nil || id.String() != raw || slices.Contains(bounds.AllowedAgents[:index], raw) {
			return false
		}
	}
	return true
}

func (m *Manager) AuthenticateDelegation(ctx context.Context, token, assertion string) (*access.Authority, error) {
	return m.authenticateDelegation(ctx, token, assertion, false)
}

func (m *Manager) ExchangeMemberConsent(ctx context.Context, agent access.Authority, consent string, bounds DelegationBounds) (DelegationGrant, error) {
	if !validDelegationBounds(bounds) {
		return DelegationGrant{}, ErrDelegationBounds
	}
	if !agent.AgentPrincipal || agent.WorkloadIssuerID == nil || agent.ProjectID == nil || agent.RevokedAt != nil || agent.ExpiresAt == nil || !agent.ExpiresAt.After(time.Now()) {
		return DelegationGrant{}, ErrInvalidKey
	}
	tx, err := m.delegationTransaction(ctx, agent.SnapshotID)
	if err != nil {
		return DelegationGrant{}, err
	}
	defer tx.Rollback(ctx)
	var keyID, project, agentID, revision string
	var memberID *string
	var approved, approvedScopes, approvedToolsets, approvedAgents []string
	var expiry time.Time
	queryErr := tx.QueryRow(ctx, `SELECT api_key_id::text,project_id::text,agent_id::text,issuer_revision::text,allowed_routes,expires_at,consenting_user_id::text,scopes,allowed_toolsets,allowed_agents FROM olp.agent_delegations WHERE consent_digest=$1 AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at>now() FOR UPDATE`, m.auth.Digest(secrets.DelegationConsentDigest, consent)).Scan(&keyID, &project, &agentID, &revision, &approved, &expiry, &memberID, &approvedScopes, &approvedToolsets, &approvedAgents)
	if errors.Is(queryErr, pgx.ErrNoRows) {
		return DelegationGrant{}, ErrInvalidKey
	}
	if queryErr != nil {
		return DelegationGrant{}, ErrStaleAuthority
	}
	if agent.ID != agentID || *agent.ProjectID != project || agent.WorkloadRevision != revision {
		return DelegationGrant{}, ErrInvalidKey
	}
	if err := memberDelegationAllowed(ctx, tx, memberID, project); err != nil {
		return DelegationGrant{}, err
	}
	for _, route := range bounds.AllowedRoutes {
		if !slices.Contains(approved, route) {
			return DelegationGrant{}, access.Forbidden()
		}
	}
	for _, scope := range bounds.Scopes {
		if !slices.Contains(approvedScopes, scope) {
			return DelegationGrant{}, access.Forbidden()
		}
	}
	for _, toolset := range bounds.AllowedToolsets {
		if !slices.Contains(approvedToolsets, toolset) {
			return DelegationGrant{}, access.Forbidden()
		}
	}
	for _, allowed := range bounds.AllowedAgents {
		if !slices.Contains(approvedAgents, allowed) {
			return DelegationGrant{}, access.Forbidden()
		}
	}
	m.mu.RLock()
	key, exists := m.authority.agentPrincipals[keyID]
	m.mu.RUnlock()
	if !exists || key.WorkloadIssuerID != nil || key.ProjectID == nil || *key.ProjectID != project || !slices.Contains(key.Policy.Scopes, "delegate") || !delegationResourcesAllowed(agent, key, bounds.AllowedRoutes, bounds.AllowedToolsets, bounds.AllowedAgents, time.Now()) {
		return DelegationGrant{}, ErrInvalidKey
	}
	token := "olpd_" + secrets.Token()
	expires := boundDelegationExpiry(boundDelegationExpiry(time.Now().Add(delegationTTL(bounds.TTLSeconds)), &expiry), agent.ExpiresAt)
	if _, err := tx.Exec(ctx, `UPDATE olp.agent_delegations SET token_digest=$1,consent_digest=NULL,allowed_routes=COALESCE($2,'{}'::text[]),expires_at=$3,scopes=$5,allowed_toolsets=COALESCE($6,'{}'::text[]),allowed_agents=COALESCE($7,'{}'::text[]) WHERE consent_digest=$4`, m.auth.Digest(secrets.DelegationTokenDigest, token), bounds.AllowedRoutes, expires, m.auth.Digest(secrets.DelegationConsentDigest, consent), bounds.Scopes, bounds.AllowedToolsets, bounds.AllowedAgents); err != nil {
		return DelegationGrant{}, ErrStaleAuthority
	}
	if err := tx.Commit(ctx); err != nil {
		return DelegationGrant{}, ErrStaleAuthority
	}
	pair, _ := m.delegationPair(project, keyID, agentID, memberID)
	return DelegationGrant{Token: token, ExpiresAt: expires, DelegationPairDigest: pair}, nil
}

func (m *Manager) ReauthenticateDelegation(ctx context.Context, token, assertion string) (*access.Authority, error) {
	return m.authenticateDelegation(ctx, token, assertion, true)
}

func (m *Manager) authenticateDelegation(ctx context.Context, token, assertion string, consumed bool) (*access.Authority, error) {
	agent, err := m.Authenticate(assertion)
	if err != nil {
		return nil, err
	}
	if agent == nil || !agent.AgentPrincipal || agent.WorkloadIssuerID == nil || agent.ProjectID == nil || agent.RevokedAt != nil || agent.ExpiresAt == nil || !agent.ExpiresAt.After(time.Now()) {
		return nil, ErrInvalidKey
	}
	tx, err := m.delegationTransaction(ctx, agent.SnapshotID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var keyID, project, agentID, revision string
	var memberID *string
	var routes, scopes, toolsets, agents []string
	var expires time.Time
	queryErr := tx.QueryRow(ctx, `SELECT api_key_id::text,project_id::text,agent_id::text,issuer_revision::text,allowed_routes,expires_at,consenting_user_id::text,scopes,allowed_toolsets,allowed_agents FROM olp.agent_delegations WHERE token_digest=$1 AND (consumed_at IS NOT NULL)=$2 AND revoked_at IS NULL AND expires_at>now() FOR UPDATE`, m.auth.Digest(secrets.DelegationTokenDigest, token), consumed).Scan(&keyID, &project, &agentID, &revision, &routes, &expires, &memberID, &scopes, &toolsets, &agents)
	if errors.Is(queryErr, pgx.ErrNoRows) {
		return nil, ErrInvalidKey
	}
	if queryErr != nil {
		return nil, ErrStaleAuthority
	}
	if agent.ID != agentID || *agent.ProjectID != project || agent.WorkloadRevision != revision {
		return nil, ErrInvalidKey
	}
	if err := memberDelegationAllowed(ctx, tx, memberID, project); err != nil {
		return nil, err
	}
	m.mu.RLock()
	key, exists := m.authority.agentPrincipals[keyID]
	m.mu.RUnlock()
	if !exists || key.WorkloadIssuerID != nil || key.ProjectID == nil || *key.ProjectID != project || !slices.Contains(key.Policy.Scopes, "delegate") {
		return nil, ErrInvalidKey
	}
	now := time.Now()
	if !delegationResourcesAllowed(*agent, key, routes, toolsets, agents, now) {
		return nil, ErrInvalidKey
	}
	if !consumed {
		if _, err := tx.Exec(ctx, `UPDATE olp.agent_delegations SET consumed_at=now() WHERE token_digest=$1`, m.auth.Digest(secrets.DelegationTokenDigest, token)); err != nil {
			return nil, ErrStaleAuthority
		}
	}
	pairDigest, member := m.delegationPair(project, key.ID, agent.ID, memberID)
	if err := limits.EnsureAttributionBudget(ctx, tx, project, attribution.DelegationPairKey, pairDigest); err != nil {
		return nil, ErrStaleAuthority
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, ErrStaleAuthority
	}
	return key.Delegated(scopes, routes, toolsets, agents, *agent, member, pairDigest, hex.EncodeToString(m.auth.Digest(secrets.DelegationTokenDigest, token)), boundDelegationExpiry(expires, agent.ExpiresAt)), nil
}

func (m *Manager) authorizeDelegatedTask(ctx context.Context, digest []byte, expectedKey, expectedProject, publication string) (*access.Authority, error) {
	if m == nil || m.pool == nil {
		return nil, ErrInvalidKey
	}
	m.mu.RLock()
	snapshot := m.authority.id
	m.mu.RUnlock()
	tx, err := m.delegationTransaction(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var keyID, project, agentID, revision string
	var memberID *string
	var routes, scopes, toolsets, agents []string
	var expires time.Time
	queryErr := tx.QueryRow(ctx, `SELECT api_key_id::text,project_id::text,agent_id::text,issuer_revision::text,allowed_routes,expires_at,consenting_user_id::text,scopes,allowed_toolsets,allowed_agents FROM olp.agent_delegations WHERE token_digest=$1 AND consumed_at IS NOT NULL AND revoked_at IS NULL AND expires_at>now() FOR SHARE`, digest).Scan(&keyID, &project, &agentID, &revision, &routes, &expires, &memberID, &scopes, &toolsets, &agents)
	if errors.Is(queryErr, pgx.ErrNoRows) {
		return nil, ErrInvalidKey
	}
	if queryErr != nil {
		return nil, ErrStaleAuthority
	}
	if keyID != expectedKey || project != expectedProject || !slices.Contains(scopes, "agents") || !slices.Contains(agents, publication) {
		return nil, ErrInvalidKey
	}
	if err := memberDelegationAllowed(ctx, tx, memberID, project); err != nil {
		return nil, err
	}
	m.mu.RLock()
	key, keyExists := m.authority.agentPrincipals[keyID]
	policy, principal, currentSnapshot, _, agentExists := m.authority.taskCapabilityPrincipal(agentID, revision, &expires)
	agent := policy.TaskAuthority(principal, currentSnapshot, revision, &expires, "", nil)
	current := agentExists && m.authority.currentAgentPrincipal(agent, "agents", publication, project, time.Now())
	m.mu.RUnlock()
	if !keyExists || key.WorkloadIssuerID != nil || key.ProjectID == nil || *key.ProjectID != project || !slices.Contains(key.Policy.Scopes, "delegate") || !current || !delegationResourcesAllowed(agent, key, routes, toolsets, agents, time.Now()) {
		return nil, ErrInvalidKey
	}
	pair, member := m.delegationPair(project, keyID, agentID, memberID)
	if err := tx.Commit(ctx); err != nil {
		return nil, ErrStaleAuthority
	}
	return key.Delegated(scopes, routes, toolsets, agents, agent, member, pair, hex.EncodeToString(digest), expires), nil
}

func (m *Manager) AuthorizeDelegatedAgentTask(ctx context.Context, digest, keyID, project, publication string) error {
	raw, err := hex.DecodeString(digest)
	if err != nil {
		return ErrInvalidKey
	}
	_, err = m.authorizeDelegatedTask(ctx, raw, keyID, project, publication)
	return err
}

func (m *Manager) delegationPair(project, key, agent string, member *string) (string, string) {
	pair := attribution.DelegationPair(m.auth, project, key, agent, member)
	if member != nil {
		return pair, *member
	}
	return pair, ""
}

func memberDelegationAllowed(ctx context.Context, tx pgx.Tx, memberID *string, project string) error {
	if memberID == nil {
		return nil
	}
	var role, scope, projectRole string
	err := tx.QueryRow(ctx, `SELECT u.role,u.access_scope,COALESCE(m.role,'') FROM olp.users u LEFT JOIN olp.effective_project_members m ON m.user_id=u.id AND m.project_id=$2 WHERE u.id=$1 AND u.active AND u.oidc_authorized`, *memberID, project).Scan(&role, &scope, &projectRole)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidKey
	}
	if err != nil {
		return ErrStaleAuthority
	}
	principal := access.Principal{User: access.User{Role: role}, Kind: "user", AllProjects: scope == "global", Projects: map[string]string{project: projectRole}}
	if principal.Authorize(access.Keys) != nil || principal.Project(&project, access.Change) != nil {
		return ErrInvalidKey
	}
	return nil
}

func (m *Manager) delegationTransaction(ctx context.Context, expected string) (pgx.Tx, error) {
	m.mu.RLock()
	status := m.authority
	m.mu.RUnlock()
	if !status.loaded || status.id != expected || time.Since(status.readAt) > AuthorityStaleAfter {
		return nil, ErrStaleAuthority
	}
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return nil, ErrStaleAuthority
	}
	var current string
	if err := tx.QueryRow(ctx, "SELECT authority_id::text FROM olp.installation WHERE singleton FOR UPDATE").Scan(&current); err != nil || current != status.id {
		tx.Rollback(ctx)
		return nil, ErrStaleAuthority
	}
	return tx, nil
}

func delegationResourcesAllowed(agent, key access.Authority, routes, toolsets, agents []string, now time.Time) bool {
	for _, route := range routes {
		if !agent.Allows("inference", route, agent.ProjectID, now) || !key.Allows("inference", route, key.ProjectID, now) {
			return false
		}
	}
	for _, toolset := range toolsets {
		if !agent.AllowsResource("tools", toolset, *agent.ProjectID, now) || !key.AllowsResource("tools", toolset, *key.ProjectID, now) {
			return false
		}
	}
	for _, allowed := range agents {
		if !agent.AllowsResource("agents", allowed, *agent.ProjectID, now) || !key.AllowsResource("agents", allowed, *key.ProjectID, now) {
			return false
		}
	}
	return true
}

func delegationTTL(seconds int64) time.Duration {
	if seconds == 0 {
		return 5 * time.Minute
	}
	return time.Duration(seconds) * time.Second
}

func boundDelegationExpiry(expiry time.Time, bound *time.Time) time.Time {
	if bound != nil && bound.Before(expiry) {
		return *bound
	}
	return expiry
}
