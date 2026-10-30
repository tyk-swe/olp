//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/a2a"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/runtime"
)

type growingMonitorRuntime struct {
	*runtime.Manager
	ctx           context.Context
	pool          *pgxpool.Pool
	oldKey        string
	backgroundKey string
	task          string
	firstCheck    chan struct{}
	continueCheck chan struct{}
	errors        chan error
	oldChecks     atomic.Int32
	background    atomic.Int32
	additions     atomic.Int32
}

func (r *growingMonitorRuntime) AuthorizeAgentTask(key, project, publication, revision, workload string, expiry *time.Time) error {
	err := r.Manager.AuthorizeAgentTask(key, project, publication, revision, workload, expiry)
	if key == r.oldKey && r.oldChecks.Add(1) == 1 {
		close(r.firstCheck)
		select {
		case <-r.continueCheck:
		case <-r.ctx.Done():
		}
	}
	if key == r.backgroundKey && r.background.Add(1)%100 == 0 {
		timer := time.NewTimer(25 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.ctx.Done():
			return r.ctx.Err()
		}
		_, insertErr := r.pool.Exec(r.ctx, `INSERT INTO olp.a2a_tasks(id,api_key_id,project_id,publication_id,revision_id,dispatch_id,upstream_id,state,deadline_at)
 SELECT uuidv7(),$2,t.project_id,t.publication_id,t.revision_id,t.dispatch_id,uuidv7()::text,'active',now()+interval '1 hour'
 FROM olp.a2a_tasks t CROSS JOIN generate_series(1,128) WHERE t.id=$1`, r.task, r.backgroundKey)
		if insertErr != nil {
			if r.ctx.Err() == nil {
				select {
				case r.errors <- insertErr:
				default:
				}
			}
			return err
		}
		r.additions.Add(1)
	}
	return err
}

func TestA2AMonitorRechecksEarlierTaskWhileTasksAreAdded(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "A2A monitor sweep")
	upstreamIDs := make(chan string, 1)
	cancelAttempts := make(chan string, 1)
	agentHandler := a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(a2asrv.AgentExecutorFunc(func(ctx context.Context, execution *a2asrv.ExecutorContext) iter.Seq2[protocol.Event, error] {
		return func(yield func(protocol.Event, error) bool) {
			upstreamIDs <- string(execution.TaskID)
			if yield(protocol.NewSubmittedTask(execution, execution.Message), nil) {
				yield(protocol.NewStatusUpdateEvent(execution, protocol.TaskStateInputRequired, nil), nil)
			}
		}
	})))
	cardFor := func(endpoint string) []byte {
		return []byte(fmt.Sprintf(`{"name":"Fixture agent","description":"A test agent","version":"1.0","supportedInterfaces":[{"url":%q,"protocolBinding":"JSONRPC","protocolVersion":"1.0"}],"capabilities":{"streaming":true},"defaultInputModes":["text"],"defaultOutputModes":["text"],"skills":[]}`, endpoint))
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rpc" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(cardFor("http://" + r.Host + "/rpc"))
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read failed", http.StatusBadRequest)
			return
		}
		var request struct {
			Method string `json:"method"`
			Params struct {
				ID string `json:"id"`
			} `json:"params"`
		}
		if json.Unmarshal(body, &request) == nil && request.Method == "CancelTask" {
			cancelAttempts <- request.Params.ID
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		agentHandler.ServeHTTP(a2aContentTypeWriter{w}, r)
	}))
	t.Cleanup(upstream.Close)
	var user string
	if err := h.Pool.QueryRow(t.Context(), `SELECT id::text FROM olp.users WHERE email='owner@example.com'`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	pricing := acctID(t)
	acctExec(t, h.Pool, `INSERT INTO olp.pricing_revisions(id,revision,effective_at,created_by) VALUES($1,1,now(),$2)`, pricing, user)
	digest := sha256.Sum256(cardFor(upstream.URL + "/rpc"))
	created := h.want(owner, http.MethodPost, "/api/v1/agents", map[string]any{
		"project_id": project, "name": "monitor sweep",
		"agent": map[string]any{"protocol": "a2a-1.0.0", "endpoint": upstream.URL + "/rpc", "card_digest": hex.EncodeToString(digest[:]), "toolset_revisions": []string{}, "route_revisions": []string{}, "pricing_revision_id": pricing, "max_cost": "1", "message_price": "0.25", "remote_work_bound_seconds": 120, "task_deadline_seconds": 600},
	}, idem("sweep-agent"), http.StatusCreated)
	agent := created["id"].(string)
	h.want(owner, http.MethodPost, "/api/v1/agents/"+agent+"/publish", nil, withMatch(created, idem("sweep-publish")), http.StatusOK)
	oldKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "sweep original", "project_id": project, "scopes": []string{"agents"}, "allowed_agents": []string{agent}}, idem("sweep-original"), http.StatusCreated)
	backgroundKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "sweep background", "project_id": project, "scopes": []string{"agents"}, "allowed_agents": []string{agent}}, idem("sweep-background"), http.StatusCreated)
	client, prefix, _ := acctKeyspace(t)
	limiter, err := limits.New(client, strings.TrimSuffix(prefix, ":"))
	if err != nil {
		t.Fatal(err)
	}
	h.A2A.Ledger = &a2a.Ledger{Pool: h.Pool, Store: &limits.DebitStore{Pool: h.Pool, Limiter: limiter}, Auth: h.AuthKey}
	h.refresh()
	response, body := h.do(nil, http.MethodPost, "/a2a/"+agent, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "SendMessage", "params": a2aTestParams{Message: &protocol.Message{ID: "sweep", Role: protocol.MessageRoleUser, Parts: protocol.ContentParts{protocol.NewTextPart("question")}}}}, map[string]string{"Authorization": "Bearer " + oldKey["secret"].(string), "A2A-Version": "1.0"})
	var result struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if response.StatusCode != http.StatusOK || json.Unmarshal(body, &result) != nil || len(result.Result) == 0 || len(result.Error) != 0 {
		t.Fatalf("original task was not created: status=%d body=%s", response.StatusCode, body)
	}
	upstreamID := <-upstreamIDs
	var taskID string
	if err := h.Pool.QueryRow(t.Context(), `SELECT id::text FROM olp.a2a_tasks WHERE api_key_id=$1`, oldKey["id"]).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.a2a_tasks(id,api_key_id,project_id,publication_id,revision_id,dispatch_id,upstream_id,state,deadline_at)
 SELECT uuidv7(),$2,t.project_id,t.publication_id,t.revision_id,t.dispatch_id,uuidv7()::text,'active',now()+interval '1 hour'
 FROM olp.a2a_tasks t CROSS JOIN generate_series(1,127) WHERE t.id=$1`, taskID, backgroundKey["id"]); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	growing := &growingMonitorRuntime{Manager: h.Runtime, ctx: ctx, pool: h.Pool, oldKey: oldKey["id"].(string), backgroundKey: backgroundKey["id"].(string), task: taskID, firstCheck: make(chan struct{}), continueCheck: make(chan struct{}), errors: make(chan error, 1)}
	server := *h.A2A
	server.Runtime = growing
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		(&a2a.TaskMonitor{Server: &server}).Run(ctx)
	}()
	defer func() {
		stop()
		<-stopped
	}()
	select {
	case <-growing.firstCheck:
	case <-time.After(5 * time.Second):
		t.Fatal("monitor did not inspect the original task")
	}
	current := h.want(owner, http.MethodGet, "/api/v1/api-keys/"+growing.oldKey, nil, nil, http.StatusOK)
	h.want(owner, http.MethodPost, "/api/v1/api-keys/"+growing.oldKey+"/revoke", nil, withMatch(current, idem("sweep-revoke")), http.StatusOK)
	h.refresh()
	close(growing.continueCheck)
	select {
	case id := <-cancelAttempts:
		if id != upstreamID || growing.additions.Load() == 0 || growing.oldChecks.Load() < 2 {
			t.Fatalf("unexpected cancellation during the growing sweep: %s, additions=%d, checks=%d", id, growing.additions.Load(), growing.oldChecks.Load())
		}
	case err := <-growing.errors:
		t.Fatalf("could not append tasks: %v", err)
	case <-time.After(8 * time.Second):
		t.Fatalf("revoked original task was not revisited while tasks were added, additions=%d", growing.additions.Load())
	}
}
