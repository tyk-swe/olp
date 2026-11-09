package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

type filesFlavor struct {
	adapter  connectors.BatchAdapter
	jobsPath string
	decode   func([]byte) (Snapshot, error)
	query    func(token string, index int) url.Values
	listing  func(body []byte, index int) ([]json.RawMessage, string, error)
}

var (
	openAIFlavor = filesFlavor{
		adapter: connectors.BatchOpenAI, jobsPath: "batches", decode: decodeOpenAIBatch,
		query: func(token string, _ int) url.Values {
			query := url.Values{"limit": {strconv.Itoa(listPageSize)}}
			if token != "" {
				query.Set("after", token)
			}
			return query
		},
		listing: openAIListing,
	}
	togetherFlavor = filesFlavor{
		adapter: connectors.BatchTogether, jobsPath: "batches",
		decode: func(body []byte) (Snapshot, error) { return decodeManagedSnapshot(connectors.BatchTogether, body) },
		query:  func(string, int) url.Values { return url.Values{} },
		listing: func(body []byte, _ int) ([]json.RawMessage, string, error) {
			var items []json.RawMessage
			if json.Unmarshal(body, &items) != nil {
				return nil, "", errors.New("Together batch list is malformed")
			}
			return items, "", nil
		},
	}
	mistralFlavor = filesFlavor{
		adapter: connectors.BatchMistral, jobsPath: "batch/jobs",
		decode: func(body []byte) (Snapshot, error) { return decodeManagedSnapshot(connectors.BatchMistral, body) },
		query: func(_ string, index int) url.Values {
			return url.Values{"page": {strconv.Itoa(index)}, "page_size": {strconv.Itoa(listPageSize)}}
		},
		listing: mistralListing,
	}
)

var managedStatuses = map[connectors.BatchAdapter]map[string]string{
	connectors.BatchTogether: {"VALIDATING": "validating", "IN_PROGRESS": "in_progress", "COMPLETED": "completed", "FAILED": "failed", "EXPIRED": "expired", "CANCELLING": "cancelling", "CANCELLED": "cancelled"},
	connectors.BatchMistral:  {"QUEUED": "validating", "RUNNING": "in_progress", "SUCCESS": "completed", "FAILED": "failed", "TIMEOUT_EXCEEDED": "expired", "CANCELLATION_REQUESTED": "cancelling", "CANCELLED": "cancelled"},
}

func JobsPath(adapter connectors.BatchAdapter, id string, cancel bool) string {
	switch {
	case adapter == connectors.BatchGeminiManaged && cancel:
		return "batches/" + url.PathEscape(id) + ":cancel"
	case adapter == connectors.BatchMistral && cancel:
		return mistralFlavor.jobsPath + "/" + url.PathEscape(id) + "/cancel"
	case adapter == connectors.BatchMistral:
		return mistralFlavor.jobsPath + "/" + url.PathEscape(id)
	case cancel:
		return "batches/" + url.PathEscape(id) + "/cancel"
	default:
		return "batches/" + url.PathEscape(id)
	}
}

type filesReader struct {
	transport transport
	flavor    filesFlavor
}

func filesSubmission(settlement batches.Settlement) (batches.FilesSubmission, error) {
	submission, ok := settlement.Submission.(batches.FilesSubmission)
	if !ok || settlement.UpstreamID == "" && submission.InputFileID == "" {
		return batches.FilesSubmission{}, errors.New("batch submission identity is unavailable")
	}
	return submission, nil
}

func (r filesReader) Poll(ctx context.Context, settlement batches.Settlement, target Target) (Snapshot, error) {
	submission, err := filesSubmission(settlement)
	if err != nil || settlement.UpstreamID == "" {
		return Snapshot{}, errors.New("batch upstream identity is unavailable")
	}
	body, err := r.transport.getResource(ctx, target, JobsPath(r.flavor.adapter, settlement.UpstreamID, false), nil, objectLimit)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot, err := r.flavor.decode(body)
	if err != nil {
		return Snapshot{}, err
	}
	operation, known := wire.BatchLineOperation(snapshot.Endpoint)
	if snapshot.ID != settlement.UpstreamID || snapshot.Endpoint != submission.Endpoint || !known ||
		operation != settlement.LineOperation ||
		snapshot.InputFileID != "" && submission.InputFileID != "" && snapshot.InputFileID != submission.InputFileID {
		return Snapshot{}, ErrProviderConflict
	}
	if r.flavor.adapter != connectors.BatchOpenAI {
		snapshot.Expected = settlement.ExpectedResults
	}
	return snapshot, nil
}

func (r filesReader) Reconcile(ctx context.Context, settlement batches.Settlement, target Target, known KnownBatch) ([]Snapshot, error) {
	submission, ok := settlement.Submission.(batches.FilesSubmission)
	if !ok || submission.InputFileID == "" {
		return nil, ErrNoEvidence
	}
	createdAfter := settlement.CreatedAt.Add(-reconcileSkew)
	found, err := findSubmission(ctx, known, func(token string, index int) ([]Snapshot, string, error) {
		return r.listPage(ctx, target, token, index)
	}, func(candidate Snapshot) bool {
		return candidate.InputFileID == submission.InputFileID && !candidate.CreatedAt.Before(createdAfter)
	})
	if err != nil {
		return nil, err
	}
	if len(found) == 1 && found[0].Endpoint != submission.Endpoint {
		return nil, ErrProviderConflict
	}
	return found, nil
}

func (r filesReader) listPage(ctx context.Context, target Target, token string, index int) ([]Snapshot, string, error) {
	body, err := r.transport.getResource(ctx, target, r.flavor.jobsPath, r.flavor.query(token, index), listingLimit)
	if err != nil {
		return nil, "", err
	}
	items, next, err := r.flavor.listing(body, index)
	if err != nil {
		return nil, "", err
	}
	snapshots := make([]Snapshot, 0, len(items))
	for _, item := range items {
		snapshot, err := r.flavor.decode(item)
		if err != nil {
			return nil, "", err
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, next, nil
}

func openAIListing(body []byte, _ int) ([]json.RawMessage, string, error) {
	var listing struct {
		Data    []json.RawMessage `json:"data"`
		HasMore bool              `json:"has_more"`
		LastID  string            `json:"last_id"`
	}
	if json.Unmarshal(body, &listing) != nil {
		return nil, "", errors.New("provider batch list is malformed")
	}
	if !listing.HasMore {
		return listing.Data, "", nil
	}
	if listing.LastID != "" || len(listing.Data) == 0 {
		return listing.Data, listing.LastID, nil
	}
	var last struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(listing.Data[len(listing.Data)-1], &last) != nil || last.ID == "" {
		return nil, "", errListCursor
	}
	return listing.Data, last.ID, nil
}

func mistralListing(body []byte, index int) ([]json.RawMessage, string, error) {
	var listing struct {
		Data    []json.RawMessage `json:"data"`
		HasMore bool              `json:"has_more"`
		Total   int               `json:"total"`
	}
	if json.Unmarshal(body, &listing) != nil {
		return nil, "", errors.New("Mistral batch list is malformed")
	}
	if !listing.HasMore && (len(listing.Data) < listPageSize || listing.Total <= (index+1)*listPageSize) {
		return listing.Data, "", nil
	}
	return listing.Data, strconv.Itoa(index + 1), nil
}

func (r filesReader) ReadResults(ctx context.Context, target Target, snapshot Snapshot, cursor batches.Cursor, commit PageCommit) (bool, error) {
	switch cursor.Phase {
	case batches.PhaseDone:
		return true, nil
	case batches.PhaseError:
		total, err := r.verifyOutput(ctx, target, snapshot, cursor)
		if err != nil {
			return false, err
		}
		return r.readErrors(ctx, target, snapshot, cursor, total, commit)
	}
	output, total, err := r.readOutput(ctx, target, snapshot, cursor, commit)
	if err != nil {
		return false, err
	}
	if snapshot.ErrorFileID != "" {
		start := batches.Cursor{
			Phase: batches.PhaseError, File: snapshot.ErrorFileID,
			OutputPrefixSHA256: output.OutputPrefixSHA256, PrefixSHA256: emptyPrefixDigest,
		}
		if err = commit(nil, start); err != nil {
			return false, err
		}
		return r.readErrors(ctx, target, snapshot, start, total, commit)
	}
	if !snapshot.Terminal {
		return false, nil
	}
	done := batches.Cursor{
		Phase: batches.PhaseDone, File: snapshot.OutputFileID,
		OutputPrefixSHA256: output.OutputPrefixSHA256, VertexBytes: output.VertexBytes,
	}
	return true, commit(nil, done)
}

func (r filesReader) readOutput(ctx context.Context, target Target, snapshot Snapshot, cursor batches.Cursor, commit PageCommit) (batches.Cursor, int64, error) {
	resuming := cursor.Phase == batches.PhaseOutput && cursor.File != ""
	if snapshot.OutputFileID == "" {
		if resuming {
			return cursor, 0, batches.ErrCursorConflict
		}
		return batches.Cursor{OutputPrefixSHA256: emptyPrefixDigest}, 0, nil
	}
	if resuming && cursor.File != snapshot.OutputFileID {
		return cursor, 0, batches.ErrCursorConflict
	}
	read, err := r.readFile(ctx, target, snapshot.OutputFileID, batches.PhaseOutput, cursor, 0, snapshot.Endpoint, commit)
	if err != nil {
		return cursor, 0, err
	}
	read.cursor.OutputPrefixSHA256 = read.digest
	return read.cursor, read.consumed, nil
}

func (r filesReader) verifyOutput(ctx context.Context, target Target, snapshot Snapshot, cursor batches.Cursor) (int64, error) {
	if cursor.OutputPrefixSHA256 == "" {
		return 0, nil
	}
	if snapshot.OutputFileID == "" {
		if cursor.OutputPrefixSHA256 != emptyPrefixDigest {
			return 0, batches.ErrCursorConflict
		}
		return 0, nil
	}
	endpoint, err := r.fileURL(target, snapshot.OutputFileID)
	if err != nil {
		return 0, err
	}
	type hashed struct {
		digest   string
		consumed int64
	}
	result, err := stream(ctx, r.transport, target, endpoint, r.transport.provider(target, target.Config, nil), batches.MaxBatchBytes,
		func(body io.Reader, remaining int64) (hashed, int64, error) {
			digest, consumed, err := hashLines(body, remaining)
			return hashed{digest: digest, consumed: consumed}, consumed, err
		})
	if err != nil {
		return 0, err
	}
	if result.digest != cursor.OutputPrefixSHA256 {
		return 0, batches.ErrCursorConflict
	}
	return result.consumed, nil
}

func (r filesReader) readErrors(ctx context.Context, target Target, snapshot Snapshot, cursor batches.Cursor, total int64, commit PageCommit) (bool, error) {
	if snapshot.ErrorFileID == "" {
		if cursor.File != "" || cursor.Line > 0 {
			return false, batches.ErrCursorConflict
		}
		if !snapshot.Terminal {
			return false, nil
		}
		cursor.Phase = batches.PhaseDone
		return true, commit(nil, cursor)
	}
	if cursor.File != "" && cursor.File != snapshot.ErrorFileID {
		return false, batches.ErrCursorConflict
	}
	read, err := r.readFile(ctx, target, snapshot.ErrorFileID, batches.PhaseError, cursor, total, snapshot.Endpoint, commit)
	if err != nil || !snapshot.Terminal {
		return false, err
	}
	read.cursor.Phase = batches.PhaseDone
	return true, commit(nil, read.cursor)
}

type fileRead struct {
	cursor   batches.Cursor
	consumed int64
	digest   string
}

func (r filesReader) readFile(ctx context.Context, target Target, fileID string, phase batches.Phase, cursor batches.Cursor, total int64, endpoint string, commit PageCommit) (fileRead, error) {
	address, err := r.fileURL(target, fileID)
	if err != nil {
		return fileRead{}, err
	}
	parse := func(line []byte, _ int64) (batches.Result, error) {
		return ParseLine(line, endpoint, phase == batches.PhaseError)
	}
	return stream(ctx, r.transport, target, address, r.transport.provider(target, target.Config, nil), batches.MaxBatchBytes-total,
		func(body io.Reader, remaining int64) (fileRead, int64, error) {
			next, consumed, digest, err := readPages(body, cursor, fileID, phase, parse, commit, remaining)
			return fileRead{cursor: next, consumed: total + consumed, digest: digest}, consumed, err
		})
}

func (r filesReader) fileURL(target Target, fileID string) (string, error) {
	if fileID == "" || len(fileID) > 512 || strings.ContainsAny(fileID, "/\\?#\x00") {
		return "", errors.New("provider returned an invalid result file identifier")
	}
	return r.transport.resourceURL(target, "files/"+url.PathEscape(fileID)+"/content", nil)
}

func (r filesReader) Adopt(settlement batches.Settlement, snapshot Snapshot) (Adoption, error) {
	submission, err := filesSubmission(settlement)
	if err != nil {
		return Adoption{}, err
	}
	if r.flavor.adapter == connectors.BatchOpenAI {
		return FilesMetadata(r.flavor.adapter, snapshot.Body, settlement.Model(), submission.LocalInputFileID, nil)
	}
	window := submission.CompletionWindow
	if window == "" {
		window = "24h"
	}
	projected, _, err := ProjectManagedBatch(r.flavor.adapter, snapshot.Body, window)
	if err != nil {
		return Adoption{}, err
	}
	return FilesMetadata(r.flavor.adapter, projected, settlement.Model(), submission.LocalInputFileID, submission.ClientMetadata)
}

func FilesMetadata(adapter connectors.BatchAdapter, body []byte, model, localInput string, clientMetadata json.RawMessage) (Adoption, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil || object == nil {
		return Adoption{}, errors.New("provider batch object is malformed")
	}
	var status struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(body, &status) != nil {
		return Adoption{}, errors.New("provider batch status is malformed")
	}
	extras := map[string]any{}
	if model != "" {
		extras["upstream_model"] = model
	}
	if localInput != "" {
		extras["local_input_file_id"] = localInput
	}
	if adapter != connectors.BatchOpenAI {
		extras["adapter"] = adapter
	}
	if !isNull(clientMetadata) {
		extras["metadata"] = clientMetadata
	}
	metadata, err := mergeFields(object, extras, "id", "local_input_file_id")
	if err != nil {
		return Adoption{}, err
	}
	state := status.Status
	if state == "" {
		state = "validating"
	}
	return Adoption{State: state, Metadata: metadata}, nil
}

func mergeFields(base map[string]json.RawMessage, extras map[string]any, drop ...string) (json.RawMessage, error) {
	fields := maps.Collect(func(yield func(string, any) bool) {
		for key, value := range base {
			if !slices.Contains(drop, key) && !yield(key, value) {
				return
			}
		}
		for key, value := range extras {
			if !yield(key, value) {
				return
			}
		}
	})
	return json.Marshal(fields)
}

func decodeOpenAIBatch(body []byte) (Snapshot, error) {
	var object struct {
		ID            string          `json:"id"`
		Status        string          `json:"status"`
		Endpoint      string          `json:"endpoint"`
		InputFileID   string          `json:"input_file_id"`
		OutputFileID  string          `json:"output_file_id"`
		ErrorFileID   string          `json:"error_file_id"`
		CreatedAt     json.RawMessage `json:"created_at"`
		RequestCounts *struct {
			Total *Count `json:"total"`
		} `json:"request_counts"`
	}
	if json.Unmarshal(body, &object) != nil || object.ID == "" || len(object.ID) > 512 ||
		wire.BatchStageOf(object.Status) == wire.BatchUnknown || len(object.InputFileID) > 512 ||
		len(object.OutputFileID) > 512 || len(object.ErrorFileID) > 512 {
		return Snapshot{}, errors.New("provider batch object is malformed")
	}
	createdAt, err := optionalTime(object.CreatedAt)
	if err != nil {
		return Snapshot{}, err
	}
	var expected *int64
	if object.RequestCounts != nil {
		expected = object.RequestCounts.Total.Int64()
	}
	return Snapshot{
		ID: object.ID, Status: object.Status, Endpoint: object.Endpoint, InputFileID: object.InputFileID,
		OutputFileID: object.OutputFileID, ErrorFileID: object.ErrorFileID, Expected: expected, CreatedAt: createdAt,
		Body: append([]byte(nil), body...), Terminal: wire.BatchStageOf(object.Status) == wire.BatchTerminal,
	}, nil
}

func optionalTime(raw json.RawMessage) (time.Time, error) {
	if isNull(raw) {
		return time.Time{}, nil
	}
	seconds, ok := UnixSeconds(raw)
	if !ok {
		return time.Time{}, errors.New("provider batch creation time is invalid")
	}
	return time.Unix(seconds, 0).UTC(), nil
}

type managedJob struct {
	ID            string          `json:"id"`
	Status        string          `json:"status"`
	Endpoint      string          `json:"endpoint"`
	InputFileID   string          `json:"input_file_id"`
	InputFiles    []string        `json:"input_files"`
	OutputFileID  string          `json:"output_file_id"`
	ErrorFileID   string          `json:"error_file_id"`
	OutputFile    string          `json:"output_file"`
	ErrorFile     string          `json:"error_file"`
	CreatedAt     json.RawMessage `json:"created_at"`
	CompletedAt   json.RawMessage `json:"completed_at"`
	TotalRequests *Count          `json:"total_requests"`
	Succeeded     *Count          `json:"succeeded_requests"`
	Failed        *Count          `json:"failed_requests"`
	Metadata      json.RawMessage `json:"metadata"`
	state         string
}

func decodeManagedJob(adapter connectors.BatchAdapter, body []byte) (managedJob, error) {
	var envelope struct {
		Job json.RawMessage `json:"job"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return managedJob{}, errors.New("provider batch object is malformed")
	}
	raw := body
	if adapter == connectors.BatchTogether && !isNull(envelope.Job) {
		raw = envelope.Job
	}
	var job managedJob
	if json.Unmarshal(raw, &job) != nil || job.ID == "" {
		return managedJob{}, errors.New("provider batch object is malformed")
	}
	state, known := managedStatuses[adapter][job.Status]
	if !known {
		return managedJob{}, errors.New("provider batch status is unknown")
	}
	job.state = state
	job.InputFileID = firstNonEmpty(job.InputFileID, first(job.InputFiles))
	job.OutputFileID = firstNonEmpty(job.OutputFileID, job.OutputFile)
	job.ErrorFileID = firstNonEmpty(job.ErrorFileID, job.ErrorFile)
	if len(job.ID) > 512 || job.Endpoint == "" || job.InputFileID == "" || len(job.InputFileID) > 512 ||
		len(job.OutputFileID) > 512 || len(job.ErrorFileID) > 512 {
		return managedJob{}, errors.New("provider batch object is malformed")
	}
	return job, nil
}

func decodeManagedSnapshot(adapter connectors.BatchAdapter, body []byte) (Snapshot, error) {
	job, err := decodeManagedJob(adapter, body)
	if err != nil {
		return Snapshot{}, err
	}
	createdAt, err := optionalTime(job.CreatedAt)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{
		ID: job.ID, Status: job.state, Endpoint: job.Endpoint, InputFileID: job.InputFileID,
		OutputFileID: job.OutputFileID, ErrorFileID: job.ErrorFileID, Expected: job.TotalRequests.Int64(),
		CreatedAt: createdAt, Body: append([]byte(nil), body...), Terminal: wire.BatchStageOf(job.state) == wire.BatchTerminal,
	}, nil
}

func ProjectManagedBatch(adapter connectors.BatchAdapter, body []byte, window string) ([]byte, string, error) {
	job, err := decodeManagedJob(adapter, body)
	if err != nil {
		return nil, "", err
	}
	projected := map[string]any{
		"id": job.ID, "object": "batch", "endpoint": job.Endpoint,
		"input_file_id": job.InputFileID, "completion_window": window, "status": job.state,
	}
	if len(job.Metadata) > 0 {
		projected["metadata"] = job.Metadata
	}
	if job.OutputFileID != "" {
		projected["output_file_id"] = job.OutputFileID
	}
	if job.ErrorFileID != "" {
		projected["error_file_id"] = job.ErrorFileID
	}
	for key, raw := range map[string]json.RawMessage{"created_at": job.CreatedAt, "completed_at": job.CompletedAt} {
		if seconds, ok := UnixSeconds(raw); ok {
			projected[key] = seconds
		} else if len(raw) > 0 && isNull(raw) {
			projected[key] = nil
		}
	}
	if job.TotalRequests != nil && job.Succeeded != nil && job.Failed != nil {
		projected["request_counts"] = map[string]Count{"total": *job.TotalRequests, "completed": *job.Succeeded, "failed": *job.Failed}
	}
	encoded, err := json.Marshal(projected)
	return encoded, job.ID, err
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}
