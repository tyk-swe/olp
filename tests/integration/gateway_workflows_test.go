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
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/agents/a2a"
	"github.com/tyk-swe/olp/internal/agents/runs"
	"github.com/tyk-swe/olp/internal/agents/tasks"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/agentcontract"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/mcp"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/rates"
	"github.com/tyk-swe/olp/tests/harness"
	"github.com/tyk-swe/olp/tests/harness/vendor"
)

func TestA2ACancellationAuditsProjectActorBeforePeerDispatch(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	actor, completedTask, revokedTask := uuid.NewString(), uuid.NewString(), uuid.NewString()
	const completedUpstream, revokedUpstream = "peer-task-ok", "peer-task-revoked"
	seedA2ACancellation(t, fixture, actor, completedTask, completedUpstream, revokedTask, revokedUpstream)

	auditCount := func(ctx context.Context, action, taskID string) int {
		var rows int
		err := pool.QueryRow(ctx, `SELECT count(*) FROM olp.audit WHERE action=$1
			AND resource_type='a2a_task' AND resource_id=$2 AND project_id=$3 AND actor_user_id=$4`,
			action, taskID, fixture.projectID, actor).Scan(&rows)
		if err != nil {
			t.Errorf("query audit %s for %s: %v", action, taskID, err)
		}
		return rows
	}
	peer := cancellationPeer{cancel: func(ctx context.Context, request *protocol.CancelTaskRequest) (*protocol.Task, error) {
		switch string(request.ID) {
		case completedUpstream:
			if rows := auditCount(ctx, "a2a_task.cancel_attempt", completedTask); rows != 1 {
				t.Errorf("attributed cancellation attempts before ok dispatch = %d, want 1", rows)
			}
		case revokedUpstream:
			if rows := auditCount(ctx, "a2a_task.cancel_attempt", revokedTask); rows != 1 {
				t.Errorf("attributed cancellation attempts before revoked dispatch = %d, want 1", rows)
			}
			if _, err := pool.Exec(ctx, `DELETE FROM olp.project_members WHERE project_id=$1 AND user_id=$2`, fixture.projectID, actor); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unexpected cancellation target %q", request.ID)
		}
		return &protocol.Task{ID: request.ID, ContextID: "peer-context", Status: protocol.TaskStatus{State: protocol.TaskStateCanceled}}, nil
	}}
	store := tasks.New(pool, tasks.Limits{}, nil, slog.New(slog.DiscardHandler))
	call := managementHTTPAs(t, pool, actor, func(mux *http.ServeMux, server *access.Server) {
		a2a.NewManagement(server, store, agents.NewTaskOperations(store, peer, nil)).Register(mux)
	})
	if result := call.call(t, "POST", "/api/v1/a2a-tasks/"+completedTask+"/cancel", "", ""); result.status != http.StatusOK {
		t.Fatalf("successful cancellation = %d %s, want 200", result.status, result.body)
	}
	if rows := auditCount(t.Context(), "a2a_task.cancel", completedTask); rows != 1 {
		t.Fatalf("attributed cancellation completions = %d, want 1", rows)
	}
	if result := call.call(t, "POST", "/api/v1/a2a-tasks/"+revokedTask+"/cancel", "", ""); result.status != http.StatusNotFound {
		t.Fatalf("cancellation after project access was removed = %d %s, want 404", result.status, result.body)
	}
	var state tasks.State
	if err := pool.QueryRow(t.Context(), `SELECT state FROM olp.a2a_tasks WHERE id=$1`, revokedTask).Scan(&state); err != nil || state != tasks.Canceled {
		t.Fatalf("stored task state = %q, want %q (%v)", state, tasks.Canceled, err)
	}
	if rows := auditCount(t.Context(), "a2a_task.cancel_attempt", revokedTask); rows != 1 {
		t.Fatalf("attributed cancellation attempts = %d, want 1", rows)
	}
	if rows := auditCount(t.Context(), "a2a_task.cancel", revokedTask); rows != 0 {
		t.Fatalf("revoked cancellation completions = %d, want 0", rows)
	}
}

func seedA2ACancellation(t *testing.T, f usageFixture, actor, task1, upstream1, task2, upstream2 string) {
	t.Helper()
	publication, revision, dispatch1, dispatch2 := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	_, err := f.pool.Exec(t.Context(), `WITH actor AS (
		INSERT INTO olp.users(id,email,display_name,role,access_scope,etag)
		VALUES($1::uuid,$1::text||'@example.test','Task operator','operator','assigned',$1::uuid) RETURNING id
	), membership AS (
		INSERT INTO olp.project_members(project_id,user_id,role,added_by) SELECT $2,id,'manager',$3 FROM actor
	), publication AS (
		INSERT INTO olp.agent_publications(id,project_id,kind,name,draft,etag,created_by)
		VALUES($4,$2,'agent','Task peer','{}',$4,$3) RETURNING id
	), revision AS (
		INSERT INTO olp.agent_publication_revisions(id,publication_id,project_id,kind,definition,digest,created_by)
		SELECT $5,id,$2,'agent',jsonb_build_object('project_id',$2::uuid,'agent','{}'::jsonb),repeat('0',64),$3 FROM publication
		RETURNING id, publication_id
	), debits AS (
		INSERT INTO olp.execution_debits(id,api_key_id,project_id,kind,affinity,intent_digest,claim_id,currency,cost_bound,charge_bound,prepared_until)
		VALUES($6,$8,$2,'agent','{}',decode(repeat('00',32),'hex'),$6,'USD',0,0,now()),
		      ($7,$8,$2,'agent','{}',decode(repeat('00',32),'hex'),$7,'USD',0,0,now())
		RETURNING id
	)
	INSERT INTO olp.a2a_tasks(id,api_key_id,project_id,publication_id,revision_id,dispatch_id,upstream_id,state)
	SELECT v.task_id,$8,$2,revision.publication_id,revision.id,v.dispatch_id,v.upstream_id,'active'
	FROM revision
	CROSS JOIN (VALUES ($9::uuid,$6::uuid,$10::text), ($11::uuid,$7::uuid,$12::text)) AS v(task_id,dispatch_id,upstream_id)
	JOIN debits ON debits.id=v.dispatch_id`,
		actor, f.projectID, f.ownerID, publication, revision, dispatch1, dispatch2, f.keyID, task1, upstream1, task2, upstream2)
	if err != nil {
		t.Fatal(err)
	}
}

func TestTaskCallbacksStopAtTheContinuationLimitAndAccountOnlyForAdmittedCalls(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	installation, err := database.Installation(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	auth := secrets.NewAuthKey(make([]byte, 32), installation)
	f := taskCallbackFixture{
		owner: uuid.NewString(), project: uuid.NewString(), key: uuid.NewString(), toolset: uuid.NewString(), toolsetRevision: uuid.NewString(),
		opening:      taskCallbackIDs{token: "olpt_opening", dispatch: uuid.NewString()},
		continuation: taskCallbackIDs{token: "olpt_continuation", dispatch: uuid.NewString(), task: uuid.NewString()},
		idle:         taskCallbackIDs{token: "olpt_idle", dispatch: uuid.NewString(), task: uuid.NewString()},
	}
	f.seed(t, pool, auth)
	manager := release.NewManager(release.ManagerConfig{Pool: pool, Installation: installation, Auth: auth, Log: slog.New(slog.DiscardHandler), Hostname: "task-callbacks", CountTaskCallback: tasks.CountCallback})
	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := []string{"admission " + f.continuation.dispatch + " 0", "admission " + f.idle.dispatch + " 0", "task " + f.continuation.task + " 2 2", "task " + f.idle.task + " 0 0", "admission " + f.opening.dispatch + " 2"}
	for _, callback := range []taskCallbackIDs{f.opening, f.continuation} {
		authority, err := manager.AuthenticateTaskCapability(t.Context(), callback.token)
		if err != nil {
			t.Fatal(err)
		}
		admit := func(accounting string) error {
			return manager.AdmitTaskCapabilityAttempt(t.Context(), *authority, "tools", f.toolset, f.toolsetRevision, &f.project, accounting)
		}
		recorded, refused := uuid.NewString(), uuid.NewString()
		if first, repeated := admit(recorded), admit(recorded); first != nil || repeated != nil {
			t.Fatalf("callbacks within the limit = %v, then %v", first, repeated)
		}
		if err := admit(refused); !errors.Is(err, release.ErrTaskAttemptLimit) {
			t.Fatalf("callback past the limit = %v", err)
		}
		want = append(want, "accounting "+callback.dispatch+" tools "+recorded)
	}
	idle, err := manager.AuthenticateTaskCapability(t.Context(), f.idle.token)
	if err != nil {
		t.Fatal(err)
	}
	for _, ended := range []string{"state='completed'", "deadline_at=now()-interval '1 second'"} {
		mustExec(t, pool, `UPDATE olp.a2a_tasks SET state='active',deadline_at=now()+interval '1 hour' WHERE id=$1`, f.idle.task)
		ending, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ending.Rollback(context.WithoutCancel(t.Context())) }()
		if _, err := ending.Exec(t.Context(), "UPDATE olp.a2a_tasks SET "+ended+" WHERE id=$1", f.idle.task); err != nil {
			t.Fatal(err)
		}
		admitted := make(chan error, 1)
		go func() {
			admitted <- manager.AdmitTaskCapabilityAttempt(t.Context(), *idle, "tools", f.toolset, f.toolsetRevision, &f.project, uuid.NewString())
		}()
		harness.Eventually(t, "the callback waiting on the ending task", 5*time.Second, func() (bool, string) {
			var blocked bool
			if err := pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1::integer=ANY(pg_blocking_pids(pid)))`, int32(ending.Conn().PgConn().PID())).Scan(&blocked); err != nil {
				t.Fatal(err)
			}
			return blocked, "the callback has not reached the task row"
		})
		if err := ending.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := <-admitted; !errors.Is(err, release.ErrTaskAttemptLimit) {
			t.Fatalf("callback for a task that ended with %s = %v", ended, err)
		}
	}
	var stored []string
	if err := pool.QueryRow(t.Context(), `SELECT array_agg(line) FROM (
		SELECT format('admission %s %s',dispatch_id,callback_attempts) AS line FROM olp.a2a_task_admissions
		UNION ALL SELECT format('task %s %s %s',id,continuations,callback_attempts) FROM olp.a2a_tasks
		UNION ALL SELECT format('accounting %s %s %s',dispatch_id,kind,accounting_id) FROM olp.a2a_task_callback_accounting) AS lines`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	slices.Sort(stored)
	slices.Sort(want)
	if !slices.Equal(stored, want) {
		t.Fatalf("stored callback counts=%q, want %q", stored, want)
	}
}

type taskCallbackIDs struct{ token, dispatch, task string }

type taskCallbackFixture struct {
	owner, project, key, toolset, toolsetRevision string
	opening, continuation, idle                   taskCallbackIDs
}

func (f taskCallbackFixture) seed(t *testing.T, pool *pgxpool.Pool, auth *secrets.AuthKey) {
	t.Helper()
	publication, revision := uuid.NewString(), uuid.NewString()
	mustExec(t, pool, `INSERT INTO olp.users(id,email,display_name,role,etag) VALUES($1::uuid,$1::text||'@example.test','Task owner','owner',$1::uuid)`, f.owner)
	mustExec(t, pool, `INSERT INTO olp.projects(id,name,etag,created_by) VALUES($1::uuid,'Callbacks',$1::uuid,$2)`, f.project, f.owner)
	mustExec(t, pool, `INSERT INTO olp.api_keys(id,lookup_id,digest,name,created_by,policy,etag,project_id)
		VALUES($1::uuid,$1::text,decode(repeat('00',32),'hex'),'Agent caller',$2,jsonb_build_object('scopes','["agents","tools"]'::jsonb,'allowed_agents',jsonb_build_array($4::text),'allowed_toolsets',jsonb_build_array($5::text)),$1::uuid,$3)`,
		f.key, f.owner, f.project, publication, f.toolset)
	mustExec(t, pool, `INSERT INTO olp.agent_publications(id,project_id,kind,name,draft,etag,created_by) VALUES($1::uuid,$2,'agent','Callback agent','{}',$1::uuid,$3)`, publication, f.project, f.owner)
	mustExec(t, pool, `INSERT INTO olp.agent_publication_revisions(id,publication_id,project_id,kind,definition,digest,created_by)
		VALUES($1,$2,$3::uuid,'agent',jsonb_build_object('project_id',$3::uuid,'agent',jsonb_build_object('toolset_revisions',jsonb_build_array($4::text),'max_task_continuations',2)),repeat('0',64),$5)`,
		revision, publication, f.project, f.toolsetRevision, f.owner)
	for _, callback := range []taskCallbackIDs{f.opening, f.continuation, f.idle} {
		mustExec(t, pool, `INSERT INTO olp.execution_debits(id,api_key_id,project_id,kind,state,affinity,intent_digest,claim_id,currency,cost_bound,charge_bound,prepared_until,dispatched_at)
			VALUES($1::uuid,$2,$3,'agent','dispatched','{}',decode(repeat('00',32),'hex'),$1::uuid,'USD',0,0,now(),now())`, callback.dispatch, f.key, f.project)
		if callback.task != "" {
			mustExec(t, pool, `INSERT INTO olp.a2a_tasks(id,api_key_id,project_id,publication_id,revision_id,dispatch_id,upstream_id,state) VALUES($1::uuid,$2,$3,$4,$5,$6,$1::text,'active')`,
				callback.task, f.key, f.project, publication, revision, callback.dispatch)
		}
		mustExec(t, pool, `INSERT INTO olp.a2a_task_admissions(dispatch_id,api_key_id,task_id) VALUES($1,$2,NULLIF($3,'')::uuid)`, callback.dispatch, f.key, callback.task)
		mustExec(t, pool, `INSERT INTO olp.a2a_task_capabilities(dispatch_id,token_digest,publication_id,revision_id,task_id,expires_at) VALUES($1,$2,$3,$4,NULLIF($5,'')::uuid,now()+interval '1 hour')`,
			callback.dispatch, auth.Digest(secrets.TaskCapabilityDigest, callback.token), publication, revision, callback.task)
	}
}

type cancellationPeer struct {
	agents.Peer
	cancel func(context.Context, *protocol.CancelTaskRequest) (*protocol.Task, error)
}

func (p cancellationPeer) Open(context.Context, agentcontract.Definition, []byte, bool) (agents.Peer, error) {
	return p, nil
}

func (p cancellationPeer) Cancel(ctx context.Context, request *protocol.CancelTaskRequest) (*protocol.Task, error) {
	return p.cancel(ctx, request)
}

func (cancellationPeer) Close() {}

type routeTools struct {
	mu    sync.Mutex
	calls []toolCall
}

type toolCall struct {
	run       string
	arguments string
}

func (*routeTools) Resolve(_ context.Context, _ keyauthority.Authority, _ *release.Release, toolsets []string) ([]kernel.WorkflowTool, error) {
	price := "0.01"
	return []kernel.WorkflowTool{{ToolsetID: toolsets[0], RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{
		Classification: "read_only", Alias: "lookup", Name: "lookup", Description: "Look up a fact.", UnitPrice: &price,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"],"additionalProperties":false}`),
	}}}, nil
}

func (*routeTools) AuthorizeWorkflowTool(keyauthority.Authority, kernel.WorkflowTool) error {
	return nil
}

func (r *routeTools) Call(_ context.Context, _ keyauthority.Authority, _ *release.Release, _ kernel.WorkflowTool, run string, _ int, _ string, arguments json.RawMessage) (json.RawMessage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, toolCall{run: run, arguments: string(arguments)})
	if strings.Contains(string(arguments), "fail") {
		return nil, &mcp.WorkflowError{Status: http.StatusBadGateway, Code: "tool_failed", Message: "The tool failed."}
	}
	return json.RawMessage(`{"content":[{"type":"text","text":"forty-two"}]}`), nil
}

func (*routeTools) CallApproved(context.Context, keyauthority.Authority, *release.Release, kernel.WorkflowTool, string, int, string, json.RawMessage, runs.WorkflowApproval) (json.RawMessage, error) {
	return nil, errors.New("the route tool needs no approval")
}

type chatWorkflowUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	CompletionDetail struct {
		ReasoningTokens          int64 `json:"reasoning_tokens"`
		AcceptedPredictionTokens int64 `json:"accepted_prediction_tokens"`
	} `json:"completion_tokens_details"`
}

func TestGatewayToolLoopRunsTheRouteToolFeedsItsResultBackAndSettlesEveryModelTurn(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	const slug, model = "tools", vendor.Model
	var pricingID string
	if err := pool.QueryRow(t.Context(), `SELECT pricing_revision_id::text FROM olp.prices WHERE provider_id=$1`,
		fixture.providerID).Scan(&pricingID); err != nil {
		t.Fatal(err)
	}
	type turn struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
			Content    any    `json:"content"`
		} `json:"messages"`
	}
	var mu sync.Mutex
	var turns []turn
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var sent turn
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Error(err)
		}
		mu.Lock()
		turns = append(turns, sent)
		first := len(turns) == 1
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if first {
			_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"`+model+`","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"answer\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8,"completion_tokens_details":{"reasoning_tokens":1}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"chatcmpl-2","object":"chat.completion","created":1,"model":"`+model+`","choices":[{"index":0,"message":{"role":"assistant","content":"It is forty-two."},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":4,"total_tokens":13,"completion_tokens_details":{"reasoning_tokens":2,"accepted_prediction_tokens":1}}}`)
	})
	price := rates.NewRoutingPrice(rates.RoutingPrice{
		Price: rates.Price{ProviderKind: "openai_compatible", Model: model, Operation: "generation",
			InputPerMillion: new("1"), OutputPerMillion: new("2"), Currency: "USD"},
		RevisionID: pricingID, Revision: 1, EffectiveAt: fixture.observedAt.Add(-time.Hour),
	})
	tools := &routeTools{}
	g := newStoredGateway(t, fixture, storedPlan{
		slug: slug, model: model, kind: "openai_compatible", operations: []string{"generation"},
		prices: []rates.RoutingPrice{*price}, upstream: upstream, toolsets: []string{uuid.NewString()}, tools: tools,
	})
	g.key(t, "owner")
	headers := map[string]string{"Authorization": "Bearer owner", "Content-Type": "application/json"}

	_, refused := g.send(t, "POST", "/v1/chat/completions", headers,
		`{"model":"tools","olp_toolsets":["`+uuid.NewString()+`"],"messages":[{"role":"user","content":"what is the answer?"}]}`, 400)
	if !strings.Contains(string(refused), "toolset_not_permitted") || len(turns) != 0 {
		t.Fatalf("a toolset outside the route reached the model %d times: %s", len(turns), refused)
	}

	_, raw := g.send(t, "POST", "/v1/chat/completions", headers,
		`{"model":"tools","messages":[{"role":"user","content":"what is the answer?"}]}`, 200)
	var reply struct {
		Model   string            `json:"model"`
		Usage   chatWorkflowUsage `json:"usage"`
		Choices []struct {
			Message struct {
				Content   string            `json:"content"`
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Model != slug || len(reply.Choices) != 1 || reply.Choices[0].Message.Content != "It is forty-two." ||
		len(reply.Choices[0].Message.ToolCalls) != 0 {
		t.Fatalf("the caller did not get only the final answer: %s", raw)
	}
	if len(tools.calls) != 1 || tools.calls[0].arguments != `{"q":"answer"}` {
		t.Fatalf("tool calls = %+v, want the one call the model asked for", tools.calls)
	}
	if len(turns) != 2 || len(turns[0].Tools) != 1 || turns[0].Tools[0].Function.Name != "lookup" {
		t.Fatalf("model turns = %+v, want the route tool offered and one continuation", turns)
	}
	result := turns[1].Messages[len(turns[1].Messages)-1]
	if text, _ := result.Content.(string); result.Role != "tool" || result.ToolCallID != "call_1" || !strings.Contains(text, "forty-two") {
		t.Fatalf("the continuation ended with %+v, want the tool result for call_1", result)
	}

	const inputTokens, outputTokens = 5 + 9, 3 + 4
	want := chatWorkflowUsage{PromptTokens: inputTokens, CompletionTokens: outputTokens, TotalTokens: inputTokens + outputTokens}
	want.CompletionDetail.ReasoningTokens = 1 + 2
	want.CompletionDetail.AcceptedPredictionTokens = 1
	if reply.Usage != want {
		t.Errorf("response usage = %+v, want %+v for both model turns", reply.Usage, want)
	}
	var settled, runCount int
	var run string
	var priced bool
	if err := pool.QueryRow(t.Context(), `SELECT count(*), count(DISTINCT run_id), coalesce(min(run_id::text),''),
		coalesce(sum(observed_cost),0) = ($2::numeric * 1 + $3::numeric * 2) / 1000000 FROM olp.execution_debits
		WHERE api_key_id=$1 AND kind='model' AND state='settled'`, g.runtime.keys["owner"].ID, inputTokens, outputTokens).
		Scan(&settled, &runCount, &run, &priced); err != nil {
		t.Fatal(err)
	}
	if settled != 2 || runCount != 1 || run != tools.calls[0].run || !priced {
		t.Fatalf("settled model debits=%d runs=%d run=%s priced for both turns=%t, want 2 debits on run %s",
			settled, runCount, run, priced, tools.calls[0].run)
	}
}

func probePass(t *testing.T, g *storedGateway) int {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	total := -1
	g.prober.Run(ctx, func(_ context.Context, probed int, err error) {
		if err != nil {
			t.Error(err)
		}
		total = probed
		cancel()
	})
	return total
}

func TestHealthProbesAskEachOptedInProviderOncePerIntervalAndOpenItsCircuitOnlyWhenTheProviderFails(t *testing.T) {
	t.Parallel()
	const slug, model = "probed", "probe-model"
	interval := &routing.HealthProbe{IntervalSeconds: 60}
	for _, test := range []struct {
		name           string
		probe          *routing.HealthProbe
		upstream       int
		probed         int
		status         fleet.ProbeStatus
		class          string
		circuit        bool
		terminalStatus int
	}{
		{name: "healthy provider", probe: interval, upstream: http.StatusOK, probed: 1, status: fleet.ProbeHealthy, terminalStatus: http.StatusOK},
		{name: "failing provider", probe: interval, upstream: http.StatusInternalServerError, probed: 1, status: fleet.ProbeUnhealthy, class: "upstream_server", circuit: true, terminalStatus: http.StatusBadGateway},
		{name: "refused credential", probe: interval, upstream: http.StatusUnauthorized, probed: 1, status: fleet.ProbeHealthy, class: "credential", terminalStatus: http.StatusBadGateway},
		{name: "provider that did not opt in", upstream: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			pool, _ := harness.Database(t)
			fixture := newUsageFixture(t, pool)
			var mu sync.Mutex
			var asked []string
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				mu.Lock()
				asked = append(asked, string(raw))
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if test.upstream != http.StatusOK {
					w.WriteHeader(test.upstream)
					_, _ = io.WriteString(w, `{"error":{"message":"refused","type":"error"}}`)
					return
				}
				_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"`+model+`","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`)
			})
			sink := &terminals{}
			g := newStoredGateway(t, fixture, storedPlan{slug: slug, model: model, kind: "openai_compatible", operations: []string{"generation"},
				probe: test.probe, sink: sink, upstream: upstream})
			started := time.Now()
			if probed := probePass(t, g); probed != test.probed {
				t.Fatalf("first pass probed %d targets, want %d", probed, test.probed)
			}
			if probed := probePass(t, g); probed != 0 {
				t.Fatalf("a pass inside the interval probed %d targets", probed)
			}
			mu.Lock()
			requests := slices.Clone(asked)
			mu.Unlock()
			if len(requests) != test.probed {
				t.Fatalf("provider received %d requests, want %d: %v", len(requests), test.probed, requests)
			}
			results, err := g.fleet.Probes(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			circuits, err := g.fleet.Circuits(t.Context(), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			seen := sink.all()
			if test.probed == 0 {
				if len(results) != 0 || len(circuits) != 0 || len(seen) != 0 {
					t.Fatalf("an unprobed provider left results %v, circuits %v, records %d", results, circuits, len(seen))
				}
				return
			}
			var sent struct {
				Model     string `json:"model"`
				MaxTokens int64  `json:"max_tokens"`
				Messages  []any  `json:"messages"`
			}
			if err := json.Unmarshal([]byte(requests[0]), &sent); err != nil {
				t.Fatal(err)
			}
			if sent.Model != model || sent.MaxTokens <= 0 || len(sent.Messages) != 1 {
				t.Fatalf("probe request = %s", requests[0])
			}
			result, ok := results[fixture.providerID]
			if !ok || len(results) != 1 || result.Status != test.status || result.Class != test.class || result.Model != model || result.ObservedAt.Before(started) {
				t.Fatalf("shared probe results = %+v", results)
			}
			until, open := circuits[fixture.providerID]
			if open != test.circuit || (open && until.Before(started.Truncate(time.Millisecond).Add(test.probe.Interval()))) {
				t.Fatalf("fleet circuits = %v, want open %t for at least the probe interval", circuits, test.circuit)
			}
			if len(seen) != 1 {
				t.Fatalf("probe wrote %d usage records", len(seen))
			}
			record := seen[0]
			if record.Actor != "system" || record.Origin != usage.OriginProbe || record.KeyID != "" || record.Route != slug ||
				record.Status != test.terminalStatus || len(record.Attempts) != 1 || record.Attempts[0].ProviderID != fixture.providerID {
				t.Fatalf("probe usage record = %+v", record)
			}
		})
	}
}

func TestAProviderOverItsOwnLimitIsRefusedByTheGatewayWithARetryHintAndIsNotCalled(t *testing.T) {
	t.Parallel()
	awayFromMinuteRollover()
	pool, _ := harness.Database(t)
	const slug, model = "limited", "limited-model"
	var calls atomic.Int64
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"`+model+`","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	})
	sink := &terminals{}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: slug, model: model, kind: "openai_compatible", operations: []string{"generation"},
		upstream: upstream, limits: &routing.Limits{RequestsPerMinute: new(int64(1))}, sink: sink,
	})
	g.key(t, "owner")
	headers := map[string]string{"Authorization": "Bearer owner", "Content-Type": "application/json"}
	const body = `{"model":"` + slug + `","messages":[{"role":"user","content":"hi"}]}`

	g.send(t, "POST", "/v1/chat/completions", headers, body, 200)
	refused, raw := g.send(t, "POST", "/v1/chat/completions", headers, body, 429)

	wait, err := strconv.Atoi(refused.Get("Retry-After"))
	if err != nil || wait < 1 || wait > 60 || !strings.Contains(string(raw), `"code":"rate_limit_exceeded"`) {
		t.Fatalf("the refusal carried Retry-After %q and %s, want a wait inside the minute and rate_limit_exceeded", refused.Get("Retry-After"), raw)
	}
	if calls.Load() != 1 {
		t.Fatalf("the provider was called %d times with one request per minute allowed", calls.Load())
	}
	seen := sink.all()
	last := seen[len(seen)-1]
	if len(seen) != 2 || len(last.Attempts) != 1 || last.Attempts[0].Class != "rate_limit" || last.Attempts[0].Status != 0 {
		t.Fatalf("recorded %d requests, the refused one with attempts %+v; want one rate limited attempt that got no provider status", len(seen), last.Attempts)
	}
}

type toolModel struct {
	mu    sync.Mutex
	turns map[string]int
}

func (m *toolModel) asked(script string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.turns[script]
}

func (m *toolModel) wants(script string, answered bool) string {
	m.mu.Lock()
	m.turns[script]++
	turn := m.turns[script]
	m.mu.Unlock()
	call := func(id, name, arguments string) string {
		return `{"index":0,"id":"` + id + `","type":"function","function":{"name":"` + name + `","arguments":` + strconv.Quote(arguments) + `}}`
	}
	switch {
	case strings.HasPrefix(script, "keep asking"):
		return call(fmt.Sprintf("call_%d", turn), "lookup", `{"q":"again"}`)
	case answered:
		return ""
	case script == "unknown tool":
		return call("call_1", "missing", `{}`)
	case script == "broken arguments":
		return call("call_1", "lookup", `{`)
	case strings.HasPrefix(script, "two at once"):
		return call("call_1", "lookup", `{"q":"a"}`) + "," + strings.Replace(call("call_2", "lookup", `{"q":"b"}`), `"index":0`, `"index":1`, 1)
	case script == "failing tool":
		return call("call_1", "lookup", `{"q":"fail"}`)
	}
	return call("call_1", "lookup", `{"q":"answer"}`)
}

func (m *toolModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var sent struct {
		Stream   bool `json:"stream"`
		Messages []struct {
			Role       string `json:"role"`
			Content    any    `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&sent); err != nil || len(sent.Messages) == 0 {
		http.Error(w, "unreadable request", http.StatusBadRequest)
		return
	}
	asked := map[string]bool{}
	for _, message := range sent.Messages {
		for _, call := range message.ToolCalls {
			asked[call.ID] = true
		}
		if message.Role == "tool" && !asked[message.ToolCallID] {
			http.Error(w, "a tool result answers no earlier tool call", http.StatusBadRequest)
			return
		}
	}
	script, _ := sent.Messages[0].Content.(string)
	calls := m.wants(script, sent.Messages[len(sent.Messages)-1].Role == "tool")
	const usage = `"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}`
	head := `{"id":"turn","created":1,"model":"` + vendor.Model + `",`
	if !sent.Stream {
		message, finish := `{"role":"assistant","content":"It is forty-two."}`, "stop"
		if calls != "" {
			message, finish = `{"role":"assistant","content":null,"tool_calls":[`+calls+`]}`, "tool_calls"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, head+`"object":"chat.completion","choices":[{"index":0,"message":`+message+`,"finish_reason":"`+finish+`"}],`+usage+`}`)
		return
	}
	chunk := func(delta, finish string) string {
		return "data: " + head + `"object":"chat.completion.chunk","choices":[{"index":0,"delta":` + delta + `,"finish_reason":` + finish + "}]}\n\n"
	}
	frames := chunk(`{"role":"assistant","content":"It is "}`, "null") + chunk(`{"content":"forty-two."}`, "null") + chunk(`{}`, `"stop"`)
	if calls != "" {
		frames = chunk(`{"role":"assistant","tool_calls":[`+calls+`]}`, "null") + chunk(`{}`, `"tool_calls"`)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, frames+"data: "+head+`"object":"chat.completion.chunk","choices":[],`+usage+"}\n\ndata: [DONE]\n\n")
}

func TestGatewayToolLoopStreamsOnlyTheFinalAnswerStopsAtItsRunLimitsAndRefusesCallsItCannotRun(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	var pricingID string
	if err := pool.QueryRow(t.Context(), `SELECT pricing_revision_id::text FROM olp.prices WHERE provider_id=$1`,
		fixture.providerID).Scan(&pricingID); err != nil {
		t.Fatal(err)
	}
	price := rates.NewRoutingPrice(rates.RoutingPrice{
		Price: rates.Price{ProviderKind: "openai_compatible", Model: vendor.Model, Operation: "generation",
			InputPerMillion: new("1"), OutputPerMillion: new("2"), Currency: "USD"},
		RevisionID: pricingID, Revision: 1, EffectiveAt: fixture.observedAt.Add(-time.Hour),
	})
	model, tools := &toolModel{turns: map[string]int{}}, &routeTools{}
	g := newStoredGateway(t, fixture, storedPlan{
		slug: "tools", model: vendor.Model, kind: "openai_compatible", operations: []string{"generation"}, streams: true,
		prices: []rates.RoutingPrice{*price}, upstream: model, toolsets: []string{uuid.NewString()}, tools: tools,
	})
	g.key(t, "owner")
	headers := map[string]string{"Authorization": "Bearer owner", "Content-Type": "application/json"}
	turns := 0
	for _, tc := range []struct {
		script, settings string
		status           int
		says             string
		turns, tools     int
		usage            *chatWorkflowUsage
	}{
		{script: "streamed", settings: `,"stream":true`, status: 200, says: `"content":"forty-two."`, turns: 2, tools: 1},
		{script: "streamed with usage", settings: `,"stream":true,"stream_options":{"include_usage":true}`, status: 200,
			says: `"content":"forty-two."`, turns: 2, tools: 1,
			usage: &chatWorkflowUsage{PromptTokens: 5 * 2, CompletionTokens: 3 * 2, TotalTokens: 8 * 2}},
		{script: "two at once", status: 200, says: "It is forty-two.", turns: 2, tools: 2},
		{script: "keep asking until the iteration limit", settings: `,"olp_run_limits":{"max_iterations":3}`,
			status: 400, says: "tool_iteration_limit", turns: 3, tools: 2},
		{script: "keep asking until the call limit", settings: `,"olp_run_limits":{"max_tool_calls":2}`,
			status: 400, says: "tool_call_limit", turns: 3, tools: 2},
		{script: "two at once over the call limit", settings: `,"olp_run_limits":{"max_tool_calls":1}`,
			status: 400, says: "tool_call_limit", turns: 1},
		{script: "unknown tool", status: 400, says: "invalid_tool_call", turns: 1},
		{script: "broken arguments", status: 400, says: "invalid_tool_call", turns: 1},
		{script: "failing tool", status: 502, says: "tool_failed", turns: 1, tools: 1},
		{script: "unreadable limits", settings: `,"olp_run_limits":{"max_iterations":"many"}`, status: 400, says: "invalid_run_limits"},
	} {
		called := len(tools.calls)
		_, raw := g.send(t, "POST", "/v1/chat/completions", headers,
			`{"model":"tools","messages":[{"role":"user","content":"`+tc.script+`"}]`+tc.settings+`}`, tc.status)
		if !strings.Contains(string(raw), tc.says) || strings.Contains(string(raw), "tool_calls") && tc.status == 200 {
			t.Fatalf("%s: the caller got %s, want %s and no tool call", tc.script, raw, tc.says)
		}
		if strings.Contains(tc.settings, `"stream":true`) {
			assertChatWorkflowStreamUsage(t, raw, tc.usage)
		}
		if asked, ran := model.asked(tc.script), len(tools.calls)-called; asked != tc.turns || ran != tc.tools {
			t.Fatalf("%s: %d model turns and %d tool calls, want %d and %d", tc.script, asked, ran, tc.turns, tc.tools)
		}
		turns += tc.turns
	}
	var settled int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.execution_debits WHERE api_key_id=$1 AND kind='model' AND state='settled'`,
		g.runtime.keys["owner"].ID).Scan(&settled); err != nil {
		t.Fatal(err)
	}
	if settled != turns {
		t.Fatalf("%d model debits settled for %d model turns, including the runs that were refused", settled, turns)
	}
}

func assertChatWorkflowStreamUsage(t *testing.T, raw []byte, want *chatWorkflowUsage) {
	t.Helper()
	var usage []chatWorkflowUsage
	for line := range strings.SplitSeq(string(raw), "\n") {
		data, found := strings.CutPrefix(line, "data:")
		if !found || strings.TrimSpace(data) == "[DONE]" {
			continue
		}
		var chunk struct {
			Usage *chatWorkflowUsage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatal(err)
		}
		if chunk.Usage != nil {
			usage = append(usage, *chunk.Usage)
		}
	}
	if want == nil && len(usage) != 0 || want != nil && (len(usage) != 1 || usage[0] != *want) {
		t.Errorf("stream usage chunks = %+v, want %v", usage, want)
	}
}

type contractProvider struct {
	mu     sync.Mutex
	bodies []string
}

func (p *contractProvider) asked() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.bodies)
}

func anthropicEvents(events ...string) string {
	var out strings.Builder
	for i := 0; i < len(events); i += 2 {
		out.WriteString("event: " + events[i] + "\ndata: " + events[i+1] + "\n\n")
	}
	return out.String()
}

func (p *contractProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	text := string(raw)
	p.mu.Lock()
	p.bodies = append(p.bodies, text)
	p.mu.Unlock()
	answered := strings.Contains(text, "tool_result")
	opening := `{"type":"message_start","message":{"id":"msg_stream","type":"message","role":"assistant","model":"claude-model","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":1}}}`
	switch {
	case strings.Contains(text, "outage?"):
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"down"}}`)
	case strings.Contains(text, "cut?"):
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, anthropicEvents("message_start", opening,
			"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_cut","name":"lookup","input":{}}}`))
	case strings.Contains(text, `"stream":true`):
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, anthropicEvents("message_start", opening,
			"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_stream","name":"lookup","input":{}}}`,
			"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"x\"}"}}`,
			"content_block_stop", `{"type":"content_block_stop","index":0}`,
			"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":3}}`,
			"message_stop", `{"type":"message_stop"}`))
	case answered:
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_2","type":"message","role":"assistant","model":"claude-model","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":9,"output_tokens":2}}`)
	default:
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-model","content":[{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":"x"}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":3}}`)
	}
}

type contractReply struct {
	Model   string `json:"model"`
	Choices []struct {
		Message json.RawMessage `json:"message"`
	} `json:"choices"`
	OLP struct {
		Handle string `json:"handle"`
	} `json:"olp"`
	Error struct {
		Code  string `json:"code"`
		Param string `json:"param"`
	} `json:"error"`
}

func TestANegotiatedToolContinuationKeepsTheProvidersTurnReplaysItsDeliveryAndRefusesWhatItCannotContinue(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	provider := &contractProvider{}
	sink := &terminals{}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: "tool-contract", model: "claude-model", kind: "anthropic", surface: "openai", profile: "anthropic-messages", fidelity: routing.FidelityStrict, streams: true,
		defaults: map[string]connectors.DefaultSet{"generation": {Dialect: "anthropic-messages", Values: map[string]json.RawMessage{"max_tokens": json.RawMessage("64")}}},
		sink:     sink, operations: []string{"generation"}, upstream: provider,
	})
	g.key(t, "owner")
	g.key(t, "stranger")
	tools := `"tools":[{"type":"function","function":{"name":"lookup","description":"Looks up.","parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}}}]`
	turn := func(question string, more ...string) string {
		return `{"model":"tool-contract","messages":[{"role":"user","content":"` + question + `"}` + strings.Join(more, "") + `],` + tools + `}`
	}
	submission := func() string { return resources.SubmissionID(time.Now(), uuid.New()) }
	ask := func(name, key, submission, handle, body string, status int, code string, attempts ...string) (contractReply, string, http.Header) {
		t.Helper()
		before, terminalsBefore := len(provider.asked()), len(sink.all())
		req, err := http.NewRequest(http.MethodPost, g.server.URL+"/v1/chat/completions", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-OLP-Continuation", "chat-anthropic-tools-v1")
		for header, value := range map[string]string{"X-OLP-Submission-ID": submission, "X-OLP-Continuation-Handle": handle} {
			if value != "" {
				req.Header.Set(header, value)
			}
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var reply contractReply
		_ = json.Unmarshal(raw, &reply)
		if resp.StatusCode != status || reply.Error.Code != code {
			t.Fatalf("%s: status %d, code %q, want %d %q: %s", name, resp.StatusCode, reply.Error.Code, status, code, raw)
		}
		harness.Eventually(t, name+" is recorded", time.Second, func() (bool, string) {
			return len(sink.all()) == terminalsBefore+1, ""
		})
		seen := []string{}
		for _, attempt := range sink.all()[terminalsBefore].Attempts {
			seen = append(seen, fmt.Sprintf("%s %s committed=%v", attempt.Mode, attempt.Class, attempt.Committed))
		}
		if !slices.Equal(seen, attempts) || len(provider.asked())-before != len(attempts) {
			t.Fatalf("%s: attempts %v with %d provider requests, want %v", name, seen, len(provider.asked())-before, attempts)
		}
		return reply, string(raw), resp.Header
	}
	replayed := func(name string, header http.Header, got, want string) {
		t.Helper()
		if got != want || header.Get("X-OLP-Delivery-Replay") != "true" || header.Get("X-Should-Retry") != "false" {
			t.Fatalf("%s: replay %q retry %q body %s, want the first delivery %s", name, header.Get("X-OLP-Delivery-Replay"), header.Get("X-Should-Retry"), got, want)
		}
	}

	first := submission()
	opening, openingBody, _ := ask("first turn", "owner", first, "", turn("weather?"), 200, "", "unary success committed=true")
	if opening.Model != "tool-contract" || !strings.HasPrefix(opening.OLP.Handle, "continuation_") || !strings.Contains(string(opening.Choices[0].Message), `"id":"toolu_1"`) {
		t.Fatalf("first turn: %s", openingBody)
	}
	if sent := provider.asked()[0]; !strings.Contains(sent, `"max_tokens":64`) || !strings.Contains(sent, `"model":"claude-model"`) || !strings.Contains(sent, `"input_schema"`) {
		t.Fatalf("first provider request: %s", sent)
	}
	_, again, header := ask("first turn again", "owner", first, "", turn("weather?"), 200, "")
	replayed("first turn again", header, again, openingBody)
	ask("submission reused for another request", "owner", first, "", turn("other?"), 409, "continuation_mismatch")

	result := `,` + string(opening.Choices[0].Message) + `,{"role":"tool","tool_call_id":"toolu_1","content":"sunny"}`
	second := submission()
	closing, closingBody, _ := ask("tool result", "owner", second, opening.OLP.Handle, turn("weather?", result), 200, "", "unary success committed=true")
	if !strings.Contains(string(closing.Choices[0].Message), `"content":"done"`) || closing.OLP.Handle == opening.OLP.Handle || !strings.HasPrefix(closing.OLP.Handle, "continuation_") {
		t.Fatalf("tool result turn: %s", closingBody)
	}
	if sent := provider.asked()[1]; !strings.Contains(sent, `"type":"tool_use"`) || !strings.Contains(sent, `"tool_use_id":"toolu_1"`) || !strings.Contains(sent, `"content":"sunny"`) {
		t.Fatalf("continued provider request: %s", sent)
	}
	_, again, header = ask("tool result again", "owner", second, opening.OLP.Handle, turn("weather?", result), 200, "")
	replayed("tool result again", header, again, closingBody)

	for _, tc := range []struct {
		name, key, submission, handle, body string
		status                              int
		code, param                         string
	}{
		{"another key's handle", "stranger", submission(), opening.OLP.Handle, turn("weather?", result), 409, "continuation_unavailable", ""},
		{"unknown handle", "owner", submission(), "continuation_" + strings.Repeat("0", 32), turn("weather?", result), 409, "continuation_unavailable", ""},
		{"no submission identity", "owner", "", "", turn("weather?"), 400, "state_carrier", ""},
		{"stale submission identity", "owner", resources.SubmissionID(time.Now().Add(-time.Hour), uuid.New()), "", turn("weather?"), 400, "invalid_submission_identity", ""},
		{"rewritten history", "owner", submission(), opening.OLP.Handle, turn("rain?", result), 400, "state_carrier", "/messages"},
	} {
		if reply, raw, _ := ask(tc.name, tc.key, tc.submission, tc.handle, tc.body, tc.status, tc.code); reply.Error.Param != tc.param {
			t.Fatalf("%s: %s", tc.name, raw)
		}
	}

	streamed := strings.Replace(turn("weather?"), `{"model"`, `{"stream":true,"model"`, 1)
	third := submission()
	_, frames, _ := ask("streamed turn", "owner", third, "", streamed, 200, "", "streaming success committed=true")
	if !strings.Contains(frames, `"id":"toolu_stream"`) || !strings.Contains(frames, `"handle":"continuation_`) || !strings.HasSuffix(frames, "data: [DONE]\n\n") || strings.Contains(frames, "msg_stream\"}}\n\nevent:") {
		t.Fatalf("streamed turn: %s", frames)
	}
	_, again, header = ask("streamed turn again", "owner", third, "", streamed, 200, "")
	replayed("streamed turn again", header, again, frames)

	lost := submission()
	if _, _, header := ask("provider outage", "owner", lost, "", turn("outage?"), 502, "ambiguous_upstream_result", "unary ambiguous committed=false"); header.Get("X-Should-Retry") != "false" {
		t.Fatalf("provider outage: retry hint %q", header.Get("X-Should-Retry"))
	}
	ask("provider outage again", "owner", lost, "", turn("outage?"), 409, "continuation_outcome_unknown")
	cut := strings.Replace(turn("cut?"), `{"model"`, `{"stream":true,"model"`, 1)
	broken := submission()
	ask("provider stream cut inside a tool call", "owner", broken, "", cut, 502, "provider_protocol_error", "streaming protocol committed=false")
	ask("cut stream again", "owner", broken, "", cut, 409, "continuation_outcome_unknown")
}
