package connectors

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

const (
	pluginDigest    = "sha256:fixture"
	installedPlugin = `{"unconfined":false,"manifest":{"name":"acme","version":"1.0.0","origins":["https://api.example.com"],"profiles":[{
		"id":"acme-chat","label":"Acme chat","dialect":"openai-chat",
		"options":[{"name":"account","label":"Account","pattern":"^[a-z0-9 ]+$"},{"name":"tier","label":"Tier","optional":true,"enum":["basic","pro"]}],
		"hosting":{"address":"https://api.example.com/accounts/{options.account}/v1","headers":{"Authorization":"Bearer {credential}"}}
	}]}}`
)

func acme(t *testing.T) *PluginProfile {
	t.Helper()
	profile, err := DecodePluginProfile(pluginDigest, []byte(installedPlugin), "acme-chat")
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func provider(profile *PluginProfile, options map[string]string) Config {
	return Config{
		Kind: KindPlugin, ProfileID: "acme-chat", ProfileRevision: pluginDigest, AuthMode: AuthStaticCredential,
		Plugin: profile, PluginOptions: options, Endpoint: profile.Address(options),
	}
}

func TestAPluginProfilePlacesItsDialectAtTheAddressItsOptionsFill(t *testing.T) {
	t.Parallel()
	c := provider(acme(t), map[string]string{"account": "team a"})
	if err := c.Validate(&egress.Policy{}); err != nil {
		t.Fatalf("Validate refused the plugin's provider: %v", err)
	}
	got, err := c.URL(wire.Chat, "model-a", false)
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://api.example.com/accounts/team%20a/v1/chat/completions"; got != want {
		t.Errorf("address = %q, want %q", got, want)
	}
	if !c.Supports("generation", "openai", "streaming") || c.Supports("embeddings", "openai", "unary") {
		t.Error("a plugin profile serves generation in its dialect and nothing else")
	}

	req := upstreamRequest(t, got)
	if _, err := NewAuth(&egress.Policy{}).Apply(t.Context(), req, c, []byte("secret"), nil); err != nil {
		t.Fatal(err)
	}
	if got, want := req.Header.Get("Authorization"), "Bearer secret"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
}

func TestAPluginProfileRefusesOptionValuesItsDeclarationDoesNotAdmit(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		options map[string]string
		option  string
	}{
		"an undeclared option":            {map[string]string{"account": "a", "region": "eu"}, "region"},
		"a missing required option":       {map[string]string{"tier": "pro"}, "account"},
		"a value outside the pattern":     {map[string]string{"account": "A"}, "account"},
		"a value outside the enum":        {map[string]string{"account": "a", "tier": "gold"}, "tier"},
		"a value with control characters": {map[string]string{"account": "a\n"}, "account"},
	}
	profile := acme(t)
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var refused *OptionError
			if err := profile.ValidateOptions(tc.options); !errors.As(err, &refused) || refused.Option != tc.option {
				t.Errorf("ValidateOptions = %v, want a refusal of %q", err, tc.option)
			}
			if err := provider(profile, tc.options).ValidateProfile(); err == nil {
				t.Error("ValidateProfile accepted a provider with the refused options")
			}
		})
	}
	if err := profile.ValidateOptions(map[string]string{"account": "a", "tier": "basic"}); err != nil {
		t.Errorf("ValidateOptions refused declared values: %v", err)
	}
}

func TestAPluginProfileReadsBackFromASnapshotAsItWasPublished(t *testing.T) {
	t.Parallel()
	published := acme(t)
	stored, err := json.Marshal(published)
	if err != nil {
		t.Fatal(err)
	}
	var read PluginProfile
	if err := json.Unmarshal(stored, &read); err != nil {
		t.Fatal(err)
	}
	options := map[string]string{"account": "a"}
	if got, want := read.Address(options), published.Address(options); got != want {
		t.Errorf("address = %q, want %q", got, want)
	}
	got, err := json.Marshal(read.Profile())
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(published.Profile())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("profile = %s, want %s", got, want)
	}
}

func TestAPluginProfileMustDeclareAnIdentityAndDialectOfItsOwn(t *testing.T) {
	t.Parallel()
	declare := func(id, dialect string) string {
		return `{"manifest":{"name":"acme","version":"1.0.0","origins":["https://api.example.com"],"profiles":[{
			"id":"` + id + `","label":"Acme","dialect":"` + dialect + `",
			"hosting":{"address":"https://api.example.com/v1","headers":{"Authorization":"Bearer {credential}"}}}]}}`
	}
	cases := map[string]struct{ id, dialect, field string }{
		"a built-in profile's id":         {"openai-chat", "openai-chat", "id"},
		"a derived built-in profile's id": {"openai-embeddings", "openai-chat", "id"},
		"a dialect plugins cannot serve":  {"acme-fim", "mistral-fim", "dialect"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var refused *ProfileError
			_, err := DecodePluginProfile(pluginDigest, []byte(declare(tc.id, tc.dialect)), tc.id)
			if !errors.As(err, &refused) || refused.Field != tc.field {
				t.Errorf("DecodePluginProfile = %v, want a refusal of %q", err, tc.field)
			}
		})
	}
	for _, dialect := range PluginDialects() {
		if _, err := DecodePluginProfile(pluginDigest, []byte(declare("acme-"+dialect, dialect)), "acme-"+dialect); err != nil {
			t.Errorf("a plugin profile serving %s was refused: %v", dialect, err)
		}
	}
}
