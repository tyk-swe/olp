package agents

import (
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

func (r delegationRow) record() DelegationRecord {
	if r.MemberID == nil {
		return r.DelegationRecord
	}
	authorized := false
	if r.MemberActive != nil && *r.MemberActive && r.MemberOIDCAuthorized != nil && *r.MemberOIDCAuthorized && r.MemberRole != nil && r.MemberScope != nil {
		projectRole := ""
		if r.ProjectRole != nil {
			projectRole = *r.ProjectRole
		}
		principal := access.Principal{User: access.User{Role: *r.MemberRole}, Kind: "user", AllProjects: *r.MemberScope == "global", Projects: map[string]string{r.ProjectID: projectRole}}
		authorized = principal.Authorize(access.Keys) == nil && principal.Project(&r.ProjectID, access.Change) == nil
	}
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

func (s *Server) listDelegations(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	project, err := projectFilter(r, p)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), `SELECT d.id::text AS id,d.project_id::text AS project_id,d.api_key_id::text AS api_key_id,
 d.agent_id::text AS agent_id,d.consenting_user_id::text AS member_id,(d.consent_digest IS NOT NULL) AS consent_pending,
 d.expires_at,d.consumed_at,d.revoked_at,k.revoked_at AS api_key_revoked_at,a.revoked_at AS agent_revoked_at,
 u.active AS member_active,u.oidc_authorized AS member_oidc_authorized,u.role AS member_role,u.access_scope AS member_scope,m.role AS project_role
 FROM olp.agent_delegations d JOIN olp.api_keys k ON k.id=d.api_key_id JOIN olp.api_keys a ON a.id=d.agent_id
 LEFT JOIN olp.users u ON u.id=d.consenting_user_id LEFT JOIN olp.effective_project_members m ON m.user_id=u.id AND m.project_id=d.project_id
 WHERE d.id<$1 AND ($2 OR d.project_id=ANY($3::uuid[])) AND ($4='' OR d.project_id=NULLIF($4,'')::uuid)
 ORDER BY d.id DESC LIMIT $5`, page.Before, p.AllProjects, p.ProjectIDs(), project, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	rowsData, err := pgx.CollectRows(rows, pgx.RowToStructByName[delegationRow])
	if err != nil {
		return access.Reply{}, err
	}
	records := make([]DelegationRecord, 0, len(rowsData))
	for _, row := range rowsData {
		records = append(records, row.record())
	}
	type pairKey struct{ project, digest string }
	type pairBudget struct {
		policy *access.BudgetPolicy
		usage  PairBudget
	}
	batch := &pgx.Batch{}
	keys := make([]pairKey, 0, len(records))
	indices := make(map[pairKey]int, len(records))
	for i, record := range records {
		if i >= page.Limit {
			break
		}
		key := pairKey{record.ProjectID, attribution.DelegationPair(s.Access.Auth, record.ProjectID, record.APIKeyID, record.AgentID, record.MemberID)}
		if _, ok := indices[key]; !ok {
			indices[key] = len(keys)
			keys = append(keys, key)
			batch.Queue(`SELECT p.attribution_budgets->'delegation_pair'->$2, `+limits.DelegationPairBudgetSQL+` FROM olp.projects p WHERE p.id=$1`, key.project, key.digest)
		}
	}
	pairs := make([]pairBudget, 0, len(keys))
	if len(keys) > 0 {
		results := s.Access.Pool.SendBatch(r.Context(), batch)
		for range keys {
			var result pairBudget
			if err := results.QueryRow().Scan(&result.policy, &result.usage); err != nil {
				_ = results.Close()
				return access.Reply{}, err
			}
			pairs = append(pairs, result)
		}
		if err := results.Close(); err != nil {
			return access.Reply{}, err
		}
	}
	items := make([]DelegationView, 0, len(records))
	for _, record := range records {
		key := pairKey{record.ProjectID, attribution.DelegationPair(s.Access.Auth, record.ProjectID, record.APIKeyID, record.AgentID, record.MemberID)}
		var result pairBudget
		if index, ok := indices[key]; ok {
			result = pairs[index]
		}
		items = append(items, DelegationView{DelegationRecord: record, BudgetPolicy: result.policy, Budget: result.usage})
	}
	return access.ListReplyBy(items, page, func(item DelegationView) string { return item.ID }), nil
}
