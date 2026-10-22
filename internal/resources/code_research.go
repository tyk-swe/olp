package resources

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
)

// ResearchAccount validates the provider draft before route publication.
// Research does not need a model offer and never selects a replacement account.
func ResearchAccount(ctx context.Context, q access.Queryer, route codemode.Route) (codemode.Account, error) {
	return researchAccount(ctx, q, route, true)
}

func researchAccount(ctx context.Context, q access.Queryer, route codemode.Route, draft bool) (codemode.Account, error) {
	var a codemode.Account
	if route.Tools == nil {
		return a, codemode.Refuse(404, "code_research_disabled")
	}
	err := q.QueryRow(ctx, `SELECT a.id::text,a.project_id::text,a.provider_id::text,a.credential_id::text,a.principal,a.name,a.enabled
		FROM olp.code_accounts a JOIN olp.code_pool_accounts pa ON pa.account_id=a.id
		JOIN olp.code_pools pool ON pool.id=pa.pool_id
		JOIN olp.providers p ON p.id=a.provider_id
		JOIN olp.provider_credentials c ON c.id=a.credential_id JOIN olp.provider_grants g ON g.credential_id=c.id
		WHERE a.id=$1 AND pa.pool_id=$2 AND pool.project_id=$3 AND a.project_id=$3 AND p.project_id=$3
		AND a.enabled AND p.state<>'disabled' AND c.provider_id=a.provider_id AND c.principal=a.principal
		AND c.profile_id=$4 AND c.revoked_at IS NULL AND g.lapsed_at IS NULL AND g.expires_at IS NULL AND g.refresh_token_id IS NULL
		AND (NOT $5 OR (p.configuration->>'kind'='plugin' AND p.configuration->>'auth_mode'='grant' AND p.configuration->>'profile_id'=$4))`,
		route.Tools.AccountID, route.PoolID, route.ProjectID, codeplans.ZAIProfile, draft).Scan(&a.ID, &a.ProjectID, &a.ProviderID, &a.CredentialID, &a.Principal, &a.Name, &a.Enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, access.Invalid("tools", "Choose an enabled, enrolled Z.ai global account in this project's route pool.")
	}
	return a, err
}

func (s *CodeStore) AuthorizeResearch(ctx context.Context, route codemode.Route, key, service string) (access.Authority, codemode.Account, error) {
	var authority access.Authority
	var account codemode.Account
	if !route.Tools.Enables(service) {
		return authority, account, codemode.Refuse(404, "code_research_disabled")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return authority, account, err
	}
	defer tx.Rollback(ctx)
	if err = codeAuthorityFor(ctx, tx, CodeAdmission{Route: route, APIKeyID: key}, &authority, false); err != nil {
		return authority, account, err
	}
	// Dispatch uses the frozen connection, which the gateway authorizer checks.
	// A provider draft can change without changing the published route.
	account, err = researchAccount(ctx, tx, route, false)
	if err != nil {
		if _, invalid := errors.AsType[*access.Problem](err); invalid {
			return authority, account, codemode.Refuse(403, "code_research_account_unavailable")
		}
		return authority, account, err
	}
	return authority, account, tx.Commit(ctx)
}

// Research sessions retain only opaque transport and authorization metadata.
// A key, publication, account or credential change makes lookup fail closed.
func (s *CodeStore) ResearchSession(ctx context.Context, id string, b codemode.ResearchSessionBinding) (string, error) {
	if _, err := access.ParseUUID(id); err != nil {
		return "", codemode.Refuse(404, "code_research_session_invalid")
	}
	var upstream string
	err := s.Pool.QueryRow(ctx, `UPDATE olp.code_research_sessions SET expires_at=LEAST(created_at+interval '24 hours',now()+interval '1 hour')
		WHERE id=$1 AND api_key_id=$2 AND revision_id=$3 AND service=$4 AND account_id=$5 AND credential_id=$6 AND generation=$7 AND expires_at>now()
		RETURNING upstream_id`, id, b.APIKeyID, b.RevisionID, b.Service, b.AccountID, b.CredentialID, b.Generation).Scan(&upstream)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", codemode.Refuse(404, "code_research_session_invalid")
	}
	return upstream, err
}

func (s *CodeStore) CreateResearchSession(ctx context.Context, b codemode.ResearchSessionBinding, upstream string) (string, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	// Bound aggregate metadata as well as per-key sessions, across replicas.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('code_research_sessions',0))`); err != nil {
		return "", err
	}
	// An older handshake may finish after a publication or credential rotation.
	// Fence its binding before pruning, and hold these rows through commit so
	// it cannot remove sessions created under a newer live binding.
	var route string
	err = tx.QueryRow(ctx, `SELECT r.id::text FROM olp.code_routes r
		JOIN olp.code_accounts a ON a.id=$2
		JOIN olp.provider_credentials c ON c.id=a.credential_id
		JOIN olp.provider_grants g ON g.credential_id=c.id
		WHERE r.latest_revision_id=$1 AND a.credential_id=$3 AND g.generation=$4
		AND a.enabled AND c.revoked_at IS NULL AND g.lapsed_at IS NULL
		FOR SHARE OF r,a,c,g`, b.RevisionID, b.AccountID, b.CredentialID, b.Generation).Scan(&route)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", codemode.Refuse(409, "code_research_session_invalid")
	}
	if err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM olp.code_research_sessions WHERE expires_at<=now()`); err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM olp.code_research_sessions s USING olp.code_route_revisions old,olp.code_route_revisions current
		WHERE s.revision_id=old.id AND current.id=$2 AND old.route_id=current.route_id AND s.api_key_id=$1
		AND (s.revision_id<>$2 OR s.account_id<>$3 OR s.credential_id<>$4 OR s.generation<>$5)`,
		b.APIKeyID, b.RevisionID, b.AccountID, b.CredentialID, b.Generation); err != nil {
		return "", err
	}
	var available bool
	if err = tx.QueryRow(ctx, `SELECT count(*)<10000 AND count(*) FILTER(WHERE api_key_id=$1)<32 FROM olp.code_research_sessions`, b.APIKeyID).Scan(&available); err != nil {
		return "", err
	}
	if !available {
		return "", codemode.Refuse(429, "code_research_session_limit")
	}
	id := access.NewID()
	_, err = tx.Exec(ctx, `INSERT INTO olp.code_research_sessions(id,api_key_id,revision_id,service,account_id,credential_id,generation,upstream_id,expires_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,now()+interval '1 hour')`, id, b.APIKeyID, b.RevisionID, b.Service, b.AccountID, b.CredentialID, b.Generation, upstream)
	if err != nil {
		return "", err
	}
	return id, tx.Commit(ctx)
}

func (s *CodeStore) DeleteResearchSession(ctx context.Context, id, key string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM olp.code_research_sessions WHERE id=$1 AND api_key_id=$2`, id, key)
	return err
}
