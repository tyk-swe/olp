//go:build integration

package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	legacy "github.com/a2aproject/a2a-go/a2a"
	legacyserver "github.com/a2aproject/a2a-go/a2asrv"
	legacyqueue "github.com/a2aproject/a2a-go/a2asrv/eventqueue"
	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tyk-swe/olp/internal/a2a"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/mcp"
	"github.com/tyk-swe/olp/internal/mcpgrants"
	"github.com/tyk-swe/olp/internal/mcpservers"
	"github.com/tyk-swe/olp/internal/prompts"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/testutil"
	inferencesdk "github.com/tyk-swe/olp/sdk/inference"
)

type legacyReferenceExecutor struct{}

type legacyReferenceHandler struct{ legacyserver.RequestHandler }

func (h legacyReferenceHandler) OnResubscribeToTask(ctx context.Context, params *legacy.TaskIDParams) iter.Seq2[legacy.Event, error] {
	return func(yield func(legacy.Event, error) bool) {
		task, err := h.OnGetTask(ctx, &legacy.TaskQueryParams{ID: params.ID})
		if err != nil {
			yield(nil, err)
			return
		}
		yield(&legacy.TaskStatusUpdateEvent{TaskID: task.ID, ContextID: task.ContextID, Status: task.Status, Final: true}, nil)
	}
}

func (legacyReferenceExecutor) Execute(ctx context.Context, request *legacyserver.RequestContext, queue legacyqueue.Queue) error {
	message := legacy.NewMessageForTask(legacy.MessageRoleAgent, request, request.Message.Parts...)
	task := request.TaskInfo()
	return queue.Write(ctx, &legacy.TaskStatusUpdateEvent{TaskID: task.TaskID, ContextID: task.ContextID, Final: true, Status: legacy.TaskStatus{State: legacy.TaskStateCompleted, Message: message}})
}

func (legacyReferenceExecutor) Cancel(ctx context.Context, request *legacyserver.RequestContext, queue legacyqueue.Queue) error {
	task := request.TaskInfo()
	return queue.Write(ctx, &legacy.TaskStatusUpdateEvent{TaskID: task.TaskID, ContextID: task.ContextID, Final: true, Status: legacy.TaskStatus{State: legacy.TaskStateCanceled}})
}

func TestA2A03AgentSendAndGet(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "A2A 0.3")
	agent := legacyserver.NewJSONRPCHandler(legacyReferenceHandler{legacyserver.NewHandler(legacyReferenceExecutor{})})
	cardFor := func(endpoint string) []byte {
		t.Helper()
		card, err := json.Marshal(legacy.AgentCard{
			Name: "Reference agent", Description: "Test agent", Version: "1",
			ProtocolVersion: string(legacy.Version), URL: endpoint, PreferredTransport: legacy.TransportProtocolJSONRPC,
			Capabilities:      legacy.AgentCapabilities{Streaming: true},
			DefaultInputModes: []string{"text"}, DefaultOutputModes: []string{"text"}, Skills: []legacy.AgentSkill{},
		})
		if err != nil {
			t.Fatal(err)
		}
		return card
	}
	var dispatched atomic.Int32
	var changedCard atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rpc" {
			dispatched.Add(1)
			agent.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/.well-known/agent-card.json" {
			card := cardFor("http://" + r.Host + "/rpc")
			if changedCard.Load() {
				_, _ = w.Write(bytes.Join([][]byte{card, []byte(" ")}, nil))
				return
			}
			_, _ = w.Write(card)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(upstream.Close)
	card := cardFor(upstream.URL + "/rpc")
	digest := sha256.Sum256(card)
	var user string
	if err := h.Pool.QueryRow(t.Context(), `SELECT id::text FROM olp.users WHERE email='owner@example.com'`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	pricing := acctID(t)
	acctExec(t, h.Pool, `INSERT INTO olp.pricing_revisions(id,revision,effective_at,created_by) VALUES($1,1,now(),$2)`, pricing, user)
	mismatched := h.want(owner, http.MethodPost, "/api/v1/agents", map[string]any{
		"project_id": project, "name": "wrong protocol",
		"agent": map[string]any{"protocol": "a2a-1.0.0", "endpoint": upstream.URL + "/rpc", "card_digest": hex.EncodeToString(digest[:]), "toolset_revisions": []string{}, "route_revisions": []string{}, "pricing_revision_id": pricing, "max_cost": "1", "message_price": "0.25"},
	}, idem("a2a-03-mismatch-create"), http.StatusCreated)
	h.want(owner, http.MethodPost, "/api/v1/agents/"+mismatched["id"].(string)+"/publish", nil, withMatch(mismatched, idem("a2a-03-mismatch-publish")), http.StatusUnprocessableEntity)
	for _, invalid := range []struct {
		name, version string
		binding       legacy.TransportProtocol
		hybrid        bool
	}{
		{name: "binding", version: string(legacy.Version), binding: legacy.TransportProtocolGRPC},
		{name: "version", version: "0.2.0", binding: legacy.TransportProtocolJSONRPC},
		{name: "undeclared-version", binding: legacy.TransportProtocolJSONRPC},
		{name: "hybrid-binding", version: string(legacy.Version), binding: legacy.TransportProtocolGRPC, hybrid: true},
	} {
		badCard := func(endpoint string) []byte {
			t.Helper()
			interfaces := func() []*protocol.AgentInterface {
				if !invalid.hybrid {
					return nil
				}
				return []*protocol.AgentInterface{{URL: endpoint, ProtocolBinding: protocol.TransportProtocolJSONRPC, ProtocolVersion: a2av0.Version}}
			}()
			card, err := json.Marshal(struct {
				legacy.AgentCard
				SupportedInterfaces []*protocol.AgentInterface `json:"supportedInterfaces,omitempty"`
			}{AgentCard: legacy.AgentCard{
				Name: "Reference agent", Version: "1", URL: endpoint, ProtocolVersion: invalid.version, PreferredTransport: invalid.binding,
			}, SupportedInterfaces: interfaces})
			if err != nil {
				t.Fatal(err)
			}
			return card
		}
		badUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(badCard("http://" + r.Host + "/rpc"))
		}))
		t.Cleanup(badUpstream.Close)
		badDigest := sha256.Sum256(badCard(badUpstream.URL + "/rpc"))
		bad := h.want(owner, http.MethodPost, "/api/v1/agents", map[string]any{
			"project_id": project, "name": "invalid " + invalid.name,
			"agent": map[string]any{"protocol": "a2a-0.3.0", "endpoint": badUpstream.URL + "/rpc", "card_digest": hex.EncodeToString(badDigest[:]), "toolset_revisions": []string{}, "route_revisions": []string{}, "pricing_revision_id": pricing, "max_cost": "1", "message_price": "0.25"},
		}, idem("a2a-03-invalid-"+invalid.name), http.StatusCreated)
		h.want(owner, http.MethodPost, "/api/v1/agents/"+bad["id"].(string)+"/publish", nil, withMatch(bad, idem("a2a-03-invalid-publish-"+invalid.name)), http.StatusUnprocessableEntity)
	}
	created := h.want(owner, http.MethodPost, "/api/v1/agents", map[string]any{
		"project_id": project, "name": "0.3 reference",
		"agent": map[string]any{"protocol": "a2a-0.3.0", "endpoint": upstream.URL + "/rpc", "card_digest": hex.EncodeToString(digest[:]), "toolset_revisions": []string{}, "route_revisions": []string{}, "pricing_revision_id": pricing, "max_cost": "1", "message_price": "0.25"},
	}, idem("a2a-03-create"), http.StatusCreated)
	agentID := created["id"].(string)
	published := h.want(owner, http.MethodPost, "/api/v1/agents/"+agentID+"/publish", nil, withMatch(created, idem("a2a-03-publish")), http.StatusOK)
	key := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "A2A caller", "project_id": project, "scopes": []string{"agents"}, "allowed_agents": []string{agentID}}, idem("a2a-03-key"), http.StatusCreated)["secret"].(string)
	valkey, prefix, _ := acctKeyspace(t)
	limiter, err := limits.New(valkey, strings.TrimSuffix(prefix, ":"))
	if err != nil {
		t.Fatal(err)
	}
	h.A2A.Ledger = &a2a.Ledger{Pool: h.Pool, Store: &limits.DebitStore{Pool: h.Pool, Limiter: limiter}, Auth: h.AuthKey}
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	cardRequest, err := http.NewRequest(http.MethodGet, h.HTTP.URL+"/a2a/"+agentID+"/.well-known/agent-card.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	cardRequest.Header.Set("Authorization", "Bearer "+key)
	cardResponse, err := http.DefaultClient.Do(cardRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer cardResponse.Body.Close()
	cardBody, err := io.ReadAll(cardResponse.Body)
	if err != nil || cardResponse.StatusCode != http.StatusOK {
		t.Fatalf("0.3 card: status %d, %v", cardResponse.StatusCode, err)
	}
	publicCard, err := a2av0.NewAgentCardParser()(cardBody)
	if err != nil || len(publicCard.SupportedInterfaces) != 1 || publicCard.SupportedInterfaces[0].URL != h.HTTP.URL+"/a2a/"+agentID || publicCard.SupportedInterfaces[0].ProtocolBinding != protocol.TransportProtocolJSONRPC || publicCard.SupportedInterfaces[0].ProtocolVersion != a2av0.Version {
		t.Fatalf("0.3 card interface: %+v, %v", publicCard, err)
	}
	security, ok := publicCard.SecuritySchemes["olp"].(protocol.HTTPAuthSecurityScheme)
	if !ok || security.Scheme != "Bearer" || len(publicCard.SecurityRequirements) != 1 {
		t.Fatalf("0.3 card authentication: %+v", publicCard.SecuritySchemes)
	}
	for _, invalid := range []struct {
		version, query, contentType, method, params string
		status, errorCode                           int
	}{
		{version: "1.0", contentType: "application/json", method: "SendMessage", params: "{}", status: http.StatusBadRequest},
		{version: "0.2", contentType: "application/json", method: "message/send", params: "{}", status: http.StatusBadRequest},
		{query: "1.0", contentType: "application/json", method: "message/send", params: "{}", status: http.StatusBadRequest},
		{version: "0.3", query: "1.0", contentType: "application/json", method: "message/send", params: "{}", status: http.StatusBadRequest},
		{version: "0.3", contentType: "application/grpc", method: "message/send", params: "{}", status: http.StatusUnsupportedMediaType},
		{version: "0.3", contentType: "application/json", method: "SendMessage", params: "{}", status: http.StatusOK, errorCode: -32601},
		{version: "0.3", contentType: "application/json", method: "message/send", params: "[]", status: http.StatusOK, errorCode: -32602},
		{version: "0.3", contentType: "application/json", method: "message/send", params: "{}", status: http.StatusOK, errorCode: -32602},
		{version: "0.3", contentType: "application/json", method: "message/send", params: `{"configuration":{"pushNotificationConfig":{"url":"https://example.invalid"}}}`, status: http.StatusOK, errorCode: -32602},
		{version: "0.3", contentType: "application/json", method: "message/send", params: `{"message":{"messageId":"msg","role":"user","parts":[{"kind":"text","text":"hi"}]},"configuration":{"pushNotificationConfig":{"url":"https://example.invalid"}}}`, status: http.StatusOK, errorCode: -32602},
	} {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, invalid.method, invalid.params)
		request, err := http.NewRequest(http.MethodPost, h.HTTP.URL+"/a2a/"+agentID+"?A2A-Version="+invalid.query, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+key)
		request.Header.Set("Content-Type", invalid.contentType)
		request.Header.Set("A2A-Version", invalid.version)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != invalid.status {
			t.Fatalf("invalid version or binding returned %d, want %d", response.StatusCode, invalid.status)
		}
		if invalid.status == http.StatusOK {
			var failure struct {
				Error *struct {
					Code int `json:"code"`
				} `json:"error"`
			}
			if err := json.NewDecoder(response.Body).Decode(&failure); err != nil || failure.Error == nil || failure.Error.Code != invalid.errorCode {
				t.Fatalf("invalid request returned the wrong error: %+v, %v", failure, err)
			}
		}
		response.Body.Close()
		if dispatched.Load() != 0 {
			t.Fatal("invalid version or binding reached the upstream")
		}
	}
	transport := a2av0.NewJSONRPCTransport(a2av0.JSONRPCTransportConfig{URL: h.HTTP.URL + "/a2a/" + agentID, Client: http.DefaultClient})
	defer transport.Destroy()
	params := a2aclient.ServiceParams{"Authorization": []string{"Bearer " + key}}
	result, err := transport.SendMessage(t.Context(), params, &protocol.SendMessageRequest{Message: protocol.NewMessage(protocol.MessageRoleUser, protocol.NewTextPart("hello"))})
	if err != nil {
		t.Fatalf("0.3 send: %v", err)
	}
	task, ok := result.(*protocol.Task)
	if !ok || task.ID == "" {
		t.Fatalf("send did not return an owned task: %+v", result)
	}
	read, err := transport.GetTask(t.Context(), params, &protocol.GetTaskRequest{ID: task.ID})
	if err != nil || read.ID != task.ID || read.Status.State != protocol.TaskStateCompleted {
		t.Fatalf("0.3 get task: %+v, %v", read, err)
	}
	if read.Status.Message == nil || len(read.Status.Message.Parts) != 1 || read.Status.Message.Parts[0].Text() != "hello" {
		t.Fatalf("0.3 message did not survive the round trip: %+v", read.Status.Message)
	}
	// Generated request editors set per-call headers on the supplied request.
	sdkClient, err := inferencesdk.NewClient(h.HTTP.URL, inferencesdk.WithRequestEditorFn(func(_ context.Context, request *http.Request) error {
		request.Header.Set("Authorization", "Bearer "+key)
		request.Header.Set("A2A-Version", "0.3")
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	var callID inferencesdk.A2ARequest_Id
	if err := callID.FromA2ARequestId0("sdk-get"); err != nil {
		t.Fatal(err)
	}
	sdkResponse, err := sdkClient.CallA2AAgent(t.Context(), uuid.MustParse(agentID), inferencesdk.A2ARequest{Id: callID, Jsonrpc: "2.0", Method: "tasks/get", Params: map[string]any{"id": task.ID}})
	if err != nil {
		t.Fatal(err)
	}
	var sdkResult struct {
		Result legacy.Task `json:"result"`
	}
	if err := json.NewDecoder(sdkResponse.Body).Decode(&sdkResult); err != nil {
		t.Fatal(err)
	}
	sdkResponse.Body.Close()
	if sdkResponse.StatusCode != http.StatusOK || string(sdkResult.Result.ID) != string(task.ID) {
		t.Fatalf("Go inference SDK task read: status %d, result %+v", sdkResponse.StatusCode, sdkResult.Result)
	}
	queryTransport := a2av0.NewJSONRPCTransport(a2av0.JSONRPCTransportConfig{URL: h.HTTP.URL + "/a2a/" + agentID + "?A2A-Version=0.3.0", Client: http.DefaultClient})
	defer queryTransport.Destroy()
	queryTask, err := queryTransport.GetTask(t.Context(), params, &protocol.GetTaskRequest{ID: task.ID})
	if err != nil || queryTask.ID != task.ID {
		t.Fatalf("0.3 task read with version query: %+v, %v", queryTask, err)
	}
	changedCard.Store(true)
	_, changedErr := transport.GetTask(t.Context(), params, &protocol.GetTaskRequest{ID: task.ID})
	if changedErr == nil {
		t.Fatal("changed pinned card was accepted")
	}
	changedCard.Store(false)
	events := slices.Collect(func(yield func(protocol.Event) bool) {
		for event, err := range transport.SendStreamingMessage(t.Context(), params, &protocol.SendMessageRequest{Message: protocol.NewMessage(protocol.MessageRoleUser, protocol.NewTextPart("streamed message"))}) {
			if err != nil {
				t.Fatalf("0.3 stream: %v", err)
			}
			if !yield(event) {
				return
			}
		}
	})
	if len(events) == 0 {
		t.Fatalf("0.3 stream omitted task updates: %+v", events)
	}
	streamedTask, ok := events[0].(*protocol.TaskStatusUpdateEvent)
	if !ok || streamedTask.TaskID == "" || streamedTask.Status.Message == nil || len(streamedTask.Status.Message.Parts) != 1 || streamedTask.Status.Message.Parts[0].Text() != "streamed message" {
		t.Fatalf("0.3 stream omitted public task: %+v", events[0])
	}
	streamedResult, err := transport.GetTask(t.Context(), params, &protocol.GetTaskRequest{ID: streamedTask.TaskID})
	if err != nil || streamedResult.ID != streamedTask.TaskID || streamedResult.Status.State != protocol.TaskStateCompleted {
		t.Fatalf("0.3 stream task read: %+v, %v", streamedResult, err)
	}
	updates := slices.Collect(func(yield func(protocol.Event) bool) {
		for event, err := range transport.SubscribeToTask(t.Context(), params, &protocol.SubscribeToTaskRequest{ID: streamedTask.TaskID}) {
			if err != nil {
				t.Fatalf("0.3 resubscribe: %v", err)
			}
			if !yield(event) {
				return
			}
		}
	})
	if len(updates) != 1 || updates[0].TaskInfo().TaskID != streamedTask.TaskID {
		t.Fatalf("0.3 resubscription lost the public task: %+v", updates)
	}
	newCard := func(endpoint string) []byte {
		data, err := json.Marshal(protocol.AgentCard{Name: "New agent", Version: "1", SupportedInterfaces: []*protocol.AgentInterface{{URL: endpoint, ProtocolBinding: protocol.TransportProtocolJSONRPC, ProtocolVersion: protocol.Version}}, Skills: []protocol.AgentSkill{}})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	replacement := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/agent-card.json" {
			_, _ = w.Write(newCard("http://" + r.Host + "/rpc"))
			return
		}
		http.Error(w, "replacement unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(replacement.Close)
	replacementDigest := sha256.Sum256(newCard(replacement.URL + "/rpc"))
	updated := h.want(owner, http.MethodPut, "/api/v1/agents/"+agentID, map[string]any{
		"project_id": project, "name": "0.3 reference",
		"agent": map[string]any{"protocol": "a2a-1.0.0", "endpoint": replacement.URL + "/rpc", "card_digest": hex.EncodeToString(replacementDigest[:]), "toolset_revisions": []string{}, "route_revisions": []string{}, "pricing_revision_id": pricing, "max_cost": "1", "message_price": "0.25"},
	}, withMatch(published, idem("a2a-03-republish-draft")), http.StatusOK)
	h.want(owner, http.MethodPost, "/api/v1/agents/"+agentID+"/publish", nil, withMatch(updated, idem("a2a-03-republish")), http.StatusOK)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	sdkCardResponse, err := sdkClient.GetA2AAgentCard(t.Context(), uuid.MustParse(agentID))
	if err != nil {
		t.Fatal(err)
	}
	var sdkCard inferencesdk.A2AAgentCard
	if err := json.NewDecoder(sdkCardResponse.Body).Decode(&sdkCard); err != nil {
		t.Fatal(err)
	}
	sdkCardResponse.Body.Close()
	if sdkCardResponse.StatusCode != http.StatusOK || len(sdkCard.SupportedInterfaces) != 1 || sdkCard.SupportedInterfaces[0].ProtocolBinding != inferencesdk.JSONRPC || sdkCard.SupportedInterfaces[0].ProtocolVersion != inferencesdk.N10 {
		t.Fatalf("Go inference SDK card after republication: %+v", sdkCard)
	}
	pinnedTask, err := transport.GetTask(t.Context(), params, &protocol.GetTaskRequest{ID: task.ID})
	if err != nil || pinnedTask.ID != task.ID || pinnedTask.Status.Message == nil || pinnedTask.Status.Message.Parts[0].Text() != "hello" {
		t.Fatalf("0.3 task no longer accessible after republishing: %+v, %v", pinnedTask, err)
	}
	_, wrongVersionErr := transport.SendMessage(t.Context(), params, &protocol.SendMessageRequest{Message: protocol.NewMessage(protocol.MessageRoleUser, protocol.NewTextPart("new task"))})
	if wrongVersionErr == nil {
		t.Fatal("new 0.3 task was accepted after 1.0 publication")
	}
	pinnedUpdates := slices.Collect(func(yield func(protocol.Event) bool) {
		for event, err := range transport.SubscribeToTask(t.Context(), params, &protocol.SubscribeToTaskRequest{ID: streamedTask.TaskID}) {
			if err != nil {
				t.Fatalf("0.3 pinned resubscribe: %v", err)
			}
			if !yield(event) {
				return
			}
		}
	})
	if len(pinnedUpdates) != 1 || pinnedUpdates[0].TaskInfo().TaskID != streamedTask.TaskID {
		t.Fatalf("0.3 pinned resubscription lost the public task: %+v", pinnedUpdates)
	}
}

func TestAgentPublicationsManagementAndConfiguration(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	org := h.want(owner, http.MethodPost, "/api/v1/organizations", map[string]any{"name": "Agent contracts"}, idem("rm4-org"), 201)
	project := h.want(owner, http.MethodPost, "/api/v1/organizations/"+org["id"].(string)+"/projects", map[string]any{"name": "Agent contracts"}, idem("rm4-project"), 201)["id"].(string)
	draft := map[string]any{"project_id": project, "name": "greeting", "prompt": map[string]any{"template": "Hello {{name}}", "variables": []any{"name"}}}
	created := h.want(owner, http.MethodPost, "/api/v1/prompts", draft, idem("rm4-create"), 201)
	replayed := h.want(owner, http.MethodPost, "/api/v1/prompts", draft, idem("rm4-create"), 201)
	if created["id"] != replayed["id"] {
		t.Fatal("creation retry produced another publication")
	}
	path := "/api/v1/prompts/" + created["id"].(string)
	headers := func(tag, key string) map[string]string {
		return map[string]string{"If-Match": `"` + tag + `"`, "Idempotency-Key": key}
	}
	published := h.want(owner, http.MethodPost, path+"/publish", nil, headers(created["etag"].(string), "rm4-publish"), 200)
	revision := published["revision_id"].(string)
	h.want(owner, http.MethodPut, path, draft, headers(created["etag"].(string), "rm4-stale"), 412)
	labeled := h.want(owner, http.MethodPut, path+"/labels/production", map[string]any{"revision_id": revision}, headers(published["etag"].(string), "rm4-label"), 200)
	read := h.want(owner, http.MethodGet, path+"/revisions/"+revision, nil, nil, 200)
	if read["digest"] == "" || len(read["labels"].([]any)) != 1 {
		t.Fatal("immutable revision lost digest or label")
	}
	changed := map[string]any{"project_id": project, "name": "greeting", "prompt": map[string]any{"template": "Goodbye {{name}}", "variables": []any{"name"}}}
	saved := h.want(owner, http.MethodPut, path, changed, headers(labeled["etag"].(string), "rm4-update"), 200)
	if saved["revision_id"] != revision {
		t.Fatal("editing a draft changed published execution")
	}
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Export keeps the mutable draft, installed revision and label distinct.
	exported := h.want(owner, http.MethodGet, "/api/v1/configuration/export", nil, nil, 200)
	document := exported["document"].(map[string]any)
	entries := document["agent_publications"].([]any)
	if len(entries) != 1 {
		t.Fatalf("exported publications: %v", entries)
	}
	e := entries[0].(map[string]any)
	if e["draft"].(map[string]any)["prompt"].(map[string]any)["template"] != "Goodbye {{name}}" || e["published"].(map[string]any)["prompt"].(map[string]any)["template"] != "Hello {{name}}" {
		t.Fatal("export merged draft and publication")
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "credential_id") {
		t.Fatal("export leaked destination secret identities")
	}
	// A nested revision is never authorized through a different publication.
	second := h.want(owner, http.MethodPost, "/api/v1/prompts", map[string]any{"project_id": project, "name": "other", "prompt": map[string]any{"template": "plain", "variables": []any{}}}, idem("rm4-other"), 201)
	h.want(owner, http.MethodGet, "/api/v1/prompts/"+second["id"].(string)+"/revisions/"+revision, nil, nil, 404)
	h.want(owner, http.MethodDelete, path, nil, headers(saved["etag"].(string), "rm4-retire"), 204)
	h.want(owner, http.MethodPost, path+"/publish", nil, headers(saved["etag"].(string), "rm4-retired-write"), 412)
	var events, unscoped int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*),count(*) FILTER (WHERE project_id IS DISTINCT FROM $2::uuid) FROM olp.audit WHERE resource_id=$1`, created["id"], project).Scan(&events, &unscoped); err != nil || events != 5 || unscoped != 0 {
		t.Fatalf("publication audits lost project ownership or included replays/failures: events=%d unscoped=%d error=%v", events, unscoped, err)
	}
}

func TestAgentPromptLabelWithoutCurrentRevision(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := h.want(owner, http.MethodPost, "/api/v1/projects", map[string]any{"name": "Label history"}, idem("label-project"), 201)["id"].(string)
	created := h.want(owner, http.MethodPost, "/api/v1/prompts", map[string]any{"project_id": project, "name": "greeting", "prompt": map[string]any{"template": "Hello", "variables": []any{}}}, idem("label-create"), 201)
	path := "/api/v1/prompts/" + created["id"].(string)
	published := h.want(owner, http.MethodPost, path+"/publish", nil, withMatch(created, idem("label-publish")), 200)
	revision := published["revision_id"].(string)
	h.want(owner, http.MethodPut, path+"/labels/production", map[string]any{"revision_id": revision}, withMatch(published, idem("label-production")), 200)
	exported := h.want(owner, http.MethodGet, "/api/v1/configuration/export", nil, nil, 200)
	document := exported["document"].(map[string]any)
	entry := document["agent_publications"].([]any)[0].(map[string]any)
	delete(entry, "published") // Keep immutable label history without a default publication.
	h.want(owner, http.MethodPost, "/api/v1/configuration/apply", map[string]any{"document": document}, idem("label-only-apply"), 200)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	read := h.want(owner, http.MethodGet, path+"/revisions/"+revision, nil, nil, 200)
	if read["current"] != false || len(read["labels"].([]any)) != 1 {
		t.Fatal("retained prompt revision lost its label or became current")
	}
}

func TestAgentPublicationKeyGrantsRemainVisible(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "Key grants")
	created := h.want(owner, http.MethodPost, "/api/v1/toolsets", map[string]any{"project_id": project, "name": "empty", "toolset": map[string]any{"tools": []any{}}}, idem("grants-toolset"), 201)
	published := h.want(owner, http.MethodPost, "/api/v1/toolsets/"+created["id"].(string)+"/publish", nil, withMatch(created, idem("grants-publish")), 200)
	key := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "Tool caller", "project_id": project, "scopes": []string{"tools"}, "allowed_toolsets": []string{created["id"].(string)}}, idem("grants-key"), 201)
	path := "/api/v1/api-keys/" + key["id"].(string)
	check := func(read map[string]any) {
		t.Helper()
		tools, toolsOK := read["allowed_toolsets"].([]any)
		agents, agentsOK := read["allowed_agents"].([]any)
		if !toolsOK || len(tools) != 1 || tools[0] != created["id"] || !agentsOK || len(agents) != 0 {
			t.Fatalf("resource grants missing from key response: %v", read)
		}
	}
	read := h.want(owner, http.MethodGet, path, nil, nil, 200)
	check(read)
	h.want(owner, http.MethodPatch, path, map[string]any{"name": "Renamed caller"}, withMatch(read, idem("grants-rename")), 200)
	check(h.want(owner, http.MethodGet, path, nil, nil, 200))
	h.want(owner, http.MethodDelete, "/api/v1/toolsets/"+created["id"].(string), nil, withMatch(published, idem("grants-retire")), 204)
	read = h.want(owner, http.MethodGet, path, nil, nil, 200)
	h.want(owner, http.MethodPatch, path, map[string]any{"name": "Historical grants", "allowed_toolsets": read["allowed_toolsets"]}, withMatch(read, idem("grants-retired-rename")), 200)
	check(h.want(owner, http.MethodGet, path, nil, nil, 200))
	for _, item := range h.want(owner, http.MethodGet, "/api/v1/api-keys", nil, nil, 200)["items"].([]any) {
		read := item.(map[string]any)
		if read["id"] == key["id"] {
			check(read)
			removed := h.want(owner, http.MethodPatch, path, map[string]any{"allowed_toolsets": []string{}}, withMatch(read, idem("grants-remove")), 200)
			h.want(owner, http.MethodPatch, path, map[string]any{"allowed_toolsets": []string{created["id"].(string)}}, withMatch(removed, idem("grants-readd-retired")), 422)
			return
		}
	}
	t.Fatal("created key missing from list")
}

func TestAgentApprovalConsumptionCommitsWithDispatch(t *testing.T) {
	f, store, project := debitFixture(t)
	server, serverRevision := acctID(t), acctID(t)
	digest := strings.Repeat("a", 64)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = tx.Exec(t.Context(), `INSERT INTO olp.mcp_servers(id,project_id,name,transport,endpoint,enabled,latest_revision_id,etag,created_by) VALUES($1,$2,'approved','streamable_http','https://tools.example.test/mcp',true,$3,$4,$5)`, server, project, serverRevision, acctID(t), f.User); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(t.Context(), `INSERT INTO olp.mcp_server_revisions(id,server_id,revision,endpoint,protocol_version,tools,digest,created_by) VALUES($1,$2,1,'https://tools.example.test/mcp','2025-11-25','[{"name":"lookup","input_schema":{"type":"object"}}]'::jsonb,$3,$4)`, serverRevision, server, digest, f.User); err != nil {
		t.Fatal(err)
	}
	tool := agentcontract.Tool{ServerID: server, RevisionID: serverRevision, CatalogDigest: digest, Name: "lookup", Alias: agentcontract.ToolAlias(server, "lookup")}
	p := &agents.Publication{Kind: "toolset", Draft: agentcontract.Draft{ProjectID: project, Name: "approved", Toolset: &agentcontract.Toolset{Tools: []agentcontract.Tool{tool}}}}
	if err = agents.SaveDraft(t.Context(), tx, f.User, nil, p, nil); err != nil {
		t.Fatal(err)
	}
	if err = agents.Publish(t.Context(), tx, f.User, p); err != nil {
		t.Fatal(err)
	}
	a := access.Authority{ID: f.Key, ProjectID: &project, Policy: access.KeyPolicy{Scopes: []string{"tools"}, AllowedToolsets: []string{p.ID}}}
	key := secrets.NewAuthKey(bytes.Repeat([]byte{1}, 32), project)
	run := acctID(t)
	if err = agents.StartRunTx(t.Context(), tx, key, a, run, "tools", p.ID, *p.RevisionID); err != nil {
		t.Fatal(err)
	}
	i := debitIntent(t, f, project)
	i.Kind = "tool"
	i.RunID = run
	i.StepID = acctID(t)
	i.Affinity.RevisionID = *p.RevisionID
	i.Affinity.ToolAlias = tool.Alias
	i.Affinity.InputDigest = strings.Repeat("b", 64)
	i.Affinity.TranscriptDigest = strings.Repeat("c", 64)
	if _, err = agents.PrepareStepTx(t.Context(), tx, store, a, i, 0, []limits.DebitOwner{{ID: f.Key, Kind: "key"}, {ID: f.Provider, Kind: "supply"}}); err != nil {
		t.Fatal(err)
	}
	var principal []byte
	if err = tx.QueryRow(t.Context(), `SELECT principal_digest FROM olp.agent_runs WHERE id=$1`, run).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	b := agents.ApprovalBinding{RunID: run, ToolsetID: p.ID, ToolsetRevisionID: *p.RevisionID, ToolAlias: tool.Alias, PrincipalDigest: hex.EncodeToString(principal), ArgumentDigest: i.Affinity.InputDigest, TranscriptDigest: i.Affinity.TranscriptDigest, MaxCost: "2", Currency: "USD"}
	approval, err := agents.CreateApprovalTx(t.Context(), tx, key, a, b, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	valkey, prefix, _ := acctKeyspace(t)
	store.Limiter, err = limits.New(valkey, strings.TrimSuffix(prefix, ":"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Admit(t.Context(), i.ID, i.ClaimID, []limits.Request{{CostOwnerID: f.Key}, {CostOwnerID: f.Provider}}); err != nil {
		t.Fatal(err)
	}
	consume := func(binding agents.ApprovalBinding, dispatch bool) error {
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			return err
		}
		defer tx.Rollback(t.Context())
		if err = agents.ConsumeApprovalTx(t.Context(), tx, key, a, approval, i.ID, binding); err != nil {
			return err
		}
		if dispatch {
			if err = limits.MarkDispatchedTx(t.Context(), tx, i.ID, i.ClaimID); err != nil {
				return err
			}
		}
		return tx.Commit(t.Context())
	}
	changed := b
	changed.ArgumentDigest = strings.Repeat("d", 64)
	if err = consume(changed, true); err == nil {
		t.Fatal("approval accepted changed arguments")
	}
	if err = consume(b, false); err == nil {
		t.Fatal("approval committed without its dispatch")
	}
	acctExec(t, f.Pool, `UPDATE olp.mcp_servers SET enabled=false WHERE id=$1`, server)
	if err = consume(b, true); err == nil {
		t.Fatal("approval bypassed live MCP revocation")
	}
	acctExec(t, f.Pool, `UPDATE olp.mcp_servers SET enabled=true WHERE id=$1`, server)
	if err = consume(b, true); err != nil {
		t.Fatal(err)
	}
	if err = consume(b, true); err == nil {
		t.Fatal("approval was reusable")
	}
}

func TestAgentPublicationImmutableRows(t *testing.T) {
	f, _, project := debitFixture(t)
	id, revision := acctID(t), acctID(t)
	d := agentcontract.Draft{ProjectID: project, Name: "pinned", Prompt: &prompts.Definition{Template: "operator template", Variables: []string{}}}
	body, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	acctExec(t, f.Pool, `INSERT INTO olp.agent_publications(id,project_id,kind,name,draft,etag,created_by) VALUES($1,$2,'prompt','pinned',$3,$4,$5)`, id, project, body, acctID(t), f.User)
	acctExec(t, f.Pool, `INSERT INTO olp.agent_publication_revisions(id,publication_id,project_id,kind,definition,digest,created_by) VALUES($1,$2,$3,'prompt',$4,$5,$6)`, revision, id, project, body, agentcontract.DraftDigest(d), f.User)
	if _, err = f.Pool.Exec(t.Context(), `UPDATE olp.agent_publication_revisions SET digest=$2 WHERE id=$1`, revision, strings.Repeat("a", 64)); err == nil {
		t.Fatal("published evidence was mutable")
	}
	if _, err = f.Pool.Exec(t.Context(), `DELETE FROM olp.agent_publication_revisions WHERE id=$1`, revision); err == nil {
		t.Fatal("published evidence could be deleted")
	}
	var text string
	if err = f.Pool.QueryRow(t.Context(), `SELECT definition::text FROM olp.agent_publication_revisions WHERE id=$1`, revision).Scan(&text); err != nil || !strings.Contains(text, "operator template") {
		t.Fatalf("immutable definition lost: %s %v", text, err)
	}
}

type a2aTestParams struct {
	ID      protocol.TaskID             `json:"id,omitempty"`
	Message *protocol.Message           `json:"message,omitempty"`
	Config  *protocol.SendMessageConfig `json:"configuration,omitempty"`
}

type a2aContentTypeWriter struct{ http.ResponseWriter }

func (w a2aContentTypeWriter) WriteHeader(status int) {
	if w.Header().Get("Content-Type") == "text/event-stream" {
		w.Header().Set("Content-Type", "Text/Event-Stream; charset=utf-8")
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w a2aContentTypeWriter) Flush() { w.ResponseWriter.(http.Flusher).Flush() }

func TestA2ATaskCapabilityAuthorizesOwnerCallback(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "A2A callbacks")
	provider := newOpenAIFixture(t, "")
	model := activeAzureProviderInProject(t, h, owner, "Callback provider", provider.URL, &project, []any{
		map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"},
		map[string]any{"operation": "generation", "surface": "anthropic", "mode": "unary"},
		map[string]any{"operation": "generation", "surface": "gemini", "mode": "unary"},
		map[string]any{"operation": "generation", "surface": "gemini", "mode": "streaming"},
	})
	draft := h.want(owner, http.MethodPost, "/api/v1/route-drafts", map[string]any{"project_id": project, "slug": "callback-chat", "overall_timeout_ms": 10000, "max_attempts": 2, "retry": map[string]any{"upstream_server": map[string]any{"max_retries": 1, "base_backoff_ms": 1, "max_backoff_ms": 1}}, "fidelity": map[string]any{"mode": "transformed"}, "targets": []any{map[string]any{"provider_id": model["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}}}, idem("callback-route"), http.StatusCreated)
	path := "/api/v1/route-drafts/" + draft["id"].(string)
	validated := h.want(owner, http.MethodPost, path+"/validate", nil, etagHeader(draft), http.StatusOK)
	h.want(owner, http.MethodPost, path+"/activate", nil, withMatch(validated, idem("callback-activate")), http.StatusOK)
	unpinned := h.want(owner, http.MethodPost, "/api/v1/route-drafts", map[string]any{"project_id": project, "slug": "callback-unpinned", "overall_timeout_ms": 10000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"}, "targets": []any{map[string]any{"provider_id": model["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}}}, idem("callback-unpinned-route"), http.StatusCreated)
	unpinnedPath := "/api/v1/route-drafts/" + unpinned["id"].(string)
	unpinnedValidated := h.want(owner, http.MethodPost, unpinnedPath+"/validate", nil, etagHeader(unpinned), http.StatusOK)
	h.want(owner, http.MethodPost, unpinnedPath+"/activate", nil, withMatch(unpinnedValidated, idem("callback-unpinned-activate")), http.StatusOK)
	type toolArguments struct {
		Query string `json:"query"`
	}
	var toolCalls atomic.Int64
	sdkProtocols := mcpsdk.SupportedProtocolVersions()
	protocols := slices.Collect(func(yield func(string) bool) {
		for _, version := range mcpservers.SupportedProtocols() {
			if slices.Contains(sdkProtocols, version) && !yield(version) {
				return
			}
		}
	})
	if len(protocols) == 0 {
		t.Fatal("MCP fixture and gateway have no common protocol version")
	}
	reference := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "callback-tools", Version: "1"}, &mcpsdk.ServerOptions{SupportedProtocolVersions: protocols})
	mcpsdk.AddTool(reference, &mcpsdk.Tool{Name: "echo", Description: "Return the supplied text"}, func(_ context.Context, _ *mcpsdk.CallToolRequest, input toolArguments) (*mcpsdk.CallToolResult, any, error) {
		toolCalls.Add(1)
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: input.Query}}}, nil, nil
	})
	toolUpstream := httptest.NewServer(referenceServerHandler(routing.MCPStreamableHTTP, reference))
	defer toolUpstream.Close()
	registered := h.want(owner, http.MethodPost, "/api/v1/mcp-servers", map[string]any{"name": "Callback tools", "project_id": project, "transport": "streamable_http", "endpoint": toolUpstream.URL, "enabled": true, "unit_price": "0.10"}, idem("callback-tools-server"), http.StatusCreated)
	serverID := registered["id"].(string)
	tool := agentcontract.Tool{ServerID: serverID, RevisionID: registered["revision_id"].(string), CatalogDigest: mcpExecObject(t, registered, "catalog")["digest"].(string), Name: "echo", Alias: agentcontract.ToolAlias(serverID, "echo")}
	createToolset := func(name string) (string, string) {
		t.Helper()
		created := h.want(owner, http.MethodPost, "/api/v1/toolsets", map[string]any{"project_id": project, "name": name, "toolset": map[string]any{"tools": []agentcontract.Tool{tool}}}, idem(name+"-create"), http.StatusCreated)
		id := created["id"].(string)
		published := h.want(owner, http.MethodPost, "/api/v1/toolsets/"+id+"/publish", nil, withMatch(created, idem(name+"-publish")), http.StatusOK)
		return id, published["revision_id"].(string)
	}
	toolsetID, toolsetRevision := createToolset("callback-pinned")
	foreignToolsetID, _ := createToolset("callback-unpinned")
	h.refresh()
	routeRevision := h.Runtime.Release().Snapshot.Routes["callback-chat"].RevisionID
	callbackStatus := make(chan int, 1)
	callbackBody := make(chan string, 1)
	seenCapability := make(chan string, 1)
	cardFor := func(endpoint string) []byte {
		card, err := json.Marshal(protocol.AgentCard{Name: "Callback agent", Description: "Callback fixture", Version: "1", SupportedInterfaces: []*protocol.AgentInterface{{URL: endpoint, ProtocolBinding: protocol.TransportProtocolJSONRPC, ProtocolVersion: protocol.Version}}, Capabilities: protocol.AgentCapabilities{}, DefaultInputModes: []string{"text"}, DefaultOutputModes: []string{"text"}, Skills: []protocol.AgentSkill{}})
		if err != nil {
			t.Fatal(err)
		}
		return card
	}
	agentHandler := a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(a2asrv.AgentExecutorFunc(func(_ context.Context, execution *a2asrv.ExecutorContext) iter.Seq2[protocol.Event, error] {
		return func(yield func(protocol.Event, error) bool) {
			yield(protocol.NewSubmittedTask(execution, execution.Message), nil)
		}
	})))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/agent-card.json" {
			_, _ = w.Write(cardFor("http://" + r.Host + "/rpc"))
			return
		}
		capability := r.Header.Get("X-OLP-Task-Capability")
		seenCapability <- capability
		request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, h.HTTP.URL+"/v1/chat/completions", strings.NewReader(`{"model":"callback-chat","messages":[{"role":"user","content":"hello"}]}`))
		if err != nil {
			callbackStatus <- 0
			callbackBody <- err.Error()
			return
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+capability)
		request.Header.Set("X-OLP-End-User", "forged-callback-user")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			callbackStatus <- 0
			callbackBody <- err.Error()
			return
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		callbackBody <- string(body)
		callbackStatus <- response.StatusCode
		agentHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	var user string
	if err := h.Pool.QueryRow(t.Context(), `SELECT id::text FROM olp.users WHERE email='owner@example.com'`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	pricing := acctID(t)
	acctExec(t, h.Pool, `INSERT INTO olp.pricing_revisions(id,revision,effective_at,created_by) VALUES($1,1,now(),$2)`, pricing, user)
	digest := sha256.Sum256(cardFor(upstream.URL + "/rpc"))
	created := h.want(owner, http.MethodPost, "/api/v1/agents", map[string]any{"project_id": project, "name": "callback agent", "agent": map[string]any{"protocol": "a2a-1.0.0", "endpoint": upstream.URL + "/rpc", "card_digest": hex.EncodeToString(digest[:]), "toolset_revisions": []string{toolsetRevision}, "route_revisions": []string{routeRevision}, "pricing_revision_id": pricing, "max_cost": "1", "message_price": "0.25", "max_task_continuations": 8}}, idem("callback-agent"), http.StatusCreated)
	agent := created["id"].(string)
	h.want(owner, http.MethodPost, "/api/v1/agents/"+agent+"/publish", nil, withMatch(created, idem("callback-publish")), http.StatusOK)
	keyRecord := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "callback owner", "project_id": project, "scopes": []string{"agents", "inference", "tools"}, "allowed_agents": []string{agent}, "allowed_routes": []string{"callback-chat", "callback-unpinned"}, "allowed_toolsets": []string{toolsetID, foreignToolsetID}, "requests_per_minute": 10, "end_user_source": "header", "end_user_policy": map[string]any{"defaults": map[string]any{"requests_per_minute": 10}}}, idem("callback-owner"), http.StatusCreated)
	key := keyRecord["secret"].(string)
	client, prefix, _ := acctKeyspace(t)
	limiter, err := limits.New(client, strings.TrimSuffix(prefix, ":"))
	if err != nil {
		t.Fatal(err)
	}
	h.A2A.Ledger = &a2a.Ledger{Pool: h.Pool, Store: &limits.DebitStore{Pool: h.Pool, Limiter: limiter}, Auth: h.AuthKey}
	toolServer := &mcp.Server{Runtime: h.Runtime, Egress: &h.Policy, Auth: h.AuthKey, Limiter: limiter, Ledger: &mcp.DurableLedger{Pool: h.Pool, Store: &limits.DebitStore{Pool: h.Pool, Limiter: limiter}, Auth: h.AuthKey}}
	h.Gateway.Admission = gateway.NewAdmission(limiter, nil, slog.New(slog.DiscardHandler))
	toolMux := http.NewServeMux()
	toolServer.Register(toolMux)
	toolGateway := httptest.NewServer(toolMux)
	defer toolGateway.Close()
	sink := &captureSink{}
	h.Gateway.Sink = sink
	h.refresh()
	initialCalls := provider.chatCalls.Load()
	initialResponses := provider.responseCalls.Load()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.HTTP.URL+"/a2a/"+agent, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"callback","role":"ROLE_USER","parts":[{"text":"hello"}]},"configuration":{"returnImmediately":true}}}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("A2A-Version", "1.0")
	request.Header.Set("X-OLP-End-User", "original-user")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var sendResult struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	decodeErr := json.NewDecoder(response.Body).Decode(&sendResult)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("agent send returned %d", response.StatusCode)
	}
	if decodeErr != nil || len(sendResult.Error) != 0 || len(sendResult.Result) == 0 {
		t.Fatalf("agent send did not settle: result=%s error=%s decode=%v", sendResult.Result, sendResult.Error, decodeErr)
	}
	capability := <-seenCapability
	if capability == "" || capability == key {
		t.Fatal("agent did not receive a distinct task capability")
	}
	if status := <-callbackStatus; status != http.StatusOK {
		t.Fatalf("task owner callback returned %d: %s", status, <-callbackBody)
	}
	<-callbackBody
	callback := func(token, model string) int {
		t.Helper()
		body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}]}`, model)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.HTTP.URL+"/v1/chat/completions", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-OLP-End-User", "forged-callback-user")
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response.StatusCode
	}
	keyPath := "/api/v1/api-keys/" + keyRecord["id"].(string)
	currentKey := h.want(owner, http.MethodGet, keyPath, nil, nil, http.StatusOK)
	currentKey = h.want(owner, http.MethodPatch, keyPath, map[string]any{"requests_per_minute": 1}, withMatch(currentKey, idem("callback-limit")), http.StatusOK)
	h.refresh()
	if status := callback(capability, "callback-chat"); status != http.StatusTooManyRequests {
		t.Fatalf("owner budget did not refuse callback: %d", status)
	}
	h.want(owner, http.MethodPatch, keyPath, map[string]any{"requests_per_minute": 10}, withMatch(currentKey, idem("callback-limit-restore")), http.StatusOK)
	h.refresh()
	for _, token := range []string{"", "olpt_foreign"} {
		if status := callback(token, "callback-chat"); status != http.StatusUnauthorized {
			t.Fatalf("missing or foreign capability returned %d", status)
		}
	}
	if status := callback(capability, "callback-unpinned"); status != http.StatusForbidden {
		t.Fatalf("unpinned route returned %d", status)
	}
	callTool := func(token, toolset, callID string) int {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"toolset_id": toolset, "name": tool.Alias, "call_id": callID, "arguments": toolArguments{Query: "hello"}})
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, toolGateway.URL+"/v1/mcp/tools/call", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-OLP-End-User", "forged-callback-user")
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response.StatusCode
	}
	for _, token := range []string{"", "olpt_foreign"} {
		if status := callTool(token, toolsetID, "missing"); status != http.StatusUnauthorized {
			t.Fatalf("tool callback without a valid capability returned %d", status)
		}
	}
	if status := callTool(capability, foreignToolsetID, "foreign"); status != http.StatusForbidden && status != http.StatusNotFound {
		t.Fatalf("unpinned toolset returned %d", status)
	}
	if status := callTool(capability, toolsetID, "permitted"); status != http.StatusOK {
		t.Fatalf("tool callback returned %d", status)
	}
	if status := callTool(capability, toolsetID, "permitted"); status != http.StatusOK {
		t.Fatalf("repeated tool callback returned %d", status)
	}
	responseBody := strings.NewReader(`{"model":"callback-chat","input":"hello"}`)
	responseRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.HTTP.URL+"/v1/responses", responseBody)
	if err != nil {
		t.Fatal(err)
	}
	responseRequest.Header.Set("Authorization", "Bearer "+capability)
	responseRequest.Header.Set("Content-Type", "application/json")
	responseRequest.Header.Set("X-OLP-End-User", "forged-callback-user")
	response, err = http.DefaultClient.Do(responseRequest)
	if err != nil {
		t.Fatal(err)
	}
	responseBytes, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("Responses callback returned %d: %s (%v)", response.StatusCode, responseBytes, err)
	}
	nativeCalls := []struct{ path, body string }{
		{"/anthropic/v1/messages", `{"model":"callback-chat","max_tokens":20,"messages":[{"role":"user","content":"hello"}]}`},
		{"/gemini/v1beta/models/callback-chat:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`},
		{"/gemini/v1beta/models/callback-chat:streamGenerateContent?alt=sse", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`},
	}
	nativeCallback := func(path, body string) (int, []byte) {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.HTTP.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+capability)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-OLP-End-User", "forged-callback-user")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		responseBody, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		return response.StatusCode, responseBody
	}
	for _, native := range nativeCalls {
		if status, body := nativeCallback(native.path, native.body); status != http.StatusOK {
			t.Fatalf("%s callback returned %d: %s", native.path, status, body)
		}
	}
	provider.failNextChat.Store(true)
	if status := callback(capability, "callback-chat"); status != http.StatusOK {
		t.Fatalf("active task callback returned %d", status)
	}
	if status := callback(capability, "callback-chat"); status != http.StatusTooManyRequests {
		t.Fatalf("callback beyond task limit returned %d", status)
	}
	if status := callTool(capability, toolsetID, "over-limit"); status != http.StatusTooManyRequests {
		t.Fatalf("tool callback beyond task limit returned %d", status)
	}
	for _, native := range nativeCalls {
		status, body := nativeCallback(native.path, native.body)
		if status != http.StatusTooManyRequests || !bytes.Contains(body, []byte("task_limit_exceeded")) {
			t.Fatalf("%s callback beyond task limit returned %d: %s", native.path, status, body)
		}
	}
	if got := toolCalls.Load(); got != 1 {
		t.Fatalf("tool received %d callbacks, want 1", got)
	}
	if got := provider.chatCalls.Load() - initialCalls + provider.responseCalls.Load() - initialResponses; got != 7 {
		t.Fatalf("provider received %d attempts, want 7", got)
	}
	var successful, attempts int
	surfaces := make(map[string]int)
	for _, event := range sink.all() {
		if event.Status == http.StatusOK && event.Route == "callback-chat" {
			if event.KeyID != keyRecord["id"] || event.EndUserDigest != h.Runtime.EndUserDigest(&project, "original-user") || event.Usage == nil {
				t.Fatalf("callback accounting lost owner or usage: %+v", event)
			}
			successful++
			attempts += len(event.Attempts)
			surfaces[event.Surface]++
		}
	}
	if successful != 6 || attempts != 7 {
		t.Fatalf("got %d successful callback records with %d attempts, want 6 and 7", successful, attempts)
	}
	if surfaces["anthropic"] != 1 || surfaces["gemini"] != 2 {
		t.Fatalf("native callback accounting records = %v", surfaces)
	}
	var settled, results int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*),(SELECT count(*) FROM olp.execution_debit_results r JOIN olp.execution_debits d ON d.id=r.dispatch_id WHERE d.kind='agent') FROM olp.execution_debits WHERE kind='agent' AND state='settled' AND api_key_id=$1`, keyRecord["id"]).Scan(&settled, &results); err != nil || settled != 1 || results != 1 {
		t.Fatalf("remote fee accounting: settled=%d results=%d error=%v", settled, results, err)
	}
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*),(SELECT count(*) FROM olp.execution_debit_results r JOIN olp.execution_debits d ON d.id=r.dispatch_id WHERE d.kind='tool') FROM olp.execution_debits WHERE kind='tool' AND state='settled' AND api_key_id=$1`, keyRecord["id"]).Scan(&settled, &results); err != nil || settled != 1 || results != 1 {
		t.Fatalf("tool accounting: settled=%d results=%d error=%v", settled, results, err)
	}
	for _, state := range []string{"completed", "outcome_unknown"} {
		acctExec(t, h.Pool, `UPDATE olp.a2a_tasks SET state=$1`, state)
		if status := callback(capability, "callback-chat"); status != http.StatusUnauthorized {
			t.Fatalf("%s task callback returned %d", state, status)
		}
	}
	acctExec(t, h.Pool, `UPDATE olp.a2a_tasks SET state='active'`)
	acctExec(t, h.Pool, `UPDATE olp.a2a_task_capabilities SET expires_at=now()-interval '1 second'`)
	if status := callback(capability, "callback-chat"); status != http.StatusUnauthorized {
		t.Fatalf("expired capability returned %d", status)
	}
	acctExec(t, h.Pool, `UPDATE olp.a2a_task_capabilities SET expires_at=now()+interval '1 minute'`)
	currentKey = h.want(owner, http.MethodGet, keyPath, nil, nil, http.StatusOK)
	h.want(owner, http.MethodPost, keyPath+"/revoke", nil, withMatch(currentKey, idem("callback-owner-revoke")), http.StatusOK)
	h.refresh()
	if status := callback(capability, "callback-chat"); status != http.StatusUnauthorized && status != http.StatusForbidden {
		t.Fatalf("revoked owner callback returned %d", status)
	}
	if got := provider.chatCalls.Load() - initialCalls + provider.responseCalls.Load() - initialResponses; got != 7 {
		t.Fatalf("refused callbacks reached provider, total %d", got)
	}
}

func TestA2AAgentCardUsesGatewayAuthenticationAndEndpoint(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "A2A discovery")
	const cardTemplate = `{"name":"Fixture agent","description":"A test agent","version":"1.0","supportedInterfaces":[{"url":"PINNED_ENDPOINT","protocolBinding":"JSONRPC","protocolVersion":"1.0"},{"url":"https://wrong.invalid/rpc","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],"capabilities":{"streaming":true,"pushNotifications":true},"defaultInputModes":["text"],"defaultOutputModes":["text"],"skills":[]}`
	cardFor := func(endpoint string) []byte {
		return bytes.ReplaceAll([]byte(cardTemplate), []byte("PINNED_ENDPOINT"), []byte(endpoint))
	}
	upstreamIDs := make(chan string, 64)
	cancelAttempts := make(chan string, 256)
	var holdCancellation atomic.Bool
	resumeStream := make(chan struct{})
	resumePinnedStream := make(chan struct{})
	resumeRevokedStream := make(chan struct{})
	resumeLongStream := make(chan struct{})
	var redirectCard, redirectRPC, dropRPC atomic.Bool
	var redirectHits atomic.Int32
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(redirectTarget.Close)
	agentHandler := a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(a2asrv.AgentExecutorFunc(func(ctx context.Context, execution *a2asrv.ExecutorContext) iter.Seq2[protocol.Event, error] {
		return func(yield func(protocol.Event, error) bool) {
			var dispatched int
			if err := h.Pool.QueryRow(ctx, `SELECT count(*) FROM olp.execution_debits WHERE kind='agent' AND state='dispatched'`).Scan(&dispatched); err != nil || dispatched < 1 {
				t.Errorf("send before durable debit: %d, %v", dispatched, err)
			}
			upstreamIDs <- string(execution.TaskID)
			if execution.Message.ID == "continue-task" {
				if yield(protocol.NewStatusUpdateEvent(execution, protocol.TaskStateWorking, nil), nil) {
					yield(protocol.NewStatusUpdateEvent(execution, protocol.TaskStateCompleted, nil), nil)
				}
				return
			}
			if !yield(protocol.NewSubmittedTask(execution, execution.Message), nil) {
				return
			}
			if execution.Message.ID == "request-input" {
				yield(protocol.NewStatusUpdateEvent(execution, protocol.TaskStateInputRequired, nil), nil)
				return
			}
			pause := map[string]<-chan struct{}{"stream-resume": resumeStream, "stream-pinned": resumePinnedStream, "stream-long": resumeLongStream}[execution.Message.ID]
			if pause != nil {
				for {
					select {
					case <-pause:
						goto resumed
					case <-time.After(250 * time.Millisecond):
						if !yield(protocol.NewStatusUpdateEvent(execution, protocol.TaskStateWorking, nil), nil) {
							return
						}
					case <-ctx.Done():
						return
					}
				}
			}
		resumed:
			if execution.Message.ID == "stream-revoke" {
				select {
				case <-resumeRevokedStream:
				case <-ctx.Done():
					return
				}
			}
			if execution.Message.ID == "stream-oversized" {
				yield(protocol.NewArtifactEvent(execution, protocol.NewTextPart(strings.Repeat("x", 1<<20))), nil)
				return
			}
			if execution.Message.ID == "stream-bounded" {
				for range 2 {
					if !yield(protocol.NewArtifactEvent(execution, protocol.NewTextPart(strings.Repeat("x", (1<<20)*3/5))), nil) {
						return
					}
				}
				yield(protocol.NewStatusUpdateEvent(execution, protocol.TaskStateCompleted, nil), nil)
				return
			}
			if !yield(protocol.NewArtifactEvent(execution, protocol.NewTextPart("PRIVATE ARTIFACT")), nil) {
				return
			}
			yield(protocol.NewStatusUpdateEvent(execution, protocol.TaskStateCompleted, nil), nil)
		}
	})))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-secret" {
			t.Error("upstream credential missing")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/rpc" {
			if holdCancellation.Load() {
				var request struct {
					Method string `json:"method"`
					Params struct {
						ID string `json:"id"`
					} `json:"params"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Method != "CancelTask" {
					t.Errorf("unexpected request during cancellation burst: %s, %v", request.Method, err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				cancelAttempts <- request.Params.ID
				<-r.Context().Done()
				return
			}
			if dropRPC.Swap(false) {
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Errorf("close upstream connection: %v", err)
					return
				}
				connection.Close()
				return
			}
			if redirectRPC.Load() {
				http.Redirect(w, r, redirectTarget.URL, http.StatusTemporaryRedirect)
				return
			}
			agentHandler.ServeHTTP(a2aContentTypeWriter{w}, r)
			return
		}
		if r.URL.Path != "/.well-known/agent-card.json" {
			t.Errorf("unexpected upstream request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if redirectCard.Load() {
			http.Redirect(w, r, redirectTarget.URL, http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cardFor("http://" + r.Host + "/rpc"))
	}))
	t.Cleanup(upstream.Close)
	card := cardFor(upstream.URL + "/rpc")
	var user string
	if err := h.Pool.QueryRow(t.Context(), `SELECT id::text FROM olp.users WHERE email='owner@example.com'`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	pricing := acctID(t)
	acctExec(t, h.Pool, `INSERT INTO olp.pricing_revisions(id,revision,effective_at,created_by) VALUES($1,1,now(),$2)`, pricing, user)
	digest := sha256.Sum256(card)
	for _, price := range []struct{ name, value string }{
		{name: "malformed", value: "not-a-number"},
		{name: "negative", value: "-0.01"},
		{name: "above-bound", value: "1.01"},
	} {
		problem := h.want(owner, http.MethodPost, "/api/v1/agents", map[string]any{
			"project_id": project, "name": "invalid " + price.name,
			"agent": map[string]any{"protocol": "a2a-1.0.0", "endpoint": upstream.URL + "/rpc", "card_digest": hex.EncodeToString(digest[:]), "toolset_revisions": []string{}, "route_revisions": []string{}, "pricing_revision_id": pricing, "max_cost": "1", "message_price": price.value},
		}, idem("a2a-invalid-price-"+price.name), http.StatusUnprocessableEntity)
		if !strings.Contains(fmt.Sprint(problem["detail"]), "message_price") {
			t.Fatalf("invalid price rejected for the wrong reason: %+v", problem)
		}
	}
	const messagePrice = "0.25"
	created := h.want(owner, http.MethodPost, "/api/v1/agents", map[string]any{
		"project_id": project, "name": "fixture", "credential": "upstream-secret",
		"agent": map[string]any{"protocol": "a2a-1.0.0", "endpoint": upstream.URL + "/rpc", "card_digest": hex.EncodeToString(digest[:]), "toolset_revisions": []string{}, "route_revisions": []string{}, "pricing_revision_id": pricing, "max_cost": "1", "message_price": messagePrice},
	}, idem("a2a-card-agent"), http.StatusCreated)
	agent := created["id"].(string)
	published := h.want(owner, http.MethodPost, "/api/v1/agents/"+agent+"/publish", nil, withMatch(created, idem("a2a-card-publish")), http.StatusOK)
	key := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "A2A caller", "project_id": project, "scopes": []string{"agents"}, "allowed_agents": []string{agent}}, idem("a2a-card-key"), http.StatusCreated)["secret"].(string)
	budgetedKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "Budgeted A2A caller", "project_id": project, "scopes": []string{"agents"}, "allowed_agents": []string{agent}, "daily_cost_limit": "5"}, idem("a2a-budgeted-key"), http.StatusCreated)["secret"].(string)
	noScope := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "missing scope", "project_id": project, "scopes": []string{"tools"}, "allowed_agents": []string{agent}}, idem("a2a-no-scope"), http.StatusCreated)["secret"].(string)
	noAgent := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "missing agent", "project_id": project, "scopes": []string{"agents"}}, idem("a2a-no-agent"), http.StatusCreated)["secret"].(string)
	otherProject := createProject(h, owner, "A2A other project")
	otherProjectKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "other project", "project_id": otherProject, "scopes": []string{"agents"}}, idem("a2a-other-project"), http.StatusCreated)["secret"].(string)
	client, prefix, _ := acctKeyspace(t)
	limiter, err := limits.New(client, strings.TrimSuffix(prefix, ":"))
	if err != nil {
		t.Fatal(err)
	}
	h.A2A.Ledger = &a2a.Ledger{Pool: h.Pool, Store: &limits.DebitStore{Pool: h.Pool, Limiter: limiter}, Auth: h.AuthKey}
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"", "0.3"} {
		request, err := http.NewRequest(http.MethodPost, h.HTTP.URL+"/a2a/"+agent, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{}}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+key)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("A2A-Version", version)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("legacy call to 1.0 agent returned %d", response.StatusCode)
		}
		select {
		case <-upstreamIDs:
			t.Fatal("legacy call reached 1.0 upstream")
		default:
		}
	}
	getCard := func(secret, agentID string) int {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, h.HTTP.URL+"/a2a/"+agentID+"/.well-known/agent-card.json", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+secret)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	if getCard(noScope, agent) == http.StatusOK || getCard(noAgent, agent) == http.StatusOK {
		t.Fatal("agent access succeeded without its scope and allowlist")
	}
	if got, missing := getCard(otherProjectKey, agent), getCard(otherProjectKey, uuid.NewString()); got != missing || got != http.StatusNotFound {
		t.Fatalf("cross-project agent differs from missing: %d, %d", got, missing)
	}
	r, err := http.NewRequest(http.MethodGet, h.HTTP.URL+"/a2a/"+agent+"/.well-known/agent-card.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+key)
	response, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("card request returned %d, want 200", response.StatusCode)
	}
	var result struct {
		SupportedInterfaces []struct {
			URL             string `json:"url"`
			ProtocolBinding string `json:"protocolBinding"`
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"supportedInterfaces"`
		Capabilities struct {
			Streaming         bool `json:"streaming"`
			PushNotifications bool `json:"pushNotifications"`
		} `json:"capabilities"`
		SecuritySchemes map[string]struct {
			HTTPAuth struct {
				Scheme string `json:"scheme"`
			} `json:"httpAuthSecurityScheme"`
		} `json:"securitySchemes"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if len(result.SupportedInterfaces) != 1 || result.SupportedInterfaces[0].URL != h.HTTP.URL+"/a2a/"+agent || result.SupportedInterfaces[0].ProtocolBinding != "JSONRPC" || !result.Capabilities.Streaming || result.Capabilities.PushNotifications {
		t.Fatalf("card advertises an unimplemented or foreign interface: %+v", result)
	}
	if len(result.SecuritySchemes) != 1 {
		t.Fatalf("card must advertise gateway authentication: %+v", result.SecuritySchemes)
	}
	for _, scheme := range result.SecuritySchemes {
		if scheme.HTTPAuth.Scheme != "Bearer" {
			t.Fatalf("unexpected security scheme: %+v", scheme)
		}
	}
	message := func(id, text string) *protocol.Message {
		return &protocol.Message{ID: id, Role: protocol.MessageRoleUser, Parts: protocol.ContentParts{protocol.NewTextPart(text)}}
	}
	rpcFor := func(agentID, secret, method string, params a2aTestParams) map[string]any {
		t.Helper()
		body, err := json.Marshal(struct {
			JSONRPC string        `json:"jsonrpc"`
			ID      int           `json:"id"`
			Method  string        `json:"method"`
			Params  a2aTestParams `json:"params"`
		}{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequest(http.MethodPost, h.HTTP.URL+"/a2a/"+agentID, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+secret)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("A2A-Version", "1.0")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s returned %d", method, response.StatusCode)
		}
		var decoded map[string]any
		if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	rpc := func(secret, method string, params a2aTestParams) map[string]any {
		return rpcFor(agent, secret, method, params)
	}
	budgeted := rpc(budgetedKey, "SendMessage", a2aTestParams{Message: message("budgeted-without-bound", "question")})
	if budgeted["error"] == nil {
		t.Fatalf("budgeted send without a remote-work bound was accepted: %+v", budgeted)
	}
	select {
	case <-upstreamIDs:
		t.Fatal("budgeted send without a remote-work bound reached the upstream")
	default:
	}
	invalidIDRequest, err := http.NewRequest(http.MethodPost, h.HTTP.URL+"/a2a/"+agent, bytes.NewBufferString(`{"jsonrpc":"2.0","id":{},"method":"GetTask","params":{"id":"missing"}}`))
	if err != nil {
		t.Fatal(err)
	}
	invalidIDRequest.Header.Set("Authorization", "Bearer "+key)
	invalidIDRequest.Header.Set("Content-Type", "application/json")
	invalidIDRequest.Header.Set("A2A-Version", "1.0")
	invalidIDResponse, err := http.DefaultClient.Do(invalidIDRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer invalidIDResponse.Body.Close()
	var invalidIDResult struct {
		ID    json.RawMessage `json:"id"`
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(invalidIDResponse.Body).Decode(&invalidIDResult); err != nil || invalidIDResult.Error.Code != -32600 || string(invalidIDResult.ID) != "null" {
		t.Fatalf("invalid JSON-RPC ID was accepted: %+v, %v", invalidIDResult, err)
	}
	sent := rpc(key, "SendMessage", a2aTestParams{Message: message("fixture-message", "PRIVATE REQUEST")})
	sendResult, ok := sent["result"].(map[string]any)
	if !ok {
		t.Fatalf("send failed: %+v", sent)
	}
	task, ok := sendResult["task"].(map[string]any)
	if !ok {
		t.Fatalf("send did not return a task: %+v", sent)
	}
	upstreamID := <-upstreamIDs
	id, ok := task["id"].(string)
	if !ok || id == "" || id == upstreamID {
		t.Fatalf("send did not mask upstream ID: %+v", task)
	}
	read := rpc(key, "GetTask", a2aTestParams{ID: protocol.TaskID(id)})
	readTask, ok := read["result"].(map[string]any)
	if !ok || readTask["id"] != id || !strings.Contains(fmt.Sprint(readTask), "PRIVATE ARTIFACT") {
		t.Fatalf("task read failed: %+v", read)
	}
	if got := rpc(key, "SendMessage", a2aTestParams{Message: &protocol.Message{ID: "upstream-reference", Role: protocol.MessageRoleUser, Parts: protocol.ContentParts{protocol.NewTextPart("reference")}, ReferenceTasks: []protocol.TaskID{protocol.TaskID(upstreamID)}}}); got["error"] == nil {
		t.Fatalf("accepted upstream task reference: %+v", got)
	}
	if got := rpc(key, "SendMessage", a2aTestParams{Message: message("push-attempt", "push"), Config: &protocol.SendMessageConfig{PushConfig: &protocol.PushConfig{URL: "https://wrong.invalid"}}}); got["error"] == nil {
		t.Fatalf("accepted an unsupported push request: %+v", got)
	}
	second := rpc(key, "SendMessage", a2aTestParams{Message: message("second-fixture-message", "another message")})
	secondResult, ok := second["result"].(map[string]any)
	if !ok || secondResult["task"] == nil {
		t.Fatalf("second send failed: %+v", second)
	}
	<-upstreamIDs
	foreign := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "other caller", "project_id": project, "scopes": []string{"agents"}, "allowed_agents": []string{agent}}, idem("a2a-foreign-key"), http.StatusCreated)["secret"].(string)
	revocableKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "revocable streaming caller", "project_id": project, "scopes": []string{"agents"}, "allowed_agents": []string{agent}}, idem("a2a-revocable-key"), http.StatusCreated)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	missing := rpc(foreign, "GetTask", a2aTestParams{ID: protocol.TaskID(uuid.NewString())})
	if got := rpc(foreign, "GetTask", a2aTestParams{ID: protocol.TaskID(id)}); !reflect.DeepEqual(got, missing) {
		t.Fatalf("foreign task differs from missing: %+v, %+v", got, missing)
	}
	if got := rpc(key, "GetTask", a2aTestParams{ID: protocol.TaskID(upstreamID)}); got["error"] == nil {
		t.Fatalf("accepted upstream task ID: %+v", got)
	}
	projectReadStatus := func(agentID, taskID string) int {
		t.Helper()
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "GetTask", "params": map[string]any{"id": taskID}})
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequest(http.MethodPost, h.HTTP.URL+"/a2a/"+agentID, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+otherProjectKey)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	if got, missing := projectReadStatus(agent, id), projectReadStatus(uuid.NewString(), uuid.NewString()); got != missing || got != http.StatusNotFound {
		t.Fatalf("cross-project task differs from missing: %d, %d", got, missing)
	}
	type debitSummary struct {
		Settled int64
		Cost    string
		Results int64
	}
	accounting := func() debitSummary {
		t.Helper()
		rows, err := h.Pool.Query(t.Context(), `SELECT count(*) AS settled,sum(d.observed_cost)::text AS cost,(SELECT count(*) FROM olp.execution_debit_results r JOIN olp.execution_debits x ON x.id=r.dispatch_id WHERE x.kind='agent') AS results FROM olp.execution_debits d WHERE d.kind='agent' AND d.state='settled'`)
		if err != nil {
			t.Fatal(err)
		}
		summary, collectErr := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[debitSummary])
		if collectErr != nil {
			t.Fatal(collectErr)
		}
		return summary
	}
	amount := func(value string) *big.Rat {
		t.Helper()
		parsed, ok := new(big.Rat).SetString(value)
		if !ok {
			t.Fatalf("invalid accounting amount %q", value)
		}
		return parsed
	}
	beforeStream := accounting()
	if beforeStream.Settled != 2 || beforeStream.Results != beforeStream.Settled || amount(beforeStream.Cost).Cmp(new(big.Rat).Mul(big.NewRat(beforeStream.Settled, 1), amount(messagePrice))) != 0 {
		t.Fatalf("incorrect settlements: %+v", beforeStream)
	}
	streamRequest := func(secret, method string, params a2aTestParams) *http.Response {
		t.Helper()
		body, err := json.Marshal(struct {
			JSONRPC string        `json:"jsonrpc"`
			ID      int           `json:"id"`
			Method  string        `json:"method"`
			Params  a2aTestParams `json:"params"`
		}{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		t.Cleanup(cancel)
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, h.HTTP.URL+"/a2a/"+agent, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+secret)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("A2A-Version", "1.0")
		request.Header.Set("Accept", "text/event-stream")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	type streamFrame struct {
		Result protocol.StreamResponse `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	nextFrame := func(reader *bufio.Reader) streamFrame {
		t.Helper()
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("stream ended before event: %v", err)
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var frame streamFrame
			if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data: "))), &frame); err != nil {
				t.Fatalf("invalid stream event: %+v, %v", frame, err)
			}
			return frame
		}
	}
	nextEvent := func(reader *bufio.Reader) protocol.Event {
		t.Helper()
		frame := nextFrame(reader)
		if frame.Error != nil || frame.Result.Event == nil {
			t.Fatalf("stream did not return an event: %+v", frame)
		}
		return frame.Result.Event
	}
	stream := streamRequest(key, "SendStreamingMessage", a2aTestParams{Message: message("stream-resume", "PRIVATE STREAM REQUEST")})
	if stream.StatusCode != http.StatusOK || !strings.HasPrefix(stream.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream was not established: %s, %s", stream.Status, stream.Header.Get("Content-Type"))
	}
	initial := nextEvent(bufio.NewReader(stream.Body))
	firstTask, firstTaskOK := initial.(*protocol.Task)
	if !firstTaskOK || firstTask.ID == "" || firstTask.Status.State != protocol.TaskStateSubmitted {
		t.Fatalf("unexpected first stream event: %+v", initial)
	}
	afterSend := accounting()
	if afterSend.Settled != beforeStream.Settled+1 || afterSend.Results != beforeStream.Results+1 || amount(afterSend.Cost).Cmp(new(big.Rat).Add(amount(beforeStream.Cost), amount(messagePrice))) != 0 {
		t.Fatalf("streaming send did not settle exactly once: before %+v, after %+v", beforeStream, afterSend)
	}
	streamUpstreamID := <-upstreamIDs
	if string(firstTask.ID) == streamUpstreamID || firstTask.ContextID != string(firstTask.ID) {
		t.Fatalf("stream did not mask task identity: %+v", firstTask)
	}
	foreignSubscription := rpc(foreign, "SubscribeToTask", a2aTestParams{ID: firstTask.ID})
	missingSubscription := rpc(foreign, "SubscribeToTask", a2aTestParams{ID: protocol.TaskID(uuid.NewString())})
	if !reflect.DeepEqual(foreignSubscription, missingSubscription) {
		t.Fatalf("foreign stream differs from missing task: %+v, %+v", foreignSubscription, missingSubscription)
	}
	stream.Body.Close()
	current := rpc(key, "GetTask", a2aTestParams{ID: firstTask.ID})
	currentTask, currentTaskOK := current["result"].(map[string]any)
	if !currentTaskOK || currentTask["status"].(map[string]any)["state"] == "TASK_STATE_COMPLETED" || currentTask["status"].(map[string]any)["state"] == "TASK_STATE_CANCELED" {
		t.Fatalf("disconnect changed the upstream task state: %+v", current)
	}
	resubscribed := streamRequest(key, "SubscribeToTask", a2aTestParams{ID: firstTask.ID})
	defer resubscribed.Body.Close()
	if resubscribed.StatusCode != http.StatusOK || !strings.HasPrefix(resubscribed.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("resubscription failed: %s, %s", resubscribed.Status, resubscribed.Header.Get("Content-Type"))
	}
	reader := bufio.NewReader(resubscribed.Body)
	progress := nextEvent(reader)
	if progress.TaskInfo().TaskID != firstTask.ID {
		t.Fatalf("resubscription changed the task identity: %+v", progress)
	}
	close(resumeStream)
	for {
		event := nextEvent(reader)
		if event.TaskInfo().TaskID != firstTask.ID || event.TaskInfo().ContextID != string(firstTask.ID) {
			t.Fatalf("resubscription exposed a foreign task identity: %+v", event)
		}
		if artifact, ok := event.(*protocol.TaskArtifactUpdateEvent); ok {
			if artifact.Artifact == nil || len(artifact.Artifact.Parts) != 1 || artifact.Artifact.Parts[0].Text() != "PRIVATE ARTIFACT" {
				t.Fatalf("resubscription lost its artifact: %+v", artifact)
			}
			break
		}
	}
	completed := nextEvent(reader)
	status, completedOK := completed.(*protocol.TaskStatusUpdateEvent)
	if !completedOK || status.Status.State != protocol.TaskStateCompleted || status.TaskID != firstTask.ID || status.ContextID != string(firstTask.ID) {
		t.Fatalf("completion did not follow the artifact: %+v", completed)
	}
	afterResubscribe := accounting()
	if afterResubscribe != afterSend {
		t.Fatalf("resubscription changed the charge: before %+v, after %+v", afterSend, afterResubscribe)
	}
	oversized := streamRequest(key, "SendStreamingMessage", a2aTestParams{Message: message("stream-oversized", "PRIVATE OVERSIZED REQUEST")})
	defer oversized.Body.Close()
	oversizedReader := bufio.NewReader(oversized.Body)
	if _, ok := nextEvent(oversizedReader).(*protocol.Task); !ok {
		t.Fatal("oversized stream did not begin with a task")
	}
	<-upstreamIDs
	oversizedResult := nextFrame(oversizedReader)
	if oversizedResult.Error == nil || !strings.Contains(oversizedResult.Error.Message, "one MiB") {
		t.Fatalf("oversized stream did not fail explicitly: %+v", oversizedResult)
	}
	bounded := streamRequest(key, "SendStreamingMessage", a2aTestParams{Message: message("stream-bounded", "bounded")})
	defer bounded.Body.Close()
	boundedReader := bufio.NewReader(bounded.Body)
	if _, ok := nextEvent(boundedReader).(*protocol.Task); !ok {
		t.Fatal("bounded stream did not begin with a task")
	}
	<-upstreamIDs
	for range 2 {
		boundedArtifact, boundedOK := nextEvent(boundedReader).(*protocol.TaskArtifactUpdateEvent)
		if !boundedOK || boundedArtifact.Artifact == nil || len(boundedArtifact.Artifact.Parts) != 1 || len(boundedArtifact.Artifact.Parts[0].Text()) != (1<<20)*3/5 {
			t.Fatal("under-limit event was not delivered intact")
		}
	}
	if complete, ok := nextEvent(boundedReader).(*protocol.TaskStatusUpdateEvent); !ok || complete.Status.State != protocol.TaskStateCompleted {
		t.Fatal("under-limit stream did not complete")
	}
	pending := streamRequest(key, "SendStreamingMessage", a2aTestParams{Message: message("request-input", "question")})
	defer pending.Body.Close()
	pendingReader := bufio.NewReader(pending.Body)
	pendingTask, pendingOK := nextEvent(pendingReader).(*protocol.Task)
	if !pendingOK || pendingTask.ID == "" {
		t.Fatal("input-required stream did not begin with a task")
	}
	<-upstreamIDs
	if status, statusOK := nextEvent(pendingReader).(*protocol.TaskStatusUpdateEvent); !statusOK || status.Status.State != protocol.TaskStateInputRequired {
		t.Fatal("task did not request input")
	}
	continuationMessage := &protocol.Message{ID: "continue-task", Role: protocol.MessageRoleUser, TaskID: pendingTask.ID, Parts: protocol.ContentParts{protocol.NewTextPart("answer")}}
	continuation := streamRequest(key, "SendStreamingMessage", a2aTestParams{Message: continuationMessage})
	defer continuation.Body.Close()
	continuationReader := bufio.NewReader(continuation.Body)
	continuedStatus, continuedOK := nextEvent(continuationReader).(*protocol.TaskStatusUpdateEvent)
	if !continuedOK || continuedStatus.TaskID != pendingTask.ID || continuedStatus.ContextID != string(pendingTask.ID) || continuedStatus.Status.State != protocol.TaskStateWorking {
		t.Fatalf("continuation did not begin with the same task's status: %+v", continuedStatus)
	}
	<-upstreamIDs
	if status, statusOK := nextEvent(continuationReader).(*protocol.TaskStatusUpdateEvent); !statusOK || status.TaskID != pendingTask.ID || status.Status.State != protocol.TaskStateCompleted {
		t.Fatal("continued task did not complete")
	}
	cancelStream := streamRequest(key, "SendStreamingMessage", a2aTestParams{Message: message("request-input", "please cancel")})
	defer cancelStream.Body.Close()
	cancelReader := bufio.NewReader(cancelStream.Body)
	cancelTask, cancelOK := nextEvent(cancelReader).(*protocol.Task)
	if !cancelOK || cancelTask.ID == "" {
		t.Fatal("task to cancel did not start")
	}
	<-upstreamIDs
	if status, statusOK := nextEvent(cancelReader).(*protocol.TaskStatusUpdateEvent); !statusOK || status.Status.State != protocol.TaskStateInputRequired {
		t.Fatal("task to cancel did not request input")
	}
	foreignCancel := rpc(foreign, "CancelTask", a2aTestParams{ID: cancelTask.ID})
	missingCancel := rpc(foreign, "CancelTask", a2aTestParams{ID: protocol.TaskID(uuid.NewString())})
	if !reflect.DeepEqual(foreignCancel, missingCancel) {
		t.Fatalf("foreign cancellation differs from missing: %+v, %+v", foreignCancel, missingCancel)
	}
	cancelled := rpc(key, "CancelTask", a2aTestParams{ID: cancelTask.ID})
	cancelledBody, err := json.Marshal(cancelled["result"])
	if err != nil {
		t.Fatal(err)
	}
	var cancelledTask protocol.Task
	if err := json.Unmarshal(cancelledBody, &cancelledTask); err != nil || cancelledTask.ID != cancelTask.ID || cancelledTask.Status.State != protocol.TaskStateCanceled {
		t.Fatalf("task was not canceled: %+v, %v", cancelled, err)
	}
	readCancelled := rpc(key, "GetTask", a2aTestParams{ID: cancelTask.ID})
	readBody, err := json.Marshal(readCancelled["result"])
	if err != nil {
		t.Fatal(err)
	}
	var readTaskAfterCancel protocol.Task
	if err := json.Unmarshal(readBody, &readTaskAfterCancel); err != nil || readTaskAfterCancel.ID != cancelTask.ID || readTaskAfterCancel.Status.State != protocol.TaskStateCanceled {
		t.Fatalf("canceled task was not preserved: %+v, %v", readCancelled, err)
	}
	activeTaskFor := func(secret string) protocol.TaskID {
		t.Helper()
		result := rpc(secret, "SendMessage", a2aTestParams{Message: message("request-input", "question")})
		payload, err := json.Marshal(result["result"])
		if err != nil {
			t.Fatal(err)
		}
		var response struct {
			Task *protocol.Task `json:"task"`
		}
		if err := json.Unmarshal(payload, &response); err != nil || response.Task == nil || response.Task.ID == "" || response.Task.Status.State != protocol.TaskStateInputRequired {
			t.Fatalf("input-required task failed: %+v, %v", result, err)
		}
		<-upstreamIDs
		return response.Task.ID
	}
	activeTask := func() protocol.TaskID { return activeTaskFor(foreign) }
	ambiguousTask := activeTask()
	dropRPC.Store(true)
	if result := rpc(foreign, "CancelTask", a2aTestParams{ID: ambiguousTask}); result["error"] == nil {
		t.Fatalf("cancel with a lost response succeeded: %+v", result)
	}
	if result := rpc(foreign, "GetTask", a2aTestParams{ID: ambiguousTask}); result["error"] != nil {
		t.Fatalf("read after ambiguous cancellation failed: %+v", result)
	}
	if result := rpc(foreign, "SendMessage", a2aTestParams{Message: &protocol.Message{ID: "ambiguous-continuation", Role: protocol.MessageRoleUser, Parts: protocol.ContentParts{protocol.NewTextPart("continue")}, TaskID: ambiguousTask}}); result["error"] == nil {
		t.Fatalf("continued a task after an ambiguous cancellation: %+v", result)
	}
	select {
	case <-upstreamIDs:
		t.Fatal("continued a task after an ambiguous cancellation reached the upstream")
	default:
	}
	if result := rpc(foreign, "CancelTask", a2aTestParams{ID: ambiguousTask}); result["error"] != nil {
		t.Fatalf("reconciling cancellation failed: %+v", result)
	}
	firstActive, secondActive, thirdActive, fourthActive := activeTask(), activeTask(), activeTask(), activeTask()
	if exceeded := rpc(foreign, "SendMessage", a2aTestParams{Message: message("too-many-tasks", "question")}); exceeded["error"] == nil {
		t.Fatalf("fifth concurrent task was accepted: %+v", exceeded)
	}
	select {
	case <-upstreamIDs:
		t.Fatal("fifth concurrent task reached the upstream")
	default:
	}
	for _, id := range []protocol.TaskID{firstActive, secondActive, thirdActive, fourthActive} {
		if result := rpc(foreign, "CancelTask", a2aTestParams{ID: id}); result["error"] != nil {
			t.Fatalf("cancel active task failed: %+v", result)
		}
	}
	if result := rpc(foreign, "CancelTask", a2aTestParams{ID: activeTask()}); result["error"] != nil {
		t.Fatalf("cancellation did not free task capacity: %+v", result)
	}
	for attempt := range 4 {
		concurrentKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "concurrent caller", "project_id": project, "scopes": []string{"agents"}, "allowed_agents": []string{agent}}, idem(fmt.Sprintf("a2a-concurrent-%d", attempt)), http.StatusCreated)["secret"].(string)
		if err := h.Runtime.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		concurrentExisting := []protocol.TaskID{activeTaskFor(concurrentKey), activeTaskFor(concurrentKey), activeTaskFor(concurrentKey)}
		concurrentStart := make(chan struct{})
		concurrentResults := make(chan map[string]any, 32)
		for range 32 {
			go func() {
				<-concurrentStart
				concurrentResults <- rpc(concurrentKey, "SendMessage", a2aTestParams{Message: message("request-input", "concurrent")})
			}()
		}
		close(concurrentStart)
		concurrentResponses := slices.Collect(func(yield func(map[string]any) bool) {
			for range 32 {
				select {
				case response := <-concurrentResults:
					if !yield(response) {
						return
					}
				case <-time.After(15 * time.Second):
					t.Fatal("concurrent sends did not complete")
				}
			}
		})
		concurrentAccepted := slices.Collect(func(yield func(map[string]any) bool) {
			for _, response := range concurrentResponses {
				if response["error"] == nil && !yield(response) {
					return
				}
			}
		})
		if len(concurrentAccepted) != 1 {
			t.Fatalf("concurrent sends admitted %d tasks with one available slot: %+v", len(concurrentAccepted), concurrentResponses)
		}
		concurrentPayload, err := json.Marshal(concurrentAccepted[0]["result"])
		if err != nil {
			t.Fatal(err)
		}
		var concurrentTask struct {
			Task *protocol.Task `json:"task"`
		}
		if err := json.Unmarshal(concurrentPayload, &concurrentTask); err != nil || concurrentTask.Task == nil || concurrentTask.Task.ID == "" {
			t.Fatalf("concurrent send did not return a task: %+v, %v", concurrentAccepted[0], err)
		}
		<-upstreamIDs
		for _, id := range append(concurrentExisting, concurrentTask.Task.ID) {
			if result := rpc(concurrentKey, "CancelTask", a2aTestParams{ID: id}); result["error"] != nil {
				t.Fatalf("cancel concurrent task failed: %+v", result)
			}
		}
	}
	continuationTask := activeTask()
	for i := range 32 {
		result := rpc(foreign, "SendMessage", a2aTestParams{Message: &protocol.Message{
			ID: "request-input", Role: protocol.MessageRoleUser, TaskID: continuationTask,
			Parts: protocol.ContentParts{protocol.NewTextPart("question")},
		}})
		if result["error"] != nil {
			t.Fatalf("continuation %d was refused before the limit: %+v", i, result)
		}
		<-upstreamIDs
	}
	if result := rpc(foreign, "SendMessage", a2aTestParams{Message: &protocol.Message{
		ID: "request-input", Role: protocol.MessageRoleUser, TaskID: continuationTask,
		Parts: protocol.ContentParts{protocol.NewTextPart("question")},
	}}); result["error"] == nil {
		t.Fatalf("continuation above the task limit was accepted: %+v", result)
	}
	select {
	case <-upstreamIDs:
		t.Fatal("continuation above the task limit reached the upstream")
	default:
	}
	if result := rpc(foreign, "CancelTask", a2aTestParams{ID: continuationTask}); result["error"] != nil {
		t.Fatalf("cancel continued task failed: %+v", result)
	}
	valkeyMinute := func() int64 {
		t.Helper()
		reply, err := client.Do(t.Context(), "TIME")
		if err != nil {
			t.Fatal(err)
		}
		values, ok := reply.([]any)
		if !ok || len(values) != 2 {
			t.Fatalf("invalid Valkey time: %v", reply)
		}
		seconds, ok := values[0].(string)
		if !ok {
			t.Fatalf("invalid Valkey time: %v", reply)
		}
		unix, err := strconv.ParseInt(seconds, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return unix / 60
	}
	verifyRate := func(rateKey string) bool {
		t.Helper()
		window := valkeyMinute()
		for i := range 60 {
			result := rpc(rateKey, "SendMessage", a2aTestParams{Message: message(fmt.Sprintf("rate-%d", i), "question")})
			if result["error"] == nil {
				<-upstreamIDs
			}
			if valkeyMinute() != window {
				return false
			}
			if result["error"] != nil {
				t.Fatalf("send %d was refused before the rate limit: %+v", i, result)
			}
		}
		result := rpc(rateKey, "SendMessage", a2aTestParams{Message: message("over-rate", "question")})
		if result["error"] == nil {
			<-upstreamIDs
		}
		if valkeyMinute() != window {
			return false
		}
		if result["error"] == nil {
			t.Fatalf("send above the per-key rate limit was accepted: %+v", result)
		}
		select {
		case <-upstreamIDs:
			t.Fatal("send above the per-key rate limit reached the upstream")
		default:
		}
		return true
	}
	for attempt := 0; ; attempt++ {
		rateKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "rate-limited caller", "project_id": project, "scopes": []string{"agents"}, "allowed_agents": []string{agent}}, idem(fmt.Sprintf("a2a-rate-key-%d", attempt)), http.StatusCreated)["secret"].(string)
		if err := h.Runtime.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		if verifyRate(rateKey) {
			break
		}
	}
	for _, invalid := range []struct {
		name  string
		field string
		value int
	}{
		{name: "zero-remote-bound", field: "remote_work_bound_seconds", value: 0},
		{name: "remote-bound-over-hour", field: "remote_work_bound_seconds", value: 3601},
		{name: "zero-continuations", field: "max_task_continuations", value: 0},
		{name: "continuations-over-ceiling", field: "max_task_continuations", value: 129},
		{name: "zero-deadline", field: "task_deadline_seconds", value: 0},
		{name: "deadline-over-hour", field: "task_deadline_seconds", value: 3601},
	} {
		h.want(owner, http.MethodPut, "/api/v1/agents/"+agent, map[string]any{
			"project_id": project, "name": "fixture", "credential": "upstream-secret",
			"agent": map[string]any{"protocol": "a2a-1.0.0", "endpoint": upstream.URL + "/rpc", "card_digest": hex.EncodeToString(digest[:]), "toolset_revisions": []string{}, "route_revisions": []string{}, "pricing_revision_id": pricing, "max_cost": "1", "message_price": messagePrice, invalid.field: invalid.value},
		}, withMatch(published, idem("a2a-invalid-"+invalid.name)), http.StatusUnprocessableEntity)
	}
	boundedDraft := h.want(owner, http.MethodPut, "/api/v1/agents/"+agent, map[string]any{
		"project_id": project, "name": "fixture", "credential": "upstream-secret",
		"agent": map[string]any{"protocol": "a2a-1.0.0", "endpoint": upstream.URL + "/rpc", "card_digest": hex.EncodeToString(digest[:]), "toolset_revisions": []string{}, "route_revisions": []string{}, "pricing_revision_id": pricing, "max_cost": "1", "message_price": messagePrice, "remote_work_bound_seconds": 120, "max_task_continuations": 2, "task_deadline_seconds": 2},
	}, withMatch(published, idem("a2a-add-work-bound")), http.StatusOK)
	boundedPublished := h.want(owner, http.MethodPost, "/api/v1/agents/"+agent+"/publish", nil, withMatch(boundedDraft, idem("a2a-publish-work-bound")), http.StatusOK)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if result := rpc(budgetedKey, "SendMessage", a2aTestParams{Message: message("bounded-work", "question")}); result["error"] != nil {
		t.Fatalf("budgeted agent with a declared work bound was refused: %+v", result)
	}
	<-upstreamIDs
	boundedStart := rpc(budgetedKey, "SendMessage", a2aTestParams{Message: message("request-input", "question")})
	boundedPayload, err := json.Marshal(boundedStart["result"])
	if err != nil {
		t.Fatal(err)
	}
	var boundedResponse struct {
		Task *protocol.Task `json:"task"`
	}
	if err := json.Unmarshal(boundedPayload, &boundedResponse); err != nil || boundedResponse.Task == nil {
		t.Fatalf("bounded task did not start: %+v, %v", boundedStart, err)
	}
	<-upstreamIDs
	for i := range 2 {
		result := rpc(budgetedKey, "SendMessage", a2aTestParams{Message: &protocol.Message{
			ID: "request-input", Role: protocol.MessageRoleUser, TaskID: boundedResponse.Task.ID,
			Parts: protocol.ContentParts{protocol.NewTextPart("question")},
		}})
		if result["error"] != nil {
			t.Fatalf("bounded continuation %d was refused: %+v", i, result)
		}
		<-upstreamIDs
	}
	if result := rpc(budgetedKey, "SendMessage", a2aTestParams{Message: &protocol.Message{
		ID: "request-input", Role: protocol.MessageRoleUser, TaskID: boundedResponse.Task.ID,
		Parts: protocol.ContentParts{protocol.NewTextPart("question")},
	}}); result["error"] == nil {
		t.Fatalf("continuation exceeded the published limit: %+v", result)
	}
	select {
	case <-upstreamIDs:
		t.Fatal("continuation above the published limit reached the upstream")
	default:
	}
	if result := rpc(budgetedKey, "CancelTask", a2aTestParams{ID: boundedResponse.Task.ID}); result["error"] != nil {
		t.Fatalf("cancel bounded task failed: %+v", result)
	}
	waitForCancellation := func(upstreamTaskID string) {
		cancellationDeadline := time.Now().Add(10 * time.Second)
		for {
			body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "GetTask", "params": map[string]any{"id": upstreamTaskID}})
			if err != nil {
				t.Fatal(err)
			}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, upstream.URL+"/rpc", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer upstream-secret")
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("A2A-Version", "1.0")
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			var upstreamResult struct {
				Result protocol.Task `json:"result"`
			}
			decodeErr := json.NewDecoder(response.Body).Decode(&upstreamResult)
			response.Body.Close()
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if upstreamResult.Result.Status.State == protocol.TaskStateCanceled {
				break
			}
			if time.Now().After(cancellationDeadline) {
				t.Fatal("upstream task was not canceled within the expected interval")
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	deadlineStart := rpc(budgetedKey, "SendMessage", a2aTestParams{Message: message("request-input", "question")})
	deadlinePayload, err := json.Marshal(deadlineStart["result"])
	if err != nil {
		t.Fatal(err)
	}
	var deadlineResponse struct {
		Task *protocol.Task `json:"task"`
	}
	if err := json.Unmarshal(deadlinePayload, &deadlineResponse); err != nil || deadlineResponse.Task == nil {
		t.Fatalf("deadline task did not start: %+v, %v", deadlineStart, err)
	}
	deadlineUpstreamID := <-upstreamIDs
	time.Sleep(2200 * time.Millisecond)
	if result := rpc(budgetedKey, "SendMessage", a2aTestParams{Message: &protocol.Message{
		ID: "request-input", Role: protocol.MessageRoleUser, TaskID: deadlineResponse.Task.ID,
		Parts: protocol.ContentParts{protocol.NewTextPart("question")},
	}}); result["error"] == nil {
		t.Fatalf("continuation after the published task deadline was accepted: %+v", result)
	}
	select {
	case <-upstreamIDs:
		t.Fatal("continuation after the published task deadline reached the upstream")
	default:
	}
	deadlineMonitorContext, stopDeadlineMonitor := context.WithCancel(t.Context())
	deadlineMonitorStopped := make(chan struct{})
	go func() {
		defer close(deadlineMonitorStopped)
		(&a2a.TaskMonitor{Server: h.A2A}).Run(deadlineMonitorContext)
	}()
	waitForCancellation(deadlineUpstreamID)
	stopDeadlineMonitor()
	<-deadlineMonitorStopped
	longDraft := h.want(owner, http.MethodPut, "/api/v1/agents/"+agent, map[string]any{
		"project_id": project, "name": "fixture", "credential": "upstream-secret",
		"agent": map[string]any{"protocol": "a2a-1.0.0", "endpoint": upstream.URL + "/rpc", "card_digest": hex.EncodeToString(digest[:]), "toolset_revisions": []string{}, "route_revisions": []string{}, "pricing_revision_id": pricing, "max_cost": "1", "message_price": messagePrice, "remote_work_bound_seconds": 120, "task_deadline_seconds": 600},
	}, withMatch(boundedPublished, idem("a2a-long-deadline")), http.StatusOK)
	longPublished := h.want(owner, http.MethodPost, "/api/v1/agents/"+agent+"/publish", nil, withMatch(longDraft, idem("a2a-publish-long-deadline")), http.StatusOK)
	monitorKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "monitor caller", "project_id": project, "scopes": []string{"agents"}, "allowed_agents": []string{agent}}, idem("a2a-monitor-key"), http.StatusCreated)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	monitor := &a2a.TaskMonitor{Server: h.A2A}
	monitorContext, stopMonitor := context.WithCancel(t.Context())
	monitorStopped := make(chan struct{})
	go func() {
		defer close(monitorStopped)
		monitor.Run(monitorContext)
	}()
	monitorTask := rpc(monitorKey["secret"].(string), "SendMessage", a2aTestParams{Message: message("request-input", "question")})
	if monitorTask["error"] != nil {
		t.Fatalf("monitor task did not start: %+v", monitorTask)
	}
	upstreamTaskID := <-upstreamIDs
	stopMonitor()
	<-monitorStopped
	currentMonitorKey := h.want(owner, http.MethodGet, "/api/v1/api-keys/"+monitorKey["id"].(string), nil, nil, http.StatusOK)
	h.want(owner, http.MethodPost, "/api/v1/api-keys/"+monitorKey["id"].(string)+"/revoke", nil, withMatch(currentMonitorKey, idem("a2a-monitor-revoke")), http.StatusOK)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	restartedContext, stopRestartedMonitor := context.WithCancel(t.Context())
	restartedMonitorStopped := make(chan struct{})
	go func() {
		defer close(restartedMonitorStopped)
		monitor.Run(restartedContext)
	}()
	t.Cleanup(func() {
		stopRestartedMonitor()
		<-restartedMonitorStopped
	})

	waitForCancellation(upstreamTaskID)
	select {
	case <-upstreamIDs:
		t.Fatal("monitor resent an accepted message")
	default:
	}
	longStream := streamRequest(key, "SendStreamingMessage", a2aTestParams{Message: message("stream-long", "long-running")})
	defer longStream.Body.Close()
	longReader := bufio.NewReader(longStream.Body)
	longTask, longTaskOK := nextEvent(longReader).(*protocol.Task)
	if !longTaskOK || longTask.ID == "" {
		t.Fatal("long stream did not begin with a task")
	}
	<-upstreamIDs
	started := time.Now()
	for time.Since(started) < 31*time.Second {
		working, workingOK := nextEvent(longReader).(*protocol.TaskStatusUpdateEvent)
		if !workingOK || working.TaskID != longTask.ID || working.Status.State != protocol.TaskStateWorking {
			t.Fatalf("long stream did not remain active: %+v", working)
		}
	}
	close(resumeLongStream)
	for {
		event := nextEvent(longReader)
		if completed, completedOK := event.(*protocol.TaskStatusUpdateEvent); completedOK && completed.Status.State == protocol.TaskStateCompleted {
			break
		}
	}
	revoked := streamRequest(revocableKey["secret"].(string), "SendStreamingMessage", a2aTestParams{Message: message("stream-revoke", "revocation")})
	defer revoked.Body.Close()
	revokedReader := bufio.NewReader(revoked.Body)
	if _, ok := nextEvent(revokedReader).(*protocol.Task); !ok {
		t.Fatal("revocable stream did not begin with a task")
	}
	<-upstreamIDs
	currentKey := h.want(owner, http.MethodGet, "/api/v1/api-keys/"+revocableKey["id"].(string), nil, nil, http.StatusOK)
	h.want(owner, http.MethodPost, "/api/v1/api-keys/"+revocableKey["id"].(string)+"/revoke", nil, withMatch(currentKey, idem("a2a-revoke-stream")), http.StatusOK)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	revocationResult := nextFrame(revokedReader)
	if revocationResult.Error == nil || revocationResult.Error.Message != "Authorization revoked" {
		t.Fatalf("revoked stream was not stopped: %+v", revocationResult)
	}
	close(resumeRevokedStream)
	redirectRPC.Store(true)
	if got := rpc(key, "SendMessage", a2aTestParams{Message: message("redirect-message", "redirect")}); got["error"] == nil {
		t.Fatalf("redirected send succeeded: %+v", got)
	}
	redirectCard.Store(true)
	if got := getCard(key, agent); got != http.StatusBadGateway || redirectHits.Load() != 0 {
		t.Fatalf("redirected credential request: status %d, target calls %d", got, redirectHits.Load())
	}
	var unknown int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.execution_debits WHERE kind='agent' AND state='outcome_unknown'`).Scan(&unknown); err != nil || unknown != 1 {
		t.Fatalf("ambiguous send was not retained: %d, %v", unknown, err)
	}
	var stored string
	if err := h.Pool.QueryRow(t.Context(), `SELECT concat(coalesce((SELECT json_agg(t)::text FROM olp.a2a_tasks t),''),coalesce((SELECT json_agg(d)::text FROM olp.execution_debits d),''),coalesce((SELECT json_agg(r)::text FROM olp.execution_debit_results r),''),coalesce((SELECT json_agg(r)::text FROM olp.agent_runs r),''),coalesce((SELECT json_agg(s)::text FROM olp.agent_steps s),''))`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "PRIVATE REQUEST") || strings.Contains(stored, "PRIVATE ARTIFACT") || strings.Contains(stored, "PRIVATE STREAM REQUEST") || strings.Contains(stored, "PRIVATE OVERSIZED REQUEST") {
		t.Fatal("message content persisted")
	}
	redirectRPC.Store(false)
	redirectCard.Store(false)
	pinnedStream := streamRequest(key, "SendStreamingMessage", a2aTestParams{Message: message("stream-pinned", "pinned")})
	pinnedTask, pinnedTaskOK := nextEvent(bufio.NewReader(pinnedStream.Body)).(*protocol.Task)
	if !pinnedTaskOK || pinnedTask.ID == "" {
		t.Fatal("pinned stream did not begin with a task")
	}
	<-upstreamIDs
	pinnedStream.Body.Close()
	replacement := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/agent-card.json" {
			_, _ = w.Write(cardFor("http://" + r.Host + "/rpc"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(replacement.Close)
	replacementDigest := sha256.Sum256(cardFor(replacement.URL + "/rpc"))
	updated := h.want(owner, http.MethodPut, "/api/v1/agents/"+agent, map[string]any{
		"project_id": project, "name": "fixture", "credential": nil,
		"agent": map[string]any{"protocol": "a2a-1.0.0", "endpoint": replacement.URL + "/rpc", "card_digest": hex.EncodeToString(replacementDigest[:]), "toolset_revisions": []string{}, "route_revisions": []string{}, "pricing_revision_id": pricing, "max_cost": "1", "message_price": "0.50"},
	}, withMatch(longPublished, idem("a2a-update")), http.StatusOK)
	h.want(owner, http.MethodPost, "/api/v1/agents/"+agent+"/publish", nil, withMatch(updated, idem("a2a-republish")), http.StatusOK)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if getCard(key, agent) != http.StatusOK {
		t.Fatal("new revision did not serve its pinned card")
	}
	pinnedSubscription := streamRequest(key, "SubscribeToTask", a2aTestParams{ID: pinnedTask.ID})
	defer pinnedSubscription.Body.Close()
	pinnedReader := bufio.NewReader(pinnedSubscription.Body)
	if first := nextEvent(pinnedReader); first.TaskInfo().TaskID != pinnedTask.ID {
		t.Fatalf("resubscription did not retain the task's published revision: %+v", first)
	}
	close(resumePinnedStream)
	for {
		event := nextEvent(pinnedReader)
		if event.TaskInfo().TaskID != pinnedTask.ID {
			t.Fatal("pinned resubscription changed the task identity")
		}
		if _, ok := event.(*protocol.TaskArtifactUpdateEvent); ok {
			break
		}
	}
	if status, ok := nextEvent(pinnedReader).(*protocol.TaskStatusUpdateEvent); !ok || status.TaskID != pinnedTask.ID || status.Status.State != protocol.TaskStateCompleted {
		t.Fatal("pinned resubscription did not complete")
	}
	pinned := rpc(key, "GetTask", a2aTestParams{ID: protocol.TaskID(id)})
	if task, ok := pinned["result"].(map[string]any); !ok || task["id"] != id || !strings.Contains(fmt.Sprint(task), "PRIVATE ARTIFACT") {
		t.Fatalf("task did not retain its published revision: %+v", pinned)
	}
	disableDraft := h.want(owner, http.MethodPost, "/api/v1/agents", map[string]any{
		"project_id": project, "name": "disable fixture", "credential": "upstream-secret",
		"agent": map[string]any{"protocol": "a2a-1.0.0", "endpoint": upstream.URL + "/rpc", "card_digest": hex.EncodeToString(digest[:]), "toolset_revisions": []string{}, "route_revisions": []string{}, "pricing_revision_id": pricing, "max_cost": "1", "message_price": messagePrice, "task_deadline_seconds": 600},
	}, idem("a2a-disable-create"), http.StatusCreated)
	disableID := disableDraft["id"].(string)
	disablePublished := h.want(owner, http.MethodPost, "/api/v1/agents/"+disableID+"/publish", nil, withMatch(disableDraft, idem("a2a-disable-publish")), http.StatusOK)
	disableKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "disable caller", "project_id": project, "scopes": []string{"agents"}, "allowed_agents": []string{disableID}}, idem("a2a-disable-key"), http.StatusCreated)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	disabledTask := rpcFor(disableID, disableKey["secret"].(string), "SendMessage", a2aTestParams{Message: message("request-input", "question")})
	if disabledTask["error"] != nil {
		t.Fatalf("task for disabled-agent test did not start: %+v", disabledTask)
	}
	disabledUpstreamID := <-upstreamIDs
	h.want(owner, http.MethodDelete, "/api/v1/agents/"+disableID, nil, withMatch(disablePublished, idem("a2a-disable-retire")), http.StatusNoContent)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitForCancellation(disabledUpstreamID)
	select {
	case <-upstreamIDs:
		t.Fatal("monitor resent an accepted message after agent disablement")
	default:
	}
	disabledBody, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "SendMessage", "params": a2aTestParams{Message: message("disabled-send", "question")}})
	if err != nil {
		t.Fatal(err)
	}
	disabledRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.HTTP.URL+"/a2a/"+disableID, bytes.NewReader(disabledBody))
	if err != nil {
		t.Fatal(err)
	}
	disabledRequest.Header.Set("Authorization", "Bearer "+disableKey["secret"].(string))
	disabledRequest.Header.Set("Content-Type", "application/json")
	disabledRequest.Header.Set("A2A-Version", "1.0")
	disabledResponse, err := http.DefaultClient.Do(disabledRequest)
	if err != nil {
		t.Fatal(err)
	}
	disabledResponse.Body.Close()
	if disabledResponse.StatusCode == http.StatusOK {
		t.Fatal("disabled agent accepted another send")
	}
	select {
	case <-upstreamIDs:
		t.Fatal("send to a disabled agent reached the upstream")
	default:
	}
	stopRestartedMonitor()
	<-restartedMonitorStopped
	burstDraft := h.want(owner, http.MethodPost, "/api/v1/agents", map[string]any{
		"project_id": project, "name": "cancellation burst", "credential": "upstream-secret",
		"agent": map[string]any{"protocol": "a2a-1.0.0", "endpoint": upstream.URL + "/rpc", "card_digest": hex.EncodeToString(digest[:]), "toolset_revisions": []string{}, "route_revisions": []string{}, "pricing_revision_id": pricing, "max_cost": "1", "message_price": messagePrice, "task_deadline_seconds": 600},
	}, idem("a2a-burst-create"), http.StatusCreated)
	burstAgent := burstDraft["id"].(string)
	h.want(owner, http.MethodPost, "/api/v1/agents/"+burstAgent+"/publish", nil, withMatch(burstDraft, idem("a2a-burst-publish")), http.StatusOK)
	burstKeys := slices.Collect(func(yield func(map[string]any) bool) {
		for i := range 16 {
			created := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": fmt.Sprintf("burst-%d", i), "project_id": project, "scopes": []string{"agents"}, "allowed_agents": []string{burstAgent}}, idem(fmt.Sprintf("a2a-burst-key-%d", i)), http.StatusCreated)
			if !yield(created) {
				return
			}
		}
	})
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	burstTasks := slices.Collect(func(yield func(string) bool) {
		for _, burstKey := range burstKeys {
			for range 4 {
				result := rpcFor(burstAgent, burstKey["secret"].(string), "SendMessage", a2aTestParams{Message: message("request-input", "question")})
				if result["error"] != nil {
					t.Fatalf("burst task was refused: %+v", result)
				}
				if !yield(<-upstreamIDs) {
					return
				}
			}
		}
	})
	for i, burstKey := range burstKeys {
		keyID := burstKey["id"].(string)
		current := h.want(owner, http.MethodGet, "/api/v1/api-keys/"+keyID, nil, nil, http.StatusOK)
		h.want(owner, http.MethodPost, "/api/v1/api-keys/"+keyID+"/revoke", nil, withMatch(current, idem(fmt.Sprintf("a2a-burst-revoke-%d", i))), http.StatusOK)
	}
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	holdCancellation.Store(true)
	burstStart := time.Now()
	burstContext, stopBurst := context.WithCancel(t.Context())
	burstStopped := make(chan struct{})
	go func() {
		defer close(burstStopped)
		monitor.Run(burstContext)
	}()
	t.Cleanup(func() {
		stopBurst()
		<-burstStopped
	})
	burstDeadline := time.NewTimer(60 * time.Second)
	defer burstDeadline.Stop()
	observed := slices.Collect(func(yield func(string) bool) {
		for range len(burstTasks) {
			for {
				select {
				case id := <-cancelAttempts:
					if !slices.Contains(burstTasks, id) {
						continue
					}
					if !yield(id) {
						return
					}
				case <-burstDeadline.C:
					t.Fatal("cancellation burst did not reach every task within 60 seconds")
				}
				break
			}
		}
	})
	for _, id := range burstTasks {
		if !slices.Contains(observed, id) {
			t.Fatalf("cancellation burst starved task %q", id)
		}
	}
	t.Logf("requested cancellation for %d distinct tasks in %s", len(burstTasks), time.Since(burstStart))
	stopBurst()
	<-burstStopped
}

func TestStructuredPromptPublicationValidatesAndExports(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := h.want(owner, http.MethodPost, "/api/v1/projects", map[string]any{"name": "Structured prompts"}, idem("structured-project"), http.StatusCreated)["id"].(string)
	definition := json.RawMessage(`{"messages":[{"role":"system","content":"Help {{name}}"},{"role":"user","content":"Hello {{name}}"}],"variable_schema":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false},"default_parameters":{"temperature":0.25,"max_output_tokens":32}}`)
	created := h.want(owner, http.MethodPost, "/api/v1/prompts", map[string]any{"project_id": project, "name": "structured", "prompt": definition}, idem("structured-create"), http.StatusCreated)
	path := "/api/v1/prompts/" + created["id"].(string)
	published := h.want(owner, http.MethodPost, path+"/publish", nil, withMatch(created, idem("structured-publish")), http.StatusOK)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	exported := h.want(owner, http.MethodGet, "/api/v1/configuration/export", nil, nil, http.StatusOK)
	entries := exported["document"].(map[string]any)["agent_publications"].([]any)
	if len(entries) != 1 {
		t.Fatalf("exported publications = %v", entries)
	}
	publishedPrompt := entries[0].(map[string]any)["published"].(map[string]any)["prompt"]
	encoded, err := json.Marshal(publishedPrompt)
	if err != nil {
		t.Fatal(err)
	}
	var original, roundTrip map[string]any
	if err := json.Unmarshal(definition, &original); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &roundTrip); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"messages", "variable_schema", "default_parameters"} {
		if !reflect.DeepEqual(original[field], roundTrip[field]) {
			t.Fatalf("export changed %s: original=%v exported=%v", field, original[field], roundTrip[field])
		}
	}
	if published["revision_id"] == nil {
		t.Fatal("publication did not create a revision")
	}
	for _, tc := range []struct{ name, definition string }{
		{"role", `{"messages":[{"role":"tool","content":"{{name}}"}],"variable_schema":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}}`},
		{"schema-type", `{"messages":[{"role":"user","content":"{{name}}"}],"variable_schema":{"type":"object","properties":{"name":{"type":"integer"}},"required":["name"]}}`},
		{"schema-extra", `{"messages":[{"role":"user","content":"{{name}}"}],"variable_schema":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":true}}`},
		{"defaults", `{"messages":[{"role":"user","content":"{{name}}"}],"variable_schema":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]},"default_parameters":{"temperature":3}}`},
		{"size", `{"messages":[{"role":"user","content":` + strconv.Quote(strings.Repeat("x", prompts.MaxTemplateBytes+1)) + `}],"variable_schema":{"type":"object","properties":{},"required":[]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.want(owner, http.MethodPost, "/api/v1/prompts", map[string]any{"project_id": project, "name": "invalid", "prompt": json.RawMessage(tc.definition)}, idem("structured-invalid-"+tc.name), http.StatusUnprocessableEntity)
		})
	}
}

func TestA2AOAuthGrantLifecycle(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "OAuth agent")
	authority := testutil.NewOAuthServer(t)
	const cardTemplate = `{"name":"OAuth fixture","version":"1.0","supportedInterfaces":[{"url":"%s","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],"capabilities":{},"defaultInputModes":["text"],"defaultOutputModes":["text"],"skills":[]}`
	var upstreamCalls, originalCallerCalls, replacementOldGrantCalls atomic.Int32
	upstreamAgent := a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(a2asrv.AgentExecutorFunc(func(ctx context.Context, execution *a2asrv.ExecutorContext) iter.Seq2[protocol.Event, error] {
		return func(yield func(protocol.Event, error) bool) {
			if yield(protocol.NewSubmittedTask(execution, execution.Message), nil) {
				yield(protocol.NewStatusUpdateEvent(execution, protocol.TaskStateInputRequired, nil), nil)
			}
		}
	})))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/agent-card.json" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, cardTemplate, "http://"+r.Host+"/rpc")
			return
		}
		upstreamCalls.Add(1)
		identity, ok := authority.Authorized(r)
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if identity.Account == "original-account" {
			originalCallerCalls.Add(1)
		}
		upstreamAgent.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	var user string
	if err := h.Pool.QueryRow(t.Context(), `SELECT id::text FROM olp.users WHERE email='owner@example.com'`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	pricing := acctID(t)
	acctExec(t, h.Pool, `INSERT INTO olp.pricing_revisions(id,revision,effective_at,created_by) VALUES($1,1,now(),$2)`, pricing, user)
	digest := sha256.Sum256([]byte(fmt.Sprintf(cardTemplate, upstream.URL+"/rpc")))
	oauth := struct {
		ClientID              string   `json:"client_id"`
		Audience              string   `json:"audience"`
		Scopes                []string `json:"scopes"`
		AuthorizationEndpoint string   `json:"authorization_endpoint"`
		TokenEndpoint         string   `json:"token_endpoint"`
	}{"agent-client", "agent-audience", []string{"agent.send"}, authority.URL + "/authorize", authority.URL + "/token"}
	created := h.want(owner, http.MethodPost, "/api/v1/agents", map[string]any{
		"project_id": project, "name": "OAuth agent",
		"agent": map[string]any{"protocol": "a2a-1.0.0", "endpoint": upstream.URL + "/rpc", "card_digest": hex.EncodeToString(digest[:]), "toolset_revisions": []string{}, "route_revisions": []string{}, "pricing_revision_id": pricing, "max_cost": "1", "message_price": "0.01", "oauth": oauth},
	}, idem("agent-oauth-create"), http.StatusCreated)
	definition, ok := created["draft"].(map[string]any)
	if !ok {
		t.Fatalf("created agent has no draft: %v", created)
	}
	agent, ok := definition["agent"].(map[string]any)
	if !ok || agent["oauth"] == nil {
		t.Fatalf("created agent did not preserve OAuth configuration: %v", created)
	}
	agentID := created["id"].(string)
	published := h.want(owner, http.MethodPost, "/api/v1/agents/"+agentID+"/publish", nil, withMatch(created, idem("agent-oauth-publish")), http.StatusOK)
	changed := map[string]any{
		"project_id": project, "name": "OAuth agent",
		"agent": map[string]any{"protocol": "a2a-1.0.0", "endpoint": upstream.URL + "/changed", "card_digest": hex.EncodeToString(digest[:]), "toolset_revisions": []string{}, "route_revisions": []string{}, "pricing_revision_id": pricing, "max_cost": "1", "message_price": "0.01", "oauth": oauth},
	}
	h.want(owner, http.MethodPut, "/api/v1/agents/"+agentID, changed, withMatch(published, idem("agent-oauth-endpoint-unchanged-credential")), http.StatusUnprocessableEntity)
	key := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "OAuth agent caller", "project_id": project, "scopes": []string{"agents"}, "allowed_agents": []string{agentID}}, idem("agent-oauth-key"), http.StatusCreated)
	client, prefix, _ := acctKeyspace(t)
	limiter, err := limits.New(client, strings.TrimSuffix(prefix, ":"))
	if err != nil {
		t.Fatal(err)
	}
	h.A2A.Ledger = &a2a.Ledger{Pool: h.Pool, Store: &limits.DebitStore{Pool: h.Pool, Limiter: limiter}, Auth: h.AuthKey}
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	rpc := func(secret, method, message string, params a2aTestParams) map[string]any {
		t.Helper()
		payload := func() a2aTestParams {
			if params.ID != "" || params.Message != nil {
				return params
			}
			return a2aTestParams{Message: &protocol.Message{ID: message, Role: protocol.MessageRoleUser, Parts: protocol.ContentParts{protocol.NewTextPart("hello")}}}
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
		request.Header.Set("Authorization", "Bearer "+secret)
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
	taskID := func(result map[string]any) string {
		t.Helper()
		var payload struct {
			Task *protocol.Task `json:"task"`
		}
		if err := json.Unmarshal(mustJSON(result["result"]), &payload); err != nil || payload.Task == nil || payload.Task.ID == "" || payload.Task.Status.State != protocol.TaskStateInputRequired {
			t.Fatalf("agent task did not start: %v, %v", result, err)
		}
		return string(payload.Task.ID)
	}
	completeEnrollment := func(handoff mcpgrants.Handoff) {
		t.Helper()
		_, fragment, _ := strings.Cut(handoff.EnrollmentURL, "#")
		enrollmentID, token, _ := strings.Cut(fragment, ".")
		h.want(owner, http.MethodPost, "/api/v1/mcp-grant-enrollments/claim", mcpPersonalClaimInput{EnrollmentID: enrollmentID, HandoffToken: token}, nil, http.StatusOK)
		authorizationResponse := h.want(owner, http.MethodPost, "/api/v1/mcp-grant-enrollments/"+enrollmentID+"/authorize", personalOAuthAuthorizeInput{Flow: mcpgrants.AuthorizationCode}, nil, http.StatusOK)
		var authorization mcpgrants.Authorization
		if err := json.Unmarshal(mustJSON(authorizationResponse), &authorization); err != nil {
			t.Fatal(err)
		}
		redirectClient := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		redirectResponse, err := redirectClient.Get(authorization.AuthorizationURL)
		if err != nil {
			t.Fatal(err)
		}
		redirectResponse.Body.Close()
		callback, err := url.Parse(redirectResponse.Header.Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		h.want(owner, http.MethodPost, "/api/v1/mcp-grant-enrollments/"+enrollmentID+"/complete", personalOAuthCompletionInput{Code: callback.Query().Get("code"), State: callback.Query().Get("state")}, nil, http.StatusOK)
	}
	enroll := func(keyID string) {
		t.Helper()
		handoffResponse := h.want(owner, http.MethodPost, "/api/v1/agents/"+agentID+"/grant-enrollments", struct {
			APIKeyID string `json:"api_key_id"`
		}{keyID}, nil, http.StatusOK)
		var handoff mcpgrants.Handoff
		if err := json.Unmarshal(mustJSON(handoffResponse), &handoff); err != nil {
			t.Fatal(err)
		}
		completeEnrollment(handoff)
	}
	secret := key["secret"].(string)
	cardRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.HTTP.URL+"/a2a/"+agentID+"/.well-known/agent-card.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	cardRequest.Header.Set("Authorization", "Bearer "+secret)
	cardResponse, err := http.DefaultClient.Do(cardRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer cardResponse.Body.Close()
	var advertised protocol.AgentCard
	if err := json.NewDecoder(cardResponse.Body).Decode(&advertised); err != nil || cardResponse.StatusCode != http.StatusOK || len(advertised.SupportedInterfaces) != 1 || advertised.SupportedInterfaces[0].URL != h.HTTP.URL+"/a2a/"+agentID {
		t.Fatalf("authorized caller could not discover the gateway card before enrollment: status %d, error %v", cardResponse.StatusCode, err)
	}
	if result := rpc(secret, "SendMessage", "missing", a2aTestParams{}); result["error"] == nil || upstreamCalls.Load() != 0 {
		t.Fatalf("missing grant did not refuse the request: %v", result)
	}
	enroll(key["id"].(string))
	firstTask := taskID(rpc(secret, "SendMessage", "first", a2aTestParams{}))
	if upstreamCalls.Load() != 1 {
		t.Fatal("bound grant did not reach the agent")
	}
	otherKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "Other caller", "project_id": project, "scopes": []string{"agents"}, "allowed_agents": []string{agentID}}, idem("agent-oauth-other-key"), http.StatusCreated)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if result := rpc(otherKey["secret"].(string), "SendMessage", "other", a2aTestParams{}); result["error"] == nil || upstreamCalls.Load() != 1 {
		t.Fatalf("another key used the first caller's grant: %v", result)
	}
	if result := rpc(otherKey["secret"].(string), "GetTask", "", a2aTestParams{ID: protocol.TaskID(firstTask)}); result["error"] == nil || upstreamCalls.Load() != 1 {
		t.Fatalf("another key accessed the first caller's task: %v", result)
	}
	standing := h.want(owner, http.MethodGet, "/api/v1/mcp-user-grants", nil, nil, http.StatusOK)
	items := mcpExecItems(t, standing, "items")
	if len(items) != 1 {
		t.Fatalf("expected one agent grant: %v", standing)
	}
	item := mcpExecItem(t, items, 0, standing)
	bound := mcpExecObject(t, item, "binding")
	if bound["agent_id"] != agentID || item["state"] != "active" {
		t.Fatalf("agent grant does not match publication: %v", item)
	}
	grantID, ok := item["grant_id"].(string)
	if !ok || grantID == "" {
		t.Fatalf("agent grant has no revocable identifier: %v", item)
	}
	exported := h.want(owner, http.MethodGet, "/api/v1/configuration/export", nil, nil, http.StatusOK)
	for _, issued := range authority.Issued() {
		if strings.Contains(string(mustJSON(exported)), issued) || strings.Contains(string(mustJSON(standing)), issued) {
			t.Fatal("OAuth grant token escaped through management output")
		}
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.mcp_user_grants SET expires_at=now()-interval '1 second' WHERE id=$1`, grantID); err != nil {
		t.Fatal(err)
	}
	secondTask := taskID(rpc(secret, "SendMessage", "refreshed", a2aTestParams{}))
	if upstreamCalls.Load() != 2 || len(authority.Issued()) < 2 {
		t.Fatal("expired agent grant did not refresh successfully")
	}
	runMonitor := func(id, expected string) {
		t.Helper()
		if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.a2a_tasks SET deadline_at=now()-interval '1 second' WHERE id=$1`, id); err != nil {
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
			if err := h.Pool.QueryRow(t.Context(), `SELECT state FROM olp.a2a_tasks WHERE id=$1`, id).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if state == expected {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("task after monitor restart: got %s, want %s", state, expected)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	runMonitor(firstTask, "canceled")
	if upstreamCalls.Load() != 3 {
		t.Fatal("restarted monitor did not cancel using the bound grant")
	}
	authority.RevokeRefreshTokens()
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.mcp_user_grants SET expires_at=now()-interval '1 second' WHERE id=$1`, grantID); err != nil {
		t.Fatal(err)
	}
	if result := rpc(secret, "SendMessage", "unrefreshable", a2aTestParams{}); result["error"] == nil || upstreamCalls.Load() != 3 {
		t.Fatalf("unrefreshable grant did not refuse the request: %v", result)
	}
	h.want(owner, http.MethodDelete, "/api/v1/mcp-user-grants/"+grantID, nil, nil, http.StatusNoContent)
	if result := rpc(secret, "SendMessage", "revoked", a2aTestParams{}); result["error"] == nil || upstreamCalls.Load() != 3 {
		t.Fatalf("revoked grant did not refuse the request: %v", result)
	}
	runMonitor(secondTask, "outcome_unknown")
	if upstreamCalls.Load() != 3 {
		t.Fatal("monitor dispatched cancellation using a revoked grant")
	}
	preservedKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "Original task caller", "project_id": project, "scopes": []string{"agents"}, "allowed_agents": []string{agentID}}, idem("agent-oauth-preserved-key"), http.StatusCreated)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	authority.SignInAs(testutil.OAuthIdentity{Subject: "original-user", Account: "original-account"})
	enroll(preservedKey["id"].(string))
	preservedSecret := preservedKey["secret"].(string)
	oldTask := taskID(rpc(preservedSecret, "SendMessage", "old-task", a2aTestParams{}))
	oldMonitorTask := taskID(rpc(preservedSecret, "SendMessage", "old-monitor-task", a2aTestParams{}))
	var preservedGrant string
	if err := h.Pool.QueryRow(t.Context(), `SELECT id::text FROM olp.mcp_user_grants WHERE agent_id=$1 AND binding->'identity'->>'id'=$2`, agentID, preservedKey["id"]).Scan(&preservedGrant); err != nil {
		t.Fatal(err)
	}
	replacement := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := authority.Authorized(r)
		if ok && identity.Account == "original-account" {
			replacementOldGrantCalls.Add(1)
		}
		if r.URL.Path == "/.well-known/agent-card.json" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, cardTemplate, "http://"+r.Host+"/rpc")
			return
		}
		upstreamCalls.Add(1)
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		upstreamAgent.ServeHTTP(w, r)
	}))
	t.Cleanup(replacement.Close)
	replacementDigest := sha256.Sum256([]byte(fmt.Sprintf(cardTemplate, replacement.URL+"/rpc")))
	replaced := h.want(owner, http.MethodPut, "/api/v1/agents/"+agentID, map[string]any{
		"project_id": project, "name": "OAuth agent", "credential": nil,
		"agent": map[string]any{"protocol": "a2a-1.0.0", "endpoint": replacement.URL + "/rpc", "card_digest": hex.EncodeToString(digest[:]), "toolset_revisions": []string{}, "route_revisions": []string{}, "pricing_revision_id": pricing, "max_cost": "1", "message_price": "0.01"},
	}, withMatch(published, idem("agent-oauth-endpoint-remove-credential")), http.StatusOK)
	h.want(owner, http.MethodPost, "/api/v1/agents/"+agentID+"/publish", nil, withMatch(replaced, idem("agent-oauth-endpoint-bad-card")), http.StatusUnprocessableEntity)
	recertified := h.want(owner, http.MethodPut, "/api/v1/agents/"+agentID, map[string]any{
		"project_id": project, "name": "OAuth agent", "credential": nil,
		"agent": map[string]any{"protocol": "a2a-1.0.0", "endpoint": replacement.URL + "/rpc", "card_digest": hex.EncodeToString(replacementDigest[:]), "toolset_revisions": []string{}, "route_revisions": []string{}, "pricing_revision_id": pricing, "max_cost": "1", "message_price": "0.01"},
	}, withMatch(replaced, idem("agent-oauth-endpoint-reviewed-card")), http.StatusOK)
	changedEndpoint := h.want(owner, http.MethodPost, "/api/v1/agents/"+agentID+"/publish", nil, withMatch(recertified, idem("agent-oauth-endpoint-recertify")), http.StatusOK)
	newCredential := h.want(owner, http.MethodPut, "/api/v1/agents/"+agentID, map[string]any{
		"project_id": project, "name": "OAuth agent", "credential": nil,
		"agent": map[string]any{"protocol": "a2a-1.0.0", "endpoint": replacement.URL + "/rpc", "card_digest": hex.EncodeToString(replacementDigest[:]), "toolset_revisions": []string{}, "route_revisions": []string{}, "pricing_revision_id": pricing, "max_cost": "1", "message_price": "0.01", "oauth": oauth},
	}, withMatch(changedEndpoint, idem("agent-oauth-endpoint-new-credential")), http.StatusOK)
	republished := h.want(owner, http.MethodPost, "/api/v1/agents/"+agentID+"/publish", nil, withMatch(newCredential, idem("agent-oauth-endpoint-publish-new-credential")), http.StatusOK)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.mcp_user_grants SET expires_at=now()-interval '1 second' WHERE id=$1`, preservedGrant); err != nil {
		t.Fatal(err)
	}
	if result := rpc(preservedSecret, "GetTask", "", a2aTestParams{ID: protocol.TaskID(oldTask)}); result["error"] != nil || result["result"] == nil {
		t.Fatalf("original task could not be read after replacement: %v", result)
	}
	if result := rpc(preservedSecret, "CancelTask", "", a2aTestParams{ID: protocol.TaskID(oldTask)}); result["error"] != nil || result["result"] == nil {
		t.Fatalf("original task could not be canceled after replacement: %v", result)
	}
	runMonitor(oldMonitorTask, "canceled")
	if originalCallerCalls.Load() != 5 || replacementOldGrantCalls.Load() != 0 {
		t.Fatal("original task operations did not use their original grant and endpoint")
	}
	if result := rpc(secret, "SendMessage", "replacement-missing", a2aTestParams{}); result["error"] == nil || upstreamCalls.Load() != 8 {
		t.Fatalf("replacement endpoint reused an old grant: %v", result)
	}
	authority.SignInAs(testutil.OAuthIdentity{Subject: "replacement-user", Account: "replacement-account"})
	enroll(key["id"].(string))
	replacementTask := taskID(rpc(secret, "SendMessage", "replacement-enrolled", a2aTestParams{}))
	if upstreamCalls.Load() != 9 || replacementOldGrantCalls.Load() != 0 {
		t.Fatal("replacement endpoint did not use its new grant")
	}
	expectedAfterRetirement := func() int32 {
		if !access.OIDCTestBuild() {
			return 10
		}
		base := "/api/v1/projects/" + project
		templates := h.want(owner, http.MethodGet, base+"/limit-templates", nil, nil, http.StatusOK)
		h.want(owner, http.MethodPut, base+"/limit-templates", map[string]any{"templates": map[string]any{"worker": map[string]any{"requests_per_minute": 100}}}, etagHeader(templates), http.StatusOK)
		publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: publicKey, KeyID: "current", Algorithm: "EdDSA", Use: "sig"}}})
		}))
		t.Cleanup(jwks.Close)
		issuerConfig := map[string]any{
			"name": "Portable CI", "issuer": jwks.URL, "jwks_url": jwks.URL,
			"enabled": true, "audiences": []string{"olp"}, "algorithms": []string{"EdDSA"},
			"max_lifetime_seconds": 600, "disabled_key_ids": []string{},
			"mappings": []any{map[string]any{
				"name": "worker", "match": map[string]string{}, "project_id": project, "limit_template": "worker",
				"route_groups": []string{}, "scopes": []string{"agents"}, "allowed_agents": []string{agentID}, "end_user_claim": "/oid",
			}},
		}
		h.want(owner, http.MethodPost, "/api/v1/workload-issuers", issuerConfig, idem("agent-oauth-workload-create"), http.StatusCreated)
		if err := h.Runtime.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: privateKey}, (&jose.SignerOptions{}).WithHeader("kid", "current"))
		if err != nil {
			t.Fatal(err)
		}
		userToken := func(id string) string {
			t.Helper()
			return signEntraAssertion(t, signer, entraTestClaims{Issuer: jwks.URL, Audience: "olp", Subject: "shared-workload", ObjectID: id, IssuedAt: time.Now().Unix(), Expires: time.Now().Add(5 * time.Minute).Unix()})
		}
		userA, userB := userToken("alice"), userToken("bob")
		authorityA, err := h.Runtime.Authenticate(userA)
		if err != nil {
			t.Fatal(err)
		}
		authorityB, err := h.Runtime.Authenticate(userB)
		if err != nil {
			t.Fatal(err)
		}
		if authorityA.ID != authorityB.ID || authorityA.EndUserDigest == authorityB.EndUserDigest {
			t.Fatal("test requires one workload principal with distinct verified end users")
		}
		startPersonal := func(token string, expected int) mcpgrants.Handoff {
			t.Helper()
			response, raw := h.do(nil, http.MethodPost, "/a2a/"+agentID+"/grant-enrollments", nil, map[string]string{"Authorization": "Bearer " + token})
			if response.StatusCode != expected {
				t.Fatalf("personal enrollment returned %d, want %d: %s", response.StatusCode, expected, raw)
			}
			if expected != http.StatusCreated {
				return mcpgrants.Handoff{}
			}
			if response.Header.Get("Cache-Control") != "no-store" {
				t.Fatal("personal enrollment handoff can be cached")
			}
			var handoff mcpgrants.Handoff
			if err := json.Unmarshal(raw, &handoff); err != nil {
				t.Fatal(err)
			}
			return handoff
		}
		startPersonal(secret, http.StatusForbidden)
		completeEnrollment(startPersonal(userA, http.StatusCreated))
		userTask := taskID(rpc(userA, "SendMessage", "alice", a2aTestParams{}))
		if result := rpc(userB, "SendMessage", "bob-before-enrollment", a2aTestParams{}); result["error"] == nil || upstreamCalls.Load() != 10 {
			t.Fatalf("another end user used Alice's grant: %v", result)
		}
		if result := rpc(userB, "GetTask", "", a2aTestParams{ID: protocol.TaskID(userTask)}); result["error"] == nil || upstreamCalls.Load() != 10 {
			t.Fatalf("another end user accessed Alice's task: %v", result)
		}
		completeEnrollment(startPersonal(userB, http.StatusCreated))
		taskID(rpc(userB, "SendMessage", "bob", a2aTestParams{}))
		if result := rpc(userB, "GetTask", "", a2aTestParams{ID: protocol.TaskID(userTask)}); result["error"] == nil || upstreamCalls.Load() != 11 {
			t.Fatalf("another enrolled end user accessed Alice's task: %v", result)
		}
		return 12
	}()
	h.want(owner, http.MethodDelete, "/api/v1/agents/"+agentID, nil, withMatch(republished, idem("agent-oauth-retire")), http.StatusNoContent)
	runMonitor(replacementTask, "canceled")
	if upstreamCalls.Load() != expectedAfterRetirement {
		t.Fatal("monitor did not cancel the task after retirement")
	}
}
