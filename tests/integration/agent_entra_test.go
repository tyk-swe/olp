//go:build integration && oidctest

package integration_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
	"github.com/tyk-swe/olp/internal/a2a"
	"github.com/tyk-swe/olp/internal/limits"
)

func TestA2AEntraOnBehalfOfGrant(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "Entra agent")
	templates := h.want(owner, http.MethodGet, "/api/v1/projects/"+project+"/limit-templates", nil, nil, http.StatusOK)
	h.want(owner, http.MethodPut, "/api/v1/projects/"+project+"/limit-templates", map[string]any{"templates": map[string]any{"worker": map[string]any{"requests_per_minute": 100}}}, etagHeader(templates), http.StatusOK)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: public, KeyID: "entra", Algorithm: "EdDSA", Use: "sig"}}})
	}))
	t.Cleanup(issuer.Close)
	const clientID = "agent-middle-tier"
	const clientSecret = "agent-client-secret-fixture"
	const replacementSecret = "replacement-client-secret-fixture"
	const downstreamScope = "downstream/agent.send"
	var exchanges, refreshes, calls, leaked atomic.Int32
	tokenEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ParseForm() != nil || r.Form.Get("client_id") != clientID || r.Form.Get("client_secret") != clientSecret {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		subject := func() string {
			if r.Form.Get("grant_type") == "refresh_token" {
				refreshes.Add(1)
				return strings.TrimPrefix(r.Form.Get("refresh_token"), "refresh-")
			}
			// Microsoft Entra OBO request fields: https://learn.microsoft.com/en-us/entra/identity-platform/v2-oauth2-on-behalf-of-flow (observed 2026-10-29).
			if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || r.Form.Get("requested_token_use") != "on_behalf_of" || r.Form.Get("scope") != downstreamScope {
				return ""
			}
			claims := &entraExchangeClaims{}
			if _, err := jwt.ParseWithClaims(r.Form.Get("assertion"), claims, func(*jwt.Token) (any, error) { return public, nil }, jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithIssuer(issuer.URL), jwt.WithAudience(clientID)); err != nil {
				return ""
			}
			exchanges.Add(1)
			return claims.ObjectID
		}()
		if subject == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-" + subject, "refresh_token": "refresh-" + subject, "token_type": "Bearer", "expires_in": 3600})
	}))
	t.Cleanup(tokenEndpoint.Close)
	const card = `{"name":"Entra fixture","version":"1.0","supportedInterfaces":[{"url":"%s","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],"capabilities":{},"defaultInputModes":["text"],"defaultOutputModes":["text"],"skills":[]}`
	handler := a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(a2asrv.AgentExecutorFunc(func(_ context.Context, execution *a2asrv.ExecutorContext) iter.Seq2[protocol.Event, error] {
		return func(yield func(protocol.Event, error) bool) {
			if yield(protocol.NewSubmittedTask(execution, execution.Message), nil) {
				yield(protocol.NewStatusUpdateEvent(execution, protocol.TaskStateInputRequired, nil), nil)
			}
		}
	})))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), clientSecret) || strings.Contains(r.Header.Get("Authorization"), replacementSecret) {
			leaked.Add(1)
		}
		if r.URL.Path == "/.well-known/agent-card.json" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, card, "http://"+r.Host+"/rpc")
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer access-") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		calls.Add(1)
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	var user string
	if err := h.Pool.QueryRow(t.Context(), `SELECT id::text FROM olp.users WHERE email='owner@example.com'`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	pricing := acctID(t)
	acctExec(t, h.Pool, `INSERT INTO olp.pricing_revisions(id,revision,effective_at,created_by) VALUES($1,1,now(),$2)`, pricing, user)
	digest := sha256.Sum256([]byte(fmt.Sprintf(card, upstream.URL+"/rpc")))
	oauth := map[string]any{"client_id": clientID, "audience": "downstream", "scopes": []string{downstreamScope}, "token_endpoint": tokenEndpoint.URL, "entra_obo": map[string]string{"issuer": issuer.URL}}
	agent := map[string]any{"protocol": "a2a-1.0.0", "endpoint": upstream.URL + "/rpc", "card_digest": hex.EncodeToString(digest[:]), "toolset_revisions": []string{}, "route_revisions": []string{}, "pricing_revision_id": pricing, "max_cost": "1", "message_price": "0.01", "oauth": oauth}
	input := map[string]any{"project_id": project, "name": "Entra agent", "agent": agent, "entra_client_secret": clientSecret}
	created := h.want(owner, http.MethodPost, "/api/v1/agents", input, idem("entra-agent-create"), http.StatusCreated)
	if strings.Contains(string(mustJSON(created)), clientSecret) {
		t.Fatal("client secret appeared in publication")
	}
	agentID := created["id"].(string)
	published := h.want(owner, http.MethodPost, "/api/v1/agents/"+agentID+"/publish", nil, withMatch(created, idem("entra-agent-publish")), http.StatusOK)
	if leaked.Load() != 0 {
		t.Fatal("client secret was sent as the agent card credential")
	}
	h.want(owner, http.MethodPost, "/api/v1/workload-issuers", map[string]any{"name": "Entra agent issuer", "issuer": issuer.URL, "jwks_url": issuer.URL, "enabled": true, "audiences": []string{clientID}, "algorithms": []string{"EdDSA"}, "max_lifetime_seconds": 600, "disabled_key_ids": []string{}, "mappings": []any{map[string]any{"name": "users", "match": map[string]string{}, "project_id": project, "limit_template": "worker", "route_groups": []string{}, "allowed_agents": []string{agentID}, "scopes": []string{"agents"}, "end_user_claim": "/oid"}}}, idem("entra-agent-issuer"), http.StatusCreated)
	client, prefix, _ := acctKeyspace(t)
	limiter, err := limits.New(client, strings.TrimSuffix(prefix, ":"))
	if err != nil {
		t.Fatal(err)
	}
	h.A2A.Ledger = &a2a.Ledger{Pool: h.Pool, Store: &limits.DebitStore{Pool: h.Pool, Limiter: limiter}, Auth: h.AuthKey}
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: private}, (&jose.SignerOptions{}).WithHeader("kid", "entra"))
	if err != nil {
		t.Fatal(err)
	}
	assertion := func(id, scope string) string {
		t.Helper()
		now := time.Now()
		return signEntraAssertion(t, signer, entraTestClaims{Issuer: issuer.URL, Audience: clientID, Subject: "shared-workload", ObjectID: id, Scopes: scope, IssuedAt: now.Unix(), Expires: now.Add(5 * time.Minute).Unix()})
	}
	rpc := func(token, method string, params a2aTestParams) map[string]any {
		t.Helper()
		payload := func() a2aTestParams {
			if params.ID != "" || params.Message != nil {
				return params
			}
			return a2aTestParams{Message: &protocol.Message{ID: "message", Role: protocol.MessageRoleUser, Parts: protocol.ContentParts{protocol.NewTextPart("hello")}}}
		}()
		body := mustJSON(struct {
			JSONRPC string        `json:"jsonrpc"`
			ID      int           `json:"id"`
			Method  string        `json:"method"`
			Params  a2aTestParams `json:"params"`
		}{"2.0", 1, method, payload})
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.HTTP.URL+"/a2a/"+agentID, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("A2A-Version", "1.0")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var result map[string]any
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	if result := rpc(assertion("alice", ""), "SendMessage", a2aTestParams{}); result["error"] == nil || exchanges.Load() != 0 || calls.Load() != 0 {
		t.Fatalf("unverified user reached exchange: %v", result)
	}
	alice := assertion("alice", "delegated.read")
	first := rpc(alice, "SendMessage", a2aTestParams{})
	var result struct {
		Task *protocol.Task `json:"task"`
	}
	if err := json.Unmarshal(mustJSON(first["result"]), &result); err != nil || result.Task == nil || exchanges.Load() != 1 || calls.Load() != 1 {
		t.Fatalf("verified user did not send: %v", first)
	}
	var grantID string
	if err := h.Pool.QueryRow(t.Context(), `SELECT id::text FROM olp.mcp_user_grants WHERE agent_id=$1`, agentID).Scan(&grantID); err != nil {
		t.Fatal(err)
	}
	if got := rpc(alice, "SendMessage", a2aTestParams{}); got["error"] != nil || exchanges.Load() != 1 || calls.Load() != 2 {
		t.Fatalf("active grant was not reused: %v", got)
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.mcp_user_grants SET expires_at=now()-interval '1 second' WHERE id=$1`, grantID); err != nil {
		t.Fatal(err)
	}
	if got := rpc(alice, "SendMessage", a2aTestParams{}); got["error"] != nil || refreshes.Load() != 1 || calls.Load() != 3 {
		t.Fatalf("expired grant did not refresh: %v", got)
	}
	bob := assertion("bob", "delegated.read")
	if got := rpc(bob, "GetTask", a2aTestParams{ID: result.Task.ID}); got["error"] == nil || calls.Load() != 3 {
		t.Fatalf("another user accessed a task: %v", got)
	}
	bobResult := rpc(bob, "SendMessage", a2aTestParams{})
	var bobTask struct {
		Task *protocol.Task `json:"task"`
	}
	if err := json.Unmarshal(mustJSON(bobResult["result"]), &bobTask); err != nil || bobTask.Task == nil || exchanges.Load() != 2 || calls.Load() != 4 {
		t.Fatalf("second user did not receive an isolated grant: %v", bobResult)
	}
	standing := h.want(owner, http.MethodGet, "/api/v1/mcp-user-grants", nil, nil, http.StatusOK)
	export := h.want(owner, http.MethodGet, "/api/v1/configuration/export", nil, nil, http.StatusOK)
	for _, secret := range []string{clientSecret, "access-alice", "refresh-alice", "access-bob", "refresh-bob"} {
		if bytes.Contains(mustJSON(standing), []byte(secret)) || bytes.Contains(mustJSON(export), []byte(secret)) {
			t.Fatal("Entra secret or grant token was exported")
		}
	}
	plan := h.want(owner, http.MethodPost, "/api/v1/configuration/plan", map[string]any{"document": export["document"]}, nil, http.StatusOK)
	if len(plan["blockers"].([]any)) != 0 || len(plan["conflicts"].([]any)) != 0 {
		t.Fatalf("configuration could not reuse the existing Entra client secret: %v", plan)
	}
	h.want(owner, http.MethodDelete, "/api/v1/mcp-user-grants/"+grantID, nil, nil, http.StatusNoContent)
	if got := rpc(alice, "SendMessage", a2aTestParams{}); got["error"] == nil || exchanges.Load() != 2 || calls.Load() != 4 {
		t.Fatalf("revoked grant was recreated or dispatched: %v", got)
	}
	changed := map[string]any{"project_id": project, "name": "Entra agent", "agent": map[string]any{"protocol": "a2a-1.0.0", "endpoint": upstream.URL + "/rpc", "card_digest": hex.EncodeToString(digest[:]), "toolset_revisions": []string{}, "route_revisions": []string{}, "pricing_revision_id": pricing, "max_cost": "1", "message_price": "0.01", "oauth": map[string]any{"client_id": "other-client", "audience": "downstream", "scopes": []string{"downstream/agent.send"}, "token_endpoint": tokenEndpoint.URL, "entra_obo": map[string]string{"issuer": issuer.URL}}}}
	h.want(owner, http.MethodPut, "/api/v1/agents/"+agentID, changed, withMatch(published, idem("entra-agent-boundary")), http.StatusUnprocessableEntity)
	rotated := h.want(owner, http.MethodPut, "/api/v1/agents/"+agentID, map[string]any{"project_id": project, "name": "Entra agent", "agent": agent, "entra_client_secret": replacementSecret}, withMatch(published, idem("entra-agent-rotate")), http.StatusOK)
	if bytes.Contains(mustJSON(rotated), []byte(replacementSecret)) {
		t.Fatal("replacement Entra client secret appeared in publication")
	}
	republished := h.want(owner, http.MethodPost, "/api/v1/agents/"+agentID+"/publish", nil, withMatch(rotated, idem("entra-agent-rotate-publish")), http.StatusOK)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.mcp_user_grants SET expires_at=now()-interval '1 second' WHERE agent_id=$1 AND revoked_at IS NULL`, agentID); err != nil {
		t.Fatal(err)
	}
	if got := rpc(bob, "GetTask", a2aTestParams{ID: bobTask.Task.ID}); got["error"] != nil || got["result"] == nil || refreshes.Load() != 2 || calls.Load() != 5 {
		t.Fatalf("original Entra task did not refresh with its original secret after replacement: %v", got)
	}
	if got := rpc(bob, "SendMessage", a2aTestParams{}); got["error"] == nil || exchanges.Load() != 2 || calls.Load() != 5 {
		t.Fatalf("replacement Entra credential reused the original grant: %v", got)
	}
	h.want(owner, http.MethodDelete, "/api/v1/agents/"+agentID, nil, withMatch(republished, idem("entra-agent-retire")), http.StatusNoContent)
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.a2a_tasks SET deadline_at=now()-interval '1 second' WHERE id=$1`, bobTask.Task.ID); err != nil {
		t.Fatal(err)
	}
	monitorCtx, cancelMonitor := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&a2a.TaskMonitor{Server: h.A2A}).Run(monitorCtx)
	}()
	defer func() { cancelMonitor(); <-done }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var state string
		if err := h.Pool.QueryRow(t.Context(), `SELECT state FROM olp.a2a_tasks WHERE id=$1`, bobTask.Task.ID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state == "canceled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("restarted monitor did not cancel Entra task: %s", state)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if calls.Load() != 6 {
		t.Fatal("restarted monitor did not use the task's Entra grant")
	}
	if leaked.Load() != 0 {
		t.Fatal("client secret was sent to the agent")
	}
}
