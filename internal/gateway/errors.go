package gateway

import (
	"encoding/json"
	"math"
	"net/http"
	"net/textproto"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Error is an OpenAI-shaped error returned to inference clients.
type Error struct {
	Status     int
	Type       string
	Code       string
	Message    string
	Param      *string
	RetryAfter time.Duration
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// redactCredentials removes attempted secrets, including header values quoted
// inside a JSON diagnostic. Replace longer values first so overlapping header
// credentials cannot leave part of the longer secret exposed.
func redactCredentials(message string, values []string) string {
	var secrets []string
	for _, value := range values {
		// HTTP strips surrounding spaces and tabs before sending header values.
		value = textproto.TrimString(value)
		if value == "" {
			continue
		}
		secrets = append(secrets, value)
		quoted, _ := json.Marshal(value)
		secrets = append(secrets, string(quoted[1:len(quoted)-1]))
		// Upstream JSON encoders may leave HTML characters unescaped.
		var unescaped strings.Builder
		encoder := json.NewEncoder(&unescaped)
		encoder.SetEscapeHTML(false)
		encoder.Encode(value)
		encoded := unescaped.String()
		secrets = append(secrets, encoded[1:len(encoded)-2]) // quotes and trailing newline
	}
	sort.SliceStable(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	pairs := make([]string, 0, 2*len(secrets))
	for _, secret := range secrets {
		pairs = append(pairs, secret, "[REDACTED]")
	}
	return strings.NewReplacer(pairs...).Replace(message)
}

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

func writeError(w http.ResponseWriter, e *Error) {
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(responseWriteTimeout))
	h := w.Header()
	h.Set("Content-Type", "application/json")
	if e.RetryAfter > 0 {
		h.Set("Retry-After", strconv.FormatInt(int64(math.Ceil(e.RetryAfter.Seconds())), 10))
	}
	w.WriteHeader(e.Status)
	w.Write(e.body())
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
