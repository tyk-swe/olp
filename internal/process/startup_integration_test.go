//go:build integration

package process

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/config"
)

func TestIntegrationFailedPublicBindDrainsStartedPlanes(t *testing.T) {
	required := func(name string) string {
		t.Helper()
		value := os.Getenv(name)
		if value == "" {
			t.Fatalf("%s is required; run make integration", name)
		}
		return value
	}
	blocked, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocked.Close()
	assets := t.TempDir()
	if err := os.WriteFile(filepath.Join(assets, "index.html"), []byte("<html>console</html>"), 0600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"OLP_DATABASE_URL": required("OLP_TEST_DATABASE_URL"), "OLP_VALKEY_URL": required("OLP_TEST_VALKEY_URL"),
		"OLP_AUTH_HMAC_KEY_FILE": required("OLP_AUTH_HMAC_KEY_FILE"), "OLP_MASTER_KEY_FILE": required("OLP_MASTER_KEY_FILE"),
		"OLP_CONSOLE_DIR": assets, "OLP_LISTEN_ADDR": blocked.Addr().String(), "OLP_OBSERVABILITY_LISTEN_ADDR": "127.0.0.1:0",
		"OLP_SHUTDOWN_TIMEOUT": "5s", "OLP_STARTUP_TIMEOUT": "30s",
	}
	c, err := config.Parse([]string{"all"}, func(name string) string {
		if value, ok := env[name]; ok {
			return value
		}
		return os.Getenv(name)
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), env["OLP_DATABASE_URL"])
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var output bytes.Buffer
	started := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	err = Run(ctx, c, slog.New(slog.NewTextHandler(&output, nil)))
	if err == nil || !strings.Contains(err.Error(), "bind public listener") {
		t.Fatalf("expected failure after planes started, got %v\n%s", err, output.String())
	}
	if strings.Contains(output.String(), "shutdown budget reached") {
		t.Fatalf("startup failure did not join its owned tasks\n%s", output.String())
	}
	var closed bool
	if err := pool.QueryRow(t.Context(), `SELECT writer_closed AND gracefully_closed_at IS NOT NULL AND NOT retrying
        FROM olp.request_metadata_gateway_epochs WHERE started_at >= $1 ORDER BY started_at DESC LIMIT 1`, started).Scan(&closed); err != nil || !closed {
		t.Fatalf("started metadata producer epoch was not durably closed: %v %v\n%s", closed, err, output.String())
	}
}
