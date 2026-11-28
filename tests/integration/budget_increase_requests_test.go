//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/budgets"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/ledger"
	"github.com/tyk-swe/olp/internal/projects"
	"github.com/tyk-swe/olp/tests/harness"
)

type budgetRequestBody struct {
	ID          string  `json:"id"`
	ProjectID   string  `json:"project_id"`
	Status      string  `json:"status"`
	Amount      string  `json:"amount"`
	BaseCap     string  `json:"base_cap"`
	RequestedBy string  `json:"requested_by"`
	IncreaseID  *string `json:"increase_id"`
}

type budgetRequestFixture struct {
	pool                                                           *pgxpool.Pool
	ownerID, requesterOneID, requesterTwoID, managerID, outsiderID string
	owner, requesterOne, requesterTwo, manager, outsider           *managementClient
}

func budgetRequestCaller(t *testing.T, pool *pgxpool.Pool, userID string) *managementClient {
	t.Helper()
	return managementHTTPAs(t, pool, userID, func(mux *http.ServeMux, server *access.Server) {
		budgets.NewAPI(server).Register(mux)
	})
}

func newBudgetRequestFixture(t *testing.T) budgetRequestFixture {
	t.Helper()
	pool, _ := harness.Database(t)
	ownerID := insertBudgetRequestUser(t, pool, "owner", "global")
	requesterOneID := insertBudgetRequestUser(t, pool, "developer", "assigned")
	requesterTwoID := insertBudgetRequestUser(t, pool, "developer", "assigned")
	managerID := insertBudgetRequestUser(t, pool, "operator", "assigned")
	outsiderID := insertBudgetRequestUser(t, pool, "operator", "assigned")
	return budgetRequestFixture{
		pool:    pool,
		ownerID: ownerID, requesterOneID: requesterOneID, requesterTwoID: requesterTwoID, managerID: managerID, outsiderID: outsiderID,
		owner:        budgetRequestCaller(t, pool, ownerID),
		requesterOne: budgetRequestCaller(t, pool, requesterOneID),
		requesterTwo: budgetRequestCaller(t, pool, requesterTwoID),
		manager:      budgetRequestCaller(t, pool, managerID),
		outsider:     budgetRequestCaller(t, pool, outsiderID),
	}
}

func (f budgetRequestFixture) project(t *testing.T, dailyCap string) string {
	t.Helper()
	project := insertBudgetRequestProject(t, f.pool, f.ownerID, dailyCap)
	addBudgetRequestMember(t, f.pool, project, f.requesterOneID, "viewer", f.ownerID)
	addBudgetRequestMember(t, f.pool, project, f.requesterTwoID, "viewer", f.ownerID)
	addBudgetRequestMember(t, f.pool, project, f.managerID, "manager", f.ownerID)
	return project
}

func insertBudgetRequestUser(t *testing.T, pool *pgxpool.Pool, role, scope string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.users(id,email,display_name,role,access_scope,etag) VALUES($1::uuid,$1::text||'@example.test','Budget request member',$2,$3,$1::uuid)`, id, role, scope); err != nil {
		t.Fatal(err)
	}
	return id
}

func insertBudgetRequestProject(t *testing.T, pool *pgxpool.Pool, owner, dailyCap string) string {
	t.Helper()
	id := uuid.NewString()
	policy := fmt.Sprintf(`{"daily_cost_limit":%q}`, dailyCap)
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.projects(id,name,etag,created_by,budget_policy) VALUES($1::uuid,$1::text,$1::uuid,$2,$3::jsonb)`, id, owner, policy); err != nil {
		t.Fatal(err)
	}
	return id
}

func addBudgetRequestMember(t *testing.T, pool *pgxpool.Pool, project, user, role, addedBy string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.project_members(project_id,user_id,role,added_by) VALUES($1,$2,$3,$4)`, project, user, role, addedBy); err != nil {
		t.Fatal(err)
	}
}

func decodeBudgetRequest(t *testing.T, result apiResult) budgetRequestBody {
	t.Helper()
	var body budgetRequestBody
	if err := json.Unmarshal(result.body, &body); err != nil {
		t.Fatalf("decode budget increase request: %v: %s", err, result.body)
	}
	return body
}

func budgetRequestProblem(t *testing.T, result apiResult, code string) {
	t.Helper()
	var problem struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(result.body, &problem); err != nil || problem.Type != "https://openllmproxy.dev/problems/"+code {
		t.Fatalf("problem = %s, want code %s (%v)", result.body, code, err)
	}
}

func createBudgetRequest(t *testing.T, client *managementClient, project, amount string) budgetRequestBody {
	t.Helper()
	body := fmt.Sprintf(`{"project_id":%q,"window":"day","amount":%q,"reason":"launch traffic"}`, project, amount)
	created := expectStatus(t, "create request", client.call(t, "POST", "/api/v1/budget-increase-requests", "", body), http.StatusCreated)
	return decodeBudgetRequest(t, created)
}

func budgetRequestPath(id, action string) string {
	path := "/api/v1/budget-increase-requests/" + id
	if action != "" {
		path += "/" + action
	}
	return path
}

func activeBudgetIncreases(t *testing.T, pool *pgxpool.Pool, project string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.budget_increases WHERE project_id=$1 AND revoked_at IS NULL AND expires_at>now()`, project).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func activeBudgetIncreaseSumIs(t *testing.T, pool *pgxpool.Pool, project, want string) bool {
	t.Helper()
	var matches bool
	if err := pool.QueryRow(t.Context(), `SELECT COALESCE(sum(amount),0)=$2::numeric FROM olp.budget_increases WHERE project_id=$1 AND revoked_at IS NULL AND expires_at>now()`, project, want).Scan(&matches); err != nil {
		t.Fatal(err)
	}
	return matches
}

func expectEnforcedBudgetIncreases(t *testing.T, pool *pgxpool.Pool, project string, wantTotal float64) {
	t.Helper()
	loaded, err := ledger.LoadBudgetIncreases(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	var grants []struct {
		Window    string `json:"window"`
		Amount    string `json:"amount"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal([]byte(loaded[limits.AggregateBudgetID("project", project)]), &grants); err != nil {
		t.Fatalf("enforced increases for project %s: %v", project, err)
	}
	var windowEnd int64
	if err := pool.QueryRow(t.Context(), `SELECT floor(extract(epoch FROM end_at)*1000)::bigint FROM olp.budget_window('day',now())`).Scan(&windowEnd); err != nil {
		t.Fatal(err)
	}
	total := 0.0
	for _, grant := range grants {
		amount, err := strconv.ParseFloat(grant.Amount, 64)
		if err != nil || grant.Window != "day" || grant.ExpiresAt != windowEnd {
			t.Fatalf("enforced increase = %+v, want a day increase expiring at the window end %d", grant, windowEnd)
		}
		total += amount
	}
	if total != wantTotal {
		t.Fatalf("enforced increases total %v, want %v", total, wantTotal)
	}
}

func runBudgetRequestPair(t *testing.T, first, second func() (apiResult, error)) (apiResult, apiResult) {
	t.Helper()
	type outcome struct {
		result apiResult
		err    error
	}
	results := make(chan outcome, 2)
	go func() {
		res, err := first()
		results <- outcome{res, err}
	}()
	go func() {
		res, err := second()
		results <- outcome{res, err}
	}()
	one, two := <-results, <-results
	if one.err != nil {
		t.Fatal(one.err)
	}
	if two.err != nil {
		t.Fatal(two.err)
	}
	return one.result, two.result
}

func TestBudgetIncreaseRequests(t *testing.T) {
	t.Parallel()
	f := newBudgetRequestFixture(t)
	t.Run("concurrent approvals add to the effective cap", func(t *testing.T) {
		t.Parallel()
		testBudgetRequestCombinedCap(t, f)
	})
	t.Run("stale cap and window approvals are refused", func(t *testing.T) {
		t.Parallel()
		testBudgetRequestStale(t, f)
	})
	t.Run("approvals share the active limit with direct grants", func(t *testing.T) {
		t.Parallel()
		testBudgetRequestLimitSplit(t, f)
	})
}

func TestInstallationBudgetWritesReplaceTheStoredPolicy(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	owner := managementHTTPAs(t, pool, insertBudgetRequestUser(t, pool, "owner", "global"), func(mux *http.ServeMux, server *access.Server) {
		projects.New(server).Register(mux)
	})
	const path = "/api/v1/budgets/installation"
	read := func() (string, *keyauthority.BudgetPolicy) {
		t.Helper()
		reply := expectStatus(t, "read installation budget", owner.call(t, "GET", path, "", ""), http.StatusOK)
		var current struct {
			Policy *keyauthority.BudgetPolicy `json:"policy"`
		}
		if err := json.Unmarshal(reply.body, &current); err != nil {
			t.Fatal(err)
		}
		return reply.etag, current.Policy
	}
	initial, _ := read()
	expectStatus(t, "cap installation", owner.call(t, "PUT", path, initial, `{"policy":{"daily_cost_limit":"12.5"}}`), http.StatusOK)
	capped, policy := read()
	if capped == initial || policy == nil || policy.DailyCostLimit == nil || !strings.HasPrefix(*policy.DailyCostLimit, "12.5") {
		t.Fatalf("stored installation budget = %+v with ETag %s after capping at ETag %s", policy, capped, initial)
	}
	expectStatus(t, "clear with a stale ETag", owner.call(t, "PUT", path, initial, `{"policy":null}`), http.StatusPreconditionFailed)
	expectStatus(t, "clear installation budget", owner.call(t, "PUT", path, capped, `{"policy":null}`), http.StatusOK)
	if cleared, policy := read(); cleared == capped || policy != nil {
		t.Fatalf("installation budget = %+v with ETag %s after clearing at ETag %s", policy, cleared, capped)
	}
}

func testBudgetRequestCombinedCap(t *testing.T, f budgetRequestFixture) {
	t.Helper()
	pool := f.pool
	project := f.project(t, "100")
	other := insertBudgetRequestProject(t, pool, f.ownerID, "100")
	addBudgetRequestMember(t, pool, other, f.outsiderID, "manager", f.ownerID)
	owner, requesterOne, requesterTwo, manager, outsider := f.owner, f.requesterOne, f.requesterTwo, f.manager, f.outsider

	own := createBudgetRequest(t, owner, project, "5")
	selfApproval := expectStatus(t, "self approval", owner.call(t, "POST", budgetRequestPath(own.ID, "approve"), "", ""), http.StatusForbidden)
	budgetRequestProblem(t, selfApproval, "budget_increase_request_self_approval")

	twenty := createBudgetRequest(t, requesterOne, project, "20")
	thirty := createBudgetRequest(t, requesterTwo, project, "30")
	if twenty.BaseCap != "100" && twenty.BaseCap != "100.000000000000" || twenty.Status != "pending" || twenty.IncreaseID != nil {
		t.Fatalf("pending twenty request = %+v", twenty)
	}
	if count := activeBudgetIncreases(t, pool, project); count != 0 {
		t.Fatalf("pending requests granted %d increases", count)
	}
	expectStatus(t, "cross-project request", outsider.call(t, "POST", "/api/v1/budget-increase-requests", "", fmt.Sprintf(`{"project_id":%q,"window":"day","amount":"5","reason":"launch traffic"}`, project)), http.StatusNotFound)
	expectStatus(t, "cross-project approval", outsider.call(t, "POST", budgetRequestPath(twenty.ID, "approve"), "", ""), http.StatusNotFound)
	expectStatus(t, "viewer approval", requesterOne.call(t, "POST", budgetRequestPath(thirty.ID, "approve"), "", ""), http.StatusForbidden)
	if count := activeBudgetIncreases(t, pool, project); count != 0 {
		t.Fatalf("rejected approvals granted %d increases", count)
	}

	const approvalKey = "approve-twenty"
	first, second := runBudgetRequestPair(t,
		func() (apiResult, error) {
			return owner.tryCall(t.Context(), "POST", budgetRequestPath(twenty.ID, "approve"), "", "", approvalKey)
		},
		func() (apiResult, error) {
			return manager.tryCall(t.Context(), "POST", budgetRequestPath(thirty.ID, "approve"), "", "")
		},
	)
	approved := map[string]budgetRequestBody{}
	for _, result := range []apiResult{first, second} {
		body := decodeBudgetRequest(t, expectStatus(t, "concurrent approval", result, http.StatusOK))
		if body.Status != "approved" || body.IncreaseID == nil {
			t.Fatalf("concurrent approval = %+v", body)
		}
		approved[body.ID] = body
	}
	if *approved[twenty.ID].IncreaseID == *approved[thirty.ID].IncreaseID {
		t.Fatalf("both approvals granted the same increase %s", *approved[twenty.ID].IncreaseID)
	}
	if count := activeBudgetIncreases(t, pool, project); count != 2 || !activeBudgetIncreaseSumIs(t, pool, project, "50") {
		t.Fatalf("active increases = %d, want two summing to 50", count)
	}
	expectEnforcedBudgetIncreases(t, pool, project, 50)

	for _, replay := range []apiResult{
		owner.call(t, "POST", budgetRequestPath(twenty.ID, "approve"), "", "", approvalKey),
		manager.call(t, "POST", budgetRequestPath(twenty.ID, "approve"), "", ""),
	} {
		body := decodeBudgetRequest(t, expectStatus(t, "approval replay", replay, http.StatusOK))
		if body.IncreaseID == nil || *body.IncreaseID != *approved[twenty.ID].IncreaseID {
			t.Fatalf("approval replay = %+v, want increase %v", body, approved[twenty.ID].IncreaseID)
		}
	}
	if count := activeBudgetIncreases(t, pool, project); count != 2 || !activeBudgetIncreaseSumIs(t, pool, project, "50") {
		t.Fatalf("replays changed the grants, active = %d", count)
	}

	declined := createBudgetRequest(t, requesterOne, project, "10")
	decline := decodeBudgetRequest(t, expectStatus(t, "decline", manager.call(t, "POST", budgetRequestPath(declined.ID, "decline"), "", ""), http.StatusOK))
	if decline.Status != "declined" || decline.IncreaseID != nil {
		t.Fatalf("declined request = %+v", decline)
	}
	decided := expectStatus(t, "approve declined", manager.call(t, "POST", budgetRequestPath(declined.ID, "approve"), "", ""), http.StatusConflict)
	budgetRequestProblem(t, decided, "budget_increase_request_decided")
	if count := activeBudgetIncreases(t, pool, project); count != 2 {
		t.Fatalf("decline flow changed the grants, active = %d", count)
	}

	var approvals, grants int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.audit WHERE action='budget.increase_request.approve' AND resource_id=$1`, twenty.ID).Scan(&approvals); err != nil || approvals != 1 {
		t.Fatalf("approval audit rows = %d, want 1 (%v)", approvals, err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.audit WHERE action='budget.increase.create' AND resource_id=$1`, *approved[twenty.ID].IncreaseID).Scan(&grants); err != nil || grants != 1 {
		t.Fatalf("grant audit rows = %d, want 1 (%v)", grants, err)
	}
}

func testBudgetRequestStale(t *testing.T, f budgetRequestFixture) {
	t.Helper()
	pool := f.pool
	project := f.project(t, "100")
	requester, manager := f.requesterOne, f.manager

	staleCap := createBudgetRequest(t, requester, project, "10")
	if _, err := pool.Exec(t.Context(), `UPDATE olp.projects SET budget_policy='{"daily_cost_limit":"110"}' WHERE id=$1`, project); err != nil {
		t.Fatal(err)
	}
	stale := expectStatus(t, "stale cap approval", manager.call(t, "POST", budgetRequestPath(staleCap.ID, "approve"), "", ""), http.StatusConflict)
	budgetRequestProblem(t, stale, "budget_increase_request_stale")
	stillPending := decodeBudgetRequest(t, expectStatus(t, "read stale cap request", manager.call(t, "GET", budgetRequestPath(staleCap.ID, ""), "", ""), http.StatusOK))
	if stillPending.Status != "pending" {
		t.Fatalf("stale cap request status = %q, want pending", stillPending.Status)
	}

	staleWindow := createBudgetRequest(t, requester, project, "10")
	if _, err := pool.Exec(t.Context(), `UPDATE olp.budget_increase_requests SET window_id=window_id-1,window_ends_at=now()-interval '1 minute' WHERE id=$1`, staleWindow.ID); err != nil {
		t.Fatal(err)
	}
	expired := decodeBudgetRequest(t, expectStatus(t, "read expired request", manager.call(t, "GET", budgetRequestPath(staleWindow.ID, ""), "", ""), http.StatusOK))
	if expired.Status != "expired" {
		t.Fatalf("rolled window request status = %q, want expired", expired.Status)
	}
	stale = expectStatus(t, "stale window approval", manager.call(t, "POST", budgetRequestPath(staleWindow.ID, "approve"), "", ""), http.StatusConflict)
	budgetRequestProblem(t, stale, "budget_increase_request_stale")
	if count := activeBudgetIncreases(t, pool, project); count != 0 {
		t.Fatalf("stale approvals granted %d increases", count)
	}

	sameCap := createBudgetRequest(t, requester, project, "10")
	if _, err := pool.Exec(t.Context(), `UPDATE olp.projects SET budget_policy='{"daily_cost_limit":"110.00"}' WHERE id=$1`, project); err != nil {
		t.Fatal(err)
	}
	approved := decodeBudgetRequest(t, expectStatus(t, "same cap approval", manager.call(t, "POST", budgetRequestPath(sameCap.ID, "approve"), "", ""), http.StatusOK))
	if approved.Status != "approved" || approved.IncreaseID == nil || !activeBudgetIncreaseSumIs(t, pool, project, "10") {
		t.Fatalf("same cap approval = %+v", approved)
	}
}

func seedBudgetRequestDirectGrants(t *testing.T, client *managementClient, project string, grants int) {
	t.Helper()
	for range grants {
		body := fmt.Sprintf(`{"target":{"kind":"project","id":%q},"window":"day","amount":"1","reason":"seed grant"}`, project)
		expectStatus(t, "seed direct grant", client.call(t, "POST", "/api/v1/budget-increases", "", body), http.StatusCreated)
	}
}

func budgetRequestLimitSplit(t *testing.T, results ...apiResult) {
	t.Helper()
	successes, conflicts := 0, 0
	for _, result := range results {
		switch result.status {
		case http.StatusOK, http.StatusCreated:
			successes++
		case http.StatusConflict:
			conflicts++
			budgetRequestProblem(t, result, "budget_increase_limit")
		default:
			t.Fatalf("concurrent grant status %d: %s", result.status, result.body)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent grants = %d successes and %d conflicts, want one each", successes, conflicts)
	}
}

func testBudgetRequestLimitSplit(t *testing.T, f budgetRequestFixture) {
	t.Helper()
	pool := f.pool
	owner, requesterOne, requesterTwo, manager := f.owner, f.requesterOne, f.requesterTwo, f.manager

	approvalsProject := f.project(t, "100")
	seedBudgetRequestDirectGrants(t, owner, approvalsProject, 7)
	first := createBudgetRequest(t, requesterOne, approvalsProject, "20")
	second := createBudgetRequest(t, requesterTwo, approvalsProject, "30")
	one, two := runBudgetRequestPair(t,
		func() (apiResult, error) {
			return owner.tryCall(t.Context(), "POST", budgetRequestPath(first.ID, "approve"), "", "")
		},
		func() (apiResult, error) {
			return manager.tryCall(t.Context(), "POST", budgetRequestPath(second.ID, "approve"), "", "")
		},
	)
	budgetRequestLimitSplit(t, one, two)
	if count := activeBudgetIncreases(t, pool, approvalsProject); count != 8 {
		t.Fatalf("active increases after approval split = %d, want 8", count)
	}
	pending := 0
	for _, id := range []string{first.ID, second.ID} {
		body := decodeBudgetRequest(t, expectStatus(t, "read split request", owner.call(t, "GET", budgetRequestPath(id, ""), "", ""), http.StatusOK))
		if body.Status == "pending" {
			pending++
		}
	}
	if pending != 1 {
		t.Fatalf("pending requests after limit split = %d, want the losing request still pending", pending)
	}

	directProject := f.project(t, "100")
	seedBudgetRequestDirectGrants(t, owner, directProject, 7)
	request := createBudgetRequest(t, requesterOne, directProject, "20")
	directBody := fmt.Sprintf(`{"target":{"kind":"project","id":%q},"window":"day","amount":"5","reason":"urgent grant"}`, directProject)
	one, two = runBudgetRequestPair(t,
		func() (apiResult, error) {
			return owner.tryCall(t.Context(), "POST", "/api/v1/budget-increases", "", directBody)
		},
		func() (apiResult, error) {
			return manager.tryCall(t.Context(), "POST", budgetRequestPath(request.ID, "approve"), "", "")
		},
	)
	budgetRequestLimitSplit(t, one, two)
	if count := activeBudgetIncreases(t, pool, directProject); count != 8 {
		t.Fatalf("active increases after direct and approval split = %d, want 8", count)
	}
}
