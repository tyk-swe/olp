package access

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/limits/ledger"
)

type budgetGroupInput struct {
	LimitTemplate     *string `json:"limit_template"`
	RequestsPerMinute *int64  `json:"requests_per_minute"`
	TokensPerMinute   *int64  `json:"tokens_per_minute"`
	MaxConcurrency    *int64  `json:"max_concurrency"`
	Name              string  `json:"name"`
	ProjectID         *string `json:"project_id"`
	DailyCostLimit    *string `json:"daily_cost_limit"`
	MonthlyCostLimit  *string `json:"monthly_cost_limit"`
	WeeklyCostLimit   *string `json:"weekly_cost_limit"`
	CostBasis         string  `json:"cost_basis,omitempty"`
}

func validateAndNormalizeBudgetGroup(input *budgetGroupInput) error {
	if err := httpkit.ValidText("name", input.Name, 100); err != nil {
		return err
	}
	input.Name = strings.TrimSpace(input.Name)
	if err := keyauthority.ValidateTemplateName(input.LimitTemplate); err != nil {
		return err
	}
	if err := (keyauthority.AdmissionLimits{
		RequestsPerMinute: input.RequestsPerMinute,
		TokensPerMinute:   input.TokensPerMinute,
		MaxConcurrency:    input.MaxConcurrency,
		DailyCostLimit:    input.DailyCostLimit,
		MonthlyCostLimit:  input.MonthlyCostLimit,
		WeeklyCostLimit:   input.WeeklyCostLimit,
		CostBasis:         input.CostBasis,
	}).ValidateAndNormalize(); err != nil {
		return err
	}
	if input.CostBasis == "" {
		input.CostBasis = "cost"
	}
	if input.LimitTemplate == nil && input.RequestsPerMinute == nil && input.TokensPerMinute == nil && input.MaxConcurrency == nil && input.DailyCostLimit == nil && input.MonthlyCostLimit == nil && input.WeeklyCostLimit == nil {
		return httpkit.Invalid("daily_cost_limit", "Set at least one limit or reference a template.")
	}
	return nil
}

const budgetGroupFields = `'limit_template',g.limit_template,'requests_per_minute',g.requests_per_minute,'tokens_per_minute',g.tokens_per_minute,'max_concurrency',g.max_concurrency,'effective_limits',g.effective_limits,'id',g.id,'name',g.name,'project_id',g.project_id,'project_name',pr.name,'daily_cost_limit',g.daily_cost_limit::text,'monthly_cost_limit',g.monthly_cost_limit::text,'weekly_cost_limit',g.weekly_cost_limit::text,'cost_basis',g.cost_basis,'created_by',g.created_by,'created_by_email',u.email,'etag',g.etag,'created_at',g.created_at,'updated_at',g.updated_at`
const budgetGroupFrom = " FROM olp.budget_groups_with_limits g JOIN olp.users u ON u.id=g.created_by LEFT JOIN olp.projects pr ON pr.id=g.project_id"

func (s *Server) budgetGroupJSON() string {
	enforcement := "false"
	if s.limitsEnforced {
		enforcement = "true"
	}
	return `jsonb_build_object(` + budgetGroupFields + `,'budget',olp.budget_allowances(g.id,g.effective_limits,` + ledger.GroupBudgetSQL("g") +
		`||jsonb_build_object('enforcement_active',` + enforcement + `)))`
}

func (s *Server) budgetGroups(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := s.Pool.Query(r.Context(), "SELECT "+s.budgetGroupJSON()+budgetGroupFrom+" WHERE g.id<$1 AND ($2 OR g.project_id=ANY($3::uuid[])) ORDER BY g.id DESC LIMIT $4", page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	items, err := httpkit.JSONRows(rows)
	return httpkit.ListReply(items, page), err
}

func (s *Server) budgetGroup(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "budget_group_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var data []byte
	var etag string
	var projectID *string
	if err = s.Pool.QueryRow(r.Context(), "SELECT "+s.budgetGroupJSON()+",g.etag::text,g.project_id::text"+budgetGroupFrom+" WHERE g.id=$1", id).Scan(&data, &etag, &projectID); err != nil {
		return httpkit.Reply{}, err
	}
	if err := p.Project(projectID, authz.View); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(json.RawMessage(data), etag), nil
}

func (s *Server) createBudgetGroup(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var input budgetGroupInput
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	projectID, err := parseProjectID(input.ProjectID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	input.ProjectID = projectID
	return Mutate(s, r, input, func(tx pgx.Tx, p authz.Principal) (struct{}, error) {
		return struct{}{}, s.RequireProject(r.Context(), tx, p, input.ProjectID)
	}, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, Event, error) {
		if err := validateAndNormalizeBudgetGroup(&input); err != nil {
			return httpkit.Reply{}, Event{}, err
		}
		if err := CheckLimitTemplates(r.Context(), tx, input.ProjectID, input.LimitTemplate); err != nil {
			return httpkit.Reply{}, Event{}, err
		}
		id, etag := httpkit.NewID(), httpkit.NewID()
		_, err := tx.Exec(r.Context(), "INSERT INTO olp.budget_groups(id,name,project_id,daily_cost_limit,monthly_cost_limit,etag,created_by,limit_template,requests_per_minute,tokens_per_minute,max_concurrency,weekly_cost_limit,cost_basis) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)", id, input.Name, input.ProjectID, input.DailyCostLimit, input.MonthlyCostLimit, etag, p.UserID(), input.LimitTemplate, input.RequestsPerMinute, input.TokensPerMinute, input.MaxConcurrency, input.WeeklyCostLimit, input.CostBasis)
		reply := httpkit.Reply{Status: 201, ETag: etag, Location: "/api/v1/budget-groups/" + id, Body: map[string]any{"id": id, "etag": etag}}
		return reply, Event{Action: "budget_group.create", Resource: "budget_group", ID: id}, err
	})
}

func (s *Server) updateBudgetGroup(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var patch map[string]json.RawMessage
	if err := httpkit.Decode(r, &patch); err != nil {
		return httpkit.Reply{}, err
	}
	if patch == nil {
		return httpkit.Reply{}, httpkit.Invalid("budget_group", "Send a budget group object.")
	}
	allowed := []string{"name", "daily_cost_limit", "monthly_cost_limit", "weekly_cost_limit", "cost_basis", "limit_template", "requests_per_minute", "tokens_per_minute", "max_concurrency"}
	for field, value := range patch {
		if !slices.Contains(allowed, field) {
			return httpkit.Reply{}, httpkit.Invalid(field, "Unknown budget group field.")
		}
		if string(value) == "null" && field == "name" {
			return httpkit.Reply{}, httpkit.Invalid(field, "This field cannot be null.")
		}
	}
	id, err := httpkit.IDParam(r, "budget_group_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return Transact(s, r, func(tx pgx.Tx, p authz.Principal) (budgetGroupState, error) {
		var etag string
		var current budgetGroupState
		if err := tx.QueryRow(r.Context(), "SELECT etag::text,project_id::text,jsonb_build_object('limit_template',limit_template,'requests_per_minute',requests_per_minute,'tokens_per_minute',tokens_per_minute,'max_concurrency',max_concurrency,'name',name,'daily_cost_limit',daily_cost_limit::text,'monthly_cost_limit',monthly_cost_limit::text,'weekly_cost_limit',weekly_cost_limit::text,'cost_basis',cost_basis) FROM olp.budget_groups WHERE id=$1 AND retired_at IS NULL", id).Scan(&etag, &current.projectID, &current.fields); err != nil {
			return budgetGroupState{}, err
		}
		if err := p.Project(current.projectID, authz.Change); err != nil {
			return budgetGroupState{}, err
		}
		return current, httpkit.Match(r, etag)
	}, func(tx pgx.Tx, _ authz.Principal, current budgetGroupState) (httpkit.Reply, Event, error) {
		input, err := current.patched(patch)
		if err != nil {
			return httpkit.Reply{}, Event{}, err
		}
		if err := CheckLimitTemplates(r.Context(), tx, current.projectID, input.LimitTemplate); err != nil {
			return httpkit.Reply{}, Event{}, err
		}
		etag := httpkit.NewID()
		if _, err := tx.Exec(r.Context(), "UPDATE olp.budget_groups SET name=$1,daily_cost_limit=$2,monthly_cost_limit=$3,etag=$4,updated_at=now(),limit_template=$6,requests_per_minute=$7,tokens_per_minute=$8,max_concurrency=$9,weekly_cost_limit=$10,cost_basis=$11 WHERE id=$5", input.Name, input.DailyCostLimit, input.MonthlyCostLimit, etag, id, input.LimitTemplate, input.RequestsPerMinute, input.TokensPerMinute, input.MaxConcurrency, input.WeeklyCostLimit, input.CostBasis); err != nil {
			return httpkit.Reply{}, Event{}, err
		}
		_, err = AdvanceAuthority(r.Context(), tx)
		return httpkit.Detail(map[string]any{"etag": etag}, etag), Event{Action: "budget_group.update", Resource: "budget_group", ID: id}, err
	})
}

type budgetGroupState struct {
	projectID *string
	fields    []byte
}

func (g budgetGroupState) patched(patch map[string]json.RawMessage) (budgetGroupInput, error) {
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(g.fields, &merged); err != nil {
		return budgetGroupInput{}, err
	}
	maps.Copy(merged, patch)
	data, err := json.Marshal(merged)
	if err != nil {
		return budgetGroupInput{}, err
	}
	var input budgetGroupInput
	if err := json.Unmarshal(data, &input); err != nil {
		return budgetGroupInput{}, httpkit.Invalid("budget_group", "Invalid budget group value.")
	}
	return input, validateAndNormalizeBudgetGroup(&input)
}

func checkBudgetGroup(ctx context.Context, q database.Queryer, groupID, projectID *string) error {
	if groupID == nil {
		return nil
	}
	var groupProject *string
	if err := q.QueryRow(ctx, "SELECT project_id::text FROM olp.budget_groups WHERE id=$1 AND retired_at IS NULL", *groupID).Scan(&groupProject); err != nil {
		return err
	}
	if (groupProject == nil) != (projectID == nil) || (groupProject != nil && *groupProject != *projectID) {
		return httpkit.Invalid("budget_group_id", "Budget group must belong to the key's project.")
	}
	return nil
}
