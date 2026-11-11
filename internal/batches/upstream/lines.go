package upstream

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/usage"
)

func ParseLine(line []byte, endpoint string, errorFile bool) (batches.Result, error) {
	canonical, err := canonicalJSON(line)
	if err != nil {
		return batches.Result{}, errors.New("provider batch result line is malformed")
	}
	var object struct {
		CustomID string `json:"custom_id"`
		Response *struct {
			StatusCode int             `json:"status_code"`
			Body       json.RawMessage `json:"body"`
		} `json:"response"`
		Error json.RawMessage `json:"error"`
	}
	if err = json.Unmarshal(canonical, &object); err != nil || object.CustomID == "" {
		return batches.Result{}, errors.New("provider batch result line has no custom_id")
	}
	if object.Response == nil {
		return classifyLine(batches.Result{
			CustomID: object.CustomID, Kind: batches.ResultErrored, Evidence: canonical,
			Usage: usage.AttemptUsage{Evidence: usage.EvidenceUnobservedUncertain},
		}, errorCode(object.Error)), nil
	}
	status := object.Response.StatusCode
	if status < 100 || status > 599 {
		return batches.Result{}, errors.New("provider batch response status is invalid")
	}
	result := batches.Result{
		CustomID: object.CustomID, Kind: batches.ResultErrored, StatusCode: &status, Evidence: canonical,
		Usage: usage.AttemptUsage{Evidence: usage.EvidenceUnobservedUncertain},
	}
	if isNull(object.Response.Body) {
		return classifyLine(result, errorCode(object.Error)), nil
	}
	var body struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(object.Response.Body, &body) != nil {
		return batches.Result{}, errors.New("provider batch response body is malformed")
	}
	code := errorCode(object.Error)
	if bodyCode := errorCode(body.Error); bodyCode != "" {
		code = bodyCode
	}
	succeeded := status >= 200 && status < 300 && code == "" && !errorFile
	observed, complete, usageErr := decodeLineUsage(object.Response.Body, endpoint)
	switch {
	case usageErr != nil && succeeded:
		return batches.Result{}, usageErr
	case usageErr == nil && observed != nil:
		operation, _ := wire.BatchLineOperation(endpoint)
		result.Usage = usage.ObservedAttemptUsage(observed, complete, string(operation))
	}
	if succeeded {
		result.Kind = batches.ResultSucceeded
	}
	return classifyLine(result, code), nil
}

func errorCode(raw json.RawMessage) string {
	var detail struct {
		Code string `json:"code"`
	}
	if isNull(raw) || json.Unmarshal(raw, &detail) != nil {
		return ""
	}
	return detail.Code
}

func classifyLine(result batches.Result, code string) batches.Result {
	unbilled := usage.AttemptUsage{Evidence: usage.EvidenceUnobservedComplete, UsageQuantities: usage.UsageQuantities{Batch: true}}
	switch strings.ToLower(code) {
	case "batch_expired":
		result.Kind, result.ErrorClass = batches.ResultExpired, "batch_expired"
	case "batch_cancelled", "batch_canceled":
		result.Kind, result.ErrorClass = batches.ResultCancelled, "batch_cancelled"
	default:
		if result.Kind != batches.ResultSucceeded {
			result.ErrorClass = "batch_line_error"
		}
		return result
	}
	if !result.Usage.Evidence.Observed() {
		result.Usage = unbilled
	}
	return result
}

func decodeLineUsage(body json.RawMessage, endpoint string) (*wire.Usage, bool, error) {
	canonical := wire.CanonicalBatchEndpoint(endpoint)
	if canonical == "/v1/responses" {
		observation, err := openai.ObserveResponseUsage(body)
		if err != nil {
			return nil, false, errors.New("provider batch response usage is malformed")
		}
		return observation.Usage, observation.Complete, nil
	}
	format, ok := lineFormat(canonical)
	if !ok {
		return nil, false, errors.New("provider batch endpoint is unsupported")
	}
	completion, err := protocols.Decode(format, format, body, "batch", "")
	if err != nil || completion == nil {
		return nil, false, errors.New("provider batch response usage is malformed")
	}
	return completion.Usage, completion.UsageComplete || completion.Usage != nil, nil
}

func lineFormat(canonicalEndpoint string) (wire.Format, bool) {
	switch canonicalEndpoint {
	case "/v1/chat/completions":
		return wire.Chat, true
	case "/v1/completions":
		return wire.Completions, true
	case "/v1/embeddings":
		return wire.Embeddings, true
	default:
		return "", false
	}
}
