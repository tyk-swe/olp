package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestProcessIdentitiesUseProvidedHost(t *testing.T) {
	t.Setenv("HOSTNAME", "ambient-host")
	host := "provided-host"
	env := map[string]string{"HOSTNAME": host, "OLP_DATABASE_URL": "postgres://localhost/test"}
	cfg, err := Parse([]string{"control"}, func(name string) string { return env[name] }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	manager := release.NewManager(nil, uuid.NewString(), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg.Hostname)
	t.Cleanup(manager.Stop)
	for _, identity := range []string{manager.InstanceID, usage.ConsumerName(cfg.Hostname)} {
		if !strings.HasPrefix(identity, host+"-") {
			t.Fatalf("identity %q does not use provided host %q", identity, host)
		}
	}
}
