//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/tests/harness"
)

type service struct{ *harness.Service }

func startService(t *testing.T) *service {
	t.Helper()
	pool, databaseURL := harness.Database(t)
	env := harness.NewEnvironment(t, databaseURL, "Service test")
	harness.RunBinary(t, env, "migrate")
	return &service{harness.StartProcess(t, pool, env)}
}

func TestStartupFailureCleanup(t *testing.T) {
	t.Parallel()
	if os.Getenv("OLP_TEST_STARTUP_FAILURE") == "1" {
		harness.StartProcess(t, nil, harness.Environment{})
		t.Fatal("process unexpectedly started")
	}
	binary := filepath.Join(t.TempDir(), "exit-early")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 23\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestStartupFailureCleanup$")
	command.Env = append(os.Environ(), "OLP_TEST_STARTUP_FAILURE=1", "OLP_TEST_BINARY="+binary)
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("startup failure cleanup did not finish: %v\n%s", ctx.Err(), output)
	}
	if err == nil || !bytes.Contains(output, []byte("olp exited during startup: exit status 23")) {
		t.Fatalf("expected the startup failure: %v\n%s", err, output)
	}
}

func TestStartupPortCollision(t *testing.T) {
	t.Parallel()
	pool, databaseURL := harness.Database(t)
	env := harness.NewEnvironment(t, databaseURL, "Service test")
	harness.RunBinary(t, env, "migrate")
	occupied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer occupied.Close()
	attempts := 0
	addresses := func(t *testing.T) (string, string) {
		attempts++
		public, private := harness.FreeAddresses(t)
		if attempts == 1 {
			private = strings.TrimPrefix(occupied.URL, "http://")
		}
		return public, private
	}
	s := harness.StartProcessAttempt(t, pool, env, addresses, time.Now().Add(harness.StartupLimit), 3)
	if attempts < 2 || attempts > 3 {
		t.Fatalf("startup attempts = %d, want 2 or 3", attempts)
	}
	// Exercise the child rather than trusting a foreign health endpoint.
	s.Owner()
}

func TestEachSplitModeAnswersOnlyItsOwnPlaneAndStopsCleanly(t *testing.T) {
	t.Parallel()
	status := func(t *testing.T, s *harness.Service, path string) int {
		t.Helper()
		response, err := http.Get(s.Origin + path)
		if err != nil {
			return 0
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	for mode, want := range map[string]struct{ management, inference int }{
		"control": {management: http.StatusOK, inference: http.StatusNotFound},
		"gateway": {management: http.StatusNotFound, inference: http.StatusUnauthorized},
		"worker":  {},
	} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			pool, databaseURL := harness.Database(t)
			env := harness.NewEnvironment(t, databaseURL, "Split mode test")
			harness.RunBinary(t, env, "migrate")
			env.Mode = mode
			s := harness.StartProcess(t, pool, env)
			if got := status(t, s, "/api/v1/setup/status"); got != want.management {
				t.Fatalf("management answered %d, want %d", got, want.management)
			}
			if got := status(t, s, "/v1/models"); got != want.inference {
				t.Fatalf("inference answered %d, want %d", got, want.inference)
			}
		})
	}
}

func describe(status int, raw []byte) string { return fmt.Sprintf("status %d: %s", status, raw) }
