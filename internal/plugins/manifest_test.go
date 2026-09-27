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
		Name:    "acme",
		Version: "1.2.0+build.7",
		Origins: []string{"https://api.acme.example", "http://127.0.0.1:8080", "https://[::1]:8443"},
		Profiles: []abi.Profile{
			{ID: "acme-chat", Label: "Acme Chat", Dialect: "openai-chat", Options: []abi.Option{{Name: "account", Label: "Account"}}, Hosting: abi.Hosting{
				Address: "https://api.acme.example/accounts/{options.account}/v1", Headers: map[string]string{"Authorization": "Token {credential}"},
			}},
			{ID: "acme-messages", Label: "Acme Messages", Dialect: "anthropic-messages", Hosting: abi.Hosting{Address: "http://127.0.0.1:8080", Query: map[string]string{"key": "{credential}"}}},
		},
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
		"framed dialect":       {func(m *abi.Manifest) { m.Profiles[0].Dialect = "bedrock-converse" }, CodeDialectUnknown, "manifest.profiles[0].dialect"},
		"no address":           {func(m *abi.Manifest) { m.Profiles[0].Hosting.Address = "" }, CodeManifestInvalid, "manifest.profiles[0].hosting.address"},
		"undeclared origin":    {func(m *abi.Manifest) { m.Profiles[0].Hosting.Address = "https://other.acme.example/v1" }, CodeManifestInvalid, "manifest.profiles[0].hosting.address"},
		"uncanonical origin":   {func(m *abi.Manifest) { m.Profiles[0].Hosting.Address = "https://api.acme.example:443/v1" }, CodeManifestInvalid, "manifest.profiles[0].hosting.address"},
		"reserved header":      {func(m *abi.Manifest) { m.Profiles[0].Hosting.Headers["Host"] = "{credential}" }, CodeManifestInvalid, "manifest.profiles[0].hosting.headers.Host"},
		"credential not given": {func(m *abi.Manifest) { m.Profiles[1].Hosting.Query = nil }, CodeManifestInvalid, "manifest.profiles[1].hosting"},
		"option name":          {func(m *abi.Manifest) { m.Profiles[0].Options[0].Name = "Account" }, CodeManifestInvalid, "manifest.profiles[0].options[0].name"},
		"undeclared option":    {func(m *abi.Manifest) { m.Profiles[1].Hosting.Query["account"] = "{options.account}" }, CodeManifestInvalid, "manifest.profiles[1].hosting.query.account"},
		"option origin":        {func(m *abi.Manifest) { m.Profiles[0].Hosting.Address = "https://{options.account}.acme.example/v1" }, CodeManifestInvalid, "manifest.profiles[0].hosting.address"},
	} {
		t.Run(name, func(t *testing.T) {
			m := validManifest()
			tc.mutate(&m)
			wantError(t, validateManifest(m), tc.code, tc.field)
		})
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
