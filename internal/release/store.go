package release

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/domain/ptr"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/limits/ledger"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

type DelegationRecord struct {
	ID               string     `db:"id" json:"id"`
	ProjectID        string     `db:"project_id" json:"project_id"`
	APIKeyID         string     `db:"api_key_id" json:"api_key_id"`
	AgentID          string     `db:"agent_id" json:"agent_id"`
	MemberID         *string    `db:"member_id" json:"member_id"`
	ConsentPending   bool       `db:"consent_pending" json:"consent_pending"`
	ExpiresAt        time.Time  `db:"expires_at" json:"expires_at"`
	ConsumedAt       *time.Time `db:"consumed_at" json:"consumed_at"`
	RevokedAt        *time.Time `db:"revoked_at" json:"revoked_at"`
	APIKeyRevokedAt  *time.Time `db:"api_key_revoked_at" json:"api_key_revoked_at"`
	AgentRevokedAt   *time.Time `db:"agent_revoked_at" json:"agent_revoked_at"`
	MemberAuthorized *bool      `db:"-" json:"member_authorized"`
}

type delegationRow struct {
	DelegationRecord
	MemberActive         *bool   `db:"member_active"`
	MemberOIDCAuthorized *bool   `db:"member_oidc_authorized"`
	MemberRole           *string `db:"member_role"`
	MemberScope          *string `db:"member_scope"`
	ProjectRole          *string `db:"project_role"`
}

func (r delegationRow) memberAuthorized() bool {
	if r.MemberActive == nil || !*r.MemberActive || r.MemberOIDCAuthorized == nil || !*r.MemberOIDCAuthorized || r.MemberRole == nil || r.MemberScope == nil {
		return false
	}
	principal := authz.Principal{User: authz.User{Role: *r.MemberRole}, Kind: "user", AllProjects: *r.MemberScope == "global", Projects: map[string]string{r.ProjectID: ptr.Value(r.ProjectRole)}}
	return principal.Authorize(authz.Keys) == nil && principal.Project(&r.ProjectID, authz.Change) == nil
}

func (r delegationRow) record() DelegationRecord {
	if r.MemberID == nil {
		return r.DelegationRecord
	}
	authorized := r.memberAuthorized()
	return DelegationRecord{ID: r.ID, ProjectID: r.ProjectID, APIKeyID: r.APIKeyID, AgentID: r.AgentID, MemberID: r.MemberID, ConsentPending: r.ConsentPending, ExpiresAt: r.ExpiresAt, ConsumedAt: r.ConsumedAt, RevokedAt: r.RevokedAt, APIKeyRevokedAt: r.APIKeyRevokedAt, AgentRevokedAt: r.AgentRevokedAt, MemberAuthorized: &authorized}
}

type PairBudgetWindow struct {
	Accrued      string    `json:"accrued"`
	WindowEndsAt time.Time `json:"window_ends_at"`
}

type PairBudget struct {
	Daily             PairBudgetWindow `json:"daily"`
	Weekly            PairBudgetWindow `json:"weekly"`
	Monthly           PairBudgetWindow `json:"monthly"`
	CostBasis         string           `json:"cost_basis"`
	UnpricedAttempts  int64            `json:"unpriced_attempts"`
	UnchargedAttempts int64            `json:"uncharged_attempts"`
}

type DelegationView struct {
	DelegationRecord
	BudgetPolicy *keyauthority.BudgetPolicy `json:"budget_policy"`
	Budget       PairBudget                 `json:"budget"`
}

type delegationPairRecord struct {
	Record    DelegationRecord `json:"-"`
	ProjectID string           `json:"project_id"`
	Digest    string           `json:"digest"`
}

type delegationPairBudget struct {
	Ordinal int                        `db:"ordinal"`
	Policy  *keyauthority.BudgetPolicy `db:"policy"`
	Usage   *PairBudget                `db:"usage"`
}

type delegationFilter struct {
	principal  authz.Principal
	page       database.Pagination
	project    string
	delegation string
}

func delegationRecords(ctx context.Context, q database.Queryer, auth *secrets.AuthKey, filter delegationFilter) ([]delegationPairRecord, error) {
	rows, queryErr := q.Query(ctx, `SELECT d.id::text AS id,d.project_id::text AS project_id,d.api_key_id::text AS api_key_id,
 d.agent_id::text AS agent_id,d.consenting_user_id::text AS member_id,(d.consent_digest IS NOT NULL) AS consent_pending,
 d.expires_at,d.consumed_at,d.revoked_at,k.revoked_at AS api_key_revoked_at,a.revoked_at AS agent_revoked_at,
 u.active AS member_active,u.oidc_authorized AS member_oidc_authorized,u.role AS member_role,u.access_scope AS member_scope,m.role AS project_role
 FROM olp.agent_delegations d JOIN olp.api_keys k ON k.id=d.api_key_id JOIN olp.api_keys a ON a.id=d.agent_id
 LEFT JOIN olp.users u ON u.id=d.consenting_user_id LEFT JOIN olp.effective_project_members m ON m.user_id=u.id AND m.project_id=d.project_id
 WHERE d.id<$1 AND ($2 OR d.project_id=ANY($3::uuid[])) AND ($4='' OR d.project_id=NULLIF($4,'')::uuid)
 AND ($6='' OR d.id=NULLIF($6,'')::uuid)
 ORDER BY d.id DESC LIMIT $5`, filter.page.Before, filter.principal.AllProjects, filter.principal.ProjectIDs(), filter.project, filter.page.Limit+1, filter.delegation)
	if queryErr != nil {
		return nil, queryErr
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (delegationPairRecord, error) {
		data, dataErr := pgx.RowToStructByName[delegationRow](row)
		if dataErr != nil {
			return delegationPairRecord{}, dataErr
		}
		return delegationPairRecord{
			Record: data.record(), ProjectID: data.ProjectID,
			Digest: secrets.DelegationPair(auth, data.ProjectID, data.APIKeyID, data.AgentID, data.MemberID),
		}, nil
	})
}

func delegationViews(ctx context.Context, q database.Queryer, records []delegationPairRecord, limit int) ([]DelegationView, error) {
	if len(records) == 0 {
		return []DelegationView{}, nil
	}
	pairInput, marshalErr := json.Marshal(records)
	if marshalErr != nil {
		return nil, marshalErr
	}
	rows, queryErr := q.Query(ctx, `WITH entries AS MATERIALIZED (
 SELECT ordinality::int AS ordinal,(value->>'project_id')::uuid AS project_id,value->>'digest' AS digest
 FROM jsonb_array_elements($1::jsonb) WITH ORDINALITY
), pairs AS MATERIALIZED (
 SELECT DISTINCT project_id,digest FROM entries WHERE ordinal<=$2
), budgets AS MATERIALIZED (
 SELECT pair.project_id,pair.digest,p.attribution_budgets->'delegation_pair'->pair.digest AS policy,`+ledger.DelegationPairBudgetSQL("p", "pair")+` AS usage
 FROM pairs pair JOIN olp.projects p ON p.id=pair.project_id
)
 SELECT entries.ordinal,budgets.policy,budgets.usage
 FROM entries LEFT JOIN budgets ON budgets.project_id=entries.project_id AND budgets.digest=entries.digest
 ORDER BY entries.ordinal`, pairInput, limit)
	if queryErr != nil {
		return nil, queryErr
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (DelegationView, error) {
		budget, dataErr := pgx.RowToStructByName[delegationPairBudget](row)
		if dataErr != nil {
			return DelegationView{}, dataErr
		}
		if budget.Usage == nil {
			if budget.Ordinal <= limit {
				return DelegationView{}, pgx.ErrNoRows
			}
			return DelegationView{DelegationRecord: records[budget.Ordinal-1].Record}, nil
		}
		return DelegationView{DelegationRecord: records[budget.Ordinal-1].Record, BudgetPolicy: budget.Policy, Budget: *budget.Usage}, nil
	})
}

func delegatingKeyProject(ctx context.Context, tx pgx.Tx, keyID string) (*string, error) {
	var projectID *string
	err := tx.QueryRow(ctx, "SELECT project_id::text FROM olp.api_keys WHERE id=$1 AND workload_issuer_id IS NULL", keyID).Scan(&projectID)
	return projectID, database.NotFoundOr(err)
}

type delegatableResources struct {
	routes   []string
	toolsets []string
	agents   []string
}

func loadDelegatableResources(ctx context.Context, tx pgx.Tx, projectID string) (delegatableResources, error) {
	routes, routesErr := collectStrings(ctx, tx, "SELECT slug FROM olp.routes WHERE project_id=$1 AND state='active' ORDER BY slug", projectID)
	if routesErr != nil {
		return delegatableResources{}, routesErr
	}
	toolsets, toolsetsErr := collectStrings(ctx, tx, "SELECT id::text FROM olp.agent_publications WHERE project_id=$1 AND kind='toolset' AND NOT retired AND latest_revision_id IS NOT NULL ORDER BY id", projectID)
	if toolsetsErr != nil {
		return delegatableResources{}, toolsetsErr
	}
	agents, agentsErr := collectStrings(ctx, tx, "SELECT id::text FROM olp.agent_publications WHERE project_id=$1 AND kind='agent' AND NOT retired AND latest_revision_id IS NOT NULL ORDER BY id", projectID)
	if agentsErr != nil {
		return delegatableResources{}, agentsErr
	}
	return delegatableResources{routes: routes, toolsets: toolsets, agents: agents}, nil
}

func collectStrings(ctx context.Context, tx pgx.Tx, query, projectID string) ([]string, error) {
	rows, err := tx.Query(ctx, query, projectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func installationAuthority(ctx context.Context, tx pgx.Tx) (string, error) {
	var authorityID string
	err := tx.QueryRow(ctx, "SELECT authority_id::text FROM olp.installation WHERE singleton").Scan(&authorityID)
	return authorityID, err
}

type pendingConsent struct {
	tokenDigest    []byte
	consentDigest  []byte
	memberID       string
	projectID      string
	keyID          string
	agentID        string
	issuerRevision string
	bounds         DelegationBounds
	expiresAt      time.Time
}

func insertConsent(ctx context.Context, tx pgx.Tx, consent pendingConsent) error {
	_, err := tx.Exec(ctx, `INSERT INTO olp.agent_delegations(token_digest,consent_digest,consenting_user_id,project_id,api_key_id,agent_id,issuer_revision,scopes,allowed_routes,allowed_toolsets,expires_at,allowed_agents) VALUES($1,$2,$3,$4,$5,$6,$7,$8,COALESCE($9,ARRAY[]::text[]),COALESCE($10,ARRAY[]::text[]),$11,COALESCE($12,ARRAY[]::text[]))`, consent.tokenDigest, consent.consentDigest, consent.memberID, consent.projectID, consent.keyID, consent.agentID, consent.issuerRevision, consent.bounds.Scopes, consent.bounds.AllowedRoutes, consent.bounds.AllowedToolsets, consent.expiresAt, consent.bounds.AllowedAgents)
	return err
}

func runtimeGenerations(ctx context.Context, q database.Queryer, page database.Pagination) ([]map[string]any, error) {
	return database.QueryJSON(ctx, q, "SELECT jsonb_build_object('id',g.id,'sequence',g.sequence,'sha256',g.sha256,'created_by',g.created_by,'created_by_email',u.email,'created_at',g.created_at) FROM olp.runtime_releases g JOIN olp.users u ON u.id=g.created_by WHERE g.id<$1 ORDER BY g.id DESC LIMIT $2", page.Before, page.Limit+1)
}
