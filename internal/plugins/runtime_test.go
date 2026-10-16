package plugins

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/testutil"
)

const fixtureSecret = "sk-fixture-secret"

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

func fixture(t *testing.T, behaviour string) []byte {
	return testutil.BuildPlugin(t, "./internal/plugins/testdata/fixture", "-X=main.behaviour="+behaviour)
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

// Limits under which the fixture's "loop" and "allocate" behaviours stop
// a call, on one instance. The time limit leaves time enough to instantiate
// the module under the race detector; the memory limit's time is generous,
// so the memory limit is what stops the call.
var (
	timeLimits   = Limits{Memory: 32 << 20, Time: 5 * time.Second, Stack: DefaultLimits.Stack, Instances: 1}
	memoryLimits = Limits{Memory: 16 << 20, Time: time.Minute, Stack: DefaultLimits.Stack, Instances: 1}
)
