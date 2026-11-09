package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/usage"
)

const bedrockManifest = "manifest.json.out"

type bedrockReader struct {
	transport transport
}

type bedrockDataConfig struct {
	Input struct {
		S3 struct {
			URI string `json:"s3Uri"`
		} `json:"s3InputDataConfig"`
	} `json:"inputDataConfig"`
	Output struct {
		S3 struct {
			URI string `json:"s3Uri"`
		} `json:"s3OutputDataConfig"`
	} `json:"outputDataConfig"`
}

type bedrockJob struct {
	bedrockDataConfig
	ARN              string          `json:"jobArn"`
	Status           string          `json:"status"`
	Message          string          `json:"message"`
	SubmitTime       json.RawMessage `json:"submitTime"`
	EndTime          json.RawMessage `json:"endTime"`
	LastModifiedTime json.RawMessage `json:"lastModifiedTime"`
	Total            *Count          `json:"totalRecordCount"`
	Succeeded        *Count          `json:"successRecordCount"`
	Errored          *Count          `json:"errorRecordCount"`
	Processed        *Count          `json:"processedRecordCount"`
}

func decodeBedrockJob(body []byte) (CloudJob, error) {
	var job bedrockJob
	if json.Unmarshal(body, &job) != nil || job.ARN == "" {
		return CloudJob{}, ErrProviderConflict
	}
	status, err := cloudStatus(connectors.BatchBedrock, job.Status)
	if err != nil {
		return CloudJob{}, err
	}
	created, _ := UnixSeconds(job.SubmitTime)
	return CloudJob{
		ID: job.ARN, State: job.Status, Status: status, CreatedAt: created,
		CompletedAt: completedAt(status, job.EndTime, job.LastModifiedTime),
		Counts:      cloudCounts(CloudCounts{Total: job.Total.Int64(), Successful: job.Succeeded.Int64(), Failed: job.Errored.Int64(), Processed: job.Processed.Int64()}),
		Error:       cloudError("", job.Message), InputURI: job.Input.S3.URI, OutputURIPrefix: job.Output.S3.URI,
		OutputLocation: strings.TrimRight(job.Output.S3.URI, "/") + "/" + lastSegment(job.ARN) + "/",
		expected:       new(int64(1)),
	}, nil
}

func (r bedrockReader) Poll(ctx context.Context, settlement batches.Settlement, target Target) (Snapshot, error) {
	return r.poll(ctx, target, settlement.UpstreamID)
}

func (r bedrockReader) poll(ctx context.Context, target Target, arn string) (Snapshot, error) {
	if arn == "" {
		return Snapshot{}, ErrProviderConflict
	}
	endpoint, err := target.Config.BedrockBatchJobsURL(arn, "")
	if err != nil {
		return Snapshot{}, err
	}
	body, err := r.transport.get(ctx, target, endpoint, listingLimit)
	if err != nil {
		return Snapshot{}, tooLarge(err)
	}
	job, err := decodeBedrockJob(body)
	if err != nil {
		return Snapshot{}, err
	}
	if job.ID != arn {
		return Snapshot{}, ErrProviderConflict
	}
	return job.snapshot(body), nil
}

func (r bedrockReader) Reconcile(ctx context.Context, settlement batches.Settlement, target Target, _ KnownBatch) ([]Snapshot, error) {
	submission, ok := settlement.Submission.(batches.BedrockSubmission)
	if !ok || submission.ClientRequestToken == "" || len(submission.CreateBody) == 0 {
		return nil, ErrProviderConflict
	}
	endpoint, err := target.Config.BedrockBatchJobsURL("", "create")
	if err != nil {
		return nil, err
	}
	status, body, err := r.transport.call(ctx, target, http.MethodPost, endpoint, submission.CreateBody, r.transport.provider(target, target.Config, submission.CreateBody), listingLimit)
	if err != nil {
		return nil, tooLarge(err)
	}
	if bedrockRejectedReissue(status, body) {
		return nil, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("Bedrock batch reissue returned HTTP %d", status)
	}
	var created struct {
		ARN string `json:"jobArn"`
	}
	if json.Unmarshal(body, &created) != nil || created.ARN == "" {
		return nil, ErrProviderConflict
	}
	snapshot, err := r.poll(ctx, target, created.ARN)
	if err != nil {
		return nil, err
	}
	return []Snapshot{snapshot}, nil
}

func bedrockRejectedReissue(status int, body []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusConflict {
		return false
	}
	var failure struct {
		Type string `json:"__type"`
	}
	if json.Unmarshal(body, &failure) != nil {
		return false
	}
	return strings.Contains(failure.Type, "ValidationException") || strings.Contains(failure.Type, "ConflictException")
}

func (bedrockReader) Adopt(settlement batches.Settlement, snapshot Snapshot) (Adoption, error) {
	submission, ok := settlement.Submission.(batches.BedrockSubmission)
	if !ok {
		return Adoption{}, ErrProviderConflict
	}
	var request bedrockDataConfig
	if json.Unmarshal(submission.CreateBody, &request) != nil {
		return Adoption{}, ErrProviderConflict
	}
	return cloudAdoption(connectors.BatchBedrock, settlement, snapshot, request.Input.S3.URI, request.Output.S3.URI)
}

func (r bedrockReader) ReadResults(ctx context.Context, target Target, snapshot Snapshot, cursor batches.Cursor, commit PageCommit) (bool, error) {
	if cursor.Phase == batches.PhaseDone {
		return true, nil
	}
	if !snapshot.Terminal {
		return false, nil
	}
	endpoint, err := bedrockManifestURL(target.Config, snapshot.OutputFileID)
	if err != nil {
		return false, err
	}
	status, body, err := r.transport.call(ctx, target, http.MethodGet, endpoint, nil, r.transport.s3(target), objectLimit)
	if err != nil {
		return false, tooLarge(err)
	}
	done := batches.Cursor{Phase: batches.PhaseDone, File: bedrockManifest, PrefixSHA256: emptyPrefixDigest}
	switch status {
	case http.StatusNotFound:
		return true, commit(nil, done)
	case http.StatusOK:
		aggregate, err := parseBedrockManifest(body)
		if err != nil {
			return false, err
		}
		return true, commit([]batches.Result{aggregate}, done)
	default:
		return false, fmt.Errorf("Bedrock S3 manifest returned HTTP %d", status)
	}
}

func bedrockManifestURL(config connectors.Config, location string) (string, error) {
	storage := config.BatchStorage
	if storage == nil || !storage.ContainsPrefix(storage.OutputURIPrefix, location) {
		return "", ErrStorageScope
	}
	rest, ok := strings.CutPrefix(location+bedrockManifest, "s3://")
	if !ok {
		return "", ErrStorageScope
	}
	bucket, key, ok := strings.Cut(rest, "/")
	if !ok {
		return "", ErrStorageScope
	}
	endpoint, err := connectors.BedrockEndpoint(config.CloudRegion, true)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	partition, ok := strings.CutPrefix(parsed.Host, "bedrock.")
	if !ok {
		return "", errors.New("Bedrock partition endpoint is not recognized")
	}
	if strings.Contains(bucket, ".") {
		return "https://s3." + partition + "/" + bucket + "/" + escapeS3Key(key), nil
	}
	return "https://" + bucket + ".s3." + partition + "/" + escapeS3Key(key), nil
}

func escapeS3Key(key string) string {
	segments := strings.Split(key, "/")
	escaped := make([]string, len(segments))
	for index, segment := range segments {
		escaped[index] = url.PathEscape(segment)
	}
	return strings.Join(escaped, "/")
}

func parseBedrockManifest(body []byte) (batches.Result, error) {
	canonical, err := canonicalJSON(body)
	if err != nil {
		return batches.Result{}, ErrProviderConflict
	}
	var manifest struct {
		Total     *Count `json:"totalRecordCount"`
		Processed *Count `json:"processedRecordCount"`
		Input     *Count `json:"inputTokenCount"`
		Output    *Count `json:"outputTokenCount"`
	}
	if json.Unmarshal(body, &manifest) != nil || manifest.Total == nil || manifest.Processed == nil || manifest.Input == nil || manifest.Output == nil || *manifest.Processed > *manifest.Total {
		return batches.Result{}, ErrProviderConflict
	}
	total, err := sumCounts(*manifest.Input, *manifest.Output)
	if err != nil {
		return batches.Result{}, ErrProviderConflict
	}
	tokens := &wire.Usage{InputTokens: int64(*manifest.Input), OutputTokens: int64(*manifest.Output), TotalTokens: total}
	evidence := usage.ObservedAttemptUsage(tokens, *manifest.Processed == *manifest.Total, string(wire.LineGeneration))
	evidence.Batch = true
	return batches.Result{CustomID: "manifest", Kind: batches.ResultAggregate, Usage: evidence, Evidence: canonical}, nil
}
