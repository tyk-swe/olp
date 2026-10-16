package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/egress"
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

// publish returns c as a published snapshot carries it, with the snapshot's
// JSON.
func publish(t *testing.T, c Config) (Config, []byte) {
	t.Helper()
	encoded, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var published Config
	if err = json.Unmarshal(encoded, &published); err != nil {
		t.Fatalf("decoded %s: %v", encoded, err)
	}
	return published, encoded
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
		"undeclared base fact": {func(p *abi.Profile) {
			p.Grant, p.Hosting.Address = &abi.GrantAuthentication{Facts: []string{"account"}}, "{grant.api_base}/v2"
		}, "hosting.address"},
		"base fact without a grant": {func(p *abi.Profile) { p.Hosting.Address = "{grant.api_base}/v2" }, "hosting.address"},
		"base fact before a name": {func(p *abi.Profile) {
			p.Grant, p.Hosting.Address = &abi.GrantAuthentication{Facts: []string{"api_base"}}, "{grant.api_base}v2"
		}, "hosting.address"},
		"base fact with a query": {func(p *abi.Profile) {
			p.Grant, p.Hosting.Address = &abi.GrantAuthentication{Facts: []string{"api_base"}}, "{grant.api_base}/v2?key=x"
		}, "hosting.address"},
		"second grant fact in address": {func(p *abi.Profile) {
			p.Grant, p.Hosting.Address = &abi.GrantAuthentication{Facts: []string{"api_base", "account"}}, "{grant.api_base}/{grant.account}"
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

// Each value fills its part of one path segment, whatever it contains.

// Published snapshots carry the options with the profile.

func TestPluginOptionValuesFollowTheirDeclaration(t *testing.T) {
	plugin := pluginConfig(t, optionsManifest()).Plugin
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

// Properties keep the declared order, which the provider wizard shows.

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
	secret, err := json.Marshal(GrantCredential{PluginDigest: pluginDigest, ProfileID: "acme-chat", AccessToken: token, Facts: facts})
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

// baseManifest declares a grant profile whose address begins with the grant
// fact that holds the upstream's base URL, at one of two approved origins.
func baseManifest() abi.Manifest {
	m := grantManifest()
	m.Origins = []string{"https://api.acme.example", "https://eu.acme.example"}
	m.Profiles[0].Grant.Facts = append(m.Profiles[0].Grant.Facts, "api_base")
	m.Profiles[0].Hosting.Address = "{grant.api_base}/v2/{options.region}"
	return m
}

// A grant fact that begins the address gives each credential version its own
// base URL: the provider's endpoint stands for it, and placement moves every
// request there, before any header or signature is made.
func TestPluginGrantBaseURLAddressesEachCredentialVersion(t *testing.T) {
	c := pluginConfig(t, baseManifest())
	c.AuthMode, c.PluginOptions = AuthGrant, map[string]string{"region": "eu"}
	c.Endpoint = c.Plugin.Address(c.PluginOptions)
	if err := c.Validate(&egress.Policy{}); err != nil || c.Endpoint != "https://grant.invalid/v2/eu" {
		t.Fatalf("endpoint %s: %v", c.Endpoint, err)
	}
	published, _ := publish(t, c)
	for base, want := range map[string]string{
		"https://api.acme.example":           "https://api.acme.example/v2/eu/chat/completions?placement=eu-acct&project=p",
		"https://eu.acme.example/regions/1/": "https://eu.acme.example/regions/1/v2/eu/chat/completions?placement=eu-acct&project=p",
		"https://eu.acme.example/a%2Fb":      "https://eu.acme.example/a%2Fb/v2/eu/chat/completions?placement=eu-acct&project=p",
		// Origins compare as manifests declare them: lowercase, without the
		// scheme's default port.
		"https://API.acme.example":      "https://api.acme.example/v2/eu/chat/completions?placement=eu-acct&project=p",
		"HTTPS://api.acme.example:443/": "https://api.acme.example/v2/eu/chat/completions?placement=eu-acct&project=p",
	} {
		for _, c := range []Config{c, published} {
			endpoint, err := c.URL(wire.Chat, "acme-large", false)
			if err != nil {
				t.Fatal(err)
			}
			req, _ := http.NewRequest(http.MethodPost, endpoint, nil)
			secret := grantCredential(t, "at-123", map[string]string{"account": "acct", "project": "p", "api_base": base})
			if _, err = NewAuth(&egress.Policy{}).Apply(context.Background(), req, c, secret, nil); err != nil {
				t.Fatal(err)
			}
			if req.URL.String() != want || req.Host != req.URL.Host || req.Header.Get("Authorization") != "Bearer at-123" {
				t.Fatalf("placed at %s (host %s) with %v, want %s", req.URL, req.Host, req.Header, want)
			}
		}
	}

	for base, reason := range map[string]string{
		"https://evil.example/v2":          "grant fact api_base places requests at https://evil.example, which is not one of the plugin's approved origins",
		"https://api.acme.example:8443":    "grant fact api_base places requests at https://api.acme.example:8443, which is not one of the plugin's approved origins",
		"http://api.acme.example":          "grant fact api_base places requests at http://api.acme.example, which is not one of the plugin's approved origins",
		"":                                 "grant fact api_base holds no base URL",
		"api.acme.example/v2":              "grant fact api_base holds no base URL",
		"https://user@api.acme.example/v2": "grant fact api_base holds no base URL",
		"https://api.acme.example/v2?x=1":  "grant fact api_base holds no base URL",
		"https://api.acme.example/v2#x":    "grant fact api_base holds no base URL",
	} {
		req, _ := http.NewRequest(http.MethodPost, c.Endpoint+"/chat/completions", nil)
		secret := grantCredential(t, "at-123", map[string]string{"account": "acct", "project": "p", "api_base": base})
		_, err := NewAuth(&egress.Policy{}).Apply(context.Background(), req, published, secret, nil)
		if !errors.Is(err, ErrCredentialRejected) || !strings.Contains(err.Error(), reason) || req.URL.Host != "grant.invalid" {
			t.Errorf("%q: placed at %s with %v", base, req.URL, err)
		}
	}
	// Only a request addressed from the provider's endpoint moves.
	req, _ := http.NewRequest(http.MethodPost, "https://api.acme.example/v2/eu/chat/completions", nil)
	secret := grantCredential(t, "at-123", map[string]string{"account": "acct", "project": "p", "api_base": "https://eu.acme.example"})
	if _, err := NewAuth(&egress.Policy{}).Apply(context.Background(), req, c, secret, nil); err == nil {
		t.Fatalf("moved a request addressed elsewhere to %s", req.URL)
	}

	// The egress policy still applies to where the request goes.
	local := baseManifest()
	local.Origins = []string{"http://127.0.0.1:9"}
	c = pluginConfig(t, local)
	c.AuthMode, c.PluginOptions = AuthGrant, map[string]string{"region": "eu"}
	req, _ = http.NewRequest(http.MethodPost, c.Plugin.Address(c.PluginOptions)+"/chat/completions", nil)
	secret = grantCredential(t, "at-123", map[string]string{"account": "acct", "project": "p", "api_base": "http://127.0.0.1:9"})
	if _, err := NewAuth(&egress.Policy{}).Apply(context.Background(), req, c, secret, nil); err != nil {
		t.Fatal(err)
	}
	client, err := egress.Policy{}.ConnectionClient(nil, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Do(req); err == nil || !strings.Contains(err.Error(), "endpoint must use https") {
		t.Fatalf("sent to %s under a policy that refuses plain HTTP: %v", req.URL, err)
	}
}
