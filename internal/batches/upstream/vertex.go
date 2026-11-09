package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/usage"
)

const gcsListingPageSize = "1000"

type vertexReader struct {
	transport transport
}

type vertexJob struct {
	Name            string            `json:"name"`
	State           string            `json:"state"`
	Labels          map[string]string `json:"labels"`
	CreateTime      json.RawMessage   `json:"createTime"`
	EndTime         json.RawMessage   `json:"endTime"`
	CompletionStats *struct {
		Successful *Count `json:"successfulCount"`
		Failed     *Count `json:"failedCount"`
		Incomplete *Count `json:"incompleteCount"`
	} `json:"completionStats"`
	OutputInfo struct {
		Directory string `json:"gcsOutputDirectory"`
	} `json:"outputInfo"`
	InputConfig struct {
		Source struct {
			URIs []string `json:"uris"`
		} `json:"gcsSource"`
	} `json:"inputConfig"`
	OutputConfig struct {
		Destination struct {
			Prefix string `json:"outputUriPrefix"`
		} `json:"gcsDestination"`
	} `json:"outputConfig"`
	Error *struct {
		Code    providerCode `json:"code"`
		Message string       `json:"message"`
	} `json:"error"`
}

func decodeVertexJob(body []byte) (CloudJob, error) {
	var job vertexJob
	if json.Unmarshal(body, &job) != nil {
		return CloudJob{}, ErrProviderConflict
	}
	id, ok := vertexJobID(job.Name)
	if !ok {
		return CloudJob{}, ErrProviderConflict
	}
	status, err := cloudStatus(connectors.BatchVertex, job.State)
	if err != nil {
		return CloudJob{}, err
	}
	expected, err := vertexExpected(job)
	if err != nil {
		return CloudJob{}, ErrProviderConflict
	}
	created, _ := UnixSeconds(job.CreateTime)
	return CloudJob{
		ID: id, State: job.State, Status: status, CreatedAt: created,
		CompletedAt: completedAt(status, job.EndTime), Counts: vertexCounts(job), Error: vertexError(job),
		Labels: job.Labels, InputURI: first(job.InputConfig.Source.URIs), OutputURIPrefix: job.OutputConfig.Destination.Prefix,
		OutputLocation: job.OutputInfo.Directory, expected: expected,
	}, nil
}

func vertexJobID(name string) (string, bool) {
	parts := strings.Split(name, "/")
	if len(parts) != 6 || parts[0] != "projects" || parts[1] == "" || parts[2] != "locations" || parts[3] == "" || parts[4] != "batchPredictionJobs" || parts[5] == "" {
		return "", false
	}
	return parts[5], true
}

func vertexExpected(job vertexJob) (*int64, error) {
	stats := job.CompletionStats
	if stats == nil {
		return nil, nil
	}
	total, err := sumCounts(stats.Successful.orZero(), stats.Failed.orZero(), stats.Incomplete.orZero())
	if err != nil {
		return nil, err
	}
	return &total, nil
}

func vertexCounts(job vertexJob) *CloudCounts {
	stats := job.CompletionStats
	if stats == nil {
		return nil
	}
	return cloudCounts(CloudCounts{Successful: stats.Successful.Int64(), Failed: stats.Failed.Int64(), Incomplete: stats.Incomplete.Int64()})
}

func vertexError(job vertexJob) *CloudError {
	if job.Error == nil {
		return nil
	}
	return cloudError(job.Error.Code, job.Error.Message)
}

func (r vertexReader) Poll(ctx context.Context, settlement batches.Settlement, target Target) (Snapshot, error) {
	endpoint, err := target.Config.VertexBatchJobsURL(settlement.UpstreamID, "")
	if err != nil {
		return Snapshot{}, err
	}
	body, err := r.transport.get(ctx, target, endpoint, listingLimit)
	if err != nil {
		return Snapshot{}, tooLarge(err)
	}
	job, err := decodeVertexJob(body)
	if err != nil {
		return Snapshot{}, err
	}
	if job.ID != settlement.UpstreamID {
		return Snapshot{}, ErrProviderConflict
	}
	return job.snapshot(body), nil
}

func (r vertexReader) Reconcile(ctx context.Context, settlement batches.Settlement, target Target, _ KnownBatch) ([]Snapshot, error) {
	submission, ok := settlement.Submission.(batches.VertexSubmission)
	if !ok || submission.Label == "" {
		return nil, ErrProviderConflict
	}
	endpoint, err := target.Config.VertexBatchJobsURL("", "")
	if err != nil {
		return nil, err
	}
	query := url.Values{"pageSize": {fmt.Sprint(listPageSize)}, "filter": {`labels.olp_submission="` + submission.Label + `"`}}
	body, err := r.transport.get(ctx, target, endpoint+"?"+query.Encode(), listingLimit)
	if err != nil {
		return nil, tooLarge(err)
	}
	var listing struct {
		Jobs []json.RawMessage `json:"batchPredictionJobs"`
	}
	if json.Unmarshal(body, &listing) != nil {
		return nil, ErrProviderConflict
	}
	return vertexMatches(listing.Jobs, submission.Label)
}

func vertexMatches(jobs []json.RawMessage, label string) ([]Snapshot, error) {
	var matches []Snapshot
	for _, raw := range jobs {
		job, err := decodeVertexJob(raw)
		if err != nil {
			return nil, err
		}
		if job.Labels["olp_submission"] == label {
			matches = append(matches, job.snapshot(raw))
		}
	}
	return matches, nil
}

func (vertexReader) Adopt(settlement batches.Settlement, snapshot Snapshot) (Adoption, error) {
	job, err := decodeVertexJob(snapshot.Body)
	if err != nil {
		return Adoption{}, err
	}
	return cloudAdoption(connectors.BatchVertex, settlement, snapshot, job.InputURI, job.OutputURIPrefix)
}

type gcsObject struct {
	Name       string `json:"name"`
	Generation string `json:"generation"`
}

func (o gcsObject) file() string { return o.Name + "#" + o.Generation }

func (r vertexReader) ReadResults(ctx context.Context, target Target, snapshot Snapshot, cursor batches.Cursor, commit PageCommit) (bool, error) {
	if cursor.Phase == batches.PhaseDone {
		return true, nil
	}
	if !snapshot.Terminal {
		return false, nil
	}
	bucket, prefix, err := vertexOutput(target.Config.BatchStorage, snapshot.OutputFileID)
	if err != nil {
		return false, err
	}
	completed, err := vertexCompletedBytes(cursor)
	if err != nil {
		return false, err
	}
	objects, err := r.listObjects(ctx, target, bucket, prefix)
	if err != nil {
		return false, err
	}
	pending, err := vertexPending(objects, cursor)
	if err != nil {
		return false, err
	}
	progress := cursor
	for _, object := range pending {
		next, err := r.readObject(ctx, target, bucket, object, progress, completed, commit)
		if err != nil {
			return false, err
		}
		progress, completed = next, *next.VertexBytes
	}
	progress.Phase = batches.PhaseDone
	return true, commit(nil, progress)
}

func vertexOutput(storage *connectors.BatchStorage, location string) (string, string, error) {
	if storage == nil || !storage.ContainsPrefix(storage.OutputURIPrefix, location) {
		return "", "", ErrStorageScope
	}
	rest, ok := strings.CutPrefix(location, "gs://")
	if !ok {
		return "", "", ErrStorageScope
	}
	bucket, prefix, ok := strings.Cut(rest, "/")
	if !ok {
		return "", "", ErrStorageScope
	}
	if prefix == "" || strings.HasSuffix(prefix, "/") {
		return bucket, prefix, nil
	}
	return bucket, prefix + "/", nil
}

func vertexCompletedBytes(cursor batches.Cursor) (int64, error) {
	if cursor.VertexBytes == nil {
		if cursor.File != "" {
			return 0, batches.ErrCursorConflict
		}
		return 0, nil
	}
	if *cursor.VertexBytes < 0 || *cursor.VertexBytes > batches.MaxBatchBytes {
		return 0, batches.ErrCursorConflict
	}
	return *cursor.VertexBytes, nil
}

func vertexPending(objects []gcsObject, cursor batches.Cursor) ([]gcsObject, error) {
	if cursor.File == "" {
		return objects, nil
	}
	separator := strings.LastIndex(cursor.File, "#")
	if separator < 0 {
		return nil, batches.ErrCursorConflict
	}
	prior := gcsObject{Name: cursor.File[:separator], Generation: cursor.File[separator+1:]}
	index := slices.IndexFunc(objects, func(object gcsObject) bool { return object.Name == prior.Name })
	if index < 0 || objects[index] != prior {
		return nil, batches.ErrCursorConflict
	}
	if cursor.Phase == batches.PhaseVertexDone {
		return objects[index+1:], nil
	}
	return objects[index:], nil
}

func (r vertexReader) listObjects(ctx context.Context, target Target, bucket, prefix string) ([]gcsObject, error) {
	var objects []gcsObject
	err := walkPages(func(token string, _ int) ([]gcsObject, string, error) {
		query := url.Values{"prefix": {prefix}, "maxResults": {gcsListingPageSize}}
		if token != "" {
			query.Set("pageToken", token)
		}
		endpoint := "https://storage.googleapis.com/storage/v1/b/" + url.PathEscape(bucket) + "/o?" + query.Encode()
		body, err := r.transport.getSigned(ctx, target, endpoint, r.transport.googleStorage(target), listingLimit)
		if err != nil {
			return nil, "", err
		}
		var page struct {
			Items []gcsObject `json:"items"`
			Next  string      `json:"nextPageToken"`
		}
		if json.Unmarshal(body, &page) != nil {
			return nil, "", ErrProviderConflict
		}
		return page.Items, page.Next, nil
	}, func(object gcsObject) (bool, error) {
		if strings.HasSuffix(object.Name, ".jsonl") && strings.HasPrefix(path.Base(object.Name), "prediction") {
			objects = append(objects, object)
		}
		return false, nil
	})
	slices.SortFunc(objects, func(a, b gcsObject) int { return strings.Compare(a.Name, b.Name) })
	return objects, err
}

func (r vertexReader) readObject(ctx context.Context, target Target, bucket string, object gcsObject, progress batches.Cursor, completed int64, commit PageCommit) (batches.Cursor, error) {
	query := url.Values{"alt": {"media"}, "generation": {object.Generation}}
	endpoint := "https://storage.googleapis.com/storage/v1/b/" + url.PathEscape(bucket) + "/o/" + url.PathEscape(object.Name) + "?" + query.Encode()
	file := object.file()
	resume := batches.Cursor{Phase: batches.PhaseOutput, File: file}
	if progress.File == file && progress.Phase == batches.PhaseOutput {
		resume.Line, resume.PrefixSHA256 = progress.Line, progress.PrefixSHA256
	}
	parse := func(line []byte, number int64) (batches.Result, error) {
		return parseVertexLine(line, object, number, target.Model)
	}
	withBytes := func(page []batches.Result, next batches.Cursor) error {
		next.VertexBytes = new(completed)
		return commit(page, next)
	}
	read := func(body io.Reader, remaining int64) (int64, int64, error) {
		_, consumed, _, err := readPages(body, resume, file, batches.PhaseOutput, parse, withBytes, remaining)
		return consumed, consumed, err
	}
	consumed, err := stream(ctx, r.transport, target, endpoint, r.transport.googleStorage(target), batches.MaxBatchBytes-completed, read)
	if err != nil {
		return progress, err
	}
	done := batches.Cursor{Phase: batches.PhaseVertexDone, File: file, VertexBytes: new(completed + consumed)}
	return done, commit(nil, done)
}

func parseVertexLine(line []byte, object gcsObject, number int64, model string) (batches.Result, error) {
	evidence, err := canonicalJSON(line)
	if err != nil {
		return batches.Result{}, ErrProviderConflict
	}
	var item struct {
		Status   string          `json:"status"`
		Response json.RawMessage `json:"response"`
	}
	if json.Unmarshal(line, &item) != nil {
		return batches.Result{}, ErrProviderConflict
	}
	id := fmt.Sprintf("%s:%d", object.file(), number)
	if item.Status != "" {
		return batches.Result{CustomID: id, Kind: batches.ResultErrored, ErrorClass: "vertex_batch_errored", Evidence: evidence, Usage: usage.AttemptUsage{Complete: true}}, nil
	}
	if len(item.Response) == 0 {
		return batches.Result{}, ErrProviderConflict
	}
	completion, err := protocols.Decode(wire.Gemini, wire.Chat, item.Response, model, "")
	if err != nil {
		return batches.Result{}, err
	}
	return batches.Result{CustomID: id, Kind: batches.ResultSucceeded, Usage: usage.ObservedAttemptUsage(completion.Usage, completion.Usage != nil, string(wire.LineGeneration)), Evidence: evidence}, nil
}
