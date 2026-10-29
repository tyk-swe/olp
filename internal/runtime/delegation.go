package runtime

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secrets"
)

var ErrDelegationBounds = errors.New("invalid delegation bounds")

type DelegationBounds struct {
	Scopes        []string `json:"scopes"`
	AllowedRoutes []string `json:"allowed_routes"`
	TTLSeconds    int64    `json:"ttl_seconds,omitempty"`
}

type DelegationGrant struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
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
	if !delegationRoutesAllowed(agent, key, bounds.AllowedRoutes, now) {
		return DelegationGrant{}, access.Forbidden()
	}
	expiry := boundDelegationExpiry(boundDelegationExpiry(now.Add(delegationTTL(bounds.TTLSeconds)), agent.ExpiresAt), key.ExpiresAt)
	token := "olpd_" + secrets.Token()
	tx, err := m.delegationTransaction(ctx, agent.SnapshotID)
	if err != nil {
		return DelegationGrant{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO olp.agent_delegations(token_digest,project_id,api_key_id,agent_id,issuer_revision,allowed_routes,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, m.auth.Digest(secrets.DelegationTokenDigest, token), *key.ProjectID, key.ID, agent.ID, agent.WorkloadRevision, bounds.AllowedRoutes, expiry); err != nil {
		return DelegationGrant{}, ErrStaleAuthority
	}
	if err := tx.Commit(ctx); err != nil {
		return DelegationGrant{}, ErrStaleAuthority
	}
	return DelegationGrant{Token: token, ExpiresAt: expiry}, nil
}

func validDelegationBounds(bounds DelegationBounds) bool {
	if len(bounds.Scopes) != 1 || bounds.Scopes[0] != "inference" || len(bounds.AllowedRoutes) == 0 || len(bounds.AllowedRoutes) > 100 || bounds.TTLSeconds < 0 || bounds.TTLSeconds > 900 {
		return false
	}
	for index, route := range bounds.AllowedRoutes {
		if !access.RouteSlug.MatchString(route) || slices.Contains(bounds.AllowedRoutes[:index], route) {
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
	var approved []string
	var expiry time.Time
	queryErr := tx.QueryRow(ctx, `SELECT api_key_id::text,project_id::text,agent_id::text,issuer_revision::text,allowed_routes,expires_at,consenting_user_id::text FROM olp.agent_delegations WHERE consent_digest=$1 AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at>now() FOR UPDATE`, m.auth.Digest(secrets.DelegationConsentDigest, consent)).Scan(&keyID, &project, &agentID, &revision, &approved, &expiry, &memberID)
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
	m.mu.RLock()
	key, exists := m.authority.agentPrincipals[keyID]
	m.mu.RUnlock()
	if !exists || key.WorkloadIssuerID != nil || key.ProjectID == nil || *key.ProjectID != project || !slices.Contains(key.Policy.Scopes, "delegate") || !delegationRoutesAllowed(agent, key, bounds.AllowedRoutes, time.Now()) {
		return DelegationGrant{}, ErrInvalidKey
	}
	token := "olpd_" + secrets.Token()
	expires := boundDelegationExpiry(boundDelegationExpiry(time.Now().Add(delegationTTL(bounds.TTLSeconds)), &expiry), agent.ExpiresAt)
	if _, err := tx.Exec(ctx, `UPDATE olp.agent_delegations SET token_digest=$1,consent_digest=NULL,allowed_routes=$2,expires_at=$3 WHERE consent_digest=$4`, m.auth.Digest(secrets.DelegationTokenDigest, token), bounds.AllowedRoutes, expires, m.auth.Digest(secrets.DelegationConsentDigest, consent)); err != nil {
		return DelegationGrant{}, ErrStaleAuthority
	}
	if err := tx.Commit(ctx); err != nil {
		return DelegationGrant{}, ErrStaleAuthority
	}
	return DelegationGrant{Token: token, ExpiresAt: expires}, nil
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
	var routes []string
	var expires time.Time
	queryErr := tx.QueryRow(ctx, `SELECT api_key_id::text,project_id::text,agent_id::text,issuer_revision::text,allowed_routes,expires_at,consenting_user_id::text FROM olp.agent_delegations WHERE token_digest=$1 AND (consumed_at IS NOT NULL)=$2 AND revoked_at IS NULL AND expires_at>now() FOR UPDATE`, m.auth.Digest(secrets.DelegationTokenDigest, token), consumed).Scan(&keyID, &project, &agentID, &revision, &routes, &expires, &memberID)
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
	if !delegationRoutesAllowed(*agent, key, routes, now) {
		return nil, ErrInvalidKey
	}
	if !consumed {
		if _, err := tx.Exec(ctx, `UPDATE olp.agent_delegations SET consumed_at=now() WHERE token_digest=$1`, m.auth.Digest(secrets.DelegationTokenDigest, token)); err != nil {
			return nil, ErrStaleAuthority
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, ErrStaleAuthority
	}
	if memberID == nil {
		pair, _ := json.Marshal([3]string{project, key.ID, agent.ID})
		return key.DelegatedInference(routes, *agent, "", hex.EncodeToString(m.auth.Digest(secrets.DelegationPairDigest, string(pair))), boundDelegationExpiry(expires, agent.ExpiresAt)), nil
	}
	pair, _ := json.Marshal([4]string{project, "member", *memberID, agent.ID})
	return key.DelegatedInference(routes, *agent, *memberID, hex.EncodeToString(m.auth.Digest(secrets.DelegationPairDigest, string(pair))), boundDelegationExpiry(expires, agent.ExpiresAt)), nil
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

func delegationRoutesAllowed(agent, key access.Authority, routes []string, now time.Time) bool {
	for _, route := range routes {
		if !agent.Allows("inference", route, agent.ProjectID, now) || !key.Allows("inference", route, key.ProjectID, now) {
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
