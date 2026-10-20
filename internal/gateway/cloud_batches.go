package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

var cloudBatchDisplayName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type cloudBatchRequest struct {
	InputURI        string
	OutputURIPrefix string
	DisplayName     string
	TimeoutHours    int
}

func validateCloudBatchRequest(raw []byte, dialect string, storage *connectors.BatchStorage) (cloudBatchRequest, *Error) {
	doc, err := parseObjectRequest(raw, oif.Limits{MaxBytes: 1 << 20})
	if err != nil || doc.Root().Kind() != oif.Object {
		return cloudBatchRequest{}, invalidRequest("invalid_json", "The batch-job body must be one JSON object.", nil)
	}
	allowed := map[string]bool{"input_uri": true, "output_uri_prefix": true, "display_name": true, "timeout_hours": true}
	for _, field := range doc.Root().Members() {
		if !allowed[field.Name] {
			return cloudBatchRequest{}, invalidRequest("unsupported_parameter", "This batch-job parameter is not supported.", strPtr(field.Name))
		}
	}
	var input cloudBatchRequest
	var present bool
	if value, ok := doc.Root().Lookup("input_uri"); ok {
		input.InputURI, present = value.Text()
	}
	if !present || input.InputURI == "" {
		return cloudBatchRequest{}, invalidRequest("missing_required_parameter", "input_uri is required.", strPtr("input_uri"))
	}
	if value, ok := doc.Root().Lookup("output_uri_prefix"); ok {
		input.OutputURIPrefix, present = value.Text()
	}
	if !present || input.OutputURIPrefix == "" {
		return cloudBatchRequest{}, invalidRequest("missing_required_parameter", "output_uri_prefix is required.", strPtr("output_uri_prefix"))
	}
	if storage == nil || !storage.Contains(storage.InputURIPrefix, input.InputURI) ||
		!storage.Contains(storage.OutputURIPrefix, input.OutputURIPrefix) {
		return cloudBatchRequest{}, invalidRequest("storage_scope", "The input or output URI is outside the configured batch storage prefixes.", nil)
	}
	if value, ok := doc.Root().Lookup("display_name"); ok {
		input.DisplayName, present = value.Text()
		if !present || !cloudBatchDisplayName.MatchString(input.DisplayName) {
			return cloudBatchRequest{}, invalidRequest("invalid_value", "display_name must use 1–128 letters, digits, underscores, or hyphens.", strPtr("display_name"))
		}
	}
	if value, ok := doc.Root().Lookup("timeout_hours"); ok {
		if dialect != "bedrock-batch-inference" {
			return cloudBatchRequest{}, invalidRequest("unsupported_parameter", "timeout_hours is only supported by Bedrock batch inference.", strPtr("timeout_hours"))
		}
		if value.Kind() != oif.Number {
			return cloudBatchRequest{}, invalidRequest("invalid_value", "timeout_hours must be an integer from 24 to 168.", strPtr("timeout_hours"))
		}
		parsed, parseErr := strconv.Atoi(value.Raw())
		if parseErr != nil || parsed < 24 || parsed > 168 {
			return cloudBatchRequest{}, invalidRequest("invalid_value", "timeout_hours must be an integer from 24 to 168.", strPtr("timeout_hours"))
		}
		input.TimeoutHours = parsed
	}
	return input, nil
}

func cloudBatchStatus(dialect, state string) (string, bool) {
	vertex := map[string]string{
		"JOB_STATE_QUEUED": "queued", "JOB_STATE_PENDING": "queued", "JOB_STATE_RUNNING": "running",
		"JOB_STATE_SUCCEEDED": "succeeded", "JOB_STATE_PARTIALLY_SUCCEEDED": "partially_succeeded",
		"JOB_STATE_FAILED": "failed", "JOB_STATE_CANCELLING": "cancelling", "JOB_STATE_CANCELLED": "cancelled",
		"JOB_STATE_EXPIRED": "expired", "JOB_STATE_PAUSED": "running", "JOB_STATE_UPDATING": "running",
	}
	bedrock := map[string]string{
		"Submitted": "queued", "Validating": "queued", "Scheduled": "queued", "InProgress": "running",
		"Completed": "succeeded", "PartiallyCompleted": "partially_succeeded", "Failed": "failed",
		"Stopping": "cancelling", "Stopped": "cancelled", "Expired": "expired",
	}
	status := vertex[state]
	if dialect == "bedrock-batch-inference" {
		status = bedrock[state]
	}
	return status, status != ""
}

func projectCloudBatchJob(body []byte, localID, dialect, model, inputURI, outputPrefix string) ([]byte, error) {
	var job map[string]json.RawMessage
	if json.Unmarshal(body, &job) != nil {
		return nil, errors.New("malformed cloud batch job")
	}
	state := ""
	for _, key := range []string{"state", "jobStatus", "status"} {
		if json.Unmarshal(job[key], &state) == nil && state != "" {
			break
		}
	}
	status, ok := cloudBatchStatus(dialect, state)
	if !ok {
		return nil, errors.New("unknown cloud batch status")
	}
	created := job["createTime"]
	if dialect == "bedrock-batch-inference" {
		created = job["creationTime"]
	}
	createdAt, _ := cloudBatchTime(created)
	var completed *int64
	for _, key := range []string{"endTime", "lastModifiedTime"} {
		if value, ok := cloudBatchTime(job[key]); ok && isCloudBatchTerminal(status) {
			completed = &value
			break
		}
	}
	result := map[string]any{
		"id": localID, "object": "batch_job", "dialect": dialect, "model": model,
		"status": status, "input_uri": inputURI, "output_uri_prefix": outputPrefix,
		"created_at": createdAt, "completed_at": completed, "request_counts": cloudBatchCounts(job, dialect),
		"error": cloudBatchError(job, dialect),
	}
	return json.Marshal(result)
}

func cloudBatchTime(raw json.RawMessage) (int64, bool) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		at, err := time.Parse(time.RFC3339Nano, text)
		return at.Unix(), err == nil
	}
	var n int64
	err := json.Unmarshal(raw, &n)
	return n, err == nil && n >= 0
}

func isCloudBatchTerminal(status string) bool {
	return status == "succeeded" || status == "partially_succeeded" || status == "failed" || status == "cancelled" || status == "expired"
}

func cloudBatchCounts(job map[string]json.RawMessage, dialect string) map[string]int64 {
	field := "completionStats"
	if dialect == "bedrock-batch-inference" {
		field = "jobStatistics"
	}
	var stats map[string]json.RawMessage
	_ = json.Unmarshal(job[field], &stats)
	if stats == nil {
		return nil
	}
	names := map[string]string{"successfulCount": "successful", "failedCount": "failed", "incompleteCount": "incomplete"}
	if dialect == "bedrock-batch-inference" {
		names = map[string]string{"totalRecordCount": "total", "successRecordCount": "successful", "errorRecordCount": "failed", "processedRecordCount": "processed"}
	}
	out := map[string]int64{}
	for upstream, target := range names {
		if n, ok := cloudCount(stats[upstream]); ok {
			out[target] = n
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cloudCount(raw json.RawMessage) (int64, bool) {
	var n int64
	if json.Unmarshal(raw, &n) == nil && n >= 0 {
		return n, true
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		value, err := strconv.ParseInt(text, 10, 64)
		return value, err == nil && value >= 0
	}
	return 0, false
}

func cloudBatchError(job map[string]json.RawMessage, dialect string) any {
	if dialect == "bedrock-batch-inference" {
		var message string
		_ = json.Unmarshal(job["message"], &message)
		if message == "" {
			return nil
		}
		return map[string]string{"code": "provider_error", "message": sanitizeCloudBatchMessage(message)}
	}
	var jobError map[string]json.RawMessage
	if json.Unmarshal(job["error"], &jobError) != nil || jobError == nil {
		return nil
	}
	var message, code string
	_ = json.Unmarshal(jobError["message"], &message)
	if json.Unmarshal(jobError["code"], &code) != nil {
		var number json.Number
		if json.Unmarshal(jobError["code"], &number) == nil {
			code = number.String()
		}
	}
	if message == "" {
		return nil
	}
	if code == "" {
		code = "provider_error"
	}
	return map[string]string{"code": code, "message": sanitizeCloudBatchMessage(message)}
}

func sanitizeCloudBatchMessage(message string) string {
	arnPattern := regexp.MustCompile(`arn:[A-Za-z0-9-:]+:[^ \t\r\n"]+`)
	message = arnPattern.ReplaceAllString(message, "[provider resource]")
	jobPattern := regexp.MustCompile(`projects/[^ \t\r\n"]+/batchPredictionJobs/[^ \t\r\n"]+`)
	return jobPattern.ReplaceAllString(message, "[provider resource]")
}

func (s *Server) registerCloudBatchState(mux *http.ServeMux) {
	mux.HandleFunc("POST /native/{dialect}/batch-jobs", s.createCloudBatch)
	mux.HandleFunc("GET /native/{dialect}/batch-jobs", s.listCloudBatches)
	mux.HandleFunc("GET /native/{dialect}/batch-jobs/{id}", s.getCloudBatch)
	mux.HandleFunc("POST /native/{dialect}/batch-jobs/{id}/cancel", s.cancelCloudBatch)
}

func (s *Server) cloudBatchFail(x *call, w http.ResponseWriter, e *Error) {
	x.failure = e
	s.finish(x, nil, e.Status)
	writeSurfaceError(w, e, "openai")
}

func (s *Server) createCloudBatch(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.stateBegin(w, r, wire.CloudBatch)
	if done {
		return
	}
	defer s.release(r.Context())
	dialect := r.PathValue("dialect")
	slug := strings.TrimSpace(r.Header.Get(stateRouteHeader))
	if slug == "" {
		s.cloudBatchFail(x, w, invalidRequest("missing_required_parameter", "X-OLP-Route must name a batch-enabled route.", nil))
		return
	}
	snapshot := x.request.release.Snapshot
	route, ok := snapshot.Routes[slug]
	if !ok || !slices.Contains(route.Operations, "batch") {
		s.cloudBatchFail(x, w, notFoundError("not_found", "No matching batch route exists."))
		return
	}
	if !authority.Allows("inference", route.Slug, route.ProjectID, s.now()) {
		s.cloudBatchFail(x, w, permissionError("route_forbidden", "This API key is not allowed to use the model `"+route.Slug+"`."))
		return
	}
	x.route = &route
	p, e := s.selectPinSurface(r.Context(), x, &route, "batch", "native", "unary", func(provider *routing.Provider, model string) bool {
		cfg := provider.Connector()
		return cfg.Supports("batch", "native", "unary") && cfg.BatchStorage != nil
	})
	if e != nil {
		s.cloudBatchFail(x, w, e)
		return
	}
	if p.provider.ProfileID != dialect || dialect != "vertex-batch-prediction" && dialect != "bedrock-batch-inference" {
		s.releaseHold(r.Context(), p.hold)
		s.cloudBatchFail(x, w, notFoundError("not_found", "No matching cloud batch dialect is available."))
		return
	}
	if x.strict() {
		s.releaseHold(r.Context(), p.hold)
		s.cloudBatchFail(x, w, policyUnavailable("target_capability", "Cloud batch jobs require a transformed route."))
		return
	}
	body, e := s.readBody(r)
	if e != nil {
		s.releaseHold(r.Context(), p.hold)
		s.cloudBatchFail(x, w, e)
		return
	}
	input, failure := validateCloudBatchRequest(body, dialect, p.provider.BatchStorage)
	if failure != nil {
		s.releaseHold(r.Context(), p.hold)
		s.cloudBatchFail(x, w, failure)
		return
	}
	if err := s.unboundedPinLimits(x.admissionAuthority(authority), p, &route); err != nil {
		s.releaseHold(r.Context(), p.hold)
		s.cloudBatchFail(x, w, err)
		return
	}
	if err := x.guardrailSurfaceGate(&route); err != nil {
		s.releaseHold(r.Context(), p.hold)
		s.cloudBatchFail(x, w, err)
		return
	}
	ctx, cancel := s.stateDeadline(r.Context(), &route)
	defer cancel()
	defer s.resourceSettle(ctx, x, p)
	if err := s.reserveState(ctx, x, authority, time.Duration(route.OverallTimeout)*time.Millisecond); err != nil {
		s.cloudBatchFail(x, w, err)
		return
	}
	if p.hold == nil {
		gate := s.gateSlot(ctx, &p.provider, &p.slot, resourceEstimate, s.now().Add(time.Duration(route.OverallTimeout)*time.Millisecond), "")
		if gate.verdict != gateAdmitted {
			s.cloudBatchFail(x, w, gateError(gate))
			return
		}
		p.hold = gate.hold
	}
	source, err := parseObjectRequest(body, oif.Limits{MaxBytes: int(s.cfg.MaxBodyBytes)})
	if err != nil {
		s.cloudBatchFail(x, w, invalidRequest("invalid_json", "The request body must be one JSON object.", nil))
		return
	}
	window := 24 * time.Hour
	if input.TimeoutHours > 0 {
		window = time.Duration(input.TimeoutHours) * time.Hour
	}
	resourceID := uuid.Must(uuid.NewV7())
	hexID := strings.ReplaceAll(resourceID.String(), "-", "")
	display := input.DisplayName
	if display == "" {
		display = "olp-" + hexID
	}
	var requestBody []byte
	var endpoint string
	var cursor json.RawMessage
	if dialect == "vertex-batch-prediction" {
		endpoint, err = p.provider.Connector().VertexBatchJobsURL("", "")
		requestBody, _ = json.Marshal(map[string]any{
			"displayName": display, "model": "publishers/google/models/" + p.model,
			"inputConfig":  map[string]any{"instancesFormat": "jsonl", "gcsSource": map[string]any{"uris": []string{input.InputURI}}},
			"outputConfig": map[string]any{"predictionsFormat": "jsonl", "gcsDestination": map[string]string{"outputUriPrefix": input.OutputURIPrefix}},
			"labels":       map[string]string{"olp_submission": hexID},
		})
		cursor = mustJSON(map[string]any{"submission": map[string]string{"vertex_label": hexID}})
	} else {
		endpoint, err = p.provider.Connector().BedrockBatchJobsURL("", "create")
		requestBody, _ = json.Marshal(map[string]any{
			"jobName": display, "roleArn": p.provider.BatchStorage.RoleARN, "modelId": p.model,
			"inputDataConfig":        map[string]any{"s3InputDataConfig": map[string]any{"s3Uri": input.InputURI, "s3InputFormat": "JSONL"}},
			"outputDataConfig":       map[string]any{"s3OutputDataConfig": map[string]string{"s3Uri": input.OutputURIPrefix}},
			"timeoutDurationInHours": input.TimeoutHours, "clientRequestToken": resourceID.String(),
		})
		if input.TimeoutHours == 0 {
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(requestBody, &fields)
			delete(fields, "timeoutDurationInHours")
			requestBody, _ = json.Marshal(fields)
		}
		cursor = mustJSON(map[string]any{"submission": map[string]any{"bedrock_client_request_token": resourceID.String(), "create_body": json.RawMessage(requestBody)}})
	}
	if err != nil {
		s.cloudBatchFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The cloud batch control-plane URL could not be resolved."))
		return
	}
	submission, e := s.prepareBatchSubmissionWithID(ctx, x, authority.ID, p, source, source.Bytes(), requestBody, oif.ServingIdentity{},
		"", "", resources.KindCloudBatch, map[string]string{"vertex-batch-prediction": "vertex", "bedrock-batch-inference": "bedrock"}[dialect],
		"native", "generation", window, nil, cursor, resourceID)
	if e != nil {
		s.cloudBatchFail(x, w, e)
		return
	}
	response, attemptFailure := s.pinnedDo(ctx, x, p, http.MethodPost, endpoint, requestBody, "application/json")
	if attemptFailure != nil {
		if attemptFailure.status == 0 && attemptFailure.dispatched {
			unknownCtx, stop := resourceCommitContext(r.Context())
			defer stop()
			_ = s.Batches.MarkUnknown(unknownCtx, resourceID, "submission_unknown", s.now())
			s.cloudBatchFail(x, w, serverError(http.StatusBadGateway, "submission_unknown", "The cloud provider may have accepted the batch job."))
			return
		}
		_ = s.Batches.DeleteSubmission(r.Context(), resourceID)
		s.cloudBatchFail(x, w, upstreamError(attemptFailure))
		return
	}
	defer response.Body.Close()
	x.dispatched = true
	result, err := readBounded(response.Body, s.cfg.MaxResponseBytes)
	if err != nil {
		s.cloudBatchFail(x, w, s.batchSubmissionUnknown(r.Context(), resourceID, "The cloud batch response could not be read; the job may have been accepted."))
		return
	}
	upstreamID := ""
	if dialect == "vertex-batch-prediction" {
		var v struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(result, &v) == nil {
			upstreamID = v.Name[strings.LastIndex(v.Name, "/")+1:]
		}
	} else {
		var b struct {
			ARN string `json:"jobArn"`
		}
		if json.Unmarshal(result, &b) == nil {
			upstreamID = b.ARN
		}
	}
	if upstreamID == "" {
		unknownCtx, stop := resourceCommitContext(r.Context())
		defer stop()
		_ = s.Batches.MarkUnknown(unknownCtx, resourceID, "submission_unknown", s.now())
		s.cloudBatchFail(x, w, serverError(http.StatusBadGateway, "submission_unknown", "The cloud provider returned no job identity."))
		return
	}
	if dialect == "bedrock-batch-inference" {
		// CreateModelInvocationJob acknowledges acceptance with only jobArn.
		// The worker obtains the authoritative lifecycle status on its next poll.
		result, _ = json.Marshal(map[string]any{"jobArn": upstreamID, "status": "Submitted", "creationTime": s.now().UTC()})
	}
	meta := map[string]any{
		"adapter": submission.settlement.Adapter, "dialect": dialect, "route_model": slug, "upstream_model": p.model,
		"input_uri": input.InputURI, "output_uri_prefix": input.OutputURIPrefix,
		"provider_job": json.RawMessage(result),
	}
	metadata, _ := json.Marshal(meta)
	projection, err := projectCloudBatchJob(result, resources.LocalID(resources.KindCloudBatch, resourceID), dialect, slug, input.InputURI, input.OutputURIPrefix)
	if err != nil {
		_ = s.Batches.MarkUnknown(ctx, resourceID, "submission_unknown", s.now())
		s.cloudBatchFail(x, w, serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The cloud provider returned an unknown batch status."))
		return
	}
	var projectedObject map[string]any
	_ = json.Unmarshal(projection, &projectedObject)
	submission.resource.UpstreamID, submission.resource.State, submission.resource.Metadata = upstreamID, statusOr(projectedObject["status"], "queued"), metadata
	commitCtx, stop := resourceCommitContext(ctx)
	defer stop()
	tx, err := s.Resources.Begin(commitCtx)
	if err == nil {
		_, err = s.Resources.PutTx(commitCtx, tx, submission.resource)
	}
	if err == nil {
		err = s.Batches.ActivateTx(commitCtx, tx, resourceID, upstreamID, nil)
	}
	if err == nil {
		err = tx.Commit(commitCtx)
	} else if tx != nil {
		_ = tx.Rollback(commitCtx)
	}
	if err != nil {
		_ = s.Batches.MarkUnknown(commitCtx, resourceID, "submission_commit_unavailable", s.now())
		s.cloudBatchFail(x, w, serverError(http.StatusBadGateway, "submission_unknown", "The cloud provider accepted the batch but local activation failed."))
		return
	}
	x.completePinned()
	s.writeStateJSON(w, x, projection)
}

func (s *Server) cloudBatchCall(w http.ResponseWriter, r *http.Request, cancel bool) {
	x, authority, done := s.stateBegin(w, r, wire.CloudBatch)
	if done {
		return
	}
	defer s.release(r.Context())
	id := r.PathValue("id")
	res, err := s.Resources.Get(r.Context(), resources.KindCloudBatch, authority.ID, id)
	if errors.Is(err, resources.ErrNotFound) {
		s.cloudBatchFail(x, w, notFoundError("not_found", "No cloud batch job with this ID exists for this key."))
		return
	}
	if err != nil {
		s.cloudBatchFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The cloud batch mapping could not be read."))
		return
	}
	var metadata map[string]json.RawMessage
	_ = json.Unmarshal(res.Metadata, &metadata)
	dialect := stringFromRaw(metadata["dialect"])
	if dialect != r.PathValue("dialect") {
		s.cloudBatchFail(x, w, notFoundError("not_found", "No cloud batch job with this ID exists for this dialect."))
		return
	}
	candidate, exists := x.request.release.Snapshot.Routes[res.RouteSlug]
	if !exists {
		s.cloudBatchFail(x, w, notFoundError("not_found", "The cloud batch route is unavailable."))
		return
	}
	x.route = &candidate
	p, route, failure := s.resolveResource(r.Context(), x, authority, res, "batch", retainedRetrieval)
	if failure != nil {
		s.cloudBatchFail(x, w, failure)
		return
	}
	if p.provider.ProfileID != dialect {
		s.releaseHold(r.Context(), p.hold)
		s.cloudBatchFail(x, w, notFoundError("not_found", "No cloud batch job with this ID exists for this dialect."))
		return
	}
	x.route = route
	if route.Fidelity.Strict() {
		s.releaseHold(r.Context(), p.hold)
		s.cloudBatchFail(x, w, policyUnavailable("target_capability", "Cloud batch jobs require a transformed route."))
		return
	}
	if err := x.guardrailSurfaceGate(route); err != nil {
		s.releaseHold(r.Context(), p.hold)
		s.cloudBatchFail(x, w, err)
		return
	}
	ctx, stop := s.stateDeadline(r.Context(), route)
	defer stop()
	defer s.resourceSettle(ctx, x, p)
	if err := s.reserveState(ctx, x, authority, time.Duration(route.OverallTimeout)*time.Millisecond); err != nil {
		s.cloudBatchFail(x, w, err)
		return
	}
	if gate := s.gateSlot(ctx, &p.provider, &p.slot, resourceEstimate, s.now().Add(time.Duration(route.OverallTimeout)*time.Millisecond), ""); gate.verdict != gateAdmitted {
		s.cloudBatchFail(x, w, gateError(gate))
		return
	} else {
		p.hold = gate.hold
	}
	if cancel {
		action := "cancel"
		if dialect == "bedrock-batch-inference" {
			action = "stop"
		}
		endpoint, urlErr := cloudBatchControlURL(p.provider.Connector(), dialect, res.UpstreamID, action)
		if urlErr != nil {
			s.cloudBatchFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The cloud control-plane URL could not be resolved."))
			return
		}
		response, attemptErr := s.pinnedDo(ctx, x, p, http.MethodPost, endpoint, []byte(`{}`), "application/json")
		if attemptErr != nil {
			s.cloudBatchFail(x, w, upstreamError(attemptErr))
			return
		}
		_ = response.Body.Close()
	}
	endpoint, urlErr := cloudBatchControlURL(p.provider.Connector(), dialect, res.UpstreamID, "")
	if urlErr != nil {
		s.cloudBatchFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The cloud control-plane URL could not be resolved."))
		return
	}
	response, attemptErr := s.pinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
	if attemptErr != nil {
		s.cloudBatchFail(x, w, upstreamError(attemptErr))
		return
	}
	defer response.Body.Close()
	body, err := readBounded(response.Body, s.cfg.MaxResponseBytes)
	if err != nil {
		s.cloudBatchFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The cloud job response could not be read."))
		return
	}
	var input, output string
	_ = json.Unmarshal(metadata["input_uri"], &input)
	_ = json.Unmarshal(metadata["output_uri_prefix"], &output)
	projected, err := projectCloudBatchJob(body, res.ID, dialect, res.RouteSlug, input, output)
	if err != nil {
		s.cloudBatchFail(x, w, serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The cloud provider returned an unknown job status."))
		return
	}
	meta := map[string]any{"dialect": dialect, "route_model": res.RouteSlug, "input_uri": input, "output_uri_prefix": output, "provider_job": json.RawMessage(body)}
	encoded, _ := json.Marshal(meta)
	var projectedObject map[string]any
	_ = json.Unmarshal(projected, &projectedObject)
	commit, release := resourceCommitContext(ctx)
	defer release()
	if err := s.Resources.Update(commit, res.ID, statusOr(projectedObject["status"], "queued"), encoded, nil); err != nil {
		s.cloudBatchFail(x, w, serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The cloud job refresh could not be committed."))
		return
	}
	x.completePinned()
	s.writeStateJSON(w, x, projected)
}

func (s *Server) getCloudBatch(w http.ResponseWriter, r *http.Request) {
	s.cloudBatchCall(w, r, false)
}

func (s *Server) cancelCloudBatch(w http.ResponseWriter, r *http.Request) {
	s.cloudBatchCall(w, r, true)
}

func (s *Server) listCloudBatches(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.stateBegin(w, r, wire.CloudBatch)
	if done {
		return
	}
	defer s.release(r.Context())
	limit, after, err := listArgs(r)
	if err != nil {
		s.cloudBatchFail(x, w, err)
		return
	}
	dialect := r.PathValue("dialect")
	if dialect != "vertex-batch-prediction" && dialect != "bedrock-batch-inference" {
		s.cloudBatchFail(x, w, notFoundError("not_found", "Unknown cloud batch dialect."))
		return
	}
	filtered, listErr := s.Resources.ListCloudBatches(r.Context(), authority.ID, dialect, limit+1, after)
	if listErr != nil {
		if errors.Is(listErr, resources.ErrNotFound) {
			s.cloudBatchFail(x, w, invalidRequest("invalid_cursor", "after must identify a local cloud batch job.", strPtr("after")))
			return
		}
		s.cloudBatchFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "Cloud batches could not be listed."))
		return
	}
	hasMore := len(filtered) > limit
	if hasMore {
		filtered = filtered[:limit]
	}
	data := make([]json.RawMessage, 0, len(filtered))
	for _, row := range filtered {
		if authErr := s.authorizeListedResource(r.Context(), authority, row); authErr != nil {
			s.cloudBatchFail(x, w, authErr)
			return
		}
		var metadata map[string]json.RawMessage
		_ = json.Unmarshal(row.Metadata, &metadata)
		var input, output, dialect string
		_ = json.Unmarshal(metadata["input_uri"], &input)
		_ = json.Unmarshal(metadata["output_uri_prefix"], &output)
		_ = json.Unmarshal(metadata["dialect"], &dialect)
		item, err := projectCloudBatchJob(metadata["provider_job"], row.ID, dialect, row.RouteSlug, input, output)
		if err != nil {
			s.cloudBatchFail(x, w, serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "A cloud batch could not be projected."))
			return
		}
		data = append(data, item)
	}
	var first, last any
	if len(filtered) > 0 {
		first, last = filtered[0].ID, filtered[len(filtered)-1].ID
	}
	s.writeStateJSON(w, x, mustJSON(map[string]any{"object": "list", "data": data, "has_more": hasMore, "first_id": first, "last_id": last}))
}

func cloudBatchControlURL(cfg connectors.Config, dialect, upstreamID, action string) (string, error) {
	if dialect == "vertex-batch-prediction" {
		return cfg.VertexBatchJobsURL(upstreamID, action)
	}
	if dialect == "bedrock-batch-inference" {
		return cfg.BedrockBatchJobsURL(upstreamID, action)
	}
	return "", errors.New("unknown cloud batch dialect")
}

func stringFromRaw(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func statusOr(value any, fallback string) string {
	if status, ok := value.(string); ok {
		return status
	}
	return fallback
}
