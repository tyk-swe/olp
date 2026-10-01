package gateway

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPlaygroundStreamDeliversTerminalErrorAfterByteLimit(t *testing.T) {
	recorder := httptest.NewRecorder()
	sw := &playgroundStreamWriter{w: recorder, maxTotal: 256}
	var err error
	for range 100 {
		if err = sw.emit([]byte("0123456789")); err != nil {
			break
		}
	}
	if !errors.Is(err, errResponseTooLarge) {
		t.Fatalf("emit error = %v, want errResponseTooLarge", err)
	}
	sw.finish(&execution{}, &outcome{err: &Error{
		Status:  http.StatusBadGateway,
		Code:    "upstream_response_too_large",
		Message: "The upstream response exceeded the byte limit.",
	}})
	body := recorder.Body.String()
	if !strings.Contains(body, "event: error\n") || !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("stream did not end with an error event: %q", body)
	}
	if strings.LastIndex(body, "event: error") < strings.LastIndex(body, "event: frame") {
		t.Fatalf("error event is not terminal: %q", body)
	}
}
