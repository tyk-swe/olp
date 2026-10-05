package codecli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/codeadapter"
)

// The coding-plan evidence names the releases OLP generates configuration
// for, and the qualification installer verifies exactly its digests.
func TestCodingPlanEvidenceMatchesPinsAndInstaller(t *testing.T) {
	raw, err := os.ReadFile("../fixtures/coding-plans/evidence.json")
	if err != nil {
		t.Fatal(err)
	}
	var evidence struct {
		Clients map[string]struct {
			Version  string
			Packages map[string]struct{ URL, Integrity string }
		}
	}
	if err := json.Unmarshal(raw, &evidence); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile("../../scripts/code-mode-qualification.sh")
	if err != nil {
		t.Fatal(err)
	}
	for client, version := range map[string]string{codeadapter.ClientClaudeCode: codeadapter.ClaudeCodeVersion, codeadapter.ClientOpenCode: codeadapter.OpenCodeVersion} {
		pinned := evidence.Clients[client]
		if pinned.Version != version || len(pinned.Packages) != 2 || !strings.Contains(string(script), "version="+version) {
			t.Fatalf("%s evidence %+v differs from the %s pin", client, pinned, version)
		}
		for platform, p := range pinned.Packages {
			digest, ok := strings.CutPrefix(p.Integrity, "sha512-")
			if !ok || !strings.Contains(string(script), "'"+digest+"'") || !strings.Contains(p.URL, platform) || !strings.HasSuffix(p.URL, version+".tgz") {
				t.Fatalf("%s %s package is not the installer's", client, platform)
			}
		}
	}
}
