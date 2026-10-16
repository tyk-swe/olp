package vendors

import (
	"testing"
)

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
