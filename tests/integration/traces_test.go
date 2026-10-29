//go:build integration

package integration_test

import (
	"bytes"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/gateway"
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
	unscopedKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "projectless trace writer", "scopes": []string{"traces_write"}}, idem("trace-projectless-key"), http.StatusCreated)
	toolset := h.want(owner, http.MethodPost, "/api/v1/toolsets", map[string]any{"project_id": project, "name": "trace fixture", "toolset": map[string]any{"tools": []any{}}}, idem("trace-toolset"), http.StatusCreated)
	h.want(owner, http.MethodPost, "/api/v1/toolsets/"+toolset["id"].(string)+"/publish", nil, withMatch(toolset, idem("trace-publish")), http.StatusOK)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	authority, err := h.Runtime.Authenticate(key["secret"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var revision string
	if err := h.Pool.QueryRow(t.Context(), `SELECT latest_revision_id::text FROM olp.agent_publications WHERE id=$1`, toolset["id"]).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	runID, attemptID, stepID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	tx, err := h.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO olp.agent_runs(id,api_key_id,project_id,revision_id,principal_digest) VALUES($1,$2,$3,$4,$5)`, runID, authority.ID, project, revision, agents.PrincipalDigest(h.AuthKey, *authority)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO olp.execution_debits(id,api_key_id,project_id,run_id,step_id,kind,affinity,intent_digest,claim_id,currency,cost_bound,charge_bound,prepared_until) VALUES($1,$2,$3,$4,$5,'model','{}',decode(repeat('00',32),'hex'),$6,'USD',0,0,now()+interval '1 hour')`, attemptID, authority.ID, project, runID, stepID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO olp.agent_steps(id,run_id,dispatch_id,ordinal) VALUES($1,$2,$3,0)`, stepID, runID, attemptID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	span := &tracepb.Span{TraceId: bytes.Repeat([]byte{1}, 16), SpanId: bytes.Repeat([]byte{2}, 8), Name: "private-span-content", StartTimeUnixNano: 1, EndTimeUnixNano: 2,
		Attributes: []*commonpb.KeyValue{traceText("olp.run_id", runID), traceText("olp.attempt_id", attemptID), traceText("gen_ai.operation.name", "chat"), traceText("gen_ai.prompt", "private-prompt-content"), {Key: "gen_ai.usage.input_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 12}}}},
		Events:     []*tracepb.Span_Event{{Name: "private-event-content"}}, Links: []*tracepb.Span_Link{{Attributes: []*commonpb.KeyValue{traceText("secret", "private-link-content")}}}, Status: &tracepb.Status{Message: "private-status-content", Code: tracepb.Status_STATUS_CODE_OK}}
	payload := protowire.AppendString(protowire.AppendTag(tracePayload(t, traceBatch(span)), 120, protowire.BytesType), "private-unknown-content")
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
	if rows != 3 || correlated != 1 || leaked != 0 || debits != 1 {
		t.Fatalf("trace rows=%d owned=%d private=%d debits=%d", rows, correlated, leaked, debits)
	}
	var category, attributes string
	if err := h.Pool.QueryRow(t.Context(), `SELECT category,attributes::text FROM olp.trace_spans WHERE api_key_id=$1`, authority.ID).Scan(&category, &attributes); err != nil {
		t.Fatal(err)
	}
	if category != "chat" || !strings.Contains(attributes, "gen_ai.usage.input_tokens") {
		t.Fatalf("safe metadata missing: category=%q attributes=%s", category, attributes)
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
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.settings(key,value,etag,updated_by) SELECT 'retention.requests_days','1',gen_random_uuid(),created_by FROM olp.api_keys WHERE id=$1 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, authority.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.trace_spans SET received_at=now()-interval '2 days' WHERE api_key_id=$1`, authority.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := usage.RunMaintenance(t.Context(), h.Pool, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.trace_spans WHERE api_key_id=$1`, authority.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("expired trace count %d: %v", rows, err)
	}
}
