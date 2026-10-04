package gateway

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/tyk-swe/olp/internal/limits"
)

// Error is an OpenAI-shaped error returned to inference clients.
type Error struct {
	Status     int
	Type       string
	Code       string
	Message    string
	Param      *string
	RetryAfter time.Duration
	NoRetry    bool
	// rate is the allowance of the key whose request limits refused the request,
	// as its reservation measured it, for the rate-limit headers of a 429. It is
	// zero for every other error.
	rate limits.RateState
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// body renders the native OpenAI error envelope.
func (e *Error) body() []byte {
	data, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": e.Message,
		"type":    e.Type,
		"param":   e.Param,
		"code":    e.Code,
	}})
	return data
}

func writeError(w http.ResponseWriter, e *Error) { writeSurfaceError(w, e, "openai") }
func writeSurfaceError(w http.ResponseWriter, e *Error, surface string) {
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(responseWriteTimeout))
	h := w.Header()
	h.Set("Content-Type", "application/json")
	if e.NoRetry {
		h.Set("X-Should-Retry", "false")
	}
	if surface == "bedrock" {
		h.Set("X-Amzn-Errortype", e.Code)
	}
	if e.RetryAfter > 0 {
		h.Set("Retry-After", strconv.FormatInt(int64(math.Ceil(e.RetryAfter.Seconds())), 10))
	}
	setRateLimitHeaders(h, surface, e.rate)
	w.WriteHeader(e.Status)
	w.Write(e.surfaceBody(surface))
}

func invalidRequest(code, message string, param *string) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "invalid_request_error", Code: code, Message: message, Param: param}
}

func authenticationError(code, message string) *Error {
	return &Error{Status: http.StatusUnauthorized, Type: "authentication_error", Code: code, Message: message}
}

func permissionError(code, message string) *Error {
	return &Error{Status: http.StatusForbidden, Type: "permission_error", Code: code, Message: message}
}

func notFoundError(code, message string) *Error {
	return &Error{Status: http.StatusNotFound, Type: "invalid_request_error", Code: code, Message: message}
}

func serverError(status int, code, message string) *Error {
	return &Error{Status: status, Type: "server_error", Code: code, Message: message}
}

func modelNotFound(model string) *Error {
	return notFoundError("route_not_found", "The model `"+model+"` does not exist or you do not have access to it.")
}

var overloaded = &Error{
	Status:     http.StatusServiceUnavailable,
	Type:       "server_error",
	Code:       "request_admission_overloaded",
	Message:    "The gateway is at its in-flight request limit. Retry shortly.",
	RetryAfter: time.Second,
}

func (e *Error) surfaceBody(surface string) []byte {
	if surface == "anthropic" {
		kind := e.Type
		switch e.Status {
		case 404:
			kind = "not_found_error"
		case 413:
			kind = "request_too_large"
		case 429:
			kind = "rate_limit_error"
		case 500, 502, 504:
			kind = "api_error"
		case 503:
			kind = "overloaded_error"
		}
		b, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": e.Message, "code": e.Code, "param": e.Param}})
		return b
	}
	if surface == "gemini" {
		status := "INTERNAL"
		switch e.Status {
		case 400, 413, 415, 422:
			status = "INVALID_ARGUMENT"
		case 401:
			status = "UNAUTHENTICATED"
		case 403:
			status = "PERMISSION_DENIED"
		case 404:
			status = "NOT_FOUND"
		case 429:
			status = "RESOURCE_EXHAUSTED"
		case 503:
			status = "UNAVAILABLE"
		case 504:
			status = "DEADLINE_EXCEEDED"
		}
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"code": e.Status, "message": e.Message, "status": status, "details": []any{map[string]any{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": e.Code, "domain": "openllmproxy", "metadata": map[string]any{"field": e.Param}}}}})
		return b
	}
	if surface == "bedrock" {
		b, _ := json.Marshal(map[string]any{"message": e.Message, "code": e.Code, "param": e.Param})
		return b
	}
	return e.body()
}
