//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/resources"
	resourcesmanage "github.com/tyk-swe/olp/internal/resources/manage"
	"github.com/tyk-swe/olp/internal/resources/retainedworker"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/tests/harness"
)

func testRetainedSettlementPollFailure(t *testing.T, fixture usageFixture) {
	t.Helper()
	pool := fixture.pool
	store := resources.New(pool)
	resource, err := store.Put(t.Context(), &resources.Resource{
		Kind: "fine_tuning_job", APIKeyID: fixture.keyID, RouteSlug: "retained-health",
		ProviderID: fixture.providerID, ProviderRevisionID: uuid.NewString(),
		RouteRevisionID: uuid.NewString(), SlotID: uuid.NewString(), TargetID: uuid.NewString(),
		UpstreamID: "fixture-job", State: "running",
	})
	if err != nil {
		t.Fatal(err)
	}
	templates, err := json.Marshal(map[string]*usage.Event{"fixture-model": fixture.event(fixture.attempt(1))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.retained_settlements
		(resource_id,operation,templates,token_consuming) VALUES($1,'fine_tuning',$2,false)`, resource.UUID, templates); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	worker := retainedworker.Worker{Store: store, Resolver: resources.NewResolver(pool), Connections: egress.NewConnectionClientCache(128), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	go func() {
		defer close(done)
		worker.Run(ctx)
	}()
	defer func() { cancel(); <-done }()
	var failures, successes, resourceFailures int64
	var lastError string
	var lastSuccess *time.Time
	harness.Eventually(t, "retained worker first pass", 5*time.Second, func() (bool, string) {
		err := pool.QueryRow(t.Context(), `SELECT h.failures_total,h.successes_total,h.last_success_at,s.failures,s.last_error
			FROM olp.worker_task_health h CROSS JOIN olp.retained_settlements s
			WHERE h.task='retained_settlement' AND s.resource_id=$1`, resource.UUID).
			Scan(&failures, &successes, &lastSuccess, &resourceFailures, &lastError)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, "retained worker has not checkpointed yet"
		}
		if err != nil {
			t.Fatal(err)
		}
		return true, ""
	})
	t.Logf("resource failures=%d error=%s worker failures=%d successes=%d has success=%t", resourceFailures, lastError, failures, successes, lastSuccess != nil)
	if resourceFailures != 1 || lastError != "credential_or_pin_unavailable" || failures != 1 {
		t.Fatal("failed retained settlement was reported as a successful worker pass")
	}
	testRetainedManagement(t, fixture, resource)
}

func testRetainedManagement(t *testing.T, fixture usageFixture, resource *resources.Resource) {
	t.Helper()
	ctx, pool := t.Context(), fixture.pool
	client := managementHTTPAs(t, pool, fixture.ownerID, func(mux *http.ServeMux, server *access.Server) {
		resourcesmanage.New(server).Register(mux)
	})
	post := func(action, body string) (int, []byte) {
		t.Helper()
		res := client.call(t, http.MethodPost, "/api/v1/provider-resources/"+resource.ID+"/"+action, "", body)
		return res.status, res.body
	}
	testRetainedUsageEvidence(t, fixture, resource, post)
	list := client.call(t, http.MethodGet, fmt.Sprintf("/api/v1/provider-resources?kind=fine_tuning_job&api_key_id=%s&provider_id=%s", fixture.keyID, fixture.providerID), "", "")
	var listed contract.ProviderResourceListResponse
	if err := json.Unmarshal(list.body, &listed); err != nil || list.status != http.StatusOK || len(listed.Items) != 1 || listed.Items[0].Id != resource.ID || listed.Items[0].ApiKeyId.String() != fixture.keyID || listed.Items[0].ProviderId.String() != fixture.providerID || listed.Items[0].RetainedSettlement == nil {
		t.Fatalf("list provider resources = %d %s, want 200 with %s", list.status, list.body, resource.ID)
	}
	oversizedStatus, oversizedBody := post("reconcile", fmt.Sprintf(`{"upstream_id":%q}`, strings.Repeat("x", 513)))
	var oversizedProblem struct{ Type string }
	if err := json.Unmarshal(oversizedBody, &oversizedProblem); err != nil || oversizedStatus != http.StatusUnprocessableEntity || !strings.HasSuffix(oversizedProblem.Type, "/validation_failed") {
		t.Fatalf("reconcile oversized hint = %d %s, want 422 validation_failed", oversizedStatus, oversizedBody)
	}
	if _, err := pool.Exec(ctx, `UPDATE olp.retained_settlements SET state='closed' WHERE resource_id=$1`, resource.UUID); err != nil {
		t.Fatal(err)
	}
	status, body := post("reconcile", `{"upstream_id":"test-hint"}`)
	var problem struct{ Type string }
	if err := json.Unmarshal(body, &problem); err != nil || status != http.StatusConflict || !strings.HasSuffix(problem.Type, "/settlement_closed") {
		t.Fatalf("reconcile closed settlement = %d %s, want 409 settlement_closed", status, body)
	}
}

func testRetainedUsageEvidence(t *testing.T, fixture usageFixture, resource *resources.Resource, post func(action, body string) (int, []byte)) {
	t.Helper()
	ctx, pool := t.Context(), fixture.pool
	audited := func() int {
		var rows int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM olp.audit WHERE action='provider_resource.usage_evidence' AND resource_id=$1`, resource.UUID.String()).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	evidence := func(model string) string {
		return fmt.Sprintf(`{"idempotency_key":"evidence-1","evidence_digest":%q,"start":%q,"end":%q,"model":%q,"training_tokens":10}`,
			strings.Repeat("ab", 32), start.Format(time.RFC3339), start.Add(time.Minute).Format(time.RFC3339), model)
	}
	if status, body := post("usage-evidence", evidence("unpinned-model")); status != http.StatusUnprocessableEntity || audited() != 0 {
		t.Fatalf("evidence for an unpinned model = %d %s with %d audit rows, want 422 and none", status, body, audited())
	}
	bystander := uuid.NewString()
	mustExec(t, pool, `INSERT INTO olp.execution_debit_versions(owner_id,version) VALUES($1,5),($2,5)
		ON CONFLICT(owner_id) DO UPDATE SET version=excluded.version`, fixture.keyID, bystander)
	status, body := post("usage-evidence", evidence("fixture-model"))
	var accepted struct {
		ID         string `json:"id"`
		Provenance string `json:"provenance"`
	}
	if err := json.Unmarshal(body, &accepted); err != nil || status != http.StatusOK || accepted.ID != resource.ID || accepted.Provenance != "operator_verified" {
		t.Fatalf("usage evidence = %d %s, want 200 for %s", status, body, resource.ID)
	}
	var stored int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM olp.retained_usage_evidence WHERE resource_id=$1 AND idempotency_key='evidence-1'`, resource.UUID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 1 || audited() != 1 {
		t.Fatalf("accepted evidence stored %d rows and %d audit rows, want one of each", stored, audited())
	}
	var charged, untouched int64
	if err := pool.QueryRow(ctx, `SELECT (SELECT version FROM olp.execution_debit_versions WHERE owner_id=$1),
		(SELECT version FROM olp.execution_debit_versions WHERE owner_id=$2)`, fixture.keyID, bystander).Scan(&charged, &untouched); err != nil {
		t.Fatal(err)
	}
	if charged != 6 || untouched != 5 {
		t.Fatalf("limiter projection versions after accepted evidence: charged key=%d unrelated owner=%d, want 6 and 5", charged, untouched)
	}
}
