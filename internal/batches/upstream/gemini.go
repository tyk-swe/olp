package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

const (
	geminiSummaryFields = "name,done,error,metadata(state,createTime,inputConfig(fileName),batchStats)"
	geminiListingFields = "operations(name,done,error(code),metadata(state,createTime,inputConfig(fileName),batchStats)),nextPageToken"
)

var geminiStatuses = map[string]string{
	"BATCH_STATE_PENDING": "validating", "JOB_STATE_PENDING": "validating", "JOB_STATE_QUEUED": "validating",
	"BATCH_STATE_RUNNING": "in_progress", "JOB_STATE_RUNNING": "in_progress",
	"BATCH_STATE_CANCELLING": "cancelling", "JOB_STATE_CANCELLING": "cancelling",
	"BATCH_STATE_SUCCEEDED": "completed", "JOB_STATE_SUCCEEDED": "completed",
	"BATCH_STATE_FAILED": "failed", "JOB_STATE_FAILED": "failed",
	"BATCH_STATE_CANCELLED": "cancelled", "JOB_STATE_CANCELLED": "cancelled",
	"BATCH_STATE_EXPIRED": "expired", "JOB_STATE_EXPIRED": "expired",
}

var (
	geminiLifecycleScalars = []string{"name", "model", "state", "displayName", "createTime", "startTime", "endTime", "updateTime", "done",
		"requestCount", "successfulRequestCount", "failedRequestCount", "pendingRequestCount", "fileName", "file_name", "responsesFile", "code"}
	geminiLifecycleObjects = append([]string{"metadata", "batchStats", "inputConfig", "input_config", "error"}, GeminiOutputFields()...)
)

func GeminiOutputFields() []string {
	return []string{"output", "response"}
}

func GeminiStatus(state string) (string, bool) {
	status, ok := geminiStatuses[state]
	return status, ok
}

type geminiInput struct {
	FileName      string `json:"fileName"`
	FileNameSnake string `json:"file_name"`
}

type inlineResponses struct {
	items   []json.RawMessage
	present bool
}

func (r *inlineResponses) UnmarshalJSON(raw []byte) error {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) == nil {
		*r = inlineResponses{items: items, present: items != nil}
		return nil
	}
	var nested struct {
		Items []json.RawMessage `json:"inlinedResponses"`
	}
	if err := json.Unmarshal(raw, &nested); err != nil {
		return err
	}
	*r = inlineResponses{items: nested.Items, present: nested.Items != nil}
	return nil
}

type geminiOutput struct {
	ResponsesFile    string          `json:"responsesFile"`
	InlinedResponses inlineResponses `json:"inlinedResponses"`
}

type geminiFields struct {
	State       string       `json:"state"`
	CreateTime  string       `json:"createTime"`
	InputConfig *geminiInput `json:"inputConfig"`
	InputSnake  *geminiInput `json:"input_config"`
	BatchStats  *struct {
		RequestCount *Count `json:"requestCount"`
	} `json:"batchStats"`
	Output   *geminiOutput `json:"output"`
	Response *geminiOutput `json:"response"`
}

func (f geminiFields) inputFile() string {
	names := []string{}
	for _, input := range []*geminiInput{f.InputConfig, f.InputSnake} {
		if input != nil {
			names = append(names, input.FileName, input.FileNameSnake)
		}
	}
	return firstNonEmpty(names...)
}

func (f geminiFields) outputs() []geminiOutput {
	return slices.Collect(func(yield func(geminiOutput) bool) {
		for _, output := range []*geminiOutput{f.Output, f.Response} {
			if output != nil && !yield(*output) {
				return
			}
		}
	})
}

type GeminiBatch struct {
	ID         string
	State      string
	InputFile  string
	OutputFile string
	CreatedAt  time.Time
	Expected   *int64
	inline     inlineResponses
}

func DecodeGeminiBatch(body []byte) (GeminiBatch, error) {
	var object struct {
		Name     string        `json:"name"`
		Metadata *geminiFields `json:"metadata"`
		geminiFields
	}
	if json.Unmarshal(body, &object) != nil {
		return GeminiBatch{}, ErrProviderConflict
	}
	id, ok := strings.CutPrefix(object.Name, "batches/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return GeminiBatch{}, ErrProviderConflict
	}
	layers := []geminiFields{object.geminiFields}
	if object.Metadata != nil {
		layers = []geminiFields{*object.Metadata, object.geminiFields}
	}
	return GeminiBatch{
		ID:         id,
		State:      firstLayer(layers, func(f geminiFields) string { return f.State }),
		InputFile:  firstLayer(layers, geminiFields.inputFile),
		OutputFile: firstLayer(layers, geminiOutputFile),
		CreatedAt:  firstLayer(layers, geminiCreatedAt),
		Expected:   firstLayer(layers, geminiRequestCount),
		inline:     geminiInline(layers),
	}, nil
}

func firstLayer[T comparable](layers []geminiFields, get func(geminiFields) T) T {
	var zero T
	for _, layer := range layers {
		if value := get(layer); value != zero {
			return value
		}
	}
	return zero
}

func geminiOutputFile(f geminiFields) string {
	files := []string{}
	for _, output := range f.outputs() {
		files = append(files, output.ResponsesFile)
	}
	return firstNonEmpty(files...)
}

func geminiCreatedAt(f geminiFields) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, f.CreateTime)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func geminiRequestCount(f geminiFields) *int64 {
	if f.BatchStats == nil {
		return nil
	}
	return f.BatchStats.RequestCount.Int64()
}

func geminiInline(layers []geminiFields) inlineResponses {
	for _, layer := range layers {
		for _, output := range layer.outputs() {
			if output.InlinedResponses.present {
				return output.InlinedResponses
			}
		}
	}
	return inlineResponses{}
}

func (b GeminiBatch) snapshot(body []byte) (Snapshot, error) {
	status, known := GeminiStatus(b.State)
	if !known {
		return Snapshot{}, ErrProviderConflict
	}
	output := b.OutputFile
	if output == "" && b.inline.present {
		output = "inline:" + b.ID
	}
	return Snapshot{
		ID: b.ID, Status: b.State, InputFileID: b.InputFile, OutputFileID: output, Expected: b.Expected,
		CreatedAt: b.CreatedAt, Body: bytes.Clone(body), Terminal: wire.BatchStageOf(status) == wire.BatchTerminal,
	}, nil
}

func decodeGeminiSnapshot(body []byte) (Snapshot, error) {
	batch, err := DecodeGeminiBatch(body)
	if err != nil {
		return Snapshot{}, err
	}
	return batch.snapshot(body)
}

func GeminiMetadata(body []byte, model, localInput, upstreamInput string) (json.RawMessage, error) {
	fields, err := geminiLifecycle(body)
	if err != nil {
		return nil, err
	}
	extras := map[string]any{"upstream_model": model}
	if localInput != "" {
		extras["local_input_file_name"], extras["upstream_input_file_name"] = localInput, upstreamInput
	}
	return mergeFields(fields, extras)
}

func geminiLifecycle(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var source map[string]json.RawMessage
	if json.Unmarshal(raw, &source) != nil {
		return nil, errors.New("Gemini batch lifecycle is malformed")
	}
	fields := make(map[string]json.RawMessage, len(source))
	for key, value := range source {
		if slices.Contains(geminiLifecycleScalars, key) {
			fields[key] = value
			continue
		}
		if !slices.Contains(geminiLifecycleObjects, key) {
			continue
		}
		nested, err := geminiLifecycle(value)
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(nested)
		if err != nil {
			return nil, err
		}
		fields[key] = encoded
	}
	return fields, nil
}
