package connectors

import (
	"net/http"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
)

func changed(c Config, change func(*Config)) Config {
	change(&c)
	return c
}

func validConfigs() map[string]Config {
	return map[string]Config{
		"openai":     direct(KindOpenAI, "openai-chat", "https://openai.example/v1"),
		"compatible": direct(KindOpenAICompatible, "compatible-chat", "https://compatible.example/v1"),
		"anthropic":  direct(KindAnthropic, "anthropic-messages", "https://anthropic.example"),
		"gemini":     direct(KindGemini, "gemini-generation", "https://gemini.example/v1beta"),
		"azure":      changed(azure("azure-legacy-chat"), func(c *Config) { c.Deployment = "dep-1" }),
		"azure v1":   direct(KindAzureOpenAI, "azure-v1-chat", "https://fixture.openai.azure.com"),
		"vertex":     vertex("vertex-gemini"),
		"bedrock":    amazon(KindBedrock, "bedrock-converse"),
		"sagemaker":  amazon(KindSageMaker, "sagemaker-openai-chat"),
		"search": changed(direct(KindOpenAICompatible, "brave-search", "https://search.example"), func(c *Config) {
			c.VendorID = "brave"
		}),
		"watsonx": {
			Kind: KindWatsonx, AuthMode: AuthIBMIAM, CloudRegion: "us-south", CloudProject: vertexProject,
			Endpoint: "https://us-south.ml.cloud.ibm.com",
		},
		"automatic": {Kind: KindOpenAI, AuthMode: AuthAPIKey, Endpoint: "https://openai.example/v1"},
	}
}

func TestValidateAcceptsAConfigurationThatFitsItsProfile(t *testing.T) {
	t.Parallel()
	valid := validConfigs()
	cases := map[string]Config{
		"an anthropic beta header": changed(valid["anthropic"], func(c *Config) {
			c.SemanticHeaders = map[string]string{"Anthropic-Beta": "files-api-2025-04-14"}
		}),
		"the dialect's anthropic version": changed(valid["anthropic"], func(c *Config) {
			c.SemanticHeaders = map[string]string{"Anthropic-Version": anthropicMessagesRevision}
		}),
		"a gemini error format": changed(valid["gemini"], func(c *Config) { c.QuerySettings = map[string]string{"$xgafv": "2"} }),
		"the provider's azure api version": changed(valid["azure"], func(c *Config) {
			c.QuerySettings = map[string]string{"api-version": c.APIVersion}
		}),
	}
	for name, config := range valid {
		cases[name] = config
	}
	for name, config := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := config.Validate(&egress.Policy{}); err != nil {
				t.Errorf("Validate refused a fitting configuration: %v", err)
			}
		})
	}
}

func TestValidateRefusesAConfigurationOutsideItsProfile(t *testing.T) {
	t.Parallel()
	valid := validConfigs()
	cases := map[string]Config{
		"an unknown profile":                   changed(valid["openai"], func(c *Config) { c.ProfileID = "openai-unknown" }),
		"a stale profile revision":             changed(valid["openai"], func(c *Config) { c.ProfileRevision = "0" }),
		"a profile of another kind":            changed(valid["openai"], func(c *Config) { c.Kind = KindAnthropic }),
		"authentication off the list":          changed(valid["openai"], func(c *Config) { c.AuthMode = AuthADC }),
		"an unreviewed search vendor":          changed(valid["search"], func(c *Config) { c.VendorID = "" }),
		"a header off the allowlist":           changed(valid["openai"], func(c *Config) { c.SemanticHeaders = map[string]string{"X-Other": "1"} }),
		"another anthropic version":            changed(valid["anthropic"], func(c *Config) { c.SemanticHeaders = map[string]string{"Anthropic-Version": "2020-01-01"} }),
		"an unknown gemini error format":       changed(valid["gemini"], func(c *Config) { c.QuerySettings = map[string]string{"$xgafv": "3"} }),
		"another azure api version":            changed(valid["azure"], func(c *Config) { c.QuerySettings = map[string]string{"api-version": "2020-01-01"} }),
		"a query setting off the allowlist":    changed(valid["openai"], func(c *Config) { c.QuerySettings = map[string]string{"api-version": "1"} }),
		"semantic headers without a profile":   changed(valid["automatic"], func(c *Config) { c.SemanticHeaders = map[string]string{"Openai-Beta": "x"} }),
		"an azure endpoint with a path":        changed(valid["azure"], func(c *Config) { c.Endpoint += "/openai" }),
		"an azure api version that is no date": changed(valid["azure"], func(c *Config) { c.APIVersion = "latest" }),
		"azure without a deployment":           changed(valid["azure"], func(c *Config) { c.Deployment = "" }),
		"a vertex endpoint of another project": changed(valid["vertex"], func(c *Config) {
			c.Endpoint = DefaultProfileEndpoint(KindVertexAI, c.ProfileID, vertexRegion, "other-project")
		}),
		"a vertex endpoint off google":        changed(valid["vertex"], func(c *Config) { c.Endpoint = "https://vertex.example/v1" }),
		"vertex without a project":            changed(valid["vertex"], func(c *Config) { c.CloudProject = "" }),
		"bedrock without a region":            changed(valid["bedrock"], func(c *Config) { c.CloudRegion = "" }),
		"a sagemaker endpoint with a path":    changed(valid["sagemaker"], func(c *Config) { c.Endpoint += "/endpoints" }),
		"watsonx without a project":           changed(valid["watsonx"], func(c *Config) { c.CloudProject = "" }),
		"cloud context on a direct connector": changed(valid["openai"], func(c *Config) { c.CloudRegion = "us-east-1" }),
		"an endpoint that is no url":          changed(valid["openai"], func(c *Config) { c.Endpoint = "openai.example" }),
	}
	for name, config := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := config.Validate(&egress.Policy{}); err == nil {
				t.Error("Validate accepted a configuration outside its profile")
			}
		})
	}
}

func TestApplySemanticSendsTheProfileHeadersAndQuerySettings(t *testing.T) {
	t.Parallel()
	valid := validConfigs()
	anthropic := changed(valid["anthropic"], func(c *Config) {
		c.SemanticHeaders = map[string]string{"Anthropic-Beta": "files-api-2025-04-14"}
	})
	gemini := changed(valid["gemini"], func(c *Config) { c.QuerySettings = map[string]string{"$xgafv": "2"} })

	t.Run("anthropic names the dialect revision and the configured beta", func(t *testing.T) {
		t.Parallel()
		req := upstreamRequest(t, "https://anthropic.example/messages")
		if err := anthropic.ApplySemantic(req); err != nil {
			t.Fatal(err)
		}
		want := http.Header{"Anthropic-Version": {anthropicMessagesRevision}, "Anthropic-Beta": {"files-api-2025-04-14"}}
		for name := range want {
			if got := req.Header.Get(name); got != want.Get(name) {
				t.Errorf("%s = %q, want %q", name, got, want.Get(name))
			}
		}
	})
	t.Run("a query setting joins the operation's query", func(t *testing.T) {
		t.Parallel()
		req := upstreamRequest(t, "https://gemini.example/v1beta/models/x:streamGenerateContent?alt=sse")
		if err := gemini.ApplySemantic(req); err != nil {
			t.Fatal(err)
		}
		if got, want := req.URL.RawQuery, "%24xgafv=2&alt=sse"; got != want {
			t.Errorf("query = %q, want %q", got, want)
		}
	})
	t.Run("a query setting that disagrees with the operation is refused", func(t *testing.T) {
		t.Parallel()
		req := upstreamRequest(t, "https://gemini.example/v1beta/models/x:generateContent?$xgafv=1")
		if err := gemini.ApplySemantic(req); err == nil {
			t.Error("ApplySemantic overwrote the operation's own query")
		}
	})
	t.Run("a provider without a profile sends nothing", func(t *testing.T) {
		t.Parallel()
		req := upstreamRequest(t, "https://openai.example/v1/chat/completions")
		if err := valid["automatic"].ApplySemantic(req); err != nil {
			t.Fatal(err)
		}
		if len(req.Header) != 0 || req.URL.RawQuery != "" {
			t.Errorf("request gained headers %v and query %q", req.Header, req.URL.RawQuery)
		}
	})
}
