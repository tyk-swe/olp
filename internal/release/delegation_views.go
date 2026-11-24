package release

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/limits/ledger"
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
	principal := authz.Principal{User: authz.User{Role: *r.MemberRole}, Kind: "user", AllProjects: *r.MemberScope == "global", Projects: map[string]string{r.ProjectID: projectRole(r.ProjectRole)}}
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

func (s *DelegationManagement) listDelegations(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	page, pageErr := httpkit.Page(r)
	if pageErr != nil {
		return httpkit.Reply{}, pageErr
	}
	project, projectErr := delegationProjectFilter(r, p)
	if projectErr != nil {
		return httpkit.Reply{}, projectErr
	}
	delegation := r.URL.Query().Get("delegation_id")
	if delegation != "" {
		id, err := uuid.Parse(delegation)
		if err != nil || id == uuid.Nil || id.String() != delegation {
			return httpkit.Reply{}, httpkit.Invalid("delegation_id", "Use a canonical delegation UUID.")
		}
	}
	rows, queryErr := s.Access.Pool.Query(r.Context(), `SELECT d.id::text AS id,d.project_id::text AS project_id,d.api_key_id::text AS api_key_id,
 d.agent_id::text AS agent_id,d.consenting_user_id::text AS member_id,(d.consent_digest IS NOT NULL) AS consent_pending,
 d.expires_at,d.consumed_at,d.revoked_at,k.revoked_at AS api_key_revoked_at,a.revoked_at AS agent_revoked_at,
 u.active AS member_active,u.oidc_authorized AS member_oidc_authorized,u.role AS member_role,u.access_scope AS member_scope,m.role AS project_role
 FROM olp.agent_delegations d JOIN olp.api_keys k ON k.id=d.api_key_id JOIN olp.api_keys a ON a.id=d.agent_id
 LEFT JOIN olp.users u ON u.id=d.consenting_user_id LEFT JOIN olp.effective_project_members m ON m.user_id=u.id AND m.project_id=d.project_id
 WHERE d.id<$1 AND ($2 OR d.project_id=ANY($3::uuid[])) AND ($4='' OR d.project_id=NULLIF($4,'')::uuid)
 AND ($6='' OR d.id=NULLIF($6,'')::uuid)
 ORDER BY d.id DESC LIMIT $5`, page.Before, p.AllProjects, p.ProjectIDs(), project, page.Limit+1, delegation)
	if queryErr != nil {
		return httpkit.Reply{}, queryErr
	}
	records, collectErr := pgx.CollectRows(rows, func(row pgx.CollectableRow) (delegationPairRecord, error) {
		data, dataErr := pgx.RowToStructByName[delegationRow](row)
		if dataErr != nil {
			return delegationPairRecord{}, dataErr
		}
		return delegationPairRecord{
			Record: data.record(), ProjectID: data.ProjectID,
			Digest: secrets.DelegationPair(s.Access.Auth, data.ProjectID, data.APIKeyID, data.AgentID, data.MemberID),
		}, nil
	})
	if collectErr != nil {
		return httpkit.Reply{}, collectErr
	}
	if len(records) == 0 {
		return httpkit.ListReplyBy([]DelegationView{}, page, func(item DelegationView) string { return item.ID }), nil
	}
	pairInput, marshalErr := json.Marshal(records)
	if marshalErr != nil {
		return httpkit.Reply{}, marshalErr
	}
	budgetRows, budgetQueryErr := s.Access.Pool.Query(r.Context(), `WITH entries AS MATERIALIZED (
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
 ORDER BY entries.ordinal`, pairInput, page.Limit)
	if budgetQueryErr != nil {
		return httpkit.Reply{}, budgetQueryErr
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
		return httpkit.Reply{}, budgetCollectErr
	}
	return httpkit.ListReplyBy(items, page, func(item DelegationView) string { return item.ID }), nil
}

func delegationProjectFilter(r *http.Request, p authz.Principal) (string, error) {
	project := r.URL.Query().Get("project_id")
	if project == "" {
		return "", nil
	}
	id, err := uuid.Parse(project)
	if err != nil || id == uuid.Nil || id.String() != project {
		return "", httpkit.Invalid("project_id", "Use a canonical project UUID.")
	}
	if err := p.Project(&project, authz.View); err != nil {
		return "", err
	}
	return project, nil
}
