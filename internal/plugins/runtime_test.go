package plugins

import (
	"context"
	"errors"
	"log/slog"
	"testing"
)

// startRuntime starts a runtime on engine, closed with the test, that discards
// plugin output unless log is given.
func startRuntime(t testing.TB, engine Engine, limits Limits, log *slog.Logger) *Runtime {
	t.Helper()
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	r, err := NewRuntime(t.Context(), engine, limits, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(context.Background()) })
	return r
}

func newTestRuntime(t *testing.T, limits Limits, log *slog.Logger) *Runtime {
	t.Helper()
	return startRuntime(t, Interpreted, limits, log)
}

func wantError(t *testing.T, err error, code, field string) {
	t.Helper()
	refusal, ok := errors.AsType[*Error](err)
	if !ok {
		t.Fatalf("want %s, got %v", code, err)
	}
	if refusal.Code != code || refusal.Field != field {
		t.Fatalf("want %s at %q, got %s at %q: %s", code, field, refusal.Code, refusal.Field, refusal.Message)
	}
}
