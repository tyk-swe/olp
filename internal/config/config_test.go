package config_test

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/config"
)

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
		{"OLP_UNCONFINED_PLUGIN_DIR": "plugins"},
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

func TestMetricsTenantLabelsAndSeriesCap(t *testing.T) {
	env := map[string]string{"OLP_DATABASE_URL": "postgres://user:secret@localhost/db", "OLP_METRICS_TENANT_LABELS": "project,key,end_user"}
	c, err := config.Parse([]string{"gateway"}, func(k string) string { return env[k] }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.MetricsTenantLabels) != 3 || c.MetricsTenantLabels[0] != "project" || c.MetricsSeriesCap != 5000 {
		t.Fatalf("labels=%v cap=%d", c.MetricsTenantLabels, c.MetricsSeriesCap)
	}
	for _, extra := range []map[string]string{
		{"OLP_METRICS_TENANT_LABELS": "organization"},
		{"OLP_METRICS_TENANT_LABELS": "project,unknown"},
		{"OLP_METRICS_SERIES_CAP": "8"},
		{"OLP_METRICS_SERIES_CAP": "1000001"},
	} {
		merged := map[string]string{"OLP_DATABASE_URL": "postgres://user:secret@localhost/db"}
		for k, v := range extra {
			merged[k] = v
		}
		if _, err := config.Parse([]string{"gateway"}, func(k string) string { return merged[k] }, io.Discard); err == nil {
			t.Fatalf("invalid metrics config accepted: %v", extra)
		}
	}
	c, err = config.Parse([]string{"gateway", "--metrics-tenant-labels=end_user", "--metrics-series-cap=64"}, func(k string) string { return env[k] }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.MetricsTenantLabels) != 1 || c.MetricsTenantLabels[0] != "end_user" || c.MetricsSeriesCap != 64 {
		t.Fatalf("flags labels=%v cap=%d", c.MetricsTenantLabels, c.MetricsSeriesCap)
	}
}
