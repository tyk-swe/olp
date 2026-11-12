package main

import (
	"flag"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/usage/ingest"
)

func TestProcessIdentitiesUseProvidedHost(t *testing.T) {
	t.Setenv("HOSTNAME", "ambient-host")
	host := "provided-host"
	env := map[string]string{"HOSTNAME": host, "OLP_DATABASE_URL": "postgres://localhost/test"}
	cfg, err := Parse([]string{"control"}, func(name string) string { return env[name] }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	manager := release.NewManager(release.ManagerConfig{Installation: uuid.NewString(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Hostname: cfg.Hostname})
	t.Cleanup(manager.Stop)
	for _, identity := range []string{manager.InstanceID(), ingest.ConsumerName(cfg.Hostname)} {
		if !strings.HasPrefix(identity, host+"-") {
			t.Fatalf("identity %q does not use provided host %q", identity, host)
		}
	}
}

func TestCommandFlagsPreserveValuesWithoutReadingConfigurationEnvironment(t *testing.T) {
	for _, test := range []struct {
		args       []string
		dryRun     bool
		consoleDir string
	}{
		{[]string{"--console-dir", "--dry-run"}, false, "--dry-run"},
		{[]string{"--console-dir", "assets", "--dry-run"}, true, "assets"},
		{[]string{"--dry-run", "--console-dir", "assets", "--dry-run=false"}, false, "assets"},
	} {
		var dryRun bool
		cfg, err := parseWithFlags(append([]string{"all"}, test.args...), func(name string) string {
			return map[string]string{"OLP_DATABASE_URL": "postgres://localhost/test", "OLP_DRY_RUN": "true"}[name]
		}, io.Discard, func(f *flag.FlagSet) { f.BoolVar(&dryRun, "dry-run", false, "dry run") })
		if err != nil {
			t.Fatal(err)
		}
		if dryRun != test.dryRun {
			t.Errorf("%v: dry run=%v, want %v", test.args, dryRun, test.dryRun)
		}
		if cfg.ConsoleDir != test.consoleDir {
			t.Errorf("console directory=%q, want %q", cfg.ConsoleDir, test.consoleDir)
		}
	}
}
