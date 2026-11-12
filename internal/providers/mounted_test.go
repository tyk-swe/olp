package providers

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
)

func TestLoadMountedRefusesACredentialItCouldNotSend(t *testing.T) {
	for name, test := range map[string]struct {
		credential string
		loads      bool
	}{
		"single line key":     {"sk-mounted\n", true},
		"multiline json":      {"{\n  \"key\": \"sk-mounted\"\n}\n", true},
		"multiline plaintext": {"sk-first\nsk-second\n", false},
		"nul byte":            {"sk-\x00mounted", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			credential := filepath.Join(dir, "credential")
			if err := os.WriteFile(credential, []byte(test.credential), 0o600); err != nil {
				t.Fatal(err)
			}
			mounted := filepath.Join(dir, "connectors.json")
			document := `{"providers":[{"provider_id":"018f1f77-9823-7b8a-b847-3658e91e4d01","configuration":{"kind":"openai","auth_mode":"api_key","endpoint":"https://api.openai.com/v1/"},"credential_file":"` + credential + `"}]}`
			if err := os.WriteFile(mounted, []byte(document), 0o600); err != nil {
				t.Fatal(err)
			}
			providers, err := LoadMounted(mounted, &egress.Policy{})
			if loaded := err == nil && len(providers) == 1; loaded != test.loads {
				t.Fatalf("loaded %v, want %v (error %v)", loaded, test.loads, err)
			}
		})
	}
}
