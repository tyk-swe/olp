package management

import (
	"bytes"
	"encoding/json"
	"strings"
)

type ErrorKind string

const (
	InvalidInput    ErrorKind = "invalid_input"
	Local           ErrorKind = "local"
	Transport       ErrorKind = "transport"
	API             ErrorKind = "api"
	InvalidResponse ErrorKind = "invalid_response"
)

type CallError struct {
	Kind   ErrorKind
	Status int
	Code   string
	cause  error
}

func (err *CallError) Error() string { return err.cause.Error() }
func (err *CallError) Unwrap() error { return err.cause }

func responseError(status int, cause error) *CallError {
	if status < 200 || status >= 300 {
		return &CallError{Kind: API, Status: status, cause: cause}
	}
	return &CallError{Kind: InvalidResponse, cause: cause}
}

func apiError(status int, data []byte) *CallError {
	const prefix = "https://openllmproxy.dev/problems/"
	var problem struct {
		Type string `json:"type"`
	}
	err := json.Unmarshal(data, &problem)
	code, found := strings.CutPrefix(problem.Type, prefix)
	cause := &APIError{Status: status, Code: legacyProblemCode(code, found)}
	if err != nil || !found || !knownProblemCode(code) || !hasSingleProblemType(data) {
		return &CallError{Kind: API, Status: status, cause: cause}
	}
	return &CallError{Kind: API, Status: status, Code: code, cause: cause}
}

func legacyProblemCode(code string, found bool) string {
	if !found || strings.ContainsAny(code, " /\\\r\n\t") || len(code) > 100 {
		return "request_failed"
	}
	return code
}

func knownProblemCode(code string) bool {
	switch code {
	case "already_exists", "authentication_required", "etag_mismatch", "idempotency_conflict",
		"idempotency_key_required", "invalid_identifier", "invalid_json", "not_found", "permission_denied",
		"precondition_required", "service_unavailable", "unsupported_media_type", "validation_failed":
		return true
	default:
		return false
	}
}

func hasSingleProblemType(data []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return false
	}
	var found bool
	for decoder.More() {
		name, err := decoder.Token()
		if err != nil {
			return false
		}
		if key, ok := name.(string); ok && strings.EqualFold(key, "type") {
			if found || key != "type" {
				return false
			}
			found = true
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return false
		}
	}
	return found
}
