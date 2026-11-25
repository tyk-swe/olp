//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/ledger"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/tests/harness"
	"github.com/tyk-swe/olp/tests/harness/money"
)

var updateMoney = flag.Bool("update", false, "rewrite the stored resource money golden file")

type moneyAttempt struct {
	Class            string       `json:"class"`
	Committed        bool         `json:"committed"`
	BillingUncertain bool         `json:"billing_uncertain"`
	Price            *money.Price `json:"price"`
}

type moneyStep struct {
	Step      string         `json:"step"`
	Operation string         `json:"operation"`
	Status    int            `json:"status"`
	Error     string         `json:"error,omitempty"`
	Cost      string         `json:"cost,omitempty"`
	Attempts  []moneyAttempt `json:"attempts"`
	Holds     []money.Hold   `json:"holds"`
}

type moneyGateway struct {
	*storedGateway
	holds    *money.Ledger
	sink     *terminals
	returned chan struct{}
}

func newMoneyGateway(t *testing.T, slug, model, kind string, operations []string, upstream http.Handler) *moneyGateway {
	t.Helper()
	pool, _ := harness.Database(t)
	holds, sink, returned := money.Record(t, harness.Valkey(t)), &terminals{}, make(chan struct{}, 1)
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: slug, model: model, kind: kind, operations: operations, upstream: upstream, sink: sink, valkey: holds,
		front: func(served http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				served.ServeHTTP(w, r)
				returned <- struct{}{}
			})
		},
	})
	g.key(t, "owner")
	g.runtime.inputs = money.Inputs(t, g.snapshot, operations...)
	return &moneyGateway{storedGateway: g, holds: holds, sink: sink, returned: returned}
}

func (g *moneyGateway) budget() {
	g.t.Helper()
	ctx, key := g.t.Context(), g.runtime.keys["owner"]
	g.runtime.keys["owner"] = g.holds.Budget(key)
	g.holds.Cap(g.snapshot, g.plan.slug)
	g.republish(g.t, func(*routing.Route) {})
	route := g.snapshot.Routes[g.plan.slug]
	tx, err := g.fixture.pool.Begin(ctx)
	if err != nil {
		g.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO olp.budget_groups(id,name,project_id,etag,created_by,daily_cost_limit,cost_basis) VALUES($1,'golden',$2,$1,$3,1000,'charge')`,
		money.BudgetGroup, g.fixture.projectID, g.fixture.ownerID); err != nil {
		g.t.Fatal(err)
	}
	if err := ledger.ReplaceSupplyBudgets(ctx, tx, []ledger.SupplyBudget{
		{OwnerID: route.ID, Kind: "route", Daily: route.Budget.DailyCostLimit},
		{OwnerID: g.fixture.providerID, Kind: "connection", Daily: route.Budget.DailyCostLimit},
	}); err != nil {
		g.t.Fatal(err)
	}
	for _, owner := range []limits.Owner{
		{Kind: limits.OwnerKey, ID: key.ID}, {Kind: limits.OwnerGroup, ID: money.BudgetGroup},
		{Kind: limits.OwnerSupply, ID: route.ID}, {Kind: limits.OwnerSupply, ID: g.fixture.providerID},
	} {
		if err := ledger.OpenFreshOwner(ctx, tx, owner.Kind, owner.ID); err != nil {
			g.t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		g.t.Fatal(err)
	}
}

func (g *moneyGateway) step(name, method, path, contentType, body string, status int) (moneyStep, http.Header, []byte) {
	g.t.Helper()
	before := len(g.sink.all())
	submission := ""
	if method == http.MethodPost {
		submission = name
	}
	header, raw := g.send(g.t, method, path, map[string]string{"Authorization": "Bearer owner", "Content-Type": contentType, "Idempotency-Key": submission}, body, status)
	<-g.returned
	harness.Eventually(g.t, name+" is recorded", time.Second, func() (bool, string) { return len(g.sink.all()) > before, "no terminal record" })
	envelope := g.sink.all()[before]
	attempts := make([]moneyAttempt, len(envelope.Attempts))
	for index, attempt := range envelope.Attempts {
		attempts[index] = moneyAttempt{
			Class: attempt.Class, Committed: attempt.Committed, BillingUncertain: attempt.BillingUncertain,
			Price: money.AttemptPrice(envelope.Operation, &attempt),
		}
	}
	var refusal struct {
		Error struct{ Code string }
	}
	_ = json.Unmarshal(raw, &refusal)
	return moneyStep{
		Step: name, Operation: envelope.Operation, Status: status, Error: refusal.Error.Code, Cost: header.Get(money.CostHeader),
		Attempts: attempts, Holds: g.holds.Drain(),
	}, header, raw
}

const providerRefusal = `{"error":{"message":"The provider refused.","type":"invalid_request_error"}}`

func vectorStoreMoney(t *testing.T) []moneyStep {
	const stores, search, query = "/v1/vector_stores", "POST " + "/vector_stores/vs_" + providerOwned + "1/search", `{"query":"q"}`
	provider := &storeProvider{issues: "1", objects: storeObjects, scripted: map[string]string{}, statuses: map[string]int{}}
	g := newMoneyGateway(t, "stores", "stores-model", "azure_openai", []string{"generation", "batch", "vector_store", "retrieval"},
		&retainedUpstream{replies: provider.replies, status: provider.status})
	created, header, _ := g.step("create without a budget", "POST", stores, "application/json", `{"name":"notes"}`, http.StatusOK)
	store := stores + "/" + header.Get("X-OLP-Resource-ID")
	g.budget()
	retrieved, _, _ := g.step("retrieve", "GET", store, "", "", http.StatusOK)
	provider.answers(search, http.StatusBadRequest, providerRefusal)
	refused, _, _ := g.step("search refused upstream", "POST", store+"/search", "application/json", query, http.StatusBadRequest)
	provider.answers(search, http.StatusOK, `{"object":"vector_store.search_results.page","search_query":["q"],"data":[],"has_more":false}`)
	searched, _, _ := g.step("search", "POST", store+"/search", "application/json", query, http.StatusOK)
	deleted, _, _ := g.step("delete", "DELETE", store, "", "", http.StatusOK)
	return []moneyStep{created, retrieved, refused, searched, deleted}
}

func videoMoney(t *testing.T) []moneyStep {
	const videos = "/v1/videos"
	provider := &storeProvider{issues: "1", objects: videoObjects, scripted: map[string]string{}, statuses: map[string]int{}}
	g := newMoneyGateway(t, "studio", "video-model", "openai_compatible", []string{"video_create", "video_get", "video_content", "video_delete", "video_list"},
		&retainedUpstream{replies: provider.replies, status: provider.status})
	g.budget()
	contentType, form := videoForm("model", "studio", "prompt", "a cat", "seconds", "4")
	provider.answers("POST /videos", http.StatusBadRequest, providerRefusal)
	refused, _, _ := g.step("create refused upstream", "POST", videos, contentType, form, http.StatusBadRequest)
	provider.recovers("POST /videos")
	created, _, raw := g.step("create", "POST", videos, contentType, form, http.StatusCreated)
	var job struct{ ID string }
	if err := json.Unmarshal(raw, &job); err != nil || job.ID == "" {
		t.Fatalf("created video answered %s: %v", raw, err)
	}
	retrieved, _, _ := g.step("retrieve", "GET", videos+"/"+job.ID, "", "", http.StatusOK)
	deleted, _, _ := g.step("delete", "DELETE", videos+"/"+job.ID, "", "", http.StatusOK)
	return []moneyStep{refused, created, retrieved, deleted}
}

func TestStoredResourceMoneyGolden(t *testing.T) {
	t.Parallel()
	resources := []struct {
		name string
		run  func(*testing.T) []moneyStep
	}{{"vector store", vectorStoreMoney}, {"video", videoMoney}}
	recorded := make([][]moneyStep, len(resources))
	t.Run("resources", func(t *testing.T) {
		for index, resource := range resources {
			t.Run(resource.name, func(t *testing.T) {
				t.Parallel()
				recorded[index] = resource.run(t)
			})
		}
	})
	if t.Failed() {
		return
	}
	got := map[string][]moneyStep{}
	for index, resource := range resources {
		got[resource.name] = recorded[index]
	}
	raw, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "money.golden.json")
	if *updateMoney {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string][]moneyStep
	if err := json.Unmarshal(stored, &want); err != nil {
		t.Fatal(err)
	}
	for name, steps := range got {
		if len(steps) != len(want[name]) {
			t.Errorf("%s recorded %d steps, want %d", name, len(steps), len(want[name]))
			continue
		}
		for index, step := range steps {
			if got, want := moneyLine(t, step), moneyLine(t, want[name][index]); got != want {
				t.Errorf("%s, %s\n got %s\nwant %s", name, step.Step, got, want)
			}
		}
	}
}

func moneyLine(t *testing.T, step moneyStep) string {
	t.Helper()
	raw, err := json.Marshal(step)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestANativeDuplexSessionHoldsItsBoundsInTheDebitLedgerAndAFailedOneStaysUnresolved(t *testing.T) {
	t.Parallel()
	const done = `{"type":"response.done","response":{"id":"r1","usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}}`
	for _, session := range []struct {
		name, state string
		answers     bool
	}{{"a served session stays dispatched", "dispatched", true}, {"a session the provider drops stays unresolved", "outcome_unknown", false}} {
		t.Run(session.name, func(t *testing.T) {
			t.Parallel()
			pool, _ := harness.Database(t)
			fixture := newUsageFixture(t, pool)
			g := newStoredGateway(t, fixture, storedPlan{
				slug: "voice", model: "voice-model", kind: "openai_compatible", profile: "xai-realtime", vendor: "xai", surface: "native",
				operations: []string{"realtime"}, fidelity: routing.FidelityStrict, sessions: true,
				upstream: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !session.answers {
						http.Error(w, "unavailable", http.StatusServiceUnavailable)
						return
					}
					conn, err := websocket.Accept(w, r, nil)
					if err != nil {
						return
					}
					defer conn.CloseNow()
					if conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"session.created","session":{"id":"sess-upstream"}}`)) != nil {
						return
					}
					if _, _, err := conn.Read(r.Context()); err != nil {
						return
					}
					if conn.Write(r.Context(), websocket.MessageText, []byte(done)) != nil {
						return
					}
					_, _, _ = conn.Read(r.Context())
				}),
			})
			g.runtime.inputs = money.Inputs(t, g.snapshot, "realtime")
			keyID := g.key(t, "owner")

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(g.server.URL, "http")+"/native/xai-realtime/sessions/voice",
				&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer owner"}}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"session.update","session":{"type":"realtime","voice":"eve"}}`)); err != nil {
				t.Fatal(err)
			}
			for {
				_, frame, err := conn.Read(ctx)
				if err != nil {
					break
				}
				if strings.Contains(string(frame), "response.done") {
					_ = conn.Close(websocket.StatusNormalClosure, "")
					break
				}
			}
			harness.Eventually(t, "the session debit reaches "+session.state, 2*time.Second, func() (bool, string) {
				var debits, held, priced, marked int
				err := pool.QueryRow(t.Context(), `SELECT count(*),count(*) FILTER (WHERE state=$2),count(*) FILTER (WHERE cost_bound>0),
					count(*) FILTER (WHERE kind='api' AND charge_bound=cost_bound*1.5 AND affinity->>'provider_id'=$3)
					FROM olp.execution_debits WHERE api_key_id=$1`, keyID, session.state, fixture.providerID).Scan(&debits, &held, &priced, &marked)
				if err != nil {
					return false, err.Error()
				}
				return debits == 1 && held == 1 && priced == 1 && marked == 1,
					"debits, in the wanted state, with a cost bound, marked up for the pinned provider = " + strings.Join([]string{strconv.Itoa(debits), strconv.Itoa(held), strconv.Itoa(priced), strconv.Itoa(marked)}, ", ")
			})
		})
	}
}
