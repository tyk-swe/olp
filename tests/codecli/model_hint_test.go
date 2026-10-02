//go:build codecli

package codecli_test

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/tests/codecli"
	codexfixture "github.com/tyk-swe/olp/tests/fixtures/codex-qualified"
)

func TestOfficialCodexModelSelectionHintAndOverride(t *testing.T) {
	for _, selected := range []string{"gpt-5.4", "gpt-5.3-codex"} {
		t.Run("configured="+selected, func(t *testing.T) {
			peer := codexfixture.New()
			peer.SetResponder(codexfixture.NewJourney("complete").Reply)
			server := httptest.NewServer(peer)
			defer server.Close()
			client := codecli.New(t, server.URL, "controlled-olp-key", true)
			config, err := routes.CodexClientConfiguration(codemode.Route{Slug: "model-hint", Enabled: true, RevisionID: "published", Models: []string{"gpt-5.4", "gpt-5.3-codex"}}, server.URL, selected)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(client.Home, "config.toml"), []byte(config.Configuration), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"Complete the controlled model selection task."}
			if selected == "gpt-5.4" {
				args = append([]string{"-m", "gpt-5.3-codex"}, args...)
			}
			client.Exec(t, args...)
			if len(peer.Handshakes()) == 0 {
				t.Fatal("no WebSocket handshake")
			}
			for _, h := range peer.Handshakes() {
				if h.Get("X-OLP-Code-Model") != selected {
					t.Fatal("configured hint changed unexpectedly")
				}
				for name := range h {
					if strings.Contains(strings.ToLower(name), "model") && !strings.EqualFold(name, "X-OLP-Code-Model") {
						t.Fatalf("new native model header needs investigation: %s", name)
					}
				}
			}
			for _, r := range peer.Requests() {
				if !r.WebSocket || !bytes.Contains(r.Body, []byte(`"model":"gpt-5.3-codex"`)) {
					t.Fatal("native body model override not observed")
				}
			}
		})
	}
}
