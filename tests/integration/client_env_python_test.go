//go:build integration && pythonsdk

package integration_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestOperatorGeneratedPythonClients(t *testing.T) {
	_ = required(t, "OLP_TEST_BINARY")
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "./tests/sdk-smoke/run.sh", "uv", "run", "--project", "tests/sdk-smoke-python", "--frozen", "python", "tests/sdk-smoke-python/client_env.py")
	// Give the launcher its trap so it can stop the fixture and remove scratch.
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	command.WaitDelay = 10 * time.Second
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated Python clients: %v\n%s", err, output)
	}
}
