package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/upstream"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

const pluginDigest = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

func pluginManifest() abi.Manifest {
	return abi.Manifest{Name: "acme", Version: "1.0.0", Origins: []string{"https://api.acme.example"}, Profiles: []abi.Profile{{
		ID: "acme-chat", Label: "Acme Chat", Dialect: "openai-chat",
		Hosting: abi.Hosting{
			Address:   "https://api.acme.example/v2",
			Headers:   map[string]string{"authorization": "Token {credential}", "X-Acme-Key": "{credential}", "X-Acme-Client": "olp"},
			Query:     map[string]string{"key": "{credential}"},
			Discovery: &abi.Discovery{Path: "/models", Models: "data", ID: "id", Pagination: &abi.Pagination{Parameter: "page_token", Cursor: "next_page_token"}},
			Classification: []abi.FailureRule{
				{Status: 400, Code: "insufficient_quota", Class: abi.ClassRateLimited},
				{Type: "overloaded", Class: abi.ClassRetryable},
			},
		},
	}}}
}

func pluginConfig(t *testing.T, manifest abi.Manifest) Config {
	t.Helper()
	plugin, err := NewPluginProfile(pluginDigest, manifest, manifest.Profiles[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	return Config{Plugin: plugin, Kind: KindPlugin, AuthMode: AuthStaticCredential, ProfileID: manifest.Profiles[0].ID, ProfileRevision: pluginDigest, Endpoint: plugin.Address(nil)}
}

// optionsManifest declares a profile whose hosting adaptation uses options.
func optionsManifest() abi.Manifest {
	manifest := pluginManifest()
	manifest.Profiles[0].Options = []abi.Option{
		{Name: "account", Label: "Account", Description: "The Acme account that serves requests."},
		{Name: "region", Label: "Region", Enum: []string{"us", "eu"}},
		{Name: "team", Label: "Team", Optional: true, Pattern: "^[a-z]+$"},
	}
	manifest.Profiles[0].Hosting = abi.Hosting{
		Address: "https://api.acme.example/accounts/{options.account}/v2",
		Headers: map[string]string{"Authorization": "Token {credential}", "X-Acme-Region": "{options.region}"},
		Query:   map[string]string{"placement": "{options.region}-{options.account}"},
	}
	return manifest
}

func TestPluginProfileHostingPlacesTheStaticCredential(t *testing.T) {
	c := pluginConfig(t, pluginManifest())
	if err := c.Validate(&egress.Policy{}); err != nil {
		t.Fatal(err)
	}
	endpoint, err := c.URL(openai.FamilyChat, "acme-large", false)
	if err != nil || endpoint != "https://api.acme.example/v2/chat/completions" {
		t.Fatalf("addressed %q: %v", endpoint, err)
	}
	req, _ := http.NewRequest(http.MethodPost, endpoint, nil)
	secret := "sk acme&secret"
	sensitive, err := NewAuth(&egress.Policy{}).Apply(context.Background(), req, c, []byte(secret), nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != "Token "+secret || req.Header.Get("X-Acme-Key") != secret || req.Header.Get("X-Acme-Client") != "olp" {
		t.Fatalf("placed headers %v", req.Header)
	}
	if req.URL.Query().Get("key") != secret || !strings.Contains(req.URL.RawQuery, "key=sk+acme%26secret") {
		t.Fatalf("placed query %q", req.URL.RawQuery)
	}
	for _, placed := range []string{secret, "Token " + secret, "sk+acme%26secret"} {
		if !slices.Contains(sensitive, placed) {
			t.Errorf("%q is not redacted: %q", placed, sensitive)
		}
	}
	if slices.Contains(sensitive, "olp") {
		t.Error("a value without the credential is redacted")
	}
}

func TestPluginProvidersSendOnlyWhatTheirProfileDeclares(t *testing.T) {
	manifest := pluginManifest()
	manifest.Profiles[0].Hosting = abi.Hosting{Address: "https://api.acme.example/v2", Query: map[string]string{"key": "{credential}"}}
	c := pluginConfig(t, manifest)
	req, _ := http.NewRequest(http.MethodPost, "https://api.acme.example/v2/chat/completions", nil)
	if _, err := NewAuth(&egress.Policy{}).Apply(context.Background(), req, c, []byte("secret"), nil); err != nil {
		t.Fatal(err)
	}
	if len(req.Header) != 0 || req.URL.RawQuery != "key=secret" {
		t.Fatalf("a plugin provider acquired a kind default: %v %q", req.Header, req.URL.RawQuery)
	}
}

func TestPluginHostingRejectsCredentialsItCannotPlace(t *testing.T) {
	c := pluginConfig(t, pluginManifest())
	for _, secret := range []string{"", "line\nbreak", "nul\x00"} {
		req, _ := http.NewRequest(http.MethodPost, "https://api.acme.example/v2/chat/completions", nil)
		if _, err := NewAuth(&egress.Policy{}).Apply(context.Background(), req, c, []byte(secret), nil); !errors.Is(err, ErrCredentialRejected) {
			t.Errorf("placed %q: %v", secret, err)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, "https://api.acme.example/v2/chat/completions?key=caller", nil)
	if _, err := NewAuth(&egress.Policy{}).Apply(context.Background(), req, c, []byte("secret"), nil); err == nil || errors.Is(err, ErrAuthentication) {
		t.Fatalf("a declared query parameter replaced operation addressing: %v", err)
	}
}

func TestPluginProfileCatalogueEntry(t *testing.T) {
	anthropic := pluginManifest()
	anthropic.Profiles[0].Dialect = "anthropic-messages"
	plugin, err := NewPluginProfile(pluginDigest, anthropic, "acme-chat")
	if err != nil {
		t.Fatal(err)
	}
	p := plugin.Profile()
	if p.Kind != KindPlugin || p.Revision != pluginDigest || p.Hosting != "plugin" || !p.Strict || p.Transport != "http" ||
		*p.Plugin != (Plugin{Digest: pluginDigest, Name: "acme", Version: "1.0.0"}) ||
		!slices.Equal(p.Authentication, []string{AuthStaticCredential}) || !slices.Equal(p.Operations, []string{"generation"}) {
		t.Fatalf("catalogued %+v", p)
	}
	if p.DialectRevision != anthropicMessagesRevision || !slices.Equal(p.SemanticHeaders, []string{"Anthropic-Version", "Anthropic-Beta"}) || p.OperationDialects["generation"] != "anthropic-messages" || p.DefaultSchemas["generation"] == nil {
		t.Fatalf("the profile does not share its dialect's revision and semantics: %+v", p)
	}
	if _, err := NewPluginProfile(pluginDigest, anthropic, "acme-other"); err == nil {
		t.Fatal("found an undeclared profile")
	}
	for _, builtin := range Profiles() {
		if !builtin.Strict || builtin.Plugin != nil {
			t.Fatalf("built-in %s lost its strict treatment", builtin.ID)
		}
	}
}

func TestPluginProfileSurvivesPublication(t *testing.T) {
	c := pluginConfig(t, pluginManifest())
	encoded, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Config
	if err = json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if err = decoded.ValidateProfile(); err != nil || decoded.Plugin.Address(nil) != c.Plugin.Address(nil) {
		t.Fatalf("decoded %s: %v", encoded, err)
	}
	req, _ := http.NewRequest(http.MethodPost, "https://api.acme.example/v2/chat/completions", nil)
	if _, err = NewAuth(&egress.Policy{}).Apply(context.Background(), req, decoded, []byte("secret"), nil); err != nil || req.Header.Get("Authorization") != "Token secret" {
		t.Fatalf("decoded profile placed %v: %v", req.Header, err)
	}
	tampered := strings.Replace(string(encoded), `{credential}"`, `{password}"`, 1)
	if err = json.Unmarshal([]byte(tampered), &decoded); err == nil {
		t.Fatal("decoded a hosting adaptation OLP does not run")
	}
}

// An unconfined plugin's profiles say so, from the installed plugin a
// provider pins through publication, so gateways know which targets their
// deployment may serve.
func TestUnconfinedPluginProfileSurvivesPublication(t *testing.T) {
	installed, _ := json.Marshal(InstalledPlugin{Manifest: pluginManifest(), Unconfined: true})
	plugin, err := DecodePluginProfile(pluginDigest, installed, "acme-chat")
	if err != nil || !plugin.Unconfined() || !plugin.Profile().Plugin.Unconfined {
		t.Fatalf("decoded %+v: %v", plugin, err)
	}
	encoded, err := json.Marshal(plugin)
	if err != nil {
		t.Fatal(err)
	}
	var decoded PluginProfile
	if err = json.Unmarshal(encoded, &decoded); err != nil || !decoded.Unconfined() {
		t.Fatalf("published %s: %v", encoded, err)
	}
	confined, err := NewPluginProfile(pluginDigest, pluginManifest(), "acme-chat")
	if err != nil || confined.Unconfined() {
		t.Fatalf("a confined plugin's profile: %v", err)
	}
}

// A profile's declared discovery and classification reach the provider
// unchanged through publication, with the classes that govern failover.
func TestPluginProfileDeclaresDiscoveryAndClassification(t *testing.T) {
	c := pluginConfig(t, pluginManifest())
	encoded, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var published Config
	if err = json.Unmarshal(encoded, &published); err != nil {
		t.Fatal(err)
	}
	for _, c := range []Config{c, published} {
		profile, _ := c.Profile()
		discovery, declared := c.Plugin.Discovery()
		if !profile.ModelDiscovery || !declared || discovery.Path != "/models" || discovery.Models != "data" || discovery.ID != "id" ||
			*discovery.Pagination != (abi.Pagination{Parameter: "page_token", Cursor: "next_page_token"}) {
			t.Fatalf("discovery %v %+v", profile.ModelDiscovery, discovery)
		}
		want := []upstream.Rule{{Status: 400, Code: "insufficient_quota", Class: upstream.RateLimit}, {Type: "overloaded", Class: upstream.ServerError}}
		if got := c.Classification(); !slices.Equal(got, want) {
			t.Fatalf("classification %+v", got)
		}
	}
	discovery, _ := c.Plugin.Discovery()
	discovery.Pagination.Cursor = "changed"
	if again, _ := c.Plugin.Discovery(); again.Pagination.Cursor != "next_page_token" {
		t.Fatal("a caller changed the profile's discovery")
	}
	for class, want := range map[string]upstream.Class{abi.ClassCredential: upstream.Credential, abi.ClassRateLimited: upstream.RateLimit, abi.ClassRetryable: upstream.ServerError, abi.ClassTerminal: upstream.ClientError} {
		manifest := pluginManifest()
		manifest.Profiles[0].Hosting.Classification = []abi.FailureRule{{Status: 409, Class: class}}
		if got := pluginConfig(t, manifest).Classification(); len(got) != 1 || got[0].Class != want {
			t.Errorf("%s classified as %+v", class, got)
		}
	}

	undeclared := pluginManifest()
	undeclared.Profiles[0].Hosting.Discovery, undeclared.Profiles[0].Hosting.Classification = nil, nil
	c = pluginConfig(t, undeclared)
	if profile, _ := c.Profile(); profile.ModelDiscovery || c.Classification() != nil {
		t.Fatalf("a profile declaring neither: %+v %+v", profile, c.Classification())
	}
	if _, declared := c.Plugin.Discovery(); declared {
		t.Fatal("found an undeclared listing")
	}
	if (Config{Kind: "openai"}).Classification() != nil {
		t.Fatal("a built-in profile declares a classification")
	}
	for _, builtin := range Profiles() {
		if builtin.ModelDiscovery {
			t.Fatalf("built-in %s declares discovery", builtin.ID)
		}
	}
}

func TestPluginProviderConfigurationFollowsItsProfile(t *testing.T) {
	c := pluginConfig(t, pluginManifest())
	for name, mutate := range map[string]func(*Config){
		"unresolved plugin":  func(c *Config) { c.Plugin = nil },
		"other revision":     func(c *Config) { c.ProfileRevision = strings.Repeat("0", 64) },
		"other kind":         func(c *Config) { c.Kind = "openai" },
		"built-in auth mode": func(c *Config) { c.AuthMode = "api_key" },
		"other endpoint":     func(c *Config) { c.Endpoint = "https://api.acme.example/v3" },
		"cloud context":      func(c *Config) { c.CloudRegion = "us-east-1" },
		"undeclared header":  func(c *Config) { c.SemanticHeaders = map[string]string{"X-Acme-Client": "other"} },
	} {
		t.Run(name, func(t *testing.T) {
			changed := c
			mutate(&changed)
			if changed.Validate(&egress.Policy{}) == nil {
				t.Fatal("accepted")
			}
		})
	}
	if !c.Supports("generation", "anthropic", "streaming") || c.Supports("generation", "bedrock", "unary") || c.Supports("embeddings", "openai", "unary") || c.Supports("token_count", "openai", "unary") {
		t.Fatal("a plugin profile supports more than generation")
	}
	if !SecretRequired(AuthStaticCredential) {
		t.Fatal("a static credential is not a stored secret")
	}
}

func TestPluginProfileValidationLocatesTheOffendingValue(t *testing.T) {
	if err := ValidatePluginProfile(pluginManifest().Profiles[0]); err != nil {
		t.Fatal(err)
	}
	many := map[string]string{}
	for _, name := range strings.Fields("a b c d e f g h i j k l m n o p q") {
		many["X-"+name] = "{credential}"
	}
	for name, tc := range map[string]struct {
		mutate func(*abi.Profile)
		field  string
	}{
		"bedrock dialect":     {func(p *abi.Profile) { p.Dialect = "bedrock-converse" }, "dialect"},
		"no address":          {func(p *abi.Profile) { p.Hosting.Address = "" }, "hosting.address"},
		"address credential":  {func(p *abi.Profile) { p.Hosting.Address = "https://api.acme.example/{credential}" }, "hosting.address"},
		"address option host": {func(p *abi.Profile) { p.Hosting.Address = "https://{options.account}.acme.example/v2" }, "hosting.address"},
		"address stray brace": {func(p *abi.Profile) { p.Hosting.Address = "https://api.acme.example/{v2" }, "hosting.address"},
		"undeclared option":   {func(p *abi.Profile) { p.Hosting.Headers["X-Acme-Client"] = "{options.account}" }, "hosting.headers.X-Acme-Client"},
		"address query":       {func(p *abi.Profile) { p.Hosting.Address = "https://api.acme.example/v2?key=x" }, "hosting.address"},
		"address credentials": {func(p *abi.Profile) { p.Hosting.Address = "https://user:pass@api.acme.example/v2" }, "hosting.address"},
		"address scheme":      {func(p *abi.Profile) { p.Hosting.Address = "wss://api.acme.example/v2" }, "hosting.address"},
		"reserved header":     {func(p *abi.Profile) { p.Hosting.Headers["Content-Type"] = "text/plain" }, "hosting.headers.Content-Type"},
		"olp header":          {func(p *abi.Profile) { p.Hosting.Headers["X-Olp-Route"] = "x" }, "hosting.headers.X-Olp-Route"},
		"malformed header":    {func(p *abi.Profile) { p.Hosting.Headers["X Acme"] = "x" }, "hosting.headers.X Acme"},
		"duplicate header":    {func(p *abi.Profile) { p.Hosting.Headers["x-acme-key"] = "{credential}" }, "hosting.headers.x-acme-key"},
		"semantic header":     {func(p *abi.Profile) { p.Hosting.Headers["OpenAI-Beta"] = "assistants=v2" }, "hosting.headers.OpenAI-Beta"},
		"unknown placeholder": {func(p *abi.Profile) { p.Hosting.Headers["X-Acme-Key"] = "{password}" }, "hosting.headers.X-Acme-Key"},
		"stray brace":         {func(p *abi.Profile) { p.Hosting.Headers["X-Acme-Client"] = "olp}" }, "hosting.headers.X-Acme-Client"},
		"control character":   {func(p *abi.Profile) { p.Hosting.Headers["X-Acme-Client"] = "olp\r\nX-Injected: 1" }, "hosting.headers.X-Acme-Client"},
		"long value":          {func(p *abi.Profile) { p.Hosting.Headers["X-Acme-Client"] = strings.Repeat("v", 2049) }, "hosting.headers.X-Acme-Client"},
		"too many headers":    {func(p *abi.Profile) { p.Hosting.Headers = many }, "hosting.headers"},
		"malformed query":     {func(p *abi.Profile) { p.Hosting.Query["a&b"] = "{credential}" }, "hosting.query.a&b"},
		"gemini addressing":   {func(p *abi.Profile) { p.Dialect, p.Hosting.Query["alt"] = "gemini-generate-content", "json" }, "hosting.query.alt"},
		"gemini semantic":     {func(p *abi.Profile) { p.Dialect, p.Hosting.Query["$xgafv"] = "gemini-generate-content", "2" }, "hosting.query.$xgafv"},
		"credential not given": {func(p *abi.Profile) {
			p.Hosting.Headers, p.Hosting.Query = map[string]string{"X-Acme-Client": "olp"}, nil
		}, "hosting"},
		"relative listing path":      {func(p *abi.Profile) { p.Hosting.Discovery.Path = "models" }, "hosting.discovery.path"},
		"listing path query":         {func(p *abi.Profile) { p.Hosting.Discovery.Path = "/models?limit=100" }, "hosting.discovery.path"},
		"listing dot segment":        {func(p *abi.Profile) { p.Hosting.Discovery.Path = "/v2/../admin" }, "hosting.discovery.path"},
		"listing placeholder":        {func(p *abi.Profile) { p.Hosting.Discovery.Path = "/{credential}/models" }, "hosting.discovery.path"},
		"no model array":             {func(p *abi.Profile) { p.Hosting.Discovery.Models = "" }, "hosting.discovery.models"},
		"no model ID field":          {func(p *abi.Profile) { p.Hosting.Discovery.ID = "" }, "hosting.discovery.id"},
		"control in a field":         {func(p *abi.Profile) { p.Hosting.Discovery.ID = "id\n" }, "hosting.discovery.id"},
		"malformed parameter":        {func(p *abi.Profile) { p.Hosting.Discovery.Pagination.Parameter = "page token" }, "hosting.discovery.pagination.parameter"},
		"placed parameter":           {func(p *abi.Profile) { p.Hosting.Discovery.Pagination.Parameter = "key" }, "hosting.discovery.pagination.parameter"},
		"no cursor field":            {func(p *abi.Profile) { p.Hosting.Discovery.Pagination.Cursor = "" }, "hosting.discovery.pagination.cursor"},
		"long more field":            {func(p *abi.Profile) { p.Hosting.Discovery.Pagination.More = strings.Repeat("m", 129) }, "hosting.discovery.pagination.more"},
		"too many rules":             {func(p *abi.Profile) { p.Hosting.Classification = make([]abi.FailureRule, 33) }, "hosting.classification"},
		"rule matching anything":     {func(p *abi.Profile) { p.Hosting.Classification[1].Type = "" }, "hosting.classification[1]"},
		"successful status":          {func(p *abi.Profile) { p.Hosting.Classification[0].Status = 200 }, "hosting.classification[0].status"},
		"control in a code":          {func(p *abi.Profile) { p.Hosting.Classification[0].Code = "quota\n" }, "hosting.classification[0].code"},
		"long type":                  {func(p *abi.Profile) { p.Hosting.Classification[1].Type = strings.Repeat("t", 257) }, "hosting.classification[1].type"},
		"unknown class":              {func(p *abi.Profile) { p.Hosting.Classification[0].Class = "fatal" }, "hosting.classification[0].class"},
		"no class":                   {func(p *abi.Profile) { p.Hosting.Classification[1].Class = "" }, "hosting.classification[1].class"},
		"grant fact without a grant": {func(p *abi.Profile) { p.Hosting.Headers["X-Acme-Account"] = "{grant.account}" }, "hosting.headers.X-Acme-Account"},
		"undeclared grant fact": {func(p *abi.Profile) {
			p.Grant, p.Hosting.Headers["X-Acme-Account"] = &abi.GrantAuthentication{Facts: []string{"project"}}, "{grant.account}"
		}, "hosting.headers.X-Acme-Account"},
		"grant fact name":      {func(p *abi.Profile) { p.Grant = &abi.GrantAuthentication{Facts: []string{"Account"}} }, "grant.facts[0]"},
		"duplicate grant fact": {func(p *abi.Profile) { p.Grant = &abi.GrantAuthentication{Facts: []string{"account", "account"}} }, "grant.facts[1]"},
		"grant fact in address": {func(p *abi.Profile) {
			p.Grant, p.Hosting.Address = &abi.GrantAuthentication{Facts: []string{"account"}}, "https://api.acme.example/{grant.account}/v2"
		}, "hosting.address"},
		"grant fact in envelope": {func(p *abi.Profile) {
			p.Grant = &abi.GrantAuthentication{Facts: []string{"account"}}
			p.Hosting.Envelope = &abi.Envelope{Request: "request", Fields: map[string]string{"account": "{grant.account}"}}
		}, "hosting.envelope.fields.account"},
	} {
		t.Run(name, func(t *testing.T) {
			p := pluginManifest().Profiles[0]
			tc.mutate(&p)
			refusal, ok := errors.AsType[*ProfileError](ValidatePluginProfile(p))
			if !ok || refusal.Field != tc.field {
				t.Fatalf("want a refusal of %s, got %v", tc.field, refusal)
			}
		})
	}
}

func TestPluginOptionsFillTheHostingAdaptation(t *testing.T) {
	c := pluginConfig(t, optionsManifest())
	c.PluginOptions = map[string]string{"account": "acme/prod", "region": "eu"}
	c.Endpoint = c.Plugin.Address(c.PluginOptions)
	if err := c.Validate(&egress.Policy{}); err != nil {
		t.Fatal(err)
	}
	// Each value fills its part of one path segment, whatever it contains.
	endpoint, err := c.URL(openai.FamilyChat, "acme-large", false)
	if err != nil || endpoint != "https://api.acme.example/accounts/acme%2Fprod/v2/chat/completions" {
		t.Fatalf("addressed %q: %v", endpoint, err)
	}
	req, _ := http.NewRequest(http.MethodPost, endpoint, nil)
	sensitive, err := NewAuth(&egress.Policy{}).Apply(context.Background(), req, c, []byte("secret"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != "Token secret" || req.Header.Get("X-Acme-Region") != "eu" || req.URL.Query().Get("placement") != "eu-acme/prod" {
		t.Fatalf("placed %v %q", req.Header, req.URL.RawQuery)
	}
	if req.URL.EscapedPath() != "/accounts/acme%2Fprod/v2/chat/completions" {
		t.Fatalf("placed the request at %s", req.URL.EscapedPath())
	}
	if !slices.Equal(sensitive, []string{"secret", "Token secret"}) {
		t.Fatalf("redacts %q; options are not secret", sensitive)
	}
	moved := c
	moved.PluginOptions = map[string]string{"account": "other", "region": "eu"}
	if moved.Validate(&egress.Policy{}) == nil {
		t.Fatal("the endpoint kept the address of other options")
	}
	// Published snapshots carry the options with the profile.
	encoded, _ := json.Marshal(c)
	var decoded Config
	if err = json.Unmarshal(encoded, &decoded); err != nil || decoded.Validate(&egress.Policy{}) != nil || !maps.Equal(decoded.PluginOptions, c.PluginOptions) {
		t.Fatalf("decoded %s: %v", encoded, err)
	}
}

func TestPluginOptionValuesFollowTheirDeclaration(t *testing.T) {
	plugin, err := NewPluginProfile(pluginDigest, optionsManifest(), "acme-chat")
	if err != nil {
		t.Fatal(err)
	}
	valid := map[string]string{"account": "acme", "region": "us"}
	if err := plugin.ValidateOptions(valid); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		change map[string]string
		option string
	}{
		"undeclared":        {map[string]string{"colour": "blue"}, "colour"},
		"required unset":    {map[string]string{"account": ""}, "account"},
		"long":              {map[string]string{"account": strings.Repeat("a", 257)}, "account"},
		"control character": {map[string]string{"account": "acme\n"}, "account"},
		"outside the enum":  {map[string]string{"region": "ap"}, "region"},
		"pattern mismatch":  {map[string]string{"team": "Blue Team"}, "team"},
		"dot segment":       {map[string]string{"account": ".."}, "account"},
	} {
		t.Run(name, func(t *testing.T) {
			values := maps.Clone(valid)
			for option, value := range tc.change {
				if value == "" {
					delete(values, option)
				} else {
					values[option] = value
				}
			}
			refusal, ok := errors.AsType[*OptionError](plugin.ValidateOptions(values))
			if !ok || refusal.Option != tc.option {
				t.Fatalf("want a refusal of %s, got %v", tc.option, refusal)
			}
		})
	}
	valid["team"] = "blue"
	if err := plugin.ValidateOptions(valid); err != nil {
		t.Fatalf("refused an optional option: %v", err)
	}
	builtin := Config{Kind: "openai", AuthMode: "api_key", Endpoint: "https://api.openai.com/v1", PluginOptions: map[string]string{"account": "acme"}}
	if builtin.ValidateProfile() == nil {
		t.Fatal("a provider without a plugin profile took plugin options")
	}
}

func TestPluginProfileCataloguesItsOptionsSchema(t *testing.T) {
	plugin, err := NewPluginProfile(pluginDigest, optionsManifest(), "acme-chat")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"additionalProperties":false,"properties":{` +
		`"account":{"description":"The Acme account that serves requests.","maxLength":256,"minLength":1,"title":"Account","type":"string"},` +
		`"region":{"enum":["us","eu"],"maxLength":256,"minLength":1,"title":"Region","type":"string"},` +
		`"team":{"maxLength":256,"minLength":1,"pattern":"^[a-z]+$","title":"Team","type":"string"}},` +
		`"required":["account","region"],"type":"object"}`
	if got := string(plugin.Profile().OptionsSchema); got != want {
		t.Fatalf("options schema %s", got)
	}
	// Properties keep the declared order, which the provider wizard shows.
	reordered := optionsManifest()
	options := reordered.Profiles[0].Options
	options[0], options[2] = options[2], options[0]
	plugin, err = NewPluginProfile(pluginDigest, reordered, "acme-chat")
	if err != nil {
		t.Fatal(err)
	}
	if schema := string(plugin.Profile().OptionsSchema); strings.Index(schema, `"team"`) > strings.Index(schema, `"account"`) {
		t.Fatalf("options schema lost the declared order: %s", schema)
	}
	for _, builtin := range Profiles() {
		if builtin.OptionsSchema != nil {
			t.Fatalf("built-in %s declares options", builtin.ID)
		}
	}
}

func TestPluginOptionDeclarationsAreValidated(t *testing.T) {
	if err := ValidatePluginProfile(optionsManifest().Profiles[0]); err != nil {
		t.Fatal(err)
	}
	many := make([]abi.Option, 17)
	for i := range many {
		many[i] = abi.Option{Name: fmt.Sprintf("option_%d", i), Label: "Option", Optional: true}
	}
	for name, tc := range map[string]struct {
		mutate func(*abi.Profile)
		field  string
	}{
		"too many options":      {func(p *abi.Profile) { p.Options = append(p.Options, many...) }, "options"},
		"malformed name":        {func(p *abi.Profile) { p.Options[2].Name = "Team" }, "options[2].name"},
		"duplicate name":        {func(p *abi.Profile) { p.Options[2].Name = "region" }, "options[2].name"},
		"no label":              {func(p *abi.Profile) { p.Options[0].Label = "" }, "options[0].label"},
		"long description":      {func(p *abi.Profile) { p.Options[0].Description = strings.Repeat("d", 501) }, "options[0].description"},
		"enum and pattern":      {func(p *abi.Profile) { p.Options[1].Pattern = "^[a-z]+$" }, "options[1].pattern"},
		"duplicate enum value":  {func(p *abi.Profile) { p.Options[1].Enum = []string{"us", "us"} }, "options[1].enum[1]"},
		"invalid pattern":       {func(p *abi.Profile) { p.Options[2].Pattern = "(" }, "options[2].pattern"},
		"optional in template":  {func(p *abi.Profile) { p.Hosting.Headers["X-Acme-Team"] = "{options.team}" }, "hosting.headers.X-Acme-Team"},
		"optional in address":   {func(p *abi.Profile) { p.Hosting.Address = "https://api.acme.example/teams/{options.team}" }, "hosting.address"},
		"undeclared in address": {func(p *abi.Profile) { p.Hosting.Address = "https://api.acme.example/{options.project}" }, "hosting.address"},
	} {
		t.Run(name, func(t *testing.T) {
			p := optionsManifest().Profiles[0]
			tc.mutate(&p)
			refusal, ok := errors.AsType[*ProfileError](ValidatePluginProfile(p))
			if !ok || refusal.Field != tc.field {
				t.Fatalf("want a refusal of %s, got %v", tc.field, refusal)
			}
		})
	}
}

// grantManifest declares a profile that authenticates with a grant, whose
// hosting adaptation places grant facts beside an option.
func grantManifest() abi.Manifest {
	m := pluginManifest()
	m.Profiles[0].Grant = &abi.GrantAuthentication{Facts: []string{"account", "project"}}
	m.Profiles[0].Options = []abi.Option{{Name: "region", Label: "Region"}}
	m.Profiles[0].Hosting = abi.Hosting{
		Address: "https://api.acme.example/v2",
		Headers: map[string]string{"Authorization": "Bearer {credential}", "X-Acme-Account": "{grant.account}"},
		Query:   map[string]string{"project": "{grant.project}", "placement": "{options.region}-{grant.account}"},
	}
	return m
}

func grantCredential(t *testing.T, token string, facts map[string]string) []byte {
	t.Helper()
	secret, err := json.Marshal(GrantCredential{AccessToken: token, Facts: facts})
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

// A profile that authenticates with a grant places the grant's current
// access token where the static credential would go, and its grant facts
// beside the provider's options.
func TestPluginGrantProfilePlacesTheAccessTokenAndGrantFacts(t *testing.T) {
	c := pluginConfig(t, grantManifest())
	c.AuthMode, c.PluginOptions = AuthGrant, map[string]string{"region": "eu"}
	if err := c.Validate(&egress.Policy{}); err != nil {
		t.Fatal(err)
	}
	if p, _ := c.Profile(); !slices.Equal(p.Authentication, []string{AuthGrant}) || !SecretRequired(AuthGrant) {
		t.Fatalf("a grant profile authenticates with %v", p.Authentication)
	}
	req, _ := http.NewRequest(http.MethodPost, "https://api.acme.example/v2/chat/completions", nil)
	sensitive, err := NewAuth(&egress.Policy{}).Apply(context.Background(), req, c, grantCredential(t, "at-123", map[string]string{"account": "acct 7", "project": "p/1"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != "Bearer at-123" || req.Header.Get("X-Acme-Account") != "acct 7" || req.URL.RawQuery != "placement=eu-acct+7&project=p%2F1" {
		t.Fatalf("placed %v %q", req.Header, req.URL.RawQuery)
	}
	if !slices.Contains(sensitive, "at-123") || !slices.Contains(sensitive, "Bearer at-123") || slices.Contains(sensitive, "acct 7") {
		t.Fatalf("redacts %q", sensitive)
	}
	for name, secret := range map[string][]byte{
		"static credential":  []byte("at-123"),
		"no access token":    grantCredential(t, "", map[string]string{"account": "a", "project": "p"}),
		"unrecorded fact":    grantCredential(t, "at-123", map[string]string{"account": "a"}),
		"fact out of header": grantCredential(t, "at-123", map[string]string{"account": "a\r\nX-Injected: 1", "project": "p"}),
	} {
		req, _ := http.NewRequest(http.MethodPost, "https://api.acme.example/v2/chat/completions", nil)
		if _, err := NewAuth(&egress.Policy{}).Apply(context.Background(), req, c, secret, nil); !errors.Is(err, ErrCredentialRejected) {
			t.Errorf("%s: placed with %v", name, err)
		}
	}
	static := c
	static.AuthMode = AuthStaticCredential
	if static.Validate(&egress.Policy{}) == nil {
		t.Fatal("a grant profile accepted a static credential")
	}
}
