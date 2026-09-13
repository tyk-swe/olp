package config_test

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/config"
)

func TestModeConfigurationWithoutServices(t *testing.T) {
	for _, mode := range []string{"all", "control", "gateway", "worker"} {
		env := map[string]string{"OLP_DATABASE_URL": "postgres://user:secret@localhost/db", "OLP_VALKEY_URL": "redis://localhost"}
		c, err := config.Parse([]string{mode, "--listen-addr=127.0.0.1:8082"}, func(k string) string { return env[k] }, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if c.ListenAddr != "127.0.0.1:8082" || c.Mode.Public() != (mode != "worker") || c.Mode.Management() != (mode == "all" || mode == "control") {
			t.Fatalf("wrong configuration for %s", mode)
		}
	}
}

func TestInvalidConfigAndSecretErrors(t *testing.T) {
	for _, extra := range []map[string]string{
		{"OLP_DATABASE_URL": "postgres://secret%broken"},
		{"OLP_DATABASE_MAX_CONNECTIONS": "0"},
		{"OLP_DATABASE_MAX_CONNECTIONS": "bad"},
		{"OLP_OBSERVABILITY_LISTEN_ADDR": "127.0.0.1:8080"},
		{"OLP_OBSERVABILITY_LISTEN_ADDR": "0.0.0.0:8080"},
		{"OLP_LOG_LEVEL": "info+1"},
		{"OLP_PUBLIC_ORIGIN": "https://user:secret@example.test"},
		{"OLP_STARTUP_TIMEOUT": "0s"},
		{"OLP_OIDC_ALLOW_INSECURE_TEST_ISSUER": "true"},
		{"OLP_OIDC_ALLOW_PRIVATE_NETWORK": "true"},
	} {
		env := map[string]string{"OLP_DATABASE_URL": "postgres://user:secret@localhost/db"}
		for k, v := range extra {
			env[k] = v
		}
		_, err := config.Parse([]string{"all"}, func(k string) string { return env[k] }, io.Discard)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("expected sanitized failure for %v: %v", extra, err)
		}
	}
	file := filepath.Join(t.TempDir(), "database-url")
	if err := os.WriteFile(file, []byte("postgres://user:secret@localhost/db\n"), 0600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"OLP_DATABASE_URL_FILE": file}
	c, err := config.Parse([]string{"all"}, func(k string) string { return env[k] }, io.Discard)
	if err != nil || strings.HasSuffix(c.DatabaseURL, "\n") {
		t.Fatalf("file URL: %v", err)
	}
	env["OLP_DATABASE_URL"] = "postgres://localhost/db"
	if _, err := config.Parse([]string{"all"}, func(k string) string { return env[k] }, io.Discard); err == nil {
		t.Fatal("accepted conflicting URL sources")
	}
	delete(env, "OLP_DATABASE_URL")
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Parse([]string{"all"}, func(k string) string { return env[k] }, io.Discard); err == nil {
		t.Fatal("accepted world-readable secret")
	}
}

func TestExplicitFlagOverridesInvalidEnvironment(t *testing.T) {
	env := map[string]string{"OLP_DATABASE_URL": "postgres://localhost/db", "OLP_DATABASE_MAX_CONNECTIONS": "invalid"}
	c, err := config.Parse([]string{"all", "--database-max-connections=3"}, func(k string) string { return env[k] }, io.Discard)
	if err != nil || c.DatabaseMaxConnections != 3 {
		t.Fatalf("flag precedence: %v", err)
	}
}
