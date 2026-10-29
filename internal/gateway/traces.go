package gateway

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/consumerauth"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/secrets"
	collector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

const traceBodyLimit = 1 << 20
const traceSpanLimit = 256
const traceAttributeLimit = 32

type TraceDeps struct {
	Pool    *pgxpool.Pool
	Limiter *limits.Limiter
	Auth    *secrets.AuthKey
}

type traceAttributes struct {
	InputTokens  *int64 `json:"gen_ai.usage.input_tokens,omitempty"`
	OutputTokens *int64 `json:"gen_ai.usage.output_tokens,omitempty"`
	HTTPStatus   *int64 `json:"http.response.status_code,omitempty"`
}

type traceRecord struct {
	TraceID    string          `json:"trace_id"`
	SpanID     string          `json:"span_id"`
	ParentID   string          `json:"parent_id"`
	RunID      string          `json:"run_id"`
	AttemptID  string          `json:"attempt_id"`
	StartNanos uint64          `json:"started_nanos"`
	EndNanos   uint64          `json:"ended_nanos"`
	Category   string          `json:"category"`
	Status     int32           `json:"status"`
	Attributes traceAttributes `json:"attributes"`
}

func (s *Server) ingestTraces(w http.ResponseWriter, r *http.Request) {
	token, _ := consumerauth.Bearer(r.Header.Get("Authorization"))
	authority, failure := consumerauth.Authenticate(s.Runtime, r, token, "traces_write", s.cfg.TrustedProxies, time.Now())
	if failure != nil {
		writeTraceError(w, failure.Status, failure.Message)
		return
	}
	if authority.ProjectID == nil || !authority.AllowsResource("traces_write", "", *authority.ProjectID, time.Now()) {
		writeTraceError(w, http.StatusForbidden, "This API key cannot write traces for a project.")
		return
	}
	if s.Traces == nil || s.Traces.Pool == nil || s.Traces.Limiter == nil || s.Traces.Auth == nil {
		writeTraceError(w, http.StatusServiceUnavailable, "Trace ingestion is unavailable.")
		return
	}
	leases, err := s.Traces.reserve(r.Context(), authority)
	if err != nil {
		var exceeded *limits.ExceededError
		if errors.As(err, &exceeded) {
			writeTraceError(w, http.StatusTooManyRequests, "Trace ingestion rate limit exceeded.")
			return
		}
		writeTraceError(w, http.StatusServiceUnavailable, "Trace ingestion limits are unavailable.")
		return
	}
	defer releaseTraceLeases(r.Context(), leases)
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/x-protobuf" {
		writeTraceError(w, http.StatusUnsupportedMediaType, "Trace requests require OTLP protobuf.")
		return
	}
	body, failureStatus := readTraceBody(r.Body, r.Header.Get("Content-Encoding"))
	if failureStatus != 0 {
		writeTraceError(w, failureStatus, "Trace request encoding or size is invalid.")
		return
	}
	request := &collector.ExportTraceServiceRequest{}
	if err := (proto.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(body, request); err != nil {
		writeTraceError(w, http.StatusBadRequest, "The OTLP trace batch is invalid.")
		return
	}
	spans := traceSpans(request)
	if len(spans) > traceSpanLimit {
		writeTraceError(w, http.StatusBadRequest, "Trace batches are limited to 256 spans.")
		return
	}
	for _, span := range spans {
		if !validTraceSpan(span) {
			writeTraceError(w, http.StatusBadRequest, "A trace span has invalid identifiers or too many attributes.")
			return
		}
	}
	records := slices.Collect(func(yield func(traceRecord) bool) {
		for _, span := range spans {
			if !yield(makeTraceRecord(span)) {
				return
			}
		}
	})
	if len(records) > 0 {
		if err := s.Traces.persist(r.Context(), authority, records); err != nil {
			writeTraceError(w, http.StatusServiceUnavailable, "Trace ingestion is unavailable.")
			return
		}
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}

func traceErrorCode(httpStatus int) codes.Code {
	switch httpStatus {
	case http.StatusUnauthorized:
		return codes.Unauthenticated
	case http.StatusForbidden:
		return codes.PermissionDenied
	case http.StatusRequestEntityTooLarge, http.StatusTooManyRequests:
		return codes.ResourceExhausted
	case http.StatusServiceUnavailable:
		return codes.Unavailable
	default:
		return codes.InvalidArgument
	}
}

func writeTraceError(w http.ResponseWriter, httpStatus int, message string) {
	body, _ := proto.Marshal(&statuspb.Status{Code: int32(traceErrorCode(httpStatus)), Message: message})
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(httpStatus)
	_, _ = w.Write(body)
}

func readTraceBody(body io.Reader, encoding string) ([]byte, int) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return readTraceBytes(body)
	case "gzip":
		reader, err := gzip.NewReader(body)
		if err != nil {
			return nil, http.StatusBadRequest
		}
		defer reader.Close()
		return readTraceBytes(reader)
	default:
		return nil, http.StatusUnsupportedMediaType
	}
}

func readTraceBytes(reader io.Reader) ([]byte, int) {
	body, err := io.ReadAll(io.LimitReader(reader, traceBodyLimit+1))
	if err != nil {
		return nil, http.StatusBadRequest
	}
	if len(body) > traceBodyLimit {
		return nil, http.StatusRequestEntityTooLarge
	}
	return body, 0
}

func (d *TraceDeps) reserve(ctx context.Context, authority access.Authority) ([]*limits.Lease, error) {
	limit := int64(60)
	hash := sha256.Sum256([]byte(authority.ID))
	traceLease, err := d.Limiter.Reserve(ctx, limits.Request{CostOwnerID: authority.ID, LookupID: "tr_" + hex.EncodeToString(hash[:16]), RequestsPerMinute: &limit, LeaseTTL: time.Minute})
	if err != nil {
		return nil, err
	}
	keyLease, err := d.reserveTraceKey(ctx, authority)
	if err != nil {
		_ = traceLease.Refund(ctx)
		return nil, err
	}
	groupLease, err := d.reserveTraceGroup(ctx, authority)
	if err != nil {
		_ = keyLease.Refund(ctx)
		_ = traceLease.Refund(ctx)
		return nil, err
	}
	return []*limits.Lease{traceLease, keyLease, groupLease}, nil
}

func (d *TraceDeps) reserveTraceKey(ctx context.Context, authority access.Authority) (*limits.Lease, error) {
	if authority.Policy.RequestsPerMinute == nil && authority.Policy.MaxConcurrency == nil {
		return nil, nil
	}
	return d.Limiter.Reserve(ctx, limits.Request{CostOwnerID: authority.ID, LookupID: authority.LimitsLookup(), RequestsPerMinute: authority.Policy.RequestsPerMinute, MaxConcurrency: authority.Policy.MaxConcurrency, LeaseTTL: time.Minute})
}

func (d *TraceDeps) reserveTraceGroup(ctx context.Context, authority access.Authority) (*limits.Lease, error) {
	if authority.BudgetGroupID == nil || authority.BudgetGroupRPM == nil && authority.BudgetGroupConcurrency == nil {
		return nil, nil
	}
	return d.Limiter.Reserve(ctx, limits.Request{CostOwnerID: *authority.BudgetGroupID, LookupID: limits.BudgetGroupLookup(*authority.BudgetGroupID), RequestsPerMinute: authority.BudgetGroupRPM, MaxConcurrency: authority.BudgetGroupConcurrency, LeaseTTL: time.Minute})
}

func releaseTraceLeases(ctx context.Context, leases []*limits.Lease) {
	for _, lease := range leases {
		_ = lease.Release(ctx)
	}
}

func traceSpans(request *collector.ExportTraceServiceRequest) []*tracepb.Span {
	return slices.Collect(func(yield func(*tracepb.Span) bool) {
		for _, resource := range request.GetResourceSpans() {
			for _, scope := range resource.GetScopeSpans() {
				for _, span := range scope.GetSpans() {
					if !yield(span) {
						return
					}
				}
			}
		}
	})
}

func traceAttributeAllowed(key string) bool {
	return slices.Contains([]string{"olp.run_id", "olp.attempt_id", "gen_ai.operation.name", "gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens", "http.response.status_code"}, key)
}

func validTraceSpan(span *tracepb.Span) bool {
	if span == nil || len(span.TraceId) != 16 || len(span.SpanId) != 8 || !slices.ContainsFunc(span.TraceId, func(b byte) bool { return b != 0 }) || !slices.ContainsFunc(span.SpanId, func(b byte) bool { return b != 0 }) || len(span.ParentSpanId) != 0 && len(span.ParentSpanId) != 8 || span.EndTimeUnixNano < span.StartTimeUnixNano {
		return false
	}
	allowed := slices.Collect(func(yield func(*commonpb.KeyValue) bool) {
		for _, attribute := range span.Attributes {
			if attribute != nil && traceAttributeAllowed(attribute.Key) && !yield(attribute) {
				return
			}
		}
	})
	if len(allowed) > traceAttributeLimit {
		return false
	}
	for _, attribute := range allowed {
		if len(attribute.GetValue().GetStringValue()) > 256 {
			return false
		}
	}
	return true
}

func traceString(attributes []*commonpb.KeyValue, key string) string {
	for _, attribute := range attributes {
		if attribute != nil && attribute.Key == key {
			return attribute.GetValue().GetStringValue()
		}
	}
	return ""
}

func traceID(attributes []*commonpb.KeyValue, key string) string {
	if !slices.ContainsFunc(attributes, func(attribute *commonpb.KeyValue) bool { return attribute != nil && attribute.Key == key }) {
		return ""
	}
	parsed, err := uuid.Parse(traceString(attributes, key))
	if err != nil {
		return uuid.Nil.String()
	}
	return parsed.String()
}

func traceInteger(attributes []*commonpb.KeyValue, key string) *int64 {
	for _, attribute := range attributes {
		if attribute == nil || attribute.Key != key || attribute.Value == nil {
			continue
		}
		value, ok := attribute.Value.Value.(*commonpb.AnyValue_IntValue)
		if !ok || value.IntValue < 0 || key == "http.response.status_code" && (value.IntValue < 100 || value.IntValue > 599) {
			return nil
		}
		return &value.IntValue
	}
	return nil
}

func traceCategory(attributes []*commonpb.KeyValue) string {
	switch traceString(attributes, "gen_ai.operation.name") {
	case "chat", "embeddings", "text_completion", "execute_tool", "invoke_agent", "create_agent":
		return traceString(attributes, "gen_ai.operation.name")
	default:
		return "other"
	}
}

func traceStatus(span *tracepb.Span) int32 {
	status := int32(span.GetStatus().GetCode())
	if status < 0 || status > 2 {
		return 0
	}
	return status
}

func makeTraceRecord(span *tracepb.Span) traceRecord {
	return traceRecord{
		TraceID: hex.EncodeToString(span.TraceId), SpanID: hex.EncodeToString(span.SpanId), ParentID: hex.EncodeToString(span.ParentSpanId),
		RunID: traceID(span.Attributes, "olp.run_id"), AttemptID: traceID(span.Attributes, "olp.attempt_id"),
		StartNanos: span.StartTimeUnixNano, EndNanos: span.EndTimeUnixNano, Category: traceCategory(span.Attributes), Status: traceStatus(span),
		Attributes: traceAttributes{InputTokens: traceInteger(span.Attributes, "gen_ai.usage.input_tokens"), OutputTokens: traceInteger(span.Attributes, "gen_ai.usage.output_tokens"), HTTPStatus: traceInteger(span.Attributes, "http.response.status_code")},
	}
}

const insertTraceSpansSQL = `INSERT INTO olp.trace_spans(project_id,api_key_id,principal_digest,trace_id,span_id,parent_span_id,run_id,attempt_id,started_nanos,ended_nanos,category,status,attributes)
SELECT $1::uuid,$2::uuid,$3,decode(v.trace_id,'hex'),decode(v.span_id,'hex'),decode(NULLIF(v.parent_id,''),'hex'),
 CASE WHEN r.id IS NOT NULL AND (v.attempt_id='' OR sr.id=r.id) THEN r.id WHEN v.run_id='' AND sr.id IS NOT NULL THEN sr.id ELSE NULL END,
 CASE WHEN sr.id IS NOT NULL AND (v.run_id='' OR r.id=sr.id) THEN s.dispatch_id ELSE NULL END,
 v.started_nanos,v.ended_nanos,v.category,v.status,v.attributes
FROM jsonb_to_recordset($4::jsonb) AS v(trace_id text,span_id text,parent_id text,run_id text,attempt_id text,started_nanos numeric,ended_nanos numeric,category text,status smallint,attributes jsonb)
LEFT JOIN olp.agent_runs r ON r.id=NULLIF(v.run_id,'')::uuid AND r.project_id=$1 AND r.api_key_id=$2 AND r.principal_digest=$3
LEFT JOIN olp.agent_steps s ON s.dispatch_id=NULLIF(v.attempt_id,'')::uuid
LEFT JOIN olp.agent_runs sr ON sr.id=s.run_id AND sr.project_id=$1 AND sr.api_key_id=$2 AND sr.principal_digest=$3
ON CONFLICT(project_id,api_key_id,principal_digest,trace_id,span_id) DO NOTHING`

func (d *TraceDeps) persist(ctx context.Context, authority access.Authority, records []traceRecord) error {
	payload, err := json.Marshal(records)
	if err != nil {
		return err
	}
	_, err = d.Pool.Exec(ctx, insertTraceSpansSQL, *authority.ProjectID, authority.ID, agents.PrincipalDigest(d.Auth, authority), string(payload))
	return err
}
