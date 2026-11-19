package cloudbatches

import (
	"cmp"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/batches/upstream"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
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

func validateCloudBatchRequest(raw []byte, adapter connectors.BatchAdapter, storage *connectors.BatchStorage) (cloudBatchRequest, *fault.Error) {
	doc, err := ingress.ParseObjectRequest(raw, oif.Limits{MaxBytes: 1 << 20})
	if err != nil || doc.Root().Kind() != oif.Object {
		return cloudBatchRequest{}, fault.InvalidRequest("invalid_json", "The batch-job body must be one JSON object.", nil)
	}
	allowed := map[string]bool{"input_uri": true, "output_uri_prefix": true, "display_name": true, "timeout_hours": true}
	for _, field := range doc.Root().Members() {
		if !allowed[field.Name] {
			return cloudBatchRequest{}, fault.InvalidRequest("unsupported_parameter", "This batch-job parameter is not supported.", new(field.Name))
		}
	}
	var input cloudBatchRequest
	var present bool
	if value, ok := doc.Root().Lookup("input_uri"); ok {
		input.InputURI, present = value.Text()
	}
	if !present || input.InputURI == "" {
		return cloudBatchRequest{}, fault.InvalidRequest("missing_required_parameter", "input_uri is required.", new("input_uri"))
	}
	if value, ok := doc.Root().Lookup("output_uri_prefix"); ok {
		input.OutputURIPrefix, present = value.Text()
	}
	if !present || input.OutputURIPrefix == "" {
		return cloudBatchRequest{}, fault.InvalidRequest("missing_required_parameter", "output_uri_prefix is required.", new("output_uri_prefix"))
	}
	if storage == nil || !storage.Contains(storage.InputURIPrefix, input.InputURI) ||
		!storage.ContainsPrefix(storage.OutputURIPrefix, input.OutputURIPrefix) {
		return cloudBatchRequest{}, fault.InvalidRequest("storage_scope", "The input or output URI is outside the configured batch storage prefixes.", nil)
	}
	if value, ok := doc.Root().Lookup("display_name"); ok {
		input.DisplayName, present = value.Text()
		if !present || !cloudBatchDisplayName.MatchString(input.DisplayName) {
			return cloudBatchRequest{}, fault.InvalidRequest("invalid_value", "display_name must use 1–128 letters, digits, underscores, or hyphens.", new("display_name"))
		}
	}
	if value, ok := doc.Root().Lookup("timeout_hours"); ok {
		if adapter != connectors.BatchBedrock {
			return cloudBatchRequest{}, fault.InvalidRequest("unsupported_parameter", "timeout_hours is only supported by Bedrock batch inference.", new("timeout_hours"))
		}
		if value.Kind() != oif.Number {
			return cloudBatchRequest{}, fault.InvalidRequest("invalid_value", "timeout_hours must be an integer from 24 to 168.", new("timeout_hours"))
		}
		parsed, parseErr := strconv.Atoi(value.Raw())
		if parseErr != nil || parsed < 24 || parsed > 168 {
			return cloudBatchRequest{}, fault.InvalidRequest("invalid_value", "timeout_hours must be an integer from 24 to 168.", new("timeout_hours"))
		}
		input.TimeoutHours = parsed
	}
	return input, nil
}

type cloudBatchObject struct {
	ID              string                `json:"id"`
	Object          string                `json:"object"`
	Dialect         string                `json:"dialect"`
	Model           string                `json:"model"`
	Status          upstream.CloudStatus  `json:"status"`
	InputURI        string                `json:"input_uri"`
	OutputURIPrefix string                `json:"output_uri_prefix"`
	CreatedAt       int64                 `json:"created_at"`
	CompletedAt     *int64                `json:"completed_at"`
	RequestCounts   *upstream.CloudCounts `json:"request_counts"`
	Error           *cloudBatchError      `json:"error"`
}

type cloudBatchError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func projectCloudBatchJob(job upstream.CloudJob, localID string, record upstream.CloudRecord) json.RawMessage {
	return kernel.MustJSON(cloudBatchObject{
		ID: localID, Object: "batch_job", Dialect: record.Dialect, Model: record.RouteModel, Status: job.Status,
		InputURI: record.InputURI, OutputURIPrefix: record.OutputURIPrefix, CreatedAt: job.CreatedAt,
		CompletedAt: job.CompletedAt, RequestCounts: job.Counts, Error: projectCloudBatchError(job.Error),
	})
}

func projectCloudBatchError(failure *upstream.CloudError) *cloudBatchError {
	if failure == nil {
		return nil
	}
	return &cloudBatchError{Code: failure.Code, Message: sanitizeCloudBatchMessage(failure.Message)}
}

var (
	cloudResourceARN = regexp.MustCompile(`arn:[A-Za-z0-9-:]+:[^ \t\r\n"]+`)
	cloudResourceJob = regexp.MustCompile(`projects/[^ \t\r\n"]+/batchPredictionJobs/[^ \t\r\n"]+`)
)

func sanitizeCloudBatchMessage(message string) string {
	return cloudResourceJob.ReplaceAllString(cloudResourceARN.ReplaceAllString(message, "[provider resource]"), "[provider resource]")
}

func (s surface) cloudBatchFail(x *kernel.Call, w http.ResponseWriter, e *fault.Error) {
	s.kernel.RefuseOn(x, w, e, "openai")
}

func (s surface) createCloudBatch(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.kernel.StateBegin(w, r, wire.CloudBatch)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	dialect := r.PathValue("dialect")
	slug := strings.TrimSpace(r.Header.Get(kernel.StateRouteHeader))
	if slug == "" {
		s.cloudBatchFail(x, w, fault.InvalidRequest("missing_required_parameter", "X-OLP-Route must name a batch-enabled route.", nil))
		return
	}
	snapshot := x.Snapshot()
	route, ok := snapshot.FindRoute(slug)
	if !ok || !slices.Contains(route.Operations, "batch") {
		s.cloudBatchFail(x, w, fault.NotFoundError("not_found", "No matching batch route exists."))
		return
	}
	if !authority.Allows("inference", route.Slug, route.ProjectID, s.kernel.Now()) {
		s.cloudBatchFail(x, w, fault.PermissionError("route_forbidden", "This API key is not allowed to use the model `"+route.Slug+"`."))
		return
	}
	x.Route = &route
	body, e := s.kernel.ReadResourceBody(w, r)
	if e != nil {
		s.cloudBatchFail(x, w, e)
		return
	}
	ctx, cancel := s.kernel.StateDeadline(r.Context(), &route)
	defer cancel()
	p, e := s.kernel.SelectPinSurface(ctx, x, &route, "batch", "native", "unary", func(provider *routing.Provider, model string) bool {
		cfg := provider.Connector()
		return provider.ProfileID == dialect && cfg.Supports("batch", "native", "unary") && cfg.BatchStorage != nil
	})
	if e != nil {
		s.cloudBatchFail(x, w, e)
		return
	}
	adapter, cloud := connectors.CloudBatchAdapter(dialect)
	if p.Provider.ProfileID != dialect || !cloud {
		s.kernel.ReleaseHold(r.Context(), p.Hold)
		s.cloudBatchFail(x, w, fault.NotFoundError("not_found", "No matching cloud batch dialect is available."))
		return
	}
	if x.Strict() {
		s.kernel.ReleaseHold(r.Context(), p.Hold)
		s.cloudBatchFail(x, w, kernel.PolicyUnavailable("target_capability", "Cloud batch jobs require a transformed route."))
		return
	}
	input, failure := validateCloudBatchRequest(body, adapter, p.Provider.BatchStorage)
	if failure != nil {
		s.kernel.ReleaseHold(r.Context(), p.Hold)
		s.cloudBatchFail(x, w, failure)
		return
	}
	if err := s.kernel.UnboundedPinLimits(x.AdmissionAuthority(authority), p, &route); err != nil {
		s.kernel.ReleaseHold(r.Context(), p.Hold)
		s.cloudBatchFail(x, w, err)
		return
	}
	if err := x.GuardrailSurfaceGate(&route); err != nil {
		s.kernel.ReleaseHold(r.Context(), p.Hold)
		s.cloudBatchFail(x, w, err)
		return
	}
	defer s.kernel.ResourceSettle(ctx, x, p)
	if err := s.kernel.ReserveStateAndSlot(ctx, x, authority, p, &route); err != nil {
		s.cloudBatchFail(x, w, err)
		return
	}
	source, err := ingress.ParseObjectRequest(body, oif.Limits{MaxBytes: int(s.kernel.Config.MaxBodyBytes)})
	if err != nil {
		s.cloudBatchFail(x, w, fault.InvalidRequest("invalid_json", "The request body must be one JSON object.", nil))
		return
	}
	plan, err := planCloudBatch(p, adapter, input)
	if err != nil {
		s.cloudBatchFail(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The cloud batch control-plane URL could not be resolved."))
		return
	}
	submission, e := s.kernel.BeginBatch(ctx, x, authority.ID, p, kernel.BatchIntent{
		ResourceID: plan.id, Kind: resources.KindCloudBatch, Adapter: adapter, Surface: batches.SurfaceNative,
		Operation: wire.LineGeneration, Window: plan.window, Submission: plan.submission,
		Source: source, Canonical: source.Bytes(), Effective: plan.body,
	})
	if e != nil {
		s.cloudBatchFail(x, w, e)
		return
	}
	result, exchange, e := s.kernel.SendBatch(ctx, x, p, plan.id, plan.endpoint, plan.body)
	if e != nil {
		s.cloudBatchFail(x, w, e)
		return
	}
	accepted, err := acceptedCloudJob(adapter, result, s.kernel.Now())
	if err != nil {
		s.cloudBatchFail(x, w, s.kernel.BatchSubmissionUnknown(ctx, plan.id, "The cloud provider returned an invalid batch job; the job may have been accepted."))
		return
	}
	job, err := upstream.DecodeCloudJob(adapter, accepted)
	if err != nil {
		s.cloudBatchFail(x, w, s.kernel.BatchSubmissionUnknown(ctx, plan.id, "The cloud provider returned an invalid batch job; the job may have been accepted."))
		return
	}
	record := upstream.CloudRecord{
		Dialect: dialect, RouteModel: slug, UpstreamModel: p.Model,
		InputURI: input.InputURI, OutputURIPrefix: input.OutputURIPrefix, ProviderJob: accepted,
	}
	exchange.Complete(kernel.ClassSuccess)
	stored, e := s.kernel.ActivateBatch(ctx, submission, kernel.AcceptedBatch{UpstreamID: job.ID, State: string(job.Status), Metadata: kernel.MustJSON(record)})
	if e != nil {
		s.cloudBatchFail(x, w, e)
		return
	}
	s.kernel.WriteStateJSON(w, x, projectCloudBatchJob(job, stored.ID, record), exchange)
}

type cloudBatchPlan struct {
	id         uuid.UUID
	endpoint   string
	body       []byte
	window     time.Duration
	submission batches.Submission
}

type vertexBatchJob struct {
	DisplayName  string            `json:"displayName"`
	Model        string            `json:"model"`
	InputConfig  vertexBatchInput  `json:"inputConfig"`
	OutputConfig vertexBatchOutput `json:"outputConfig"`
	Labels       map[string]string `json:"labels"`
}

type vertexBatchInput struct {
	InstancesFormat string `json:"instancesFormat"`
	GCSSource       struct {
		URIs []string `json:"uris"`
	} `json:"gcsSource"`
}

type vertexBatchOutput struct {
	PredictionsFormat string `json:"predictionsFormat"`
	GCSDestination    struct {
		OutputURIPrefix string `json:"outputUriPrefix"`
	} `json:"gcsDestination"`
}

type bedrockBatchJob struct {
	JobName            string `json:"jobName"`
	RoleARN            string `json:"roleArn"`
	ModelID            string `json:"modelId"`
	ClientRequestToken string `json:"clientRequestToken"`
	TimeoutHours       int    `json:"timeoutDurationInHours,omitempty"`
	InputDataConfig    struct {
		S3InputDataConfig struct {
			S3URI    string `json:"s3Uri"`
			S3Format string `json:"s3InputFormat"`
		} `json:"s3InputDataConfig"`
	} `json:"inputDataConfig"`
	OutputDataConfig struct {
		S3OutputDataConfig struct {
			S3URI string `json:"s3Uri"`
		} `json:"s3OutputDataConfig"`
	} `json:"outputDataConfig"`
}

func planCloudBatch(p *kernel.Pin, adapter connectors.BatchAdapter, input cloudBatchRequest) (cloudBatchPlan, error) {
	id := uuid.Must(uuid.NewV7())
	label := strings.ReplaceAll(id.String(), "-", "")
	display := cmp.Or(input.DisplayName, "olp-"+label)
	window := kernel.DefaultBatchCompletionWindow
	if input.TimeoutHours > 0 {
		window = time.Duration(input.TimeoutHours) * time.Hour
	}
	cfg := p.Provider.Connector()
	if adapter == connectors.BatchVertex {
		endpoint, err := cfg.VertexBatchJobsURL("", "")
		job := vertexBatchJob{
			DisplayName: display, Model: "publishers/google/models/" + cfg.Model(p.Model),
			InputConfig: vertexBatchInput{InstancesFormat: "jsonl"}, OutputConfig: vertexBatchOutput{PredictionsFormat: "jsonl"},
			Labels: map[string]string{"olp_submission": label},
		}
		job.InputConfig.GCSSource.URIs = []string{input.InputURI}
		job.OutputConfig.GCSDestination.OutputURIPrefix = input.OutputURIPrefix
		return cloudBatchPlan{id: id, endpoint: endpoint, body: kernel.MustJSON(job), window: window, submission: batches.VertexSubmission{Label: label}}, err
	}
	endpoint, err := cfg.BedrockBatchJobsURL("", "create")
	job := bedrockBatchJob{
		JobName: display, RoleARN: p.Provider.BatchStorage.RoleARN, ModelID: cfg.Model(p.Model),
		ClientRequestToken: id.String(), TimeoutHours: input.TimeoutHours,
	}
	job.InputDataConfig.S3InputDataConfig.S3URI = input.InputURI
	job.InputDataConfig.S3InputDataConfig.S3Format = "JSONL"
	job.OutputDataConfig.S3OutputDataConfig.S3URI = input.OutputURIPrefix
	body := kernel.MustJSON(job)
	return cloudBatchPlan{
		id: id, endpoint: endpoint, body: body, window: window,
		submission: batches.BedrockSubmission{ClientRequestToken: id.String(), CreateBody: body},
	}, err
}

func acceptedCloudJob(adapter connectors.BatchAdapter, result []byte, now time.Time) ([]byte, error) {
	if adapter != connectors.BatchBedrock {
		return result, nil
	}
	var created struct {
		ARN string `json:"jobArn"`
	}
	if err := json.Unmarshal(result, &created); err != nil || created.ARN == "" {
		return nil, errors.New("bedrock create response has no job ARN")
	}
	return json.Marshal(struct {
		ARN        string    `json:"jobArn"`
		Status     string    `json:"status"`
		SubmitTime time.Time `json:"submitTime"`
	}{ARN: created.ARN, Status: "Submitted", SubmitTime: now.UTC()})
}

func (s surface) cloudBatchCall(w http.ResponseWriter, r *http.Request, cancel bool) {
	x, authority, done := s.kernel.StateBegin(w, r, wire.CloudBatch)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	id := r.PathValue("id")
	res, err := s.kernel.Deps.Resources.Get(r.Context(), resources.KindCloudBatch, authority.ID, id)
	if errors.Is(err, resources.ErrNotFound) {
		s.cloudBatchFail(x, w, fault.NotFoundError("not_found", "No cloud batch job with this ID exists for this key."))
		return
	}
	if err != nil {
		s.cloudBatchFail(x, w, fault.ServerError(http.StatusInternalServerError, "internal_error", "The cloud batch mapping could not be read."))
		return
	}
	record, recordErr := upstream.DecodeCloudRecord(res.Metadata)
	adapter, cloud := connectors.CloudBatchAdapter(record.Dialect)
	if recordErr != nil || !cloud {
		s.cloudBatchFail(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The stored cloud batch mapping is malformed."))
		return
	}
	dialect := record.Dialect
	if dialect != r.PathValue("dialect") {
		s.cloudBatchFail(x, w, fault.NotFoundError("not_found", "No cloud batch job with this ID exists for this dialect."))
		return
	}
	if !cancel {
		candidate, exists := x.Snapshot().FindRoute(res.RouteSlug)
		if !exists {
			s.cloudBatchFail(x, w, fault.NotFoundError("not_found", "The cloud batch route is unavailable."))
			return
		}
		x.Route = &candidate
	}
	use := kernel.RetainedRetrieval
	if cancel {
		use = kernel.RetainedHousekeeping
	}
	p, route, failure := s.kernel.ResolveResource(r.Context(), x, authority, res, "batch", use)
	if failure != nil {
		s.cloudBatchFail(x, w, failure)
		return
	}
	if p.Provider.ProfileID != dialect {
		s.kernel.ReleaseHold(r.Context(), p.Hold)
		s.cloudBatchFail(x, w, fault.NotFoundError("not_found", "No cloud batch job with this ID exists for this dialect."))
		return
	}
	x.Route = route
	if route.Fidelity.Strict() {
		s.kernel.ReleaseHold(r.Context(), p.Hold)
		s.cloudBatchFail(x, w, kernel.PolicyUnavailable("target_capability", "Cloud batch jobs require a transformed route."))
		return
	}
	if !cancel {
		if err := x.GuardrailSurfaceGate(route); err != nil {
			s.kernel.ReleaseHold(r.Context(), p.Hold)
			s.cloudBatchFail(x, w, err)
			return
		}
	}
	ctx, stop := s.kernel.StateDeadline(r.Context(), route)
	defer stop()
	defer s.kernel.ResourceSettle(ctx, x, p)
	if err := s.kernel.ReserveStateAndSlot(ctx, x, authority, p, route); err != nil {
		s.cloudBatchFail(x, w, err)
		return
	}
	cfg := p.Provider.Connector()
	if cancel {
		endpoint, urlErr := cloudBatchControlURL(cfg, adapter, res.UpstreamID, cloudBatchCancelAction(adapter))
		if urlErr != nil {
			s.cloudBatchFail(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The cloud control-plane URL could not be resolved."))
			return
		}
		response, attemptErr := s.kernel.PinnedDo(ctx, x, p, http.MethodPost, endpoint, []byte(`{}`), "application/json")
		if attemptErr != nil {
			s.cloudBatchFail(x, w, kernel.UpstreamError(attemptErr))
			return
		}
		_ = response.Body.Close()
		response.Exchange.Complete(kernel.ClassSuccess)
	}
	endpoint, urlErr := cloudBatchControlURL(cfg, adapter, res.UpstreamID, "")
	if urlErr != nil {
		s.cloudBatchFail(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The cloud control-plane URL could not be resolved."))
		return
	}
	response, attemptErr := s.kernel.PinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
	if attemptErr != nil {
		s.cloudBatchFail(x, w, kernel.UpstreamError(attemptErr))
		return
	}
	defer response.Body.Close()
	body, err := response.ReadBody(s.kernel.Config.MaxResponseBytes)
	if err != nil {
		s.cloudBatchFail(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The cloud job response could not be read."))
		return
	}
	job, err := upstream.DecodeCloudJob(adapter, body)
	if errors.Is(err, upstream.ErrProviderConflict) {
		s.cloudBatchFail(x, w, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The cloud provider returned an unknown job status."))
		return
	}
	if err != nil || job.ID != res.UpstreamID {
		s.cloudBatchFail(x, w, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The cloud provider returned a mismatched job identity."))
		return
	}
	response.Exchange.Complete(kernel.ClassSuccess)
	refreshed := record.WithJob(body)
	commit, release := kernel.ResourceCommitContext(ctx)
	defer release()
	if err := s.kernel.Deps.Resources.Update(commit, res.ID, string(job.Status), kernel.MustJSON(refreshed), nil); err != nil {
		s.cloudBatchFail(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The cloud job refresh could not be committed."))
		return
	}
	s.kernel.WriteStateJSON(w, x, projectCloudBatchJob(job, res.ID, refreshed), response.Exchange)
}

func cloudBatchCancelAction(adapter connectors.BatchAdapter) string {
	if adapter == connectors.BatchBedrock {
		return "stop"
	}
	return "cancel"
}

func (s surface) getCloudBatch(w http.ResponseWriter, r *http.Request) {
	s.cloudBatchCall(w, r, false)
}

func (s surface) cancelCloudBatch(w http.ResponseWriter, r *http.Request) {
	s.cloudBatchCall(w, r, true)
}

func (s surface) listCloudBatches(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.kernel.StateBegin(w, r, wire.CloudBatch)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	limit, after, err := kernel.ListArgs(r)
	if err != nil {
		s.cloudBatchFail(x, w, err)
		return
	}
	dialect := r.PathValue("dialect")
	if _, cloud := connectors.CloudBatchAdapter(dialect); !cloud {
		s.cloudBatchFail(x, w, fault.NotFoundError("not_found", "Unknown cloud batch dialect."))
		return
	}
	filtered, continuation, listErr := s.kernel.ListedResourcePage(authority.ID+"/"+dialect, "after", limit, after, func(cursor resources.ListCursor, count int) ([]*resources.Resource, *fault.Error) {
		batch, err := s.kernel.Deps.Resources.ListCloudBatches(r.Context(), authority.ID, dialect, count, cursor)
		if errors.Is(err, resources.ErrNotFound) {
			return nil, fault.InvalidRequest("invalid_cursor", "after must identify a local cloud batch job.", new("after"))
		}
		if err != nil {
			return nil, fault.ServerError(http.StatusInternalServerError, "internal_error", "Cloud batches could not be listed.")
		}
		return batch, nil
	}, func(row *resources.Resource) *fault.Error {
		return s.kernel.AuthorizeListedResource(r.Context(), authority, row)
	})
	if listErr != nil {
		s.cloudBatchFail(x, w, listErr)
		return
	}
	data := make([]json.RawMessage, 0, len(filtered))
	for _, row := range filtered {
		item, itemErr := listedCloudBatch(row)
		if itemErr != nil {
			s.cloudBatchFail(x, w, itemErr)
			return
		}
		data = append(data, item)
	}
	var first, last any
	if len(filtered) > 0 {
		first, last = filtered[0].ID, filtered[len(filtered)-1].ID
	}
	if continuation != "" {
		last = continuation
	}
	s.kernel.WriteStateJSON(w, x, kernel.MustJSON(map[string]any{"object": "list", "data": data, "has_more": continuation != "", "first_id": first, "last_id": last}))
}

func listedCloudBatch(row *resources.Resource) (json.RawMessage, *fault.Error) {
	record, err := upstream.DecodeCloudRecord(row.Metadata)
	adapter, cloud := connectors.CloudBatchAdapter(record.Dialect)
	if err != nil || !cloud {
		return nil, fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "A cloud batch could not be projected.")
	}
	job, err := upstream.DecodeCloudJob(adapter, record.ProviderJob)
	if err != nil || job.ID != row.UpstreamID {
		return nil, fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "A stored cloud batch has a mismatched job identity.")
	}
	return projectCloudBatchJob(job, row.ID, record), nil
}

func cloudBatchControlURL(cfg connectors.Config, adapter connectors.BatchAdapter, upstreamID, action string) (string, error) {
	if adapter == connectors.BatchVertex {
		return cfg.VertexBatchJobsURL(upstreamID, action)
	}
	return cfg.BedrockBatchJobsURL(upstreamID, action)
}
