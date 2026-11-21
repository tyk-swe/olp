//go:build integration

package integration_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/budgets"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/platform/secrets"
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

type budgetRequestServers struct {
	pool         *pgxpool.Pool
	auth         *secrets.AuthKey
	keys         *secrets.KeyRing
	installation string
}

func newBudgetRequestServers(t *testing.T, pool *pgxpool.Pool) budgetRequestServers {
	t.Helper()
	keys, installation := notificationKeys(t, pool)
	auth := secrets.NewAuthKey(make([]byte, 32), installation)
	if err := access.PrepareInstallation(t.Context(), pool, installation, auth, keys); err != nil {
		t.Fatal(err)
	}
	return budgetRequestServers{pool: pool, auth: auth, keys: keys, installation: installation}
}

func (s budgetRequestServers) caller(t *testing.T, userID string) notificationCaller {
	t.Helper()
	ctx := t.Context()
	token := uuid.NewString()
	if _, err := s.pool.Exec(ctx, `INSERT INTO olp.sessions(id,user_id,digest,expires_at,browser_hint) VALUES($1,$2,$3,now()+interval '1 hour','test')`, uuid.NewString(), userID, s.auth.Digest(secrets.SessionDigest, token)); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	budgets.NewAPI(access.New(access.Config{Pool: s.pool, Installation: s.installation, Origin: "https://test.example", Auth: s.auth, Keys: s.keys, Log: slog.New(slog.DiscardHandler)})).Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	csrf := base64.RawURLEncoding.EncodeToString(s.auth.Digest(secrets.CSRFDigest, token))
	return func(method, path, ifMatch, body string, idempotencyKey ...string) apiResult {
		t.Helper()
		r, err := http.NewRequestWithContext(ctx, method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		key := uuid.NewString()
		if len(idempotencyKey) > 0 && idempotencyKey[0] != "" {
			key = idempotencyKey[0]
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "https://test.example")
		r.Header.Set("Idempotency-Key", key)
		r.Header.Set("X-CSRF-Token", csrf)
		if ifMatch != "" {
			r.Header.Set("If-Match", ifMatch)
		}
		r.AddCookie(&http.Cookie{Name: "__Host-olp_session", Value: token})
		response, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return apiResult{status: response.StatusCode, etag: response.Header.Get("ETag"), body: raw}
	}
}

type budgetRequestFixture struct {
	pool                                                           *pgxpool.Pool
	ownerID, requesterOneID, requesterTwoID, managerID, outsiderID string
	owner, requesterOne, requesterTwo, manager, outsider           notificationCaller
}

func newBudgetRequestFixture(t *testing.T) budgetRequestFixture {
	t.Helper()
	pool, _ := harness.Database(t)
	servers := newBudgetRequestServers(t, pool)
	f := budgetRequestFixture{
		pool:           pool,
		ownerID:        insertBudgetRequestUser(t, pool, "owner", "global"),
		requesterOneID: insertBudgetRequestUser(t, pool, "developer", "assigned"),
		requesterTwoID: insertBudgetRequestUser(t, pool, "developer", "assigned"),
		managerID:      insertBudgetRequestUser(t, pool, "operator", "assigned"),
		outsiderID:     insertBudgetRequestUser(t, pool, "operator", "assigned"),
	}
	f.owner = servers.caller(t, f.ownerID)
	f.requesterOne = servers.caller(t, f.requesterOneID)
	f.requesterTwo = servers.caller(t, f.requesterTwoID)
	f.manager = servers.caller(t, f.managerID)
	f.outsider = servers.caller(t, f.outsiderID)
	return f
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

func expectBudgetRequestStatus(t *testing.T, step string, got apiResult, status int) apiResult {
	t.Helper()
	if got.status != status {
		t.Fatalf("%s: HTTP %d, want %d: %s", step, got.status, status, got.body)
	}
	return got
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

func createBudgetRequest(t *testing.T, call notificationCaller, project, amount string) budgetRequestBody {
	t.Helper()
	body := fmt.Sprintf(`{"project_id":%q,"window":"day","amount":%q,"reason":"launch traffic"}`, project, amount)
	created := expectBudgetRequestStatus(t, "create request", call("POST", "/api/v1/budget-increase-requests", "", body), http.StatusCreated)
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

func runBudgetRequestPair(first, second func() apiResult) (apiResult, apiResult) {
	results := make(chan apiResult, 2)
	go func() { results <- first() }()
	go func() { results <- second() }()
	return <-results, <-results
}

func TestBudgetIncreaseRequests(t *testing.T) {
	t.Parallel()
	f := newBudgetRequestFixture(t)
	t.Run("concurrent approvals add to the effective cap", func(t *testing.T) {
		testBudgetRequestCombinedCap(t, f)
	})
	t.Run("stale cap and window approvals are refused", func(t *testing.T) {
		testBudgetRequestStale(t, f)
	})
	t.Run("approvals share the active limit with direct grants", func(t *testing.T) {
		testBudgetRequestLimitSplit(t, f)
	})
}

func testBudgetRequestCombinedCap(t *testing.T, f budgetRequestFixture) {
	t.Helper()
	pool := f.pool
	project := f.project(t, "100")
	other := insertBudgetRequestProject(t, pool, f.ownerID, "100")
	addBudgetRequestMember(t, pool, other, f.outsiderID, "manager", f.ownerID)
	owner, requesterOne, requesterTwo, manager, outsider := f.owner, f.requesterOne, f.requesterTwo, f.manager, f.outsider

	own := createBudgetRequest(t, owner, project, "5")
	selfApproval := expectBudgetRequestStatus(t, "self approval", owner("POST", budgetRequestPath(own.ID, "approve"), "", ""), http.StatusForbidden)
	budgetRequestProblem(t, selfApproval, "budget_increase_request_self_approval")

	twenty := createBudgetRequest(t, requesterOne, project, "20")
	thirty := createBudgetRequest(t, requesterTwo, project, "30")
	if twenty.BaseCap != "100" && twenty.BaseCap != "100.000000000000" || twenty.Status != "pending" || twenty.IncreaseID != nil {
		t.Fatalf("pending twenty request = %+v", twenty)
	}
	if count := activeBudgetIncreases(t, pool, project); count != 0 {
		t.Fatalf("pending requests granted %d increases", count)
	}
	expectBudgetRequestStatus(t, "cross-project approval", outsider("POST", budgetRequestPath(twenty.ID, "approve"), "", ""), http.StatusNotFound)
	expectBudgetRequestStatus(t, "viewer approval", requesterOne("POST", budgetRequestPath(thirty.ID, "approve"), "", ""), http.StatusForbidden)
	if count := activeBudgetIncreases(t, pool, project); count != 0 {
		t.Fatalf("rejected approvals granted %d increases", count)
	}

	const approvalKey = "approve-twenty"
	first, second := runBudgetRequestPair(
		func() apiResult { return owner("POST", budgetRequestPath(twenty.ID, "approve"), "", "", approvalKey) },
		func() apiResult { return manager("POST", budgetRequestPath(thirty.ID, "approve"), "", "") },
	)
	approved := map[string]budgetRequestBody{}
	for _, result := range []apiResult{first, second} {
		body := decodeBudgetRequest(t, expectBudgetRequestStatus(t, "concurrent approval", result, http.StatusOK))
		if body.Status != "approved" || body.IncreaseID == nil {
			t.Fatalf("concurrent approval = %+v", body)
		}
		approved[body.ID] = body
	}
	if approved[twenty.ID].IncreaseID == approved[thirty.ID].IncreaseID {
		t.Fatalf("both approvals granted the same increase %v", approved[twenty.ID].IncreaseID)
	}
	if count := activeBudgetIncreases(t, pool, project); count != 2 || !activeBudgetIncreaseSumIs(t, pool, project, "50") {
		t.Fatalf("active increases = %d, want two summing to 50", count)
	}
	ownerBudget := limits.AggregateBudgetID("project", project)
	var effectiveIs150 bool
	if err := pool.QueryRow(t.Context(), `SELECT 100+olp.active_budget_increase($1::uuid,'day',(SELECT window_id FROM olp.budget_window('day',now())),now())=150`, ownerBudget).Scan(&effectiveIs150); err != nil || !effectiveIs150 {
		t.Fatalf("effective cap is not 150 (%v)", err)
	}

	for _, replay := range []apiResult{
		owner("POST", budgetRequestPath(twenty.ID, "approve"), "", "", approvalKey),
		manager("POST", budgetRequestPath(twenty.ID, "approve"), "", ""),
	} {
		body := decodeBudgetRequest(t, expectBudgetRequestStatus(t, "approval replay", replay, http.StatusOK))
		if body.IncreaseID == nil || *body.IncreaseID != *approved[twenty.ID].IncreaseID {
			t.Fatalf("approval replay = %+v, want increase %v", body, approved[twenty.ID].IncreaseID)
		}
	}
	if count := activeBudgetIncreases(t, pool, project); count != 2 || !activeBudgetIncreaseSumIs(t, pool, project, "50") {
		t.Fatalf("replays changed the grants, active = %d", count)
	}

	declined := createBudgetRequest(t, requesterOne, project, "10")
	decline := decodeBudgetRequest(t, expectBudgetRequestStatus(t, "decline", manager("POST", budgetRequestPath(declined.ID, "decline"), "", ""), http.StatusOK))
	if decline.Status != "declined" || decline.IncreaseID != nil {
		t.Fatalf("declined request = %+v", decline)
	}
	decided := expectBudgetRequestStatus(t, "approve declined", manager("POST", budgetRequestPath(declined.ID, "approve"), "", ""), http.StatusConflict)
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
	stale := expectBudgetRequestStatus(t, "stale cap approval", manager("POST", budgetRequestPath(staleCap.ID, "approve"), "", ""), http.StatusConflict)
	budgetRequestProblem(t, stale, "budget_increase_request_stale")
	stillPending := decodeBudgetRequest(t, expectBudgetRequestStatus(t, "read stale cap request", manager("GET", budgetRequestPath(staleCap.ID, ""), "", ""), http.StatusOK))
	if stillPending.Status != "pending" {
		t.Fatalf("stale cap request status = %q, want pending", stillPending.Status)
	}

	staleWindow := createBudgetRequest(t, requester, project, "10")
	if _, err := pool.Exec(t.Context(), `UPDATE olp.budget_increase_requests SET window_id=window_id-1,window_ends_at=now()-interval '1 minute' WHERE id=$1`, staleWindow.ID); err != nil {
		t.Fatal(err)
	}
	expired := decodeBudgetRequest(t, expectBudgetRequestStatus(t, "read expired request", manager("GET", budgetRequestPath(staleWindow.ID, ""), "", ""), http.StatusOK))
	if expired.Status != "expired" {
		t.Fatalf("rolled window request status = %q, want expired", expired.Status)
	}
	stale = expectBudgetRequestStatus(t, "stale window approval", manager("POST", budgetRequestPath(staleWindow.ID, "approve"), "", ""), http.StatusConflict)
	budgetRequestProblem(t, stale, "budget_increase_request_stale")
	if count := activeBudgetIncreases(t, pool, project); count != 0 {
		t.Fatalf("stale approvals granted %d increases", count)
	}

	sameCap := createBudgetRequest(t, requester, project, "10")
	if _, err := pool.Exec(t.Context(), `UPDATE olp.projects SET budget_policy='{"daily_cost_limit":"110.00"}' WHERE id=$1`, project); err != nil {
		t.Fatal(err)
	}
	approved := decodeBudgetRequest(t, expectBudgetRequestStatus(t, "same cap approval", manager("POST", budgetRequestPath(sameCap.ID, "approve"), "", ""), http.StatusOK))
	if approved.Status != "approved" || approved.IncreaseID == nil || !activeBudgetIncreaseSumIs(t, pool, project, "10") {
		t.Fatalf("same cap approval = %+v", approved)
	}
}

func seedBudgetRequestDirectGrants(t *testing.T, call notificationCaller, project string, grants int) {
	t.Helper()
	for range grants {
		body := fmt.Sprintf(`{"target":{"kind":"project","id":%q},"window":"day","amount":"1","reason":"seed grant"}`, project)
		expectBudgetRequestStatus(t, "seed direct grant", call("POST", "/api/v1/budget-increases", "", body), http.StatusCreated)
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
	one, two := runBudgetRequestPair(
		func() apiResult { return owner("POST", budgetRequestPath(first.ID, "approve"), "", "") },
		func() apiResult { return manager("POST", budgetRequestPath(second.ID, "approve"), "", "") },
	)
	budgetRequestLimitSplit(t, one, two)
	if count := activeBudgetIncreases(t, pool, approvalsProject); count != 8 {
		t.Fatalf("active increases after approval split = %d, want 8", count)
	}
	pending := 0
	for _, id := range []string{first.ID, second.ID} {
		body := decodeBudgetRequest(t, expectBudgetRequestStatus(t, "read split request", owner("GET", budgetRequestPath(id, ""), "", ""), http.StatusOK))
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
	one, two = runBudgetRequestPair(
		func() apiResult { return owner("POST", "/api/v1/budget-increases", "", directBody) },
		func() apiResult { return manager("POST", budgetRequestPath(request.ID, "approve"), "", "") },
	)
	budgetRequestLimitSplit(t, one, two)
	if count := activeBudgetIncreases(t, pool, directProject); count != 8 {
		t.Fatalf("active increases after direct and approval split = %d, want 8", count)
	}
}
