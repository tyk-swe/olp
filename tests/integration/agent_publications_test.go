//go:build integration

package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"math/big"
	"net/http"
	"net/http/httptest"
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
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/a2a"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/prompts"
	"github.com/tyk-swe/olp/internal/secrets"
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

func TestA2AAgentCardUsesGatewayAuthenticationAndEndpoint(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "A2A discovery")
	const cardTemplate = `{"name":"Fixture agent","description":"A test agent","version":"1.0","supportedInterfaces":[{"url":"PINNED_ENDPOINT","protocolBinding":"JSONRPC","protocolVersion":"1.0"},{"url":"https://wrong.invalid/rpc","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],"capabilities":{"streaming":true,"pushNotifications":true},"defaultInputModes":["text"],"defaultOutputModes":["text"],"skills":[]}`
	cardFor := func(endpoint string) []byte {
		return bytes.ReplaceAll([]byte(cardTemplate), []byte("PINNED_ENDPOINT"), []byte(endpoint))
	}
	upstreamIDs := make(chan string, 1)
	resumeStream := make(chan struct{})
	resumePinnedStream := make(chan struct{})
	resumeRevokedStream := make(chan struct{})
	resumeLongStream := make(chan struct{})
	var redirectCard, redirectRPC atomic.Bool
	var redirectHits atomic.Int32
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(redirectTarget.Close)
	agentHandler := a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(a2asrv.AgentExecutorFunc(func(ctx context.Context, execution *a2asrv.ExecutorContext) iter.Seq2[protocol.Event, error] {
		return func(yield func(protocol.Event, error) bool) {
			var dispatched int
			if err := h.Pool.QueryRow(ctx, `SELECT count(*) FROM olp.execution_debits WHERE kind='agent' AND state='dispatched'`).Scan(&dispatched); err != nil || dispatched != 1 {
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
	rateKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "rate-limited caller", "project_id": project, "scopes": []string{"agents"}, "allowed_agents": []string{agent}}, idem("a2a-rate-key"), http.StatusCreated)["secret"].(string)
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
	activeTask := func() protocol.TaskID {
		t.Helper()
		result := rpc(foreign, "SendMessage", a2aTestParams{Message: message("request-input", "question")})
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
	for i := range 60 {
		result := rpc(rateKey, "SendMessage", a2aTestParams{Message: message(fmt.Sprintf("rate-%d", i), "question")})
		if result["error"] != nil {
			t.Fatalf("send %d was refused before the rate limit: %+v", i, result)
		}
		<-upstreamIDs
	}
	if result := rpc(rateKey, "SendMessage", a2aTestParams{Message: message("over-rate", "question")}); result["error"] == nil {
		t.Fatalf("send above the per-key rate limit was accepted: %+v", result)
	}
	select {
	case <-upstreamIDs:
		t.Fatal("send above the per-key rate limit reached the upstream")
	default:
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
			err = json.NewDecoder(response.Body).Decode(&upstreamResult)
			response.Body.Close()
			if err != nil {
				t.Fatal(err)
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
