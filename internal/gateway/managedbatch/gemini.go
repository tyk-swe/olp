package managedbatch

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/tyk-swe/olp/internal/batches/upstream"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

var (
	ErrGeminiManagedBatchState = errors.New("Gemini managed batch state is unknown")
	ErrGeminiBatchFidelity     = errors.New("Gemini batch contains an unprojected provider identifier")
)

type GeminiCounts struct {
	Total     *int64 `json:"total,omitempty"`
	Completed *int64 `json:"completed,omitempty"`
	Failed    *int64 `json:"failed,omitempty"`
}

type GeminiRecord struct {
	Manifest
	ProviderBatch    json.RawMessage `json:"provider_batch"`
	UpstreamModel    string          `json:"upstream_model"`
	CompletionWindow string          `json:"completion_window"`
	LocalInputFileID string          `json:"local_input_file_id"`
	Status           string          `json:"status"`
	ClientMetadata   json.RawMessage `json:"client_metadata,omitempty"`
	CreatedAt        *int64          `json:"created_at,omitempty"`
	CompletedAt      *int64          `json:"completed_at,omitempty"`
	RequestCounts    *GeminiCounts   `json:"request_counts,omitempty"`
	ResponsesFile    string          `json:"gemini_responses_file,omitempty"`
}

type GeminiState struct {
	Metadata json.RawMessage
	Status   string
}

func NewGeminiRecord(localInput string, manifest Manifest, clientMetadata json.RawMessage) GeminiRecord {
	record := GeminiRecord{Manifest: manifest, CompletionWindow: "24h", LocalInputFileID: localInput}
	if string(clientMetadata) != "null" {
		record.ClientMetadata = clientMetadata
	}
	return record
}

func DecodeGeminiRecord(metadata json.RawMessage) (GeminiRecord, error) {
	var record GeminiRecord
	if err := json.Unmarshal(metadata, &record); err != nil {
		return GeminiRecord{}, ErrGeminiBatchFidelity
	}
	return record, nil
}

func GeminiBatchMetadata(body []byte, model string, base GeminiRecord) (GeminiState, error) {
	var provider struct {
		State    string `json:"state"`
		Metadata struct {
			State      string            `json:"state"`
			CreateTime json.RawMessage   `json:"createTime"`
			EndTime    json.RawMessage   `json:"endTime"`
			BatchStats *geminiBatchStats `json:"batchStats"`
		} `json:"metadata"`
		BatchStats *geminiBatchStats `json:"batchStats"`
	}
	if err := json.Unmarshal(body, &provider); err != nil {
		return GeminiState{}, errors.New("Gemini batch object is malformed")
	}
	status, ok := upstream.GeminiStatus(cmp.Or(provider.Metadata.State, provider.State))
	if !ok {
		return GeminiState{}, ErrGeminiManagedBatchState
	}
	lifecycle, err := upstream.GeminiMetadata(body, model, "", "")
	if err != nil {
		return GeminiState{}, err
	}
	record := base
	record.Adapter = connectors.BatchGeminiManaged
	record.ProviderBatch, record.UpstreamModel, record.Status = lifecycle, model, status
	record.CreatedAt = optionalUnix(provider.Metadata.CreateTime)
	record.CompletedAt = optionalUnix(provider.Metadata.EndTime)
	record.RequestCounts = cmp.Or(provider.Metadata.BatchStats, provider.BatchStats).counts()
	encoded, err := json.Marshal(record)
	return GeminiState{Metadata: encoded, Status: status}, err
}

type geminiBatchStats struct {
	RequestCount *upstream.Count `json:"requestCount"`
	Successful   *upstream.Count `json:"successfulRequestCount"`
	Failed       *upstream.Count `json:"failedRequestCount"`
}

func (s *geminiBatchStats) counts() *GeminiCounts {
	if s == nil || s.RequestCount == nil && s.Successful == nil && s.Failed == nil {
		return nil
	}
	return &GeminiCounts{Total: countValue(s.RequestCount), Completed: countValue(s.Successful), Failed: countValue(s.Failed)}
}

func countValue(count *upstream.Count) *int64 {
	if count == nil {
		return nil
	}
	return count.Int64()
}

func optionalUnix(raw json.RawMessage) *int64 {
	seconds, ok := upstream.UnixSeconds(raw)
	if !ok {
		return nil
	}
	return &seconds
}

func TranslateGeminiStream(src io.Reader, dst io.Writer, routeSlug, role string) error {
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 64<<10), LineLimit+2)
	for scanner.Scan() {
		encoded, ok, err := translateGeminiLine(scanner.Bytes(), routeSlug, role)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if _, err := dst.Write(append(encoded, '\n')); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return fmt.Errorf("%w: %w", ErrGeminiBatchFidelity, err)
		}
		return err
	}
	return nil
}

func translateGeminiLine(raw []byte, routeSlug, role string) ([]byte, bool, error) {
	var line struct {
		Key      string          `json:"key"`
		Response json.RawMessage `json:"response"`
		Error    json.RawMessage `json:"error"`
	}
	hasResponse := func() bool { return len(line.Response) > 0 && string(line.Response) != "null" }
	hasError := func() bool { return len(line.Error) > 0 && string(line.Error) != "null" }
	if json.Unmarshal(raw, &line) != nil || line.Key == "" || (!hasResponse() && !hasError()) || (hasResponse() && hasError()) {
		return nil, false, ErrGeminiBatchFidelity
	}
	if role == "output" && !hasResponse() || role == "error" && !hasError() {
		return nil, false, nil
	}
	var translated map[string]any
	var err error
	if role == "output" {
		translated, err = translateGeminiOutputLine(line.Key, line.Response, routeSlug)
	} else {
		translated, err = translateGeminiErrorLine(line.Key, line.Error)
	}
	if err != nil {
		return nil, false, err
	}
	encoded, err := json.Marshal(translated)
	if err != nil {
		return nil, false, ErrGeminiBatchFidelity
	}
	return encoded, true, nil
}

func translateGeminiOutputLine(key string, response json.RawMessage, routeSlug string) (map[string]any, error) {
	completion, err := protocols.Decode(wire.Gemini, wire.Chat, response, routeSlug, "")
	if err != nil || completion == nil {
		return nil, ErrGeminiBatchFidelity
	}
	body := completion.Body
	if completion.UpstreamID == "" {
		var fields map[string]json.RawMessage
		if json.Unmarshal(body, &fields) != nil {
			return nil, ErrGeminiBatchFidelity
		}
		delete(fields, "id")
		body = mustJSON(fields)
	}
	var decoded any
	if json.Unmarshal(body, &decoded) != nil {
		return nil, ErrGeminiBatchFidelity
	}
	return map[string]any{
		"custom_id": key,
		"response":  map[string]any{"status_code": http.StatusOK, "body": decoded},
		"error":     nil,
	}, nil
}

func translateGeminiErrorLine(key string, rawError json.RawMessage) (map[string]any, error) {
	var providerError struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(rawError, &providerError) != nil {
		return nil, ErrGeminiBatchFidelity
	}
	code := ""
	if json.Unmarshal(providerError.Code, &code) != nil {
		var number json.Number
		if json.Unmarshal(providerError.Code, &number) == nil {
			code = number.String()
		}
	}
	return map[string]any{
		"custom_id": key, "response": nil,
		"error": map[string]string{"code": code, "message": providerError.Message},
	}, nil
}

func GeminiBatchHasFileReference(request oif.Value) bool {
	var parts func(oif.Value) bool
	parts = func(values oif.Value) bool {
		for _, part := range values.Elements() {
			for _, field := range part.Members() {
				switch field.Name {
				case "fileData", "file_data":
					for _, source := range field.Value.Members() {
						if source.Name == "fileUri" || source.Name == "file_uri" {
							return true
						}
					}
				case "functionResponse", "function_response":
					for _, response := range field.Value.Members() {
						if response.Name == "parts" && parts(response.Value) {
							return true
						}
					}
				}
			}
		}
		return false
	}
	content := func(value oif.Value) bool {
		for _, field := range value.Members() {
			if field.Name == "parts" && parts(field.Value) {
				return true
			}
		}
		return false
	}
	for _, field := range request.Members() {
		switch field.Name {
		case "cachedContent", "cached_content":
			if field.Value.Kind() != oif.Null {
				return true
			}
		case "systemInstruction", "system_instruction":
			if content(field.Value) {
				return true
			}
		case "contents":
			for _, item := range field.Value.Elements() {
				if content(item) {
					return true
				}
			}
		}
	}
	return false
}
