//go:build integration

package integration_test

import (
	"bytes"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/gateway/traceingest"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/tests/harness"
	collector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

const traceSecret = "olp_trace_writer"

type traceKeys map[string]keyauthority.Authority

func (k traceKeys) Authenticate(secret string) (*keyauthority.Authority, error) {
	authority, ok := k[secret]
	if !ok {
		return nil, release.ErrInvalidKey
	}
	return &authority, nil
}

type storedSpan struct {
	trace, span, parent, category string
	status                        int
	attributes                    int
	inputTokens, outputTokens     int64
	upstreamStatus                int64
}

func seedTraceWriter(t *testing.T, pool *pgxpool.Pool, requestsPerMinute *int64) keyauthority.Authority {
	t.Helper()
	user, project, key := uuid.NewString(), uuid.NewString(), uuid.NewString()
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.users(id, email, display_name, role, etag) VALUES($1, $2, 'Tracer', 'owner', $3)`, []any{user, user + "@example.invalid", uuid.NewString()}},
		{`INSERT INTO olp.projects(id, name, etag, created_by) VALUES($1::uuid, $1::text, $2, $3)`, []any{project, uuid.NewString(), user}},
		{`INSERT INTO olp.api_keys(id, lookup_id, digest, name, created_by, policy, etag, project_id) VALUES($1::uuid, $1::text, '\x00', 'tracer', $2, '{"scopes":["traces_write"]}', $3, $4)`, []any{key, user, uuid.NewString(), project}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	return keyauthority.Authority{
		ID: key, LookupID: strings.ReplaceAll(key, "-", ""), ProjectID: &project,
		Policy: keyauthority.KeyPolicy{Scopes: []string{"traces_write"}, RequestsPerMinute: requestsPerMinute},
	}
}

func traceIngester(t *testing.T, pool *pgxpool.Pool, authority keyauthority.Authority) http.Handler {
	t.Helper()
	store := &traceingest.Store{
		Pool:    pool,
		Limiter: admission.New(newRunner(t)),
		Auth:    secrets.NewAuthKey([]byte(strings.Repeat("k", 32)), uuid.NewString()),
	}
	return traceingest.New(traceKeys{traceSecret: authority}, nil, store)
}

func traceBatch(t *testing.T) []byte {
	t.Helper()
	integer := func(key string, value int64) *commonpb.KeyValue {
		return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: value}}}
	}
	text := func(key, value string) *commonpb.KeyValue {
		return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
	}
	trace := bytes.Repeat([]byte{0xab}, 16)
	root, child := bytes.Repeat([]byte{0x01}, 8), bytes.Repeat([]byte{0x02}, 8)
	body, err := proto.Marshal(&collector.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{
		{
			TraceId: trace, SpanId: root, StartTimeUnixNano: 10, EndTimeUnixNano: 40,
			Attributes: []*commonpb.KeyValue{text("gen_ai.operation.name", "invoke_agent"), text("user.prompt", "never stored")},
		},
		{
			TraceId: trace, SpanId: child, ParentSpanId: root, StartTimeUnixNano: 20, EndTimeUnixNano: 30,
			Status: &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR},
			Attributes: []*commonpb.KeyValue{
				text("gen_ai.operation.name", "chat"), integer("gen_ai.usage.input_tokens", 11),
				integer("gen_ai.usage.output_tokens", 7), integer("http.response.status_code", 502),
			},
		},
	}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func postTraces(t *testing.T, ingester http.Handler, secret string, body []byte) int {
	t.Helper()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/traces", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+secret)
	request.Header.Set("Content-Type", "application/x-protobuf")
	recorder := httptest.NewRecorder()
	ingester.ServeHTTP(recorder, request)
	return recorder.Code
}

func storedSpans(t *testing.T, pool *pgxpool.Pool, authority keyauthority.Authority) []storedSpan {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT encode(trace_id,'hex'), encode(span_id,'hex'), COALESCE(encode(parent_span_id,'hex'),''), category, status,
 (SELECT count(*) FROM jsonb_object_keys(attributes)), COALESCE((attributes->>'gen_ai.usage.input_tokens')::bigint,0),
 COALESCE((attributes->>'gen_ai.usage.output_tokens')::bigint,0), COALESCE((attributes->>'http.response.status_code')::bigint,0)
FROM olp.trace_spans WHERE project_id=$1 AND api_key_id=$2 ORDER BY started_nanos`, *authority.ProjectID, authority.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	spans := []storedSpan{}
	for rows.Next() {
		var span storedSpan
		if err := rows.Scan(&span.trace, &span.span, &span.parent, &span.category, &span.status, &span.attributes, &span.inputTokens, &span.outputTokens, &span.upstreamStatus); err != nil {
			t.Fatal(err)
		}
		spans = append(spans, span)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return spans
}

func TestTraceIngestStoresABatchOnceForTheWritingKey(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	authority := seedTraceWriter(t, pool, nil)
	ingester := traceIngester(t, pool, authority)
	batch := traceBatch(t)

	for range 2 {
		if status := postTraces(t, ingester, traceSecret, batch); status != http.StatusOK {
			t.Fatalf("status = %d, want an accepted batch", status)
		}
	}
	if status := postTraces(t, ingester, "olp_unknown", batch); status != http.StatusUnauthorized {
		t.Fatalf("status = %d for an unknown key, want 401", status)
	}

	trace := hex.EncodeToString(bytes.Repeat([]byte{0xab}, 16))
	root, child := hex.EncodeToString(bytes.Repeat([]byte{0x01}, 8)), hex.EncodeToString(bytes.Repeat([]byte{0x02}, 8))
	want := []storedSpan{
		{trace: trace, span: root, category: "invoke_agent"},
		{trace: trace, span: child, parent: root, category: "chat", status: 2, attributes: 3, inputTokens: 11, outputTokens: 7, upstreamStatus: 502},
	}
	got := storedSpans(t, pool, authority)
	if len(got) != len(want) {
		t.Fatalf("stored %d spans, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("span %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestTraceIngestHonorsTheKeyRequestRate(t *testing.T) {
	t.Parallel()
	awayFromMinuteRollover()
	pool, _ := harness.Database(t)
	authority := seedTraceWriter(t, pool, new(int64(1)))
	ingester := traceIngester(t, pool, authority)

	if status := postTraces(t, ingester, traceSecret, traceBatch(t)); status != http.StatusOK {
		t.Fatalf("status = %d, want the first batch accepted", status)
	}
	if status := postTraces(t, ingester, traceSecret, traceBatch(t)); status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the second batch rate limited", status)
	}
	if got := len(storedSpans(t, pool, authority)); got != 2 {
		t.Fatalf("stored %d spans, want only the first batch", got)
	}
}
