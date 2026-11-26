//go:build qualification

package qualification_test

import (
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/tests/harness"
)

type service struct{ *harness.Service }

func startService(t *testing.T, adjust func(*harness.Environment)) *service {
	t.Helper()
	pool, databaseURL := harness.Database(t)
	env := harness.NewEnvironment(t, databaseURL, "Qualification")
	if adjust != nil {
		adjust(&env)
	}
	return &service{harness.StartProcess(t, pool, env)}
}

func uniqueName(prefix string) string { return prefix + "-" + uuid.NewString()[:8] }

func openReportWriter(t *testing.T, path string) *reportWriter {
	t.Helper()
	if path == "" {
		return &reportWriter{out: os.Stdout}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return &reportWriter{out: file}
}

func pollUntil(window, interval time.Duration, ready func() bool) bool {
	deadline := time.Now().Add(window)
	for {
		if ready() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(interval)
	}
}
