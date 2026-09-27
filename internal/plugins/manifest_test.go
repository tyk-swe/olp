package plugins

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

func validManifest() abi.Manifest {
	return abi.Manifest{
		Name:     "acme",
		Version:  "1.2.0+build.7",
		Origins:  []string{"https://api.acme.example", "http://127.0.0.1:8080", "https://[::1]:8443"},
		Profiles: []abi.Profile{{ID: "acme-chat", Label: "Acme Chat", Dialect: "openai-chat"}, {ID: "acme-messages", Label: "Acme Messages", Dialect: "anthropic-messages"}},
	}
}

func TestManifestValidation(t *testing.T) {
	if err := validateManifest(validManifest()); err != nil {
		t.Fatalf("refused a valid manifest: %v", err)
	}
	many := func(n int, item func(int) string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = item(i)
		}
		return out
	}
	for name, tc := range map[string]struct {
		mutate      func(*abi.Manifest)
		code, field string
	}{
		"uppercase name":      {func(m *abi.Manifest) { m.Name = "Acme" }, CodeManifestInvalid, "manifest.name"},
		"trailing hyphen":     {func(m *abi.Manifest) { m.Name = "acme-" }, CodeManifestInvalid, "manifest.name"},
		"missing version":     {func(m *abi.Manifest) { m.Version = "" }, CodeManifestInvalid, "manifest.version"},
		"spaced version":      {func(m *abi.Manifest) { m.Version = "1 2" }, CodeManifestInvalid, "manifest.version"},
		"control description": {func(m *abi.Manifest) { m.Description = "a\nb" }, CodeManifestInvalid, "manifest.description"},
		"long description":    {func(m *abi.Manifest) { m.Description = strings.Repeat("d", 501) }, CodeManifestInvalid, "manifest.description"},
		"origin path":         {func(m *abi.Manifest) { m.Origins[0] = "https://api.acme.example/v1" }, CodeManifestInvalid, "manifest.origins[0]"},
		"origin slash":        {func(m *abi.Manifest) { m.Origins[0] = "https://api.acme.example/" }, CodeManifestInvalid, "manifest.origins[0]"},
		"origin case":         {func(m *abi.Manifest) { m.Origins[0] = "https://API.acme.example" }, CodeManifestInvalid, "manifest.origins[0]"},
		"origin default port": {func(m *abi.Manifest) { m.Origins[0] = "https://api.acme.example:443" }, CodeManifestInvalid, "manifest.origins[0]"},
		"origin credentials":  {func(m *abi.Manifest) { m.Origins[0] = "https://user@api.acme.example" }, CodeManifestInvalid, "manifest.origins[0]"},
		"origin query":        {func(m *abi.Manifest) { m.Origins[0] = "https://api.acme.example?x=1" }, CodeManifestInvalid, "manifest.origins[0]"},
		"origin scheme":       {func(m *abi.Manifest) { m.Origins[0] = "wss://api.acme.example" }, CodeManifestInvalid, "manifest.origins[0]"},
		"origin port":         {func(m *abi.Manifest) { m.Origins[1] = "http://127.0.0.1:080" }, CodeManifestInvalid, "manifest.origins[1]"},
		"duplicate origin":    {func(m *abi.Manifest) { m.Origins[2] = m.Origins[0] }, CodeManifestInvalid, "manifest.origins[2]"},
		"too many origins": {func(m *abi.Manifest) {
			m.Origins = many(17, func(i int) string { return fmt.Sprintf("https://h%d.example", i) })
		}, CodeManifestInvalid, "manifest.origins"},
		"no profiles":          {func(m *abi.Manifest) { m.Profiles = nil }, CodeManifestInvalid, "manifest.profiles"},
		"duplicate profile id": {func(m *abi.Manifest) { m.Profiles[1].ID = "acme-chat" }, CodeManifestInvalid, "manifest.profiles[1].id"},
		"missing label":        {func(m *abi.Manifest) { m.Profiles[0].Label = "" }, CodeManifestInvalid, "manifest.profiles[0].label"},
		"missing dialect":      {func(m *abi.Manifest) { m.Profiles[1].Dialect = "" }, CodeManifestInvalid, "manifest.profiles[1].dialect"},
		"unknown dialect":      {func(m *abi.Manifest) { m.Profiles[1].Dialect = "acme-native" }, CodeDialectUnknown, "manifest.profiles[1].dialect"},
		"hosting label":        {func(m *abi.Manifest) { m.Profiles[0].Dialect = "direct-openai" }, CodeDialectUnknown, "manifest.profiles[0].dialect"},
	} {
		t.Run(name, func(t *testing.T) {
			m := validManifest()
			tc.mutate(&m)
			wantError(t, validateManifest(m), tc.code, tc.field)
		})
	}
	if err := validateManifest(abi.Manifest{Name: "bare", Version: "1", Profiles: validManifest().Profiles}); err != nil {
		t.Fatalf("refused a plugin that reaches no origin: %v", err)
	}
}

func TestManifestDecodingRefusesUnknownDeclarations(t *testing.T) {
	data, err := json.Marshal(validManifest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = decodeManifest(data); err != nil {
		t.Fatal(err)
	}
	extended := strings.Replace(string(data), `"profiles":`, `"signing":true,"profiles":`, 1)
	_, err = decodeManifest([]byte(extended))
	wantError(t, err, CodeManifestInvalid, "manifest")
	_, err = decodeManifest([]byte(`{"name":"` + strings.Repeat("a", maxManifestBytes) + `"}`))
	wantError(t, err, CodeManifestInvalid, "manifest")
}
