package batches

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/usage"
)

var ErrStorageScope = errors.New("cloud batch output is outside configured storage")

type VertexReader struct {
	Auth   *connectors.Auth
	Egress *egress.Policy
}
type BedrockReader struct {
	Auth   *connectors.Auth
	Egress *egress.Policy
}

func cloudState(dialect, state string) (string, bool) {
	vertex := map[string]string{
		"JOB_STATE_QUEUED": "queued", "JOB_STATE_PENDING": "queued", "JOB_STATE_RUNNING": "running",
		"JOB_STATE_SUCCEEDED": "succeeded", "JOB_STATE_PARTIALLY_SUCCEEDED": "partially_succeeded",
		"JOB_STATE_FAILED": "failed", "JOB_STATE_CANCELLING": "cancelling", "JOB_STATE_CANCELLED": "cancelled",
		"JOB_STATE_EXPIRED": "expired", "JOB_STATE_PAUSED": "running", "JOB_STATE_UPDATING": "running",
	}
	bedrock := map[string]string{"Submitted": "queued", "Validating": "queued", "Scheduled": "queued", "InProgress": "running",
		"Completed": "succeeded", "PartiallyCompleted": "partially_succeeded", "Failed": "failed", "Stopping": "cancelling", "Stopped": "cancelled", "Expired": "expired"}
	if dialect == "vertex-batch-prediction" {
		v, ok := vertex[state]
		return v, ok
	}
	v, ok := bedrock[state]
	return v, ok
}
func cloudIsTerminal(status string) bool {
	return status == "succeeded" || status == "partially_succeeded" || status == "failed" || status == "cancelled" || status == "expired"
}

func sameVertexObjectGeneration(name, generation string, object gcsObject) bool {
	return name == object.Name && generation == object.Generation
}

func vertexSubmissionLabelMatches(labels map[string]string, label string) bool {
	return label != "" && labels["olp_submission"] == label
}

func bedrockCreateReissueHasEvidence(status int, body []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusConflict {
		return true
	}
	var failure map[string]json.RawMessage
	_ = json.Unmarshal(body, &failure)
	var code string
	_ = json.Unmarshal(failure["__type"], &code)
	return !strings.Contains(code, "ValidationException") && !strings.Contains(code, "ConflictException")
}

func bedrockReissueNoEvidence(status int, body []byte) bool {
	return !bedrockCreateReissueHasEvidence(status, body)
}

func cloudAdoptMetadata(settlement Settlement, snapshot BatchSnapshot) (json.RawMessage, string, error) {
	dialect := "vertex-batch-prediction"
	var root map[string]json.RawMessage
	if json.Unmarshal(snapshot.Body, &root) != nil {
		return nil, "", ErrProviderConflict
	}
	inputURI, outputURI := "", ""
	if settlement.Adapter == "vertex" {
		var inputConfig, gcsSource, outputConfig, destination map[string]json.RawMessage
		_ = json.Unmarshal(root["inputConfig"], &inputConfig)
		_ = json.Unmarshal(inputConfig["gcsSource"], &gcsSource)
		var uris []string
		_ = json.Unmarshal(gcsSource["uris"], &uris)
		if len(uris) > 0 {
			inputURI = uris[0]
		}
		_ = json.Unmarshal(root["outputConfig"], &outputConfig)
		_ = json.Unmarshal(outputConfig["gcsDestination"], &destination)
		_ = json.Unmarshal(destination["outputUriPrefix"], &outputURI)
	} else {
		dialect = "bedrock-batch-inference"
		var cursor struct {
			Submission struct {
				Body json.RawMessage `json:"create_body"`
			} `json:"submission"`
		}
		if json.Unmarshal(settlement.Cursor, &cursor) != nil {
			return nil, "", ErrProviderConflict
		}
		var body map[string]json.RawMessage
		_ = json.Unmarshal(cursor.Submission.Body, &body)
		var inputData, s3Input, outputData, s3Output map[string]json.RawMessage
		_ = json.Unmarshal(body["inputDataConfig"], &inputData)
		_ = json.Unmarshal(inputData["s3InputDataConfig"], &s3Input)
		_ = json.Unmarshal(body["outputDataConfig"], &outputData)
		_ = json.Unmarshal(outputData["s3OutputDataConfig"], &s3Output)
		_ = json.Unmarshal(s3Input["s3Uri"], &inputURI)
		_ = json.Unmarshal(s3Output["s3Uri"], &outputURI)
	}
	var rawState string
	if settlement.Adapter == "vertex" {
		_ = json.Unmarshal(root["state"], &rawState)
	} else {
		_ = json.Unmarshal(root["status"], &rawState)
	}
	status, ok := cloudState(dialect, rawState)
	if !ok {
		return nil, "", ErrProviderConflict
	}
	metadata, _ := json.Marshal(map[string]any{"dialect": dialect, "route_model": settlement.RouteSlug, "upstream_model": resourceModel(settlement.Template), "input_uri": inputURI, "output_uri_prefix": outputURI, "provider_job": json.RawMessage(snapshot.Body)})
	return metadata, status, nil
}

func cloudRequest(ctx context.Context, auth *connectors.Auth, policy *egress.Policy, target *PinnedTarget, method, endpoint string, body []byte, limit int64) (int, []byte, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return 0, nil, err
	}
	validationURL := *u
	validationURL.RawQuery, validationURL.Fragment, validationURL.ForceQuery = "", "", false
	if _, err = policy.ValidateEndpoint(validationURL.String()); err != nil {
		return 0, nil, err
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	cfg := target.Config
	if _, err = auth.Apply(ctx, req, cfg, target.Secret, body); err != nil {
		return 0, nil, err
	}
	resp, err := target.Client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if int64(len(data)) > limit {
		return resp.StatusCode, nil, ErrBatchTooLarge
	}
	return resp.StatusCode, data, nil
}

func (r *VertexReader) Poll(ctx context.Context, settlement Settlement, target *PinnedTarget) (BatchSnapshot, error) {
	if settlement.UpstreamID == nil {
		return BatchSnapshot{}, ErrProviderConflict
	}
	endpoint, err := target.Config.VertexBatchJobsURL(*settlement.UpstreamID, "")
	if err != nil {
		return BatchSnapshot{}, err
	}
	status, body, err := cloudRequest(ctx, r.Auth, r.Egress, target, http.MethodGet, endpoint, nil, 4<<20)
	if err != nil {
		return BatchSnapshot{}, err
	}
	if status != http.StatusOK {
		return BatchSnapshot{}, fmt.Errorf("Vertex batch poll HTTP %d", status)
	}
	var job map[string]json.RawMessage
	if json.Unmarshal(body, &job) != nil {
		return BatchSnapshot{}, ErrProviderConflict
	}
	var name, state string
	_ = json.Unmarshal(job["name"], &name)
	_ = json.Unmarshal(job["state"], &state)
	id := name[strings.LastIndex(name, "/")+1:]
	terminal := cloudTerminal("vertex-batch-prediction", state)
	if id != *settlement.UpstreamID || !cloudKnown("vertex-batch-prediction", state) {
		return BatchSnapshot{}, ErrProviderConflict
	}
	snapshot := BatchSnapshot{ID: id, Status: state, Body: body, Terminal: terminal}
	if stats := vertexCounts(job); stats != nil && terminal {
		n := stats["successful"] + stats["failed"] + stats["incomplete"]
		snapshot.Expected = &n
	}
	if snapshot.Terminal {
		var outputInfo map[string]json.RawMessage
		_ = json.Unmarshal(job["outputInfo"], &outputInfo)
		_ = json.Unmarshal(outputInfo["gcsOutputDirectory"], &snapshot.OutputFileID)
	}
	return snapshot, nil
}

func (r *VertexReader) Reconcile(ctx context.Context, settlement Settlement, target *PinnedTarget, _, _ string) (Reconciliation, error) {
	var cursor struct {
		Submission struct {
			Label string `json:"vertex_label"`
		} `json:"submission"`
	}
	if json.Unmarshal(settlement.Cursor, &cursor) != nil || cursor.Submission.Label == "" {
		return Reconciliation{}, ErrProviderConflict
	}
	endpoint, err := target.Config.VertexBatchJobsURL("", "")
	if err != nil {
		return Reconciliation{}, err
	}
	u, _ := url.Parse(endpoint)
	q := u.Query()
	q.Set("pageSize", "100")
	q.Set("filter", `labels.olp_submission="`+cursor.Submission.Label+`"`)
	u.RawQuery = q.Encode()
	status, body, err := cloudRequest(ctx, r.Auth, r.Egress, target, http.MethodGet, u.String(), nil, 4<<20)
	if err != nil {
		return Reconciliation{}, err
	}
	if status != 200 {
		return Reconciliation{}, fmt.Errorf("Vertex list HTTP %d", status)
	}
	var list struct {
		Jobs []json.RawMessage `json:"batchPredictionJobs"`
	}
	if json.Unmarshal(body, &list) != nil {
		return Reconciliation{}, ErrProviderConflict
	}
	matches := []BatchSnapshot{}
	for _, raw := range list.Jobs {
		var job map[string]json.RawMessage
		_ = json.Unmarshal(raw, &job)
		var name, state string
		_ = json.Unmarshal(job["name"], &name)
		_ = json.Unmarshal(job["state"], &state)
		var labels map[string]string
		_ = json.Unmarshal(job["labels"], &labels)
		if !vertexSubmissionLabelMatches(labels, cursor.Submission.Label) {
			continue
		}
		id := name[strings.LastIndex(name, "/")+1:]
		status, known := cloudState("vertex-batch-prediction", state)
		if !known {
			return Reconciliation{}, ErrProviderConflict
		}
		if id != "" {
			snapshot := BatchSnapshot{ID: id, Status: state, Body: raw, Terminal: cloudIsTerminal(status)}
			if stats := vertexCounts(job); stats != nil && snapshot.Terminal {
				total := stats["successful"] + stats["failed"] + stats["incomplete"]
				snapshot.Expected = &total
			}
			if snapshot.Terminal {
				var info map[string]json.RawMessage
				_ = json.Unmarshal(job["outputInfo"], &info)
				_ = json.Unmarshal(info["gcsOutputDirectory"], &snapshot.OutputFileID)
			}
			matches = append(matches, snapshot)
		}
	}
	return Reconciliation{Matches: matches}, nil
}

func (r *VertexReader) ReadResults(ctx context.Context, target *PinnedTarget, snapshot BatchSnapshot, cursor Cursor, commit ResultPageCommit) (Cursor, bool, error) {
	if cursor.Phase == "done" {
		return cursor, true, nil
	}
	if !snapshot.Terminal {
		return cursor, false, nil
	}
	var job map[string]json.RawMessage
	_ = json.Unmarshal(snapshot.Body, &job)
	var info map[string]json.RawMessage
	_ = json.Unmarshal(job["outputInfo"], &info)
	var outURI string
	_ = json.Unmarshal(info["gcsOutputDirectory"], &outURI)
	storage := target.Config.BatchStorage
	if storage == nil || !storage.Contains(storage.OutputURIPrefix, outURI) {
		return cursor, false, ErrStorageScope
	}
	scheme, bucket, prefix, ok := parseGCSURI(outURI)
	if !ok || scheme != "gs" {
		return cursor, false, ErrStorageScope
	}
	objects, err := listGCSObjects(ctx, r.Auth, r.Egress, target, bucket, prefix)
	if err != nil {
		return cursor, false, err
	}
	var priorName, priorGen string
	if cursor.File != "" {
		sep := strings.LastIndex(cursor.File, "#")
		if sep < 0 {
			return cursor, false, ErrCursorConflict
		}
		priorName, priorGen = cursor.File[:sep], cursor.File[sep+1:]
	}
	started := priorName == ""
	foundPrior := priorName == ""
	for _, obj := range objects {
		if obj.Name == priorName {
			foundPrior = true
			if !sameVertexObjectGeneration(priorName, priorGen, obj) {
				return cursor, false, ErrCursorConflict
			}
			started = true
			if cursor.Phase == "vertex_done" {
				continue
			}
		}
		if !started {
			continue
		}
		file := obj.Name + "#" + obj.Generation
		body, err := downloadGCSObject(ctx, r.Auth, r.Egress, target, bucket, obj)
		if err != nil {
			return cursor, false, err
		}
		lineNo := int64(0)
		prefixDigest := ""
		if cursor.File == file && cursor.Phase == "output" {
			lineNo = cursor.Line
			prefixDigest = cursor.PrefixSHA256
		}
		next, _, _, err := ReadJSONLPages(bytes.NewReader(body), Cursor{Phase: "output", File: file, Line: lineNo, PrefixSHA256: prefixDigest}, file, "output", func(line []byte) (Result, error) {
			lineNo++
			return parseVertexLine(line, obj.Name, obj.Generation, lineNo, target.Model)
		}, commit)
		if err != nil {
			return next, false, err
		}
		next.Phase = "vertex_done"
		if err = commit(nil, next); err != nil {
			return next, false, err
		}
		cursor = next
		started = true
	}
	if !foundPrior {
		return cursor, false, ErrCursorConflict
	}
	cursor.Phase = "done"
	if err = commit(nil, cursor); err != nil {
		return cursor, false, err
	}
	return cursor, true, nil
}

type gcsObject struct {
	Name       string `json:"name"`
	Generation string `json:"generation"`
}

func parseGCSURI(uri string) (scheme, bucket, object string, ok bool) {
	scheme, rest, ok := strings.Cut(uri, "://")
	if !ok {
		return
	}
	bucket, object, ok = strings.Cut(rest, "/")
	return
}
func listGCSObjects(ctx context.Context, auth *connectors.Auth, policy *egress.Policy, target *PinnedTarget, bucket, prefix string) ([]gcsObject, error) {
	var all []gcsObject
	token := ""
	for page := 0; page < 10; page++ {
		q := url.Values{"prefix": {prefix}, "maxResults": {"1000"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		endpoint := "https://storage.googleapis.com/storage/v1/b/" + url.PathEscape(bucket) + "/o?" + q.Encode()
		body, err := cloudStorageRequest(ctx, auth, policy, target, endpoint, nil, 4<<20)
		if err != nil {
			return nil, err
		}
		var res struct {
			Items []gcsObject `json:"items"`
			Next  string      `json:"nextPageToken"`
		}
		if json.Unmarshal(body, &res) != nil {
			return nil, ErrProviderConflict
		}
		for _, o := range res.Items {
			if strings.HasSuffix(o.Name, ".jsonl") && strings.HasPrefix(path.Base(o.Name), "prediction") {
				all = append(all, o)
			}
		}
		token = res.Next
		if token == "" {
			break
		}
	}
	if token != "" {
		return nil, errors.New("GCS batch output listing exceeded the page bound")
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	return all, nil
}
func cloudStorageRequest(ctx context.Context, auth *connectors.Auth, policy *egress.Policy, target *PinnedTarget, endpoint string, body []byte, limit int64) ([]byte, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	validationURL := *u
	validationURL.RawQuery, validationURL.Fragment, validationURL.ForceQuery = "", "", false
	if _, err = policy.ValidateEndpoint(validationURL.String()); err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if _, err = auth.ApplyGoogleStorage(ctx, req, target.Config.AuthMode, target.Secret); err != nil {
		return nil, err
	}
	resp, err := target.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 404 {
		return nil, resources.ErrNotFound
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GCS HTTP %d", resp.StatusCode)
	}
	if int64(len(data)) > limit {
		return nil, ErrBatchTooLarge
	}
	return data, nil
}
func downloadGCSObject(ctx context.Context, auth *connectors.Auth, policy *egress.Policy, target *PinnedTarget, bucket string, obj gcsObject) ([]byte, error) {
	endpoint := "https://storage.googleapis.com/storage/v1/b/" + url.PathEscape(bucket) + "/o/" + url.PathEscape(obj.Name) + "?alt=media&generation=" + url.QueryEscape(obj.Generation)
	return cloudStorageRequest(ctx, auth, policy, target, endpoint, nil, MaxBatchBytes)
}
func parseVertexLine(line []byte, object, generation string, lineNo int64, model string) (Result, error) {
	evidence, err := canonicalJSON(line)
	if err != nil {
		return Result{}, ErrProviderConflict
	}
	var item struct {
		Status   string          `json:"status"`
		Response json.RawMessage `json:"response"`
	}
	if json.Unmarshal(line, &item) != nil {
		return Result{}, ErrProviderConflict
	}
	id := fmt.Sprintf("%s#%s:%d", object, generation, lineNo)
	if item.Status != "" {
		return Result{CustomID: id, Kind: ResultErrored, ErrorClass: item.Status, Evidence: evidence, Usage: usage.AttemptUsage{Complete: true}}, nil
	}
	if len(item.Response) == 0 {
		return Result{}, ErrProviderConflict
	}
	completion, err := protocols.Decode(wire.Gemini, wire.Chat, item.Response, model, "")
	if err != nil {
		return Result{}, err
	}
	return Result{CustomID: id, Kind: ResultSucceeded, Usage: usage.ObservedAttemptUsage(completion.Usage, completion.Usage != nil, "generation"), Evidence: evidence}, nil
}

func (r *BedrockReader) Poll(ctx context.Context, settlement Settlement, target *PinnedTarget) (BatchSnapshot, error) {
	if settlement.UpstreamID == nil || *settlement.UpstreamID == "" {
		return BatchSnapshot{}, ErrProviderConflict
	}
	endpoint, err := target.Config.BedrockBatchJobsURL(*settlement.UpstreamID, "")
	if err != nil {
		return BatchSnapshot{}, err
	}
	status, body, err := cloudRequest(ctx, r.Auth, r.Egress, target, http.MethodGet, endpoint, nil, 4<<20)
	if err != nil {
		return BatchSnapshot{}, err
	}
	if status != http.StatusOK {
		return BatchSnapshot{}, fmt.Errorf("Bedrock batch poll HTTP %d", status)
	}
	var job map[string]json.RawMessage
	if json.Unmarshal(body, &job) != nil {
		return BatchSnapshot{}, ErrProviderConflict
	}
	var arn, state string
	_ = json.Unmarshal(job["jobArn"], &arn)
	_ = json.Unmarshal(job["status"], &state)
	projected, known := cloudState("bedrock-batch-inference", state)
	if !known || arn != *settlement.UpstreamID {
		return BatchSnapshot{}, ErrProviderConflict
	}
	expected := int64(1)
	snapshot := BatchSnapshot{ID: arn, Status: state, Body: body, Terminal: cloudIsTerminal(projected), Expected: &expected}
	if snapshot.Terminal {
		snapshot.OutputFileID = "manifest.json.out"
	}
	return snapshot, nil
}

func (r *BedrockReader) ReadResults(ctx context.Context, target *PinnedTarget, snapshot BatchSnapshot, cursor Cursor, commit ResultPageCommit) (Cursor, bool, error) {
	if cursor.Phase == "done" {
		return cursor, true, nil
	}
	var job map[string]json.RawMessage
	_ = json.Unmarshal(snapshot.Body, &job)
	var output, s3 map[string]json.RawMessage
	_ = json.Unmarshal(job["outputDataConfig"], &output)
	_ = json.Unmarshal(output["s3OutputDataConfig"], &s3)
	var base string
	_ = json.Unmarshal(s3["s3Uri"], &base)
	location := bedrockOutputLocation(base, snapshot.ID)
	if target.Config.BatchStorage == nil || !target.Config.BatchStorage.Contains(target.Config.BatchStorage.OutputURIPrefix, location) {
		return cursor, false, ErrStorageScope
	}
	bucket, key, ok := parseS3URI(location + "manifest.json.out")
	if !ok {
		return cursor, false, ErrStorageScope
	}
	endpoint := bedrockManifestURL(bucket, key, target.Config.CloudRegion)
	u, err := url.Parse(endpoint)
	if err != nil {
		return cursor, false, err
	}
	if _, err = r.Egress.ValidateEndpoint(u.String()); err != nil {
		return cursor, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return cursor, false, err
	}
	cfg := target.Config
	cfg.SigningService = "s3"
	if _, err = r.Auth.Apply(ctx, req, cfg, target.Secret, nil); err != nil {
		return cursor, false, err
	}
	resp, err := target.Client.Do(req)
	if err != nil {
		return cursor, false, err
	}
	defer resp.Body.Close()
	next := Cursor{Phase: "done", File: "manifest.json.out", PrefixSHA256: emptyPrefixDigest()}
	if resp.StatusCode == http.StatusNotFound {
		if err = commit(nil, next); err != nil {
			return cursor, false, err
		}
		return next, true, nil
	}
	if resp.StatusCode != http.StatusOK {
		return cursor, false, fmt.Errorf("Bedrock S3 manifest HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return cursor, false, err
	}
	if len(body) > 1<<20 {
		return cursor, false, ErrBatchTooLarge
	}
	aggregate, err := parseBedrockManifest(body)
	if err != nil {
		return cursor, false, err
	}
	if err = commit([]Result{aggregate}, next); err != nil {
		return cursor, false, err
	}
	return next, true, nil
}

func bedrockOutputLocation(base, arn string) string {
	jobID := arn[strings.LastIndex(arn, "/")+1:]
	return strings.TrimRight(base, "/") + "/" + jobID + "/"
}

func bedrockManifestURL(bucket, key, region string) string {
	if strings.Contains(bucket, ".") {
		return "https://s3." + region + ".amazonaws.com/" + bucket + "/" + escapeS3Key(key)
	}
	return "https://" + bucket + ".s3." + region + ".amazonaws.com/" + escapeS3Key(key)
}

func parseBedrockManifest(body []byte) (Result, error) {
	canonical, err := canonicalJSON(body)
	if err != nil {
		return Result{}, ErrProviderConflict
	}
	var manifest struct {
		Total     *int64 `json:"totalRecordCount"`
		Processed *int64 `json:"processedRecordCount"`
		Input     *int64 `json:"inputTokenCount"`
		Output    *int64 `json:"outputTokenCount"`
	}
	if json.Unmarshal(body, &manifest) != nil || manifest.Total == nil || manifest.Processed == nil || manifest.Input == nil || manifest.Output == nil ||
		*manifest.Total < 0 || *manifest.Processed < 0 || *manifest.Processed > *manifest.Total || *manifest.Input < 0 || *manifest.Output < 0 {
		return Result{}, ErrProviderConflict
	}
	usageValue := &wire.Usage{InputTokens: *manifest.Input, OutputTokens: *manifest.Output, TotalTokens: *manifest.Input + *manifest.Output}
	evidence := usage.ObservedAttemptUsage(usageValue, *manifest.Processed == *manifest.Total, "generation")
	evidence.Batch = true
	return Result{CustomID: "manifest", Kind: ResultAggregate, Usage: evidence, Evidence: canonical}, nil
}

func (r *BedrockReader) Reconcile(ctx context.Context, settlement Settlement, target *PinnedTarget, _, _ string) (Reconciliation, error) {
	var cursor struct {
		Submission struct {
			Token string          `json:"bedrock_client_request_token"`
			Body  json.RawMessage `json:"create_body"`
		} `json:"submission"`
	}
	if json.Unmarshal(settlement.Cursor, &cursor) != nil || cursor.Submission.Token == "" || len(cursor.Submission.Body) == 0 {
		return Reconciliation{}, ErrProviderConflict
	}
	endpoint, err := target.Config.BedrockBatchJobsURL("", "create")
	if err != nil {
		return Reconciliation{}, err
	}
	status, body, err := cloudRequest(ctx, r.Auth, r.Egress, target, http.MethodPost, endpoint, cursor.Submission.Body, 4<<20)
	if err != nil {
		return Reconciliation{}, err
	}
	if bedrockReissueNoEvidence(status, body) {
		return Reconciliation{}, nil
	}
	if status != http.StatusOK {
		return Reconciliation{}, fmt.Errorf("Bedrock batch reissue HTTP %d", status)
	}
	var response struct {
		ARN string `json:"jobArn"`
	}
	if json.Unmarshal(body, &response) != nil || response.ARN == "" {
		return Reconciliation{}, ErrProviderConflict
	}
	snapshot, err := r.Poll(ctx, Settlement{UpstreamID: &response.ARN}, target)
	if err != nil {
		return Reconciliation{}, err
	}
	return Reconciliation{Matches: []BatchSnapshot{snapshot}}, nil
}

func parseS3URI(uri string) (bucket, key string, ok bool) {
	scheme, rest, ok := strings.Cut(uri, "://")
	if !ok || scheme != "s3" {
		return "", "", false
	}
	bucket, key, ok = strings.Cut(rest, "/")
	return
}

func escapeS3Key(key string) string {
	parts := strings.Split(key, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func vertexCounts(job map[string]json.RawMessage) map[string]int64 {
	var stats map[string]json.RawMessage
	_ = json.Unmarshal(job["completionStats"], &stats)
	if stats == nil {
		return nil
	}
	out := map[string]int64{}
	for source, target := range map[string]string{"successfulCount": "successful", "failedCount": "failed", "incompleteCount": "incomplete"} {
		if _, present := stats[source]; !present {
			out[target] = 0
			continue
		}
		if bytes.Equal(bytes.TrimSpace(stats[source]), []byte("null")) {
			return nil
		}
		var text string
		if json.Unmarshal(stats[source], &text) == nil {
			n, err := strconv.ParseInt(text, 10, 64)
			if err == nil && n >= 0 {
				out[target] = n
			}
			continue
		}
		var n int64
		if json.Unmarshal(stats[source], &n) == nil && n >= 0 {
			out[target] = n
		}
	}
	if len(out) != 3 {
		return nil
	}
	return out
}

func cloudKnown(dialect, state string) bool { _, ok := cloudState(dialect, state); return ok }
func cloudTerminal(dialect, state string) bool {
	s, ok := cloudState(dialect, state)
	return ok && cloudIsTerminal(s)
}
