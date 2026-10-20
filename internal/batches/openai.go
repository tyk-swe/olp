package batches

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/usage"
)

const (
	openAIListLimit      = 100
	openAIMaxPages       = 10
	openAIRequestTimeout = 5 * time.Minute
)

var ErrProviderConflict = errors.New("provider batch identity changed")

type PinnedTarget struct {
	Config connectors.Config
	Model  string
	Secret []byte
	Client *http.Client
}

type BatchSnapshot struct {
	ID           string
	Status       string
	Endpoint     string
	InputFileID  string
	OutputFileID string
	ErrorFileID  string
	Expected     *int64
	CreatedAt    time.Time
	Body         []byte
	Terminal     bool
}

type OpenAIReader struct {
	Auth   *connectors.Auth
	Egress *egress.Policy
}

type Reconciliation struct {
	Matches []BatchSnapshot
}

func (r *OpenAIReader) Poll(ctx context.Context, settlement Settlement, target *PinnedTarget) (BatchSnapshot, error) {
	if settlement.UpstreamID == nil || *settlement.UpstreamID == "" {
		return BatchSnapshot{}, errors.New("batch upstream identity is unavailable")
	}
	path := "batches/" + url.PathEscape(*settlement.UpstreamID)
	body, err := r.getJSON(ctx, target, path, nil, 1<<20)
	if err != nil {
		return BatchSnapshot{}, err
	}
	snapshot, err := parseBatchSnapshot(body)
	if err != nil {
		return BatchSnapshot{}, err
	}
	if snapshot.ID != *settlement.UpstreamID {
		return BatchSnapshot{}, ErrProviderConflict
	}
	if snapshot.Endpoint != "" && (lineOperation(snapshot.Endpoint) == "" ||
		lineOperation(snapshot.Endpoint) != settlement.LineOperation) {
		return BatchSnapshot{}, ErrProviderConflict
	}
	inputFileID, _, endpoint, err := submissionFiles(settlement.Cursor)
	if err != nil {
		return BatchSnapshot{}, err
	}
	if snapshot.InputFileID != "" && inputFileID != "" && snapshot.InputFileID != inputFileID {
		return BatchSnapshot{}, ErrProviderConflict
	}
	if snapshot.Endpoint != endpoint {
		return BatchSnapshot{}, ErrProviderConflict
	}
	return snapshot, nil
}

func (r *OpenAIReader) Reconcile(ctx context.Context, settlement Settlement, target *PinnedTarget, inputFileID, endpoint string) (Reconciliation, error) {
	var matches []BatchSnapshot
	after := ""
	for page := 0; page < openAIMaxPages; page++ {
		query := url.Values{"limit": {strconv.Itoa(openAIListLimit)}}
		if after != "" {
			query.Set("after", after)
		}
		body, err := r.getJSON(ctx, target, "batches", query, 4<<20)
		if err != nil {
			return Reconciliation{}, err
		}
		var listing struct {
			Data    []json.RawMessage `json:"data"`
			HasMore bool              `json:"has_more"`
			LastID  string            `json:"last_id"`
		}
		if err = json.Unmarshal(body, &listing); err != nil {
			return Reconciliation{}, errors.New("provider batch list is malformed")
		}
		var candidates []BatchSnapshot
		for _, raw := range listing.Data {
			snapshot, parseErr := parseBatchSnapshot(raw)
			if parseErr != nil {
				return Reconciliation{}, parseErr
			}
			candidates = append(candidates, snapshot)
		}
		matches = append(matches, MatchSubmission(candidates, inputFileID, settlement.CreatedAt.Add(-5*time.Minute))...)
		if len(matches) > 1 {
			return Reconciliation{Matches: matches}, nil
		}
		if !listing.HasMore {
			if len(matches) == 1 && matches[0].Endpoint != endpoint {
				return Reconciliation{}, ErrProviderConflict
			}
			return Reconciliation{Matches: matches}, nil
		}
		if listing.LastID == "" && len(listing.Data) > 0 {
			var last struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(listing.Data[len(listing.Data)-1], &last) == nil {
				listing.LastID = last.ID
			}
		}
		if listing.LastID == "" || listing.LastID == after {
			return Reconciliation{}, errors.New("provider batch list cursor is malformed")
		}
		after = listing.LastID
	}
	if len(matches) == 1 && matches[0].Endpoint != endpoint {
		return Reconciliation{}, ErrProviderConflict
	}
	return Reconciliation{}, errors.New("provider batch list exceeded the reconciliation page limit")
}

func MatchSubmission(candidates []BatchSnapshot, inputFileID string, createdAfter time.Time) []BatchSnapshot {
	var matches []BatchSnapshot
	for _, candidate := range candidates {
		if candidate.InputFileID == inputFileID && !candidate.CreatedAt.Before(createdAfter) {
			matches = append(matches, candidate)
		}
	}
	return matches
}

func (r *OpenAIReader) ReadResults(ctx context.Context, target *PinnedTarget, snapshot BatchSnapshot, cursor Cursor, commit ResultPageCommit) (Cursor, bool, error) {
	if cursor.Phase == "done" {
		return cursor, true, nil
	}
	total := int64(0)
	if cursor.Phase == "error" {
		if cursor.OutputPrefixSHA256 != "" {
			outputHash, read, err := r.hashFile(ctx, target, snapshot.OutputFileID, MaxBatchBytes-total)
			if err != nil {
				return cursor, false, err
			}
			total += read
			if outputHash != cursor.OutputPrefixSHA256 {
				return cursor, false, ErrCursorConflict
			}
		}
		return r.readErrorFile(ctx, target, snapshot, cursor, total, snapshot.Terminal, commit)
	}

	current := cursor
	if snapshot.OutputFileID != "" {
		if current.Phase == "output" && current.File != "" && current.File != snapshot.OutputFileID {
			return cursor, false, ErrCursorConflict
		}
		var outputHash string
		var err error
		current, total, outputHash, err = r.readFilePages(ctx, target, snapshot.OutputFileID, "output", current, total,
			func(line []byte) (Result, error) { return ParseLine(line, snapshot.Endpoint, false) }, commit)
		if err != nil {
			return current, false, err
		}
		current.OutputPrefixSHA256 = outputHash
	} else if current.Phase == "output" && current.File != "" {
		return cursor, false, ErrCursorConflict
	} else {
		current.OutputPrefixSHA256 = emptyPrefixDigest()
	}
	if snapshot.ErrorFileID != "" {
		current = Cursor{
			Phase:              "error",
			File:               snapshot.ErrorFileID,
			OutputPrefixSHA256: current.OutputPrefixSHA256,
			PrefixSHA256:       emptyPrefixDigest(),
		}
		if err := commit(nil, current); err != nil {
			return current, false, err
		}
		return r.readErrorFile(ctx, target, snapshot, current, total, snapshot.Terminal, commit)
	}
	if !snapshot.Terminal {
		return current, false, nil
	}
	current.Phase = "done"
	current.File = snapshot.OutputFileID
	current.Line = 0
	current.PrefixSHA256 = ""
	if err := commit(nil, current); err != nil {
		return current, false, err
	}
	return current, true, nil
}

func (r *OpenAIReader) readErrorFile(ctx context.Context, target *PinnedTarget, snapshot BatchSnapshot, cursor Cursor, total int64, terminal bool, commit ResultPageCommit) (Cursor, bool, error) {
	if snapshot.ErrorFileID == "" {
		if cursor.File != "" || cursor.Line > 0 {
			return cursor, false, ErrCursorConflict
		}
		if terminal {
			cursor.Phase = "done"
			if err := commit(nil, cursor); err != nil {
				return cursor, false, err
			}
		}
		return cursor, terminal, nil
	}
	if cursor.File != "" && cursor.File != snapshot.ErrorFileID {
		return cursor, false, ErrCursorConflict
	}
	next, _, _, err := r.readFilePages(ctx, target, snapshot.ErrorFileID, "error", cursor, total,
		func(line []byte) (Result, error) { return ParseLine(line, snapshot.Endpoint, true) }, commit)
	if err != nil {
		return next, false, err
	}
	if terminal {
		next.Phase = "done"
		if err = commit(nil, next); err != nil {
			return next, false, err
		}
	}
	return next, terminal, nil
}

func (r *OpenAIReader) readFilePages(ctx context.Context, target *PinnedTarget, fileID, phase string, cursor Cursor, total int64, parse ResultLineParser, commit ResultPageCommit) (Cursor, int64, string, error) {
	if fileID == "" {
		return cursor, total, emptyPrefixDigest(), nil
	}
	remaining := int64(MaxBatchBytes) - total
	if remaining < 0 {
		return cursor, total, "", ErrBatchTooLarge
	}
	requestCtx, cancel := context.WithTimeout(ctx, openAIRequestTimeout)
	defer cancel()
	response, err := r.getFile(requestCtx, target, fileID)
	if err != nil {
		return cursor, total, "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return cursor, total, "", fmt.Errorf("provider result file returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > remaining {
		return cursor, total, "", ErrBatchTooLarge
	}
	limited := &io.LimitedReader{R: response.Body, N: remaining + 1}
	next, consumed, fullHash, err := ReadJSONLPages(limited, cursor, fileID, phase, parse, commit)
	if err != nil {
		return next, total + consumed, "", err
	}
	if limited.N == 0 || consumed > remaining {
		return next, total + consumed, "", ErrBatchTooLarge
	}
	return next, total + consumed, fullHash, nil
}

func (r *OpenAIReader) hashFile(ctx context.Context, target *PinnedTarget, fileID string, remaining int64) (string, int64, error) {
	if fileID == "" {
		return emptyPrefixDigest(), 0, nil
	}
	requestCtx, cancel := context.WithTimeout(ctx, openAIRequestTimeout)
	defer cancel()
	response, err := r.getFile(requestCtx, target, fileID)
	if err != nil {
		return "", 0, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", 0, fmt.Errorf("provider result file returned HTTP %d", response.StatusCode)
	}
	if remaining < 0 || response.ContentLength > remaining {
		return "", 0, ErrBatchTooLarge
	}
	limited := &io.LimitedReader{R: response.Body, N: remaining + 1}
	hash, consumed, err := hashJSONL(limited)
	if err != nil {
		return "", consumed, err
	}
	if limited.N == 0 || consumed > remaining {
		return "", consumed, ErrBatchTooLarge
	}
	return hash, consumed, nil
}

func hashJSONL(reader io.Reader) (string, int64, error) {
	buffered := newJSONLReader(reader)
	hash := sha256New()
	var total int64
	for {
		raw, ok, err := buffered.readLine()
		if err != nil {
			return "", total, err
		}
		if !ok {
			return hex.EncodeToString(hash.Sum(nil)), total, nil
		}
		total += int64(len(raw))
		if total > MaxBatchBytes {
			return "", total, ErrBatchTooLarge
		}
		hash.Write(raw)
	}
}

func (r *OpenAIReader) getJSON(ctx context.Context, target *PinnedTarget, path string, query url.Values, limit int64) ([]byte, error) {
	requestCtx, cancel := context.WithTimeout(ctx, openAIRequestTimeout)
	defer cancel()
	response, err := r.do(requestCtx, target, http.MethodGet, path, query, nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("provider batch request returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(body)) > limit {
		return nil, errors.New("provider batch response exceeds the limit")
	}
	return body, nil
}

func (r *OpenAIReader) getFile(ctx context.Context, target *PinnedTarget, fileID string) (*http.Response, error) {
	if fileID == "" || len(fileID) > 512 || strings.ContainsAny(fileID, "/\\?#\x00") {
		return nil, errors.New("provider returned an invalid result file identifier")
	}
	return r.do(ctx, target, http.MethodGet, "files/"+url.PathEscape(fileID)+"/content", nil, nil)
}

func (r *OpenAIReader) do(ctx context.Context, target *PinnedTarget, method, path string, query url.Values, body []byte) (*http.Response, error) {
	if target == nil || target.Client == nil || r.Auth == nil || r.Egress == nil {
		return nil, errors.New("batch provider transport is unavailable")
	}
	endpoint, err := target.Config.ResourceURL(target.Model, path, query)
	if err != nil {
		return nil, errors.New("provider resource address is invalid")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.User != nil {
		return nil, errors.New("provider resource address is invalid")
	}
	origin := *parsed
	origin.Path = strings.TrimSuffix(origin.Path, "/"+strings.TrimPrefix(path, "/"))
	origin.RawQuery, origin.Fragment = "", ""
	if _, err = r.Egress.ValidateEndpoint(origin.String()); err != nil {
		return nil, errors.New("provider resource address is outside egress policy")
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, errors.New("build provider batch request")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "olp/batch-settlement")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if _, err = r.Auth.Apply(ctx, request, target.Config, target.Secret, body); err != nil {
		return nil, errors.New("provider batch credential could not be applied")
	}
	response, err := target.Client.Do(request)
	if err != nil {
		return nil, errors.New("provider batch request failed")
	}
	return response, nil
}

func parseBatchSnapshot(body []byte) (BatchSnapshot, error) {
	var object struct {
		ID            string          `json:"id"`
		Status        string          `json:"status"`
		Endpoint      string          `json:"endpoint"`
		InputFileID   string          `json:"input_file_id"`
		OutputFileID  string          `json:"output_file_id"`
		ErrorFileID   string          `json:"error_file_id"`
		CreatedAt     json.RawMessage `json:"created_at"`
		RequestCounts *struct {
			Total *int64 `json:"total"`
		} `json:"request_counts"`
	}
	if err := json.Unmarshal(body, &object); err != nil || object.ID == "" || len(object.ID) > 512 || object.Status == "" ||
		!validBatchStatus(object.Status) || len(object.InputFileID) > 512 ||
		len(object.OutputFileID) > 512 || len(object.ErrorFileID) > 512 {
		return BatchSnapshot{}, errors.New("provider batch object is malformed")
	}
	snapshot := BatchSnapshot{
		ID: object.ID, Status: object.Status, Endpoint: object.Endpoint, InputFileID: object.InputFileID,
		OutputFileID: object.OutputFileID, ErrorFileID: object.ErrorFileID, Body: append([]byte(nil), body...),
		Terminal: terminalStatus(object.Status),
	}
	if object.RequestCounts != nil && object.RequestCounts.Total != nil {
		if *object.RequestCounts.Total < 0 {
			return BatchSnapshot{}, errors.New("provider batch result count is invalid")
		}
		total := *object.RequestCounts.Total
		snapshot.Expected = &total
	}
	if len(object.CreatedAt) > 0 && string(object.CreatedAt) != "null" {
		var seconds int64
		if json.Unmarshal(object.CreatedAt, &seconds) == nil {
			snapshot.CreatedAt = time.Unix(seconds, 0).UTC()
		} else {
			var text string
			if json.Unmarshal(object.CreatedAt, &text) != nil {
				return BatchSnapshot{}, errors.New("provider batch creation time is invalid")
			}
			parsed, err := time.Parse(time.RFC3339Nano, text)
			if err != nil {
				return BatchSnapshot{}, errors.New("provider batch creation time is invalid")
			}
			snapshot.CreatedAt = parsed
		}
	}
	return snapshot, nil
}

func validBatchStatus(status string) bool {
	switch status {
	case "validating", "failed", "in_progress", "finalizing", "completed", "expired", "cancelling", "cancelled":
		return true
	}
	return false
}

func terminalStatus(status string) bool {
	switch status {
	case "completed", "failed", "expired", "cancelled":
		return true
	}
	return false
}

func ParseLine(line []byte, endpoint string, errorFile bool) (Result, error) {
	canonical, err := canonicalJSON(line)
	if err != nil {
		return Result{}, errors.New("provider batch result line is malformed")
	}
	var object struct {
		CustomID string `json:"custom_id"`
		Response *struct {
			StatusCode int             `json:"status_code"`
			Body       json.RawMessage `json:"body"`
		} `json:"response"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err = json.Unmarshal(canonical, &object); err != nil || object.CustomID == "" {
		return Result{}, errors.New("provider batch result line has no custom_id")
	}
	result := Result{
		CustomID: object.CustomID,
		Kind:     ResultErrored,
		Usage:    usage.AttemptUsage{Observed: false, Complete: false, BillingUncertain: true},
		Evidence: canonical,
	}
	errorCode := ""
	if object.Error != nil {
		errorCode = object.Error.Code
	}
	if !errorFile && object.Response != nil {
		status := object.Response.StatusCode
		if status < 100 || status > 599 {
			return Result{}, errors.New("provider batch response status is invalid")
		}
		result.StatusCode = &status
		if len(object.Response.Body) > 0 && string(object.Response.Body) != "null" {
			var bodyObject map[string]json.RawMessage
			if json.Unmarshal(object.Response.Body, &bodyObject) != nil {
				return Result{}, errors.New("provider batch response body is malformed")
			}
			if rawError, ok := bodyObject["error"]; ok && string(rawError) != "null" {
				var providerError struct {
					Code string `json:"code"`
				}
				_ = json.Unmarshal(rawError, &providerError)
				if providerError.Code != "" {
					errorCode = providerError.Code
				}
			}
			if errorCode == "" {
				observed, complete, usageErr := decodeLineUsage(object.Response.Body, endpoint)
				if usageErr != nil {
					if status >= 200 && status < 300 {
						return Result{}, usageErr
					}
				} else if observed != nil {
					result.Usage = usage.ObservedAttemptUsage(observed, complete, lineOperation(endpoint))
				}
			}
			if status >= 200 && status < 300 && errorCode == "" {
				result.Kind = ResultSucceeded
			}
		}
	}
	switch strings.ToLower(errorCode) {
	case "batch_expired":
		result.Kind = ResultExpired
		result.ErrorClass = "batch_expired"
	case "batch_cancelled", "batch_canceled":
		result.Kind = ResultCancelled
		result.ErrorClass = "batch_cancelled"
	default:
		if result.Kind != ResultSucceeded {
			result.ErrorClass = "batch_line_error"
		}
	}
	return result, nil
}

func decodeLineUsage(body json.RawMessage, endpoint string) (*wire.Usage, bool, error) {
	var completion *wire.Completion
	var err error
	switch endpoint {
	case "/v1/chat/completions":
		completion, err = protocols.Decode(wire.Chat, wire.Chat, body, "batch", "")
	case "/v1/completions":
		completion, err = protocols.Decode(wire.Completions, wire.Completions, body, "batch", "")
	case "/v1/responses":
		observation, observeErr := openai.ObserveResponseUsage(body)
		if observeErr != nil {
			return nil, false, errors.New("provider batch response usage is malformed")
		}
		return observation.Usage, observation.Complete, nil
	case "/v1/embeddings":
		completion, err = protocols.Decode(wire.Embeddings, wire.Embeddings, body, "batch", "")
	default:
		return nil, false, errors.New("provider batch endpoint is unsupported")
	}
	if err != nil || completion == nil {
		return nil, false, errors.New("provider batch response usage is malformed")
	}
	return completion.Usage, completion.UsageComplete || completion.Usage != nil, nil
}

func lineOperation(endpoint string) string {
	switch endpoint {
	case "/v1/embeddings":
		return "embeddings"
	case "/v1/chat/completions", "/v1/responses", "/v1/completions":
		return "generation"
	}
	return ""
}

func canonicalJSON(raw []byte) ([]byte, error) {
	if _, err := oif.ParseJSON(raw, oif.Limits{MaxBytes: MaxLineBytes}); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("trailing JSON data")
	}
	return json.Marshal(value)
}

func emptyPrefixDigest() string {
	sum := sha256.Sum256(nil)
	return hex.EncodeToString(sum[:])
}

type jsonlReader struct {
	reader *bufio.Reader
}

func newJSONLReader(reader io.Reader) *jsonlReader {
	return &jsonlReader{reader: bufio.NewReaderSize(reader, 64<<10)}
}

func (r *jsonlReader) readLine() ([]byte, bool, error) {
	return readJSONLLine(r.reader)
}

func sha256New() hash.Hash {
	return sha256.New()
}
