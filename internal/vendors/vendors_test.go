package vendors

import (
	"slices"
	"testing"
)

func TestEveryConnectorKindHasOneDefaultVendor(t *testing.T) {
	defaults := map[string]string{}
	for _, c := range All() {
		if !c.KindDefault {
			continue
		}
		if prior, ok := defaults[c.Connector]; ok {
			t.Fatalf("%s has default vendors %s and %s", c.Connector, prior, c.ID)
		}
		defaults[c.Connector] = c.ID
	}
	for kind, want := range map[string]string{"openai": "openai", "openai_compatible": "openai_compatible", "anthropic": "anthropic", "gemini": "google", "vertex_ai": "google-vertex", "bedrock": "amazon-bedrock", "azure_openai": "azure", "plugin": "plugin"} {
		if got := DefaultFor(kind); got != want {
			t.Fatalf("DefaultFor(%s) = %s, want %s", kind, got, want)
		}
	}
}

func TestUncontractedVendorsAreRestrictedOnlyByTheirKind(t *testing.T) {
	if !Serves("unreviewed", "rerank") || !Speaks("unreviewed", "openai-responses") {
		t.Fatal("a vendor without a contract was restricted by the catalogue")
	}
	if Serves("voyage", "generation") || Speaks("deepseek", "openai-responses") {
		t.Fatal("a reviewed restriction was not applied")
	}
}

func TestContractsAreDetached(t *testing.T) {
	c, _ := Lookup("voyage")
	c.Operations[0] = "generation"
	c.Requests["embeddings"].Rewrites[0] = Rewrite{}
	again, _ := Lookup("voyage")
	if again.Serves("generation") || again.Requests["embeddings"].Rewrites[0].From != "dimensions" {
		t.Fatal("a caller changed the reviewed catalogue")
	}
}

func TestRequestShapeIncludesVendorWideRefusals(t *testing.T) {
	c, _ := Lookup("cohere")
	shape := c.Request("rerank")
	if !slices.Contains(shape.Unsupported, "truncation") || !slices.Contains(shape.Unsupported, "dimensions") {
		t.Fatalf("rerank shape = %+v", shape)
	}
	if c.Request("generation").Rewrites[0].To != "max_tokens" {
		t.Fatal("generation shape lost its token limit rewrite")
	}
}

func TestInvalidContractsAreRefused(t *testing.T) {
	valid := Contract{ID: "x", Name: "X", Maintainer: "X", Description: "X.", Connector: "openai_compatible", Endpoint: "https://x.example/v1", Documentation: Link{"X", "https://x.example/docs"}, Operations: []string{"generation"}, Preset: &Preset{AuthMode: "api_key"}}
	if err := valid.validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Contract){
		"insecure documentation": func(c *Contract) { c.Documentation.URL = "http://x.example/docs" },
		"insecure endpoint":      func(c *Contract) { c.Endpoint = "http://x.example/v1" },
		"endpoint with query":    func(c *Contract) { c.Endpoint = "https://x.example/v1?key=1" },
		"unknown auth":           func(c *Contract) { c.Preset.AuthMode = "azure_default" },
		"unserved probe":         func(c *Contract) { c.ProbeOperation = "embeddings" },
		"unserved shape":         func(c *Contract) { c.Requests = map[string]RequestShape{"rerank": {}} },
		"default preset":         func(c *Contract) { c.KindDefault = true },
		"profile without revision": func(c *Contract) {
			c.Preset.Profile = &ProfileRef{ID: "compatible-chat"}
		},
	} {
		c := valid.clone()
		mutate(&c)
		if c.validate() == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}
