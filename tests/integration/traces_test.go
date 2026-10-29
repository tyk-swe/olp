//go:build integration

package integration_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/export"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/sinks"
	"github.com/tyk-swe/olp/internal/usage"
	collector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func traceText(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
}

func traceBatch(spans ...*tracepb.Span) *collector.ExportTraceServiceRequest {
	return &collector.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{traceText("service.name", "private-resource-content")}}, ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}}}}}
}

func tracePayload(t *testing.T, request *collector.ExportTraceServiceRequest) []byte {
	t.Helper()
	payload, err := proto.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func postTrace(h *accessHarness, key string, payload []byte) (int, []byte) {
	code, body, _ := h.gatewayRaw(http.MethodPost, "/v1/traces", key, bytes.NewReader(payload), map[string]string{"Content-Type": "application/x-protobuf"})
	return code, body
}

func gzipTrace(t *testing.T, payload []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func postGzipTrace(h *accessHarness, key string, payload []byte) (int, []byte) {
	code, body, _ := h.gatewayRaw(http.MethodPost, "/v1/traces", key, bytes.NewReader(payload), map[string]string{"Content-Type": "application/x-protobuf", "Content-Encoding": "gzip"})
	return code, body
}

func TestEvidenceExportStreamsAcceptProjectScopedConfiguration(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "Evidence export")
	for _, test := range []struct {
		path    string
		request map[string]any
	}{
		{path: "/api/v1/observability/sinks", request: map[string]any{"name": "Workflow evidence", "type": "https", "project_id": project, "destination": "https://example.invalid/evidence", "enabled": true, "streams": []string{"runs", "trace_spans"}, "format": "json"}},
	} {
		response := h.want(owner, http.MethodPost, test.path, test.request, idem(uuid.NewString()), http.StatusCreated)
		if response["project_id"] != project {
			t.Fatalf("%s did not preserve the project boundary: %v", test.path, response["project_id"])
		}
	}
}

func TestGlobalEvidenceSinksRecheckEachSourceProject(t *testing.T) {
	h := newAccessHarness(t)
	_, _, limiter := limSetup(t, "global-evidence-export")
	h.Gateway.Traces = &gateway.TraceDeps{Pool: h.Pool, Limiter: limiter, Auth: h.AuthKey}
	owner := h.owner()
	project := createProject(h, owner, "Global evidence allowed")
	foreignProject := createProject(h, owner, "Global evidence foreign")
	creator := h.invite(owner, "evidence-global@example.test", "owner")
	creatorID := mcpExecString(t, h.want(creator, http.MethodGet, "/api/v1/profile", nil, nil, http.StatusOK), "id")
	observabilityReceiver, observabilityEndpoint := newExportReceiver(t, "", http.StatusNoContent)
	managedReceiver, managedEndpoint := newExportReceiver(t, "", http.StatusNoContent)
	h.want(creator, http.MethodPost, "/api/v1/observability/sinks", map[string]any{"name": "Global observability evidence", "type": "https", "destination": observabilityEndpoint.URL, "enabled": true, "streams": []string{"trace_spans"}, "format": "json"}, idem(uuid.NewString()), http.StatusCreated)
	h.want(creator, http.MethodPost, "/api/v1/sinks", map[string]any{"name": "Global managed evidence", "type": "https", "destination": managedEndpoint.URL, "enabled": true, "streams": []string{"trace_spans"}}, idem(uuid.NewString()), http.StatusCreated)
	creatorRecord := h.want(owner, http.MethodGet, "/api/v1/users/"+creatorID, nil, nil, http.StatusOK)
	h.want(owner, http.MethodPatch, "/api/v1/users/"+creatorID, map[string]any{"access_scope": "assigned"}, etagHeader(creatorRecord), http.StatusOK)
	addMember(h, owner, project, creatorID, "manager")
	key := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "Allowed trace writer", "project_id": project, "scopes": []string{"traces_write"}}, idem(uuid.NewString()), http.StatusCreated)
	foreignKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "Foreign trace writer", "project_id": foreignProject, "scopes": []string{"traces_write"}}, idem(uuid.NewString()), http.StatusCreated)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	span := func(id byte) *tracepb.Span {
		return &tracepb.Span{TraceId: bytes.Repeat([]byte{id}, 16), SpanId: bytes.Repeat([]byte{id}, 8)}
	}
	if status, body := postTrace(h, mcpExecString(t, key, "secret"), tracePayload(t, traceBatch(span(1)))); status != http.StatusOK {
		t.Fatalf("allowed trace upload: %d %s", status, body)
	}
	if status, body := postTrace(h, mcpExecString(t, foreignKey, "secret"), tracePayload(t, traceBatch(span(2)))); status != http.StatusOK {
		t.Fatalf("foreign trace upload: %d %s", status, body)
	}
	observabilityWorker := &export.Worker{Pool: h.Pool, Keys: h.Ring2(), Installation: h.Server.Installation, Sender: export.NewSender(loopbackPolicy()), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	managedWorker := &sinks.Worker{Pool: h.Pool, Egress: loopbackPolicy()}
	if err := observabilityWorker.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := managedWorker.Deliver(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, receiver := range []*exportReceiver{observabilityReceiver, managedReceiver} {
		got := receiver.received()
		if len(got) != 1 {
			t.Fatalf("expected one authorized global delivery, got %d", len(got))
		}
		data, ok := got[0]["data"].(map[string]any)
		if !ok || data["project_id"] != project || data["evidence_origin"] != "client_asserted" {
			t.Fatalf("global sink delivered the wrong evidence: %v", got[0])
		}
	}
	h.want(owner, http.MethodDelete, "/api/v1/projects/"+project+"/members/"+creatorID, nil, projectEtag(h, owner, project), http.StatusNoContent)
	if status, body := postTrace(h, mcpExecString(t, key, "secret"), tracePayload(t, traceBatch(span(3)))); status != http.StatusOK {
		t.Fatalf("trace after membership removal: %d %s", status, body)
	}
	if err := observabilityWorker.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := managedWorker.Deliver(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(observabilityReceiver.received()) != 1 || len(managedReceiver.received()) != 1 {
		t.Fatal("a global sink delivered evidence after its creator lost project access")
	}
}

func TestTraceEvidenceExportPreservesOriginProjectAndPrivacy(t *testing.T) {
	h := newAccessHarness(t)
	_, _, limiter := limSetup(t, "trace-evidence-export")
	h.Gateway.Traces = &gateway.TraceDeps{Pool: h.Pool, Limiter: limiter, Auth: h.AuthKey}
	owner := h.owner()
	project := createProject(h, owner, "Trace evidence")
	otherProject := createProject(h, owner, "Other trace evidence")
	key := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "trace evidence writer", "project_id": project, "scopes": []string{"traces_write"}}, idem(uuid.NewString()), http.StatusCreated)
	otherKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "other trace writer", "project_id": otherProject, "scopes": []string{"traces_write"}}, idem(uuid.NewString()), http.StatusCreated)
	receiver, server := newExportReceiver(t, "", http.StatusNoContent)
	h.want(owner, http.MethodPost, "/api/v1/observability/sinks", map[string]any{"name": "Trace evidence", "type": "https", "project_id": project, "destination": server.URL, "enabled": true, "streams": []string{"trace_spans"}, "format": "json"}, idem(uuid.NewString()), http.StatusCreated)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	span := &tracepb.Span{TraceId: bytes.Repeat([]byte{1}, 16), SpanId: bytes.Repeat([]byte{2}, 8), Name: "private-span-content", Attributes: []*commonpb.KeyValue{traceText("olp.evidence_origin", "gateway_observed"), traceText("gen_ai.prompt", "private-prompt-content")}}
	for _, writer := range []string{key["secret"].(string), otherKey["secret"].(string)} {
		if status, body := postTrace(h, writer, tracePayload(t, traceBatch(span))); status != http.StatusOK {
			t.Fatalf("trace upload status %d: %s", status, body)
		}
	}
	worker := &export.Worker{Pool: h.Pool, Keys: h.Ring2(), Installation: h.Server.Installation, Sender: export.NewSender(loopbackPolicy()), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := worker.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	envelopes := receiver.received()
	if len(envelopes) != 1 {
		t.Fatalf("expected one project-scoped trace export, got %d", len(envelopes))
	}
	payload, err := json.Marshal(envelopes[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(payload, []byte(project)) || !bytes.Contains(payload, []byte("client_asserted")) || bytes.Contains(payload, []byte("gateway_observed")) || bytes.Contains(payload, []byte("private-")) {
		t.Fatalf("trace evidence provenance, scope or privacy mismatch: %s", payload)
	}
	expired := &tracepb.Span{TraceId: bytes.Repeat([]byte{1}, 16), SpanId: bytes.Repeat([]byte{5}, 8)}
	if status, body := postTrace(h, key["secret"].(string), tracePayload(t, traceBatch(expired))); status != http.StatusOK {
		t.Fatalf("expired trace upload status %d: %s", status, body)
	}
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.settings(key,value,etag,updated_by) SELECT 'retention.requests_days','1',gen_random_uuid(),created_by FROM olp.api_keys WHERE id=$1 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key["id"]); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.trace_spans SET received_at=now()-interval '2 days' WHERE project_id=$1 AND span_id=$2`, project, expired.SpanId); err != nil {
		t.Fatal(err)
	}
	if err := worker.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := receiver.received(); len(got) != 1 {
		t.Fatalf("trace evidence delivered beyond request retention: %d envelopes", len(got))
	}
	deleted := &tracepb.Span{TraceId: bytes.Repeat([]byte{1}, 16), SpanId: bytes.Repeat([]byte{6}, 8)}
	if status, body := postTrace(h, key["secret"].(string), tracePayload(t, traceBatch(deleted))); status != http.StatusOK {
		t.Fatalf("deleted trace upload status %d: %s", status, body)
	}
	if _, err := h.Pool.Exec(t.Context(), `DELETE FROM olp.trace_spans WHERE project_id=$1 AND span_id=$2`, project, deleted.SpanId); err != nil {
		t.Fatal(err)
	}
	if err := worker.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := receiver.received(); len(got) != 1 {
		t.Fatalf("deleted trace evidence was delivered: %d envelopes", len(got))
	}
}

func TestTraceEvidenceExportStopsAfterProjectAccessIsRevoked(t *testing.T) {
	h := newAccessHarness(t)
	_, _, limiter := limSetup(t, "trace-evidence-revocation")
	h.Gateway.Traces = &gateway.TraceDeps{Pool: h.Pool, Limiter: limiter, Auth: h.AuthKey}
	owner := h.owner()
	project := createProject(h, owner, "Trace evidence authority")
	invited := h.invite(owner, "evidence-manager@example.com", "developer")
	profile := h.want(invited, http.MethodGet, "/api/v1/profile", nil, nil, http.StatusOK)
	managerID := profile["id"].(string)
	h.want(owner, http.MethodPatch, "/api/v1/users/"+managerID, map[string]any{"access_scope": "assigned"}, etagHeader(profile), http.StatusOK)
	addMember(h, owner, project, managerID, "manager")
	manager := login(h, "evidence-manager@example.com")
	key := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "trace evidence writer", "project_id": project, "scopes": []string{"traces_write"}}, idem(uuid.NewString()), http.StatusCreated)
	receiver, server := newExportReceiver(t, "", http.StatusNoContent)
	h.want(manager, http.MethodPost, "/api/v1/observability/sinks", map[string]any{"name": "Revocable trace evidence", "type": "https", "project_id": project, "destination": server.URL, "enabled": true, "streams": []string{"trace_spans"}, "format": "json"}, idem(uuid.NewString()), http.StatusCreated)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	span := &tracepb.Span{TraceId: bytes.Repeat([]byte{3}, 16), SpanId: bytes.Repeat([]byte{4}, 8)}
	if status, body := postTrace(h, key["secret"].(string), tracePayload(t, traceBatch(span))); status != http.StatusOK {
		t.Fatalf("trace upload status %d: %s", status, body)
	}
	h.want(owner, http.MethodDelete, "/api/v1/projects/"+project+"/members/"+managerID, nil, projectEtag(h, owner, project), http.StatusNoContent)
	worker := &export.Worker{Pool: h.Pool, Keys: h.Ring2(), Installation: h.Server.Installation, Sender: export.NewSender(loopbackPolicy()), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := worker.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := receiver.received(); len(got) != 0 {
		t.Fatalf("trace evidence delivered after project access was revoked: %v", got)
	}
}

func TestOTLPTraceIngestionPrivacyOwnershipAndLimits(t *testing.T) {
	h := newAccessHarness(t)
	_, _, limiter := limSetup(t, "traces")
	h.Gateway.Traces = &gateway.TraceDeps{Pool: h.Pool, Limiter: limiter, Auth: h.AuthKey}
	owner := h.owner()
	project := createProject(h, owner, "Trace project")
	otherProject := createProject(h, owner, "Trace other project")
	key := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "trace writer", "project_id": project, "scopes": []string{"traces_write"}}, idem("trace-key"), http.StatusCreated)
	otherKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "other trace writer", "project_id": project, "scopes": []string{"traces_write"}}, idem("trace-other-key"), http.StatusCreated)
	foreignKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "foreign trace writer", "project_id": otherProject, "scopes": []string{"traces_write"}}, idem("trace-foreign-key"), http.StatusCreated)
	limitedKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "rate-limited trace writer", "project_id": project, "scopes": []string{"traces_write"}, "requests_per_minute": 2}, idem("trace-limited-key"), http.StatusCreated)
	group := h.want(owner, http.MethodPost, "/api/v1/budget-groups", map[string]any{"name": "trace group", "project_id": project, "requests_per_minute": 1}, idem("trace-group"), http.StatusCreated)
	groupKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "group trace writer", "project_id": project, "budget_group_id": group["id"], "scopes": []string{"traces_write"}}, idem("trace-group-key"), http.StatusCreated)
	otherGroupKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "other group trace writer", "project_id": project, "budget_group_id": group["id"], "scopes": []string{"traces_write"}}, idem("trace-group-other-key"), http.StatusCreated)
	concurrencyGroup := h.want(owner, http.MethodPost, "/api/v1/budget-groups", map[string]any{"name": "trace concurrency group", "project_id": project, "max_concurrency": 1}, idem("trace-concurrency-group"), http.StatusCreated)
	concurrencyKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "concurrency trace writer", "project_id": project, "budget_group_id": concurrencyGroup["id"], "scopes": []string{"traces_write"}}, idem("trace-concurrency-key"), http.StatusCreated)
	otherConcurrencyKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "other concurrency trace writer", "project_id": project, "budget_group_id": concurrencyGroup["id"], "scopes": []string{"traces_write"}}, idem("trace-concurrency-other-key"), http.StatusCreated)
	unscopedKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "projectless trace writer", "scopes": []string{"traces_write"}}, idem("trace-projectless-key"), http.StatusCreated)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	runID, attemptID := uuid.NewString(), uuid.NewString()
	keyID := key["id"].(string)
	span := &tracepb.Span{TraceId: bytes.Repeat([]byte{1}, 16), SpanId: bytes.Repeat([]byte{2}, 8), Name: "private-span-content", StartTimeUnixNano: 1, EndTimeUnixNano: 2,
		Attributes: []*commonpb.KeyValue{traceText("olp.run_id", runID), traceText("olp.attempt_id", attemptID), traceText("gen_ai.operation.name", "chat"), traceText("gen_ai.prompt", "private-prompt-content"), {Key: "gen_ai.usage.input_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 12}}}},
		Events:     []*tracepb.Span_Event{{Name: "private-event-content"}}, Links: []*tracepb.Span_Link{{Attributes: []*commonpb.KeyValue{traceText("secret", "private-link-content")}}}, Status: &tracepb.Status{Message: "private-status-content", Code: tracepb.Status_STATUS_CODE_OK}}
	second := &tracepb.Span{TraceId: bytes.Repeat([]byte{1}, 16), SpanId: bytes.Repeat([]byte{5}, 8)}
	payload := protowire.AppendString(protowire.AppendTag(tracePayload(t, traceBatch(span, second, span)), 120, protowire.BytesType), "private-unknown-content")
	for _, writer := range []string{key["secret"].(string), otherKey["secret"].(string), foreignKey["secret"].(string)} {
		status, body := postTrace(h, writer, payload)
		if status != http.StatusOK {
			t.Fatalf("trace upload status %d: %s", status, body)
		}
	}
	status, body := postTrace(h, key["secret"].(string), payload)
	if status != http.StatusOK {
		t.Fatalf("duplicate trace status %d: %s", status, body)
	}
	var rows, correlated, leaked, debits int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*),count(*) FILTER (WHERE run_id=$1 AND attempt_id=$2),count(*) FILTER (WHERE row_to_json(trace_spans)::text LIKE '%private-%') FROM olp.trace_spans`, runID, attemptID).Scan(&rows, &correlated, &leaked); err != nil {
		t.Fatal(err)
	}
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.execution_debits`).Scan(&debits); err != nil {
		t.Fatal(err)
	}
	if rows != 6 || correlated != 0 || leaked != 0 || debits != 0 {
		t.Fatalf("trace rows=%d owned=%d private=%d debits=%d", rows, correlated, leaked, debits)
	}
	var category, attributes string
	if err := h.Pool.QueryRow(t.Context(), `SELECT category,attributes::text FROM olp.trace_spans WHERE api_key_id=$1 AND category='chat' `, keyID).Scan(&category, &attributes); err != nil {
		t.Fatal(err)
	}
	if category != "chat" || !strings.Contains(attributes, "gen_ai.usage.input_tokens") {
		t.Fatalf("safe metadata missing: category=%q attributes=%s", category, attributes)
	}
	if code, body := postGzipTrace(h, key["secret"].(string), gzipTrace(t, payload)); code != http.StatusOK {
		t.Fatalf("compressed trace status %d: %s", code, body)
	}
	if code, _ := postGzipTrace(h, key["secret"].(string), gzipTrace(t, bytes.Repeat([]byte{1}, (1<<20)+1))); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized decompressed batch status %d", code)
	}
	for _, invalid := range []*tracepb.Span{
		{TraceId: make([]byte, 16), SpanId: bytes.Repeat([]byte{1}, 8)},
		{TraceId: bytes.Repeat([]byte{1}, 16), SpanId: make([]byte, 8)},
	} {
		if code, _ := postTrace(h, key["secret"].(string), tracePayload(t, traceBatch(invalid))); code != http.StatusBadRequest {
			t.Fatalf("zero identifier status %d", code)
		}
	}
	oversized := bytes.Repeat([]byte{1}, (1<<20)+1)
	if code, _ := postTrace(h, key["secret"].(string), oversized); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized batch status %d", code)
	}
	if code, _ := postTrace(h, foreignKey["secret"].(string), tracePayload(t, traceBatch(slices.Repeat([]*tracepb.Span{span}, 256)...))); code != http.StatusOK {
		t.Fatalf("256-span batch status %d", code)
	}
	tooMany := slices.Repeat([]*tracepb.Span{span}, 257)
	if code, _ := postTrace(h, key["secret"].(string), tracePayload(t, traceBatch(tooMany...))); code != http.StatusBadRequest {
		t.Fatalf("257-span batch status %d", code)
	}
	attributesOverLimit := slices.Repeat([]*commonpb.KeyValue{traceText("gen_ai.operation.name", "chat")}, 33)
	if code, _ := postTrace(h, key["secret"].(string), tracePayload(t, traceBatch(&tracepb.Span{TraceId: bytes.Repeat([]byte{3}, 16), SpanId: bytes.Repeat([]byte{4}, 8), Attributes: attributesOverLimit}))); code != http.StatusBadRequest {
		t.Fatalf("33-attribute batch status %d", code)
	}
	if code, _ := postTrace(h, unscopedKey["secret"].(string), tracePayload(t, traceBatch())); code != http.StatusForbidden {
		t.Fatalf("projectless key status %d", code)
	}
	verifyTraceGroupConcurrency(t, h, limiter, concurrencyGroup["id"].(string), concurrencyKey["secret"].(string), otherConcurrencyKey["secret"].(string))
	if code, _ := postTrace(h, groupKey["secret"].(string), tracePayload(t, traceBatch())); code != http.StatusOK {
		t.Fatalf("first budget group request status %d", code)
	}
	if code, _ := postTrace(h, otherGroupKey["secret"].(string), tracePayload(t, traceBatch())); code != http.StatusTooManyRequests {
		t.Fatalf("budget group rate request status %d", code)
	}
	for i := 0; i < 3; i++ {
		code, _ := postTrace(h, limitedKey["secret"].(string), tracePayload(t, traceBatch()))
		if i < 2 && code != http.StatusOK || i == 2 && code != http.StatusTooManyRequests {
			t.Fatalf("key rate request %d returned %d", i+1, code)
		}
	}
	for i := 0; i < 60; i++ {
		code, _ := postTrace(h, otherKey["secret"].(string), tracePayload(t, traceBatch()))
		if i < 59 && code != http.StatusOK || i == 59 && code != http.StatusTooManyRequests {
			t.Fatalf("request %d returned %d", i+2, code)
		}
	}
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.settings(key,value,etag,updated_by) SELECT 'retention.requests_days','1',gen_random_uuid(),created_by FROM olp.api_keys WHERE id=$1 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, keyID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.trace_spans SET received_at=now()-interval '2 days' WHERE api_key_id=$1`, keyID); err != nil {
		t.Fatal(err)
	}
	if _, err := usage.RunMaintenance(t.Context(), h.Pool, time.Now()); err != nil {
		t.Fatal(err)
	}
	var recent int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE api_key_id=$1),count(*) FILTER (WHERE api_key_id=$2) FROM olp.trace_spans`, keyID, otherKey["id"]).Scan(&rows, &recent); err != nil || rows != 0 || recent != 2 {
		t.Fatalf("expired trace count %d, recent trace count %d: %v", rows, recent, err)
	}
}

func traceFinancialSnapshot(t *testing.T, h *accessHarness) string {
	t.Helper()
	var snapshot string
	if err := h.Pool.QueryRow(t.Context(), `SELECT jsonb_build_object(
		'debits', (SELECT jsonb_agg(to_jsonb(d) ORDER BY d.id) FROM olp.execution_debits d),
		'facts', (SELECT jsonb_agg(to_jsonb(f) ORDER BY f.attempt_id) FROM olp.attempt_usage_facts f),
		'key_windows', (SELECT jsonb_agg(to_jsonb(w) ORDER BY w.api_key_id,w.window_kind,w.window_id) FROM olp.api_key_cost_windows w),
		'supply_windows', (SELECT jsonb_agg(to_jsonb(w) ORDER BY w.owner_id,w.window_kind,w.window_id) FROM olp.supply_cost_windows w),
		'group_windows', (SELECT jsonb_agg(to_jsonb(w) ORDER BY w.budget_group_id,w.window_kind,w.window_id) FROM olp.budget_group_cost_windows w)
	)::text`).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func verifyWorkflowTraceCorrelation(t *testing.T, h *accessHarness, secret, keyID, runID string, limiter *limits.Limiter) {
	t.Helper()
	h.Gateway.Traces = &gateway.TraceDeps{Pool: h.Pool, Limiter: limiter, Auth: h.AuthKey}
	var attempt string
	if err := h.Pool.QueryRow(t.Context(), `SELECT id::text FROM olp.execution_debits WHERE run_id=$1 ORDER BY id LIMIT 1`, runID).Scan(&attempt); err != nil {
		t.Fatal(err)
	}
	traceID := bytes.Repeat([]byte{31}, 16)
	owned := &tracepb.Span{TraceId: traceID, SpanId: bytes.Repeat([]byte{1}, 8), StartTimeUnixNano: 200, EndTimeUnixNano: 200, Attributes: []*commonpb.KeyValue{traceText("olp.run_id", runID), traceText("olp.attempt_id", attempt), traceText("gen_ai.prompt", "workflow-private-span")}}
	missing := &tracepb.Span{TraceId: traceID, SpanId: bytes.Repeat([]byte{2}, 8), Attributes: []*commonpb.KeyValue{traceText("olp.run_id", runID), traceText("olp.attempt_id", uuid.NewString())}}
	runOnly := &tracepb.Span{TraceId: traceID, SpanId: bytes.Repeat([]byte{3}, 8), StartTimeUnixNano: 100, EndTimeUnixNano: 100, Attributes: []*commonpb.KeyValue{traceText("olp.run_id", runID)}}
	attemptOnly := &tracepb.Span{TraceId: traceID, SpanId: bytes.Repeat([]byte{4}, 8), StartTimeUnixNano: 200, EndTimeUnixNano: 200, Attributes: []*commonpb.KeyValue{traceText("olp.attempt_id", attempt)}}
	malformed := &tracepb.Span{TraceId: traceID, SpanId: bytes.Repeat([]byte{5}, 8), Attributes: []*commonpb.KeyValue{traceText("olp.run_id", runID), traceText("olp.attempt_id", "invalid")}}
	payload := tracePayload(t, traceBatch(owned, missing, runOnly, attemptOnly, malformed, owned))
	before := traceFinancialSnapshot(t, h)
	for range 2 {
		if status, body := postTrace(h, secret, payload); status != http.StatusOK {
			t.Fatalf("workflow trace upload status %d: %s", status, body)
		}
	}
	var total, validRun, validAttempt, uncorrelated int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*), count(*) FILTER (WHERE run_id=$2), count(*) FILTER (WHERE attempt_id=$3), count(*) FILTER (WHERE run_id IS NULL AND attempt_id IS NULL) FROM olp.trace_spans WHERE api_key_id=$1`, keyID, runID, attempt).Scan(&total, &validRun, &validAttempt, &uncorrelated); err != nil {
		t.Fatal(err)
	}
	if total != 5 || validRun != 3 || validAttempt != 2 || uncorrelated != 2 {
		t.Fatalf("workflow traces total=%d run=%d attempt=%d uncorrelated=%d", total, validRun, validAttempt, uncorrelated)
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.trace_spans SET id = CASE encode(span_id, 'hex')
 WHEN '0101010101010101' THEN '00000000-0000-0000-0000-000000000001'::uuid
 WHEN '0303030303030303' THEN '00000000-0000-0000-0000-000000000003'::uuid
 WHEN '0404040404040404' THEN '00000000-0000-0000-0000-000000000002'::uuid END
 WHERE run_id=$1`, runID); err != nil {
		t.Fatal(err)
	}
	if after := traceFinancialSnapshot(t, h); after != before {
		t.Fatalf("trace upload changed financial state: before=%s after=%s", before, after)
	}
}

type traceHTTPResult struct {
	status int
	err    error
}

func verifyTraceGroupConcurrency(t *testing.T, h *accessHarness, limiter *limits.Limiter, groupID, firstKey, secondKey string) {
	t.Helper()
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.HTTP.URL+"/v1/traces", reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-protobuf")
	request.Header.Set("Authorization", "Bearer "+firstKey)
	result := make(chan traceHTTPResult, 1)
	go func() {
		response, err := h.HTTP.Client().Do(request)
		if err != nil {
			result <- traceHTTPResult{err: err}
			return
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, response.Body)
		result <- traceHTTPResult{status: response.StatusCode}
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		usage, err := limiter.ProviderUsage(t.Context(), limits.BudgetGroupLookup(groupID))
		if err != nil {
			t.Fatal(err)
		}
		if usage.ConcurrentRequests == 1 {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("trace request did not acquire group concurrency")
		case <-ticker.C:
		}
	}
	if status, _ := postTrace(h, secondKey, tracePayload(t, traceBatch())); status != http.StatusTooManyRequests {
		t.Fatalf("concurrent group trace status %d", status)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case first := <-result:
		if first.err != nil || first.status != http.StatusOK {
			t.Fatalf("first group trace status %d: %v", first.status, first.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first group trace did not finish")
	}
	if status, _ := postTrace(h, secondKey, tracePayload(t, traceBatch())); status != http.StatusOK {
		t.Fatalf("group concurrency was not released: %d", status)
	}
}
