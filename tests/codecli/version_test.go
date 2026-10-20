package codecli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolvedClientsUseSharedBinariesAndIsolatedEnvironments(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "workstation-secret")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	for _, test := range []struct{ variable, output string }{
		{"OLP_CODEX", "codex-cli 7.8.9"},
		{"OLP_CLAUDE_CODE", "7.8.9 (Claude Code)"},
		{"OLP_OPENCODE", "7.8.9"},
	} {
		binary := filepath.Join(t.TempDir(), "client")
		if err := os.WriteFile(binary, []byte(fmt.Sprintf("#!/bin/sh\n[ -z \"${ANTHROPIC_API_KEY:-}\" ] || exit 9\nprintf '%%s\\n' '%s'\n", test.output)), 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(test.variable+"_BINARY", binary)
		t.Setenv(test.variable+"_VERSION", "7.8.9")
		var got, version string
		var environment []string
		switch test.variable {
		case "OLP_CODEX":
			client := New(t, "http://127.0.0.1/code/test", "fixture-key", false)
			got, version = client.Binary, client.Version
			environment = client.Command(t.Context(), "--version").Env
		case "OLP_CLAUDE_CODE":
			client := NewClaudeCode(t, "fixture-key")
			got, version, environment = client.Binary, client.Version, client.env
		case "OLP_OPENCODE":
			client := NewOpenCode(t, "fixture-key")
			got, version, environment = client.Binary, client.Version, client.env
		}
		if got != binary || version != "7.8.9" {
			t.Fatalf("%s: binary=%q version=%q", test.variable, got, version)
		}
		for _, value := range environment {
			if strings.Contains(value, "workstation-secret") || strings.HasPrefix(value, "OLP_CODECLI_") || strings.HasPrefix(value, test.variable+"_BINARY=") {
				t.Fatalf("resolution metadata or workstation secret leaked to client: %s", value)
			}
		}
	}
}
