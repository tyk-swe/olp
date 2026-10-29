package agents

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/limits"
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

func projectRole(role *string) string {
	if role == nil {
		return ""
	}
	return *role
}

func (r delegationRow) memberAuthorized() bool {
	if r.MemberActive == nil || !*r.MemberActive || r.MemberOIDCAuthorized == nil || !*r.MemberOIDCAuthorized || r.MemberRole == nil || r.MemberScope == nil {
		return false
	}
	principal := access.Principal{User: access.User{Role: *r.MemberRole}, Kind: "user", AllProjects: *r.MemberScope == "global", Projects: map[string]string{r.ProjectID: projectRole(r.ProjectRole)}}
	return principal.Authorize(access.Keys) == nil && principal.Project(&r.ProjectID, access.Change) == nil
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
	BudgetPolicy *access.BudgetPolicy `json:"budget_policy"`
	Budget       PairBudget           `json:"budget"`
}

type delegationPairRecord struct {
	Record    DelegationRecord `json:"-"`
	ProjectID string           `json:"project_id"`
	Digest    string           `json:"digest"`
}

type delegationPairBudget struct {
	Ordinal int                  `db:"ordinal"`
	Policy  *access.BudgetPolicy `db:"policy"`
	Usage   *PairBudget          `db:"usage"`
}

func (s *Server) listDelegations(r *http.Request, p access.Principal) (access.Reply, error) {
	page, pageErr := access.Page(r)
	if pageErr != nil {
		return access.Reply{}, pageErr
	}
	project, projectErr := projectFilter(r, p)
	if projectErr != nil {
		return access.Reply{}, projectErr
	}
	rows, queryErr := s.Access.Pool.Query(r.Context(), `SELECT d.id::text AS id,d.project_id::text AS project_id,d.api_key_id::text AS api_key_id,
 d.agent_id::text AS agent_id,d.consenting_user_id::text AS member_id,(d.consent_digest IS NOT NULL) AS consent_pending,
 d.expires_at,d.consumed_at,d.revoked_at,k.revoked_at AS api_key_revoked_at,a.revoked_at AS agent_revoked_at,
 u.active AS member_active,u.oidc_authorized AS member_oidc_authorized,u.role AS member_role,u.access_scope AS member_scope,m.role AS project_role
 FROM olp.agent_delegations d JOIN olp.api_keys k ON k.id=d.api_key_id JOIN olp.api_keys a ON a.id=d.agent_id
 LEFT JOIN olp.users u ON u.id=d.consenting_user_id LEFT JOIN olp.effective_project_members m ON m.user_id=u.id AND m.project_id=d.project_id
 WHERE d.id<$1 AND ($2 OR d.project_id=ANY($3::uuid[])) AND ($4='' OR d.project_id=NULLIF($4,'')::uuid)
 ORDER BY d.id DESC LIMIT $5`, page.Before, p.AllProjects, p.ProjectIDs(), project, page.Limit+1)
	if queryErr != nil {
		return access.Reply{}, queryErr
	}
	records, collectErr := pgx.CollectRows(rows, func(row pgx.CollectableRow) (delegationPairRecord, error) {
		data, dataErr := pgx.RowToStructByName[delegationRow](row)
		if dataErr != nil {
			return delegationPairRecord{}, dataErr
		}
		return delegationPairRecord{
			Record: data.record(), ProjectID: data.ProjectID,
			Digest: attribution.DelegationPair(s.Access.Auth, data.ProjectID, data.APIKeyID, data.AgentID, data.MemberID),
		}, nil
	})
	if collectErr != nil {
		return access.Reply{}, collectErr
	}
	if len(records) == 0 {
		return access.ListReplyBy([]DelegationView{}, page, func(item DelegationView) string { return item.ID }), nil
	}
	pairInput, marshalErr := json.Marshal(records)
	if marshalErr != nil {
		return access.Reply{}, marshalErr
	}
	budgetRows, budgetQueryErr := s.Access.Pool.Query(r.Context(), `WITH entries AS MATERIALIZED (
 SELECT ordinality::int AS ordinal,(value->>'project_id')::uuid AS project_id,value->>'digest' AS digest
 FROM jsonb_array_elements($1::jsonb) WITH ORDINALITY
), pairs AS MATERIALIZED (
 SELECT DISTINCT project_id,digest FROM entries WHERE ordinal<=$2
), budgets AS MATERIALIZED (
 SELECT pair.project_id,pair.digest,p.attribution_budgets->'delegation_pair'->pair.digest AS policy,`+limits.DelegationPairBudgetSQL+` AS usage
 FROM pairs pair JOIN olp.projects p ON p.id=pair.project_id
)
 SELECT entries.ordinal,budgets.policy,budgets.usage
 FROM entries LEFT JOIN budgets ON budgets.project_id=entries.project_id AND budgets.digest=entries.digest
 ORDER BY entries.ordinal`, pairInput, page.Limit)
	if budgetQueryErr != nil {
		return access.Reply{}, budgetQueryErr
	}
	items, budgetCollectErr := pgx.CollectRows(budgetRows, func(row pgx.CollectableRow) (DelegationView, error) {
		budget, dataErr := pgx.RowToStructByName[delegationPairBudget](row)
		if dataErr != nil {
			return DelegationView{}, dataErr
		}
		if budget.Usage == nil {
			if budget.Ordinal <= page.Limit {
				return DelegationView{}, pgx.ErrNoRows
			}
			return DelegationView{DelegationRecord: records[budget.Ordinal-1].Record}, nil
		}
		return DelegationView{DelegationRecord: records[budget.Ordinal-1].Record, BudgetPolicy: budget.Policy, Budget: *budget.Usage}, nil
	})
	if budgetCollectErr != nil {
		return access.Reply{}, budgetCollectErr
	}
	return access.ListReplyBy(items, page, func(item DelegationView) string { return item.ID }), nil
}
