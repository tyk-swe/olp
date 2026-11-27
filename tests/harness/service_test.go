package harness_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/tyk-swe/olp/tests/harness"
)

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
