package process

import (
	"bytes"
	"log/slog"
	"net/netip"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/config"
)

func TestEgressExceptionsAreAnnouncedAtStartup(t *testing.T) {
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, nil))
	warnEgressExceptions(log, config.Config{})
	if logged.Len() != 0 {
		t.Fatal("a process without egress exceptions must not warn", logged.String())
	}
	warnEgressExceptions(log, config.Config{
		ProviderEgressAllowCIDRs:     []netip.Prefix{netip.MustParsePrefix("10.1.0.0/16")},
		ProviderEgressAllowHTTPHosts: []string{"vllm.internal"},
	})
	if line := logged.String(); !strings.Contains(line, "level=WARN") || !strings.Contains(line, "10.1.0.0/16") || !strings.Contains(line, "vllm.internal") {
		t.Fatal("egress exceptions were not announced", line)
	}
}
