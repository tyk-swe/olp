package access

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/limits"
)

type budgetGroupInput struct {
	Name             string  `json:"name"`
	ProjectID        *string `json:"project_id"`
	DailyCostLimit   *string `json:"daily_cost_limit"`
	MonthlyCostLimit *string `json:"monthly_cost_limit"`
}

func validateBudgetGroup(input *budgetGroupInput) error {
	if err := ValidText("name", input.Name, 100); err != nil {
		return err
	}
	input.Name = strings.TrimSpace(input.Name)
	for field, value := range map[string]*string{"daily_cost_limit": input.DailyCostLimit, "monthly_cost_limit": input.MonthlyCostLimit} {
		if value != nil {
			amount := strings.TrimSpace(*value)
			if !decimal.MatchString(amount) || !strings.ContainsAny(amount, "123456789") {
				return Invalid(field, "Use a positive decimal amount with at most 12 integer and 12 fractional digits.")
			}
			*value = amount
		}
	}
	if input.DailyCostLimit == nil && input.MonthlyCostLimit == nil {
		return Invalid("daily_cost_limit", "Set at least one cost limit.")
	}
	return nil
}

const budgetGroupFields = `'id',g.id,'name',g.name,'project_id',g.project_id,'project_name',pr.name,'daily_cost_limit',g.daily_cost_limit::text,'monthly_cost_limit',g.monthly_cost_limit::text,'created_by',g.created_by,'created_by_email',u.email,'etag',g.etag,'created_at',g.created_at,'updated_at',g.updated_at`
const budgetGroupFrom = " FROM olp_go.budget_groups g JOIN olp_go.users u ON u.id=g.created_by LEFT JOIN olp_go.projects pr ON pr.id=g.project_id"

func (s *Server) budgetGroupJSON() string {
	enforcement := "false"
	if s.LimitsEnforced {
		enforcement = "true"
	}
	return `jsonb_build_object(` + budgetGroupFields + `,'budget',` + limits.GroupBudgetSQL +
		`||jsonb_build_object('enforcement_active',` + enforcement + `))`
}

func (s *Server) budgetGroups(r *http.Request) (Reply, error) {
	p, err := s.Principal(r, s.Pool, "read")
	if err != nil {
		return Reply{}, err
	}
	page, err := Page(r)
	if err != nil {
		return Reply{}, err
	}
	rows, err := s.Pool.Query(r.Context(), "SELECT "+s.budgetGroupJSON()+budgetGroupFrom+" WHERE g.id<$1 AND ($2 OR g.project_id=ANY($3::uuid[])) ORDER BY g.id DESC LIMIT $4", page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return Reply{}, err
	}
	items, err := JSONRows(rows)
	return ListReply(items, page), err
}

func (s *Server) budgetGroup(r *http.Request) (Reply, error) {
	p, err := s.Principal(r, s.Pool, "read")
	if err != nil {
		return Reply{}, err
	}
	id, err := IDParam(r, "budget_group_id")
	if err != nil {
		return Reply{}, err
	}
	var data []byte
	var etag string
	var projectID *string
	if err = s.Pool.QueryRow(r.Context(), "SELECT "+s.budgetGroupJSON()+",g.etag::text,g.project_id::text"+budgetGroupFrom+" WHERE g.id=$1", id).Scan(&data, &etag, &projectID); err != nil {
		return Reply{}, err
	}
	if !p.CanProject(projectID, false) {
		return Reply{}, pgx.ErrNoRows
	}
	return Detail(json.RawMessage(data), etag), nil
}

func (s *Server) createBudgetGroup(r *http.Request) (Reply, error) {
	var input budgetGroupInput
	if err := Decode(r, &input); err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Principal(r, tx, "keys")
	if err != nil {
		return Reply{}, err
	}
	claim, replayed, err := s.Replay(r, tx, p, input)
	if err != nil {
		return Reply{}, err
	}
	if replayed != nil {
		return Commit(r, tx, *replayed)
	}
	if err := validateBudgetGroup(&input); err != nil {
		return Reply{}, err
	}
	if err = s.RequireProject(r.Context(), tx, p, input.ProjectID, true); err != nil {
		return Reply{}, err
	}
	id, etag := NewID(), NewID()
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp_go.budget_groups(id,name,project_id,daily_cost_limit,monthly_cost_limit,etag,created_by) VALUES($1,$2,$3,$4,$5,$6,$7)", id, input.Name, input.ProjectID, input.DailyCostLimit, input.MonthlyCostLimit, etag, p.UserID()); err != nil {
		return Reply{}, err
	}
	result := Reply{Status: 201, ETag: etag, Location: "/api/v1/budget-groups/" + id, Body: map[string]any{"id": id, "etag": etag}}
	if err = Audit(r.Context(), tx, r, p.ID, "budget_group.create", "budget_group", id, "success"); err != nil {
		return Reply{}, err
	}
	if err = s.CompleteReplay(r, tx, claim, result); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, result)
}

func (s *Server) updateBudgetGroup(r *http.Request) (Reply, error) {
	var patch map[string]json.RawMessage
	if err := Decode(r, &patch); err != nil {
		return Reply{}, err
	}
	if patch == nil {
		return Reply{}, Invalid("budget_group", "Send a budget group object.")
	}
	allowed := []string{"name", "daily_cost_limit", "monthly_cost_limit"}
	for field, value := range patch {
		if !slices.Contains(allowed, field) {
			return Reply{}, Invalid(field, "Unknown budget group field.")
		}
		if string(value) == "null" && field == "name" {
			return Reply{}, Invalid(field, "This field cannot be null.")
		}
	}
	id, err := IDParam(r, "budget_group_id")
	if err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Principal(r, tx, "keys")
	if err != nil {
		return Reply{}, err
	}
	var etag string
	var projectID *string
	var data []byte
	if err = tx.QueryRow(r.Context(), "SELECT etag::text,project_id::text,jsonb_build_object('name',name,'daily_cost_limit',daily_cost_limit::text,'monthly_cost_limit',monthly_cost_limit::text) FROM olp_go.budget_groups WHERE id=$1", id).Scan(&etag, &projectID, &data); err != nil {
		return Reply{}, err
	}
	if err := ProjectAccess(p, projectID, true); err != nil {
		return Reply{}, err
	}
	if err = Match(r, etag); err != nil {
		return Reply{}, err
	}
	var merged map[string]json.RawMessage
	if err = json.Unmarshal(data, &merged); err != nil {
		return Reply{}, err
	}
	maps.Copy(merged, patch)
	data, err = json.Marshal(merged)
	if err != nil {
		return Reply{}, err
	}
	var input budgetGroupInput
	if err = json.Unmarshal(data, &input); err != nil {
		return Reply{}, Invalid("budget_group", "Invalid budget group value.")
	}
	if err = validateBudgetGroup(&input); err != nil {
		return Reply{}, err
	}
	etag = NewID()
	if _, err = tx.Exec(r.Context(), "UPDATE olp_go.budget_groups SET name=$1,daily_cost_limit=$2,monthly_cost_limit=$3,etag=$4,updated_at=now() WHERE id=$5", input.Name, input.DailyCostLimit, input.MonthlyCostLimit, etag, id); err != nil {
		return Reply{}, err
	}
	if _, err = AdvanceAuthority(r, tx); err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.ID, "budget_group.update", "budget_group", id, "success"); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, Detail(map[string]any{"etag": etag}, etag))
}

func checkBudgetGroup(ctx context.Context, q Queryer, groupID, projectID *string) error {
	if groupID == nil {
		return nil
	}
	var groupProject *string
	if err := q.QueryRow(ctx, "SELECT project_id::text FROM olp_go.budget_groups WHERE id=$1", *groupID).Scan(&groupProject); err != nil {
		return err
	}
	if (groupProject == nil) != (projectID == nil) || (groupProject != nil && *groupProject != *projectID) {
		return Invalid("budget_group_id", "Budget group must belong to the key's project.")
	}
	return nil
}
