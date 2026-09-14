package process

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"
)

func TestReadinessReflectsDependenciesAndShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var failure error
	h := healthHandler(ctx, time.Second, func(context.Context) error { return failure }, nil, nil)
	for _, status := range []int{200, 503, 503} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/health/ready", nil))
		if w.Code != status {
			t.Fatalf("readiness: %d; want %d", w.Code, status)
		}
		if failure == nil {
			failure = errors.New("offline")
		} else {
			failure = nil
			cancel()
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/health/live", nil))
	if w.Code != 200 {
		t.Fatalf("liveness depends on services: %d", w.Code)
	}
}
