package connectors

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func TestLegacyCompletionProfilesAreVendorBound(t *testing.T) {
	for _, vendor := range []string{"openai", "vllm", "tgi", "fireworks", "together"} {
		kind := "openai_compatible"
		if vendor == "openai" {
			kind = "openai"
		}
		c := Config{Kind: kind, VendorID: vendor, ProfileID: vendor + "-completions", ProfileRevision: "1", Endpoint: "https://provider.example/v1", AuthMode: "api_key"}
		if err := c.ValidateProfile(); err != nil {
			t.Fatalf("%s: %v", vendor, err)
		}
		for _, stream := range []bool{false, true} {
			endpoint, err := c.URL(wire.Completions, "native-model", stream)
			if err != nil || !strings.HasSuffix(endpoint, "/v1/completions") {
				t.Fatalf("%s: %s %v", vendor, endpoint, err)
			}
		}
		if _, err := c.URL(wire.Chat, "native-model", false); err == nil {
			t.Fatalf("%s accepted a different native dialect", vendor)
		}
		c.VendorID = "unreviewed"
		if c.ValidateProfile() == nil {
			t.Fatalf("%s can be impersonated", vendor)
		}
	}
}

func TestTGICompletionRequestSubset(t *testing.T) {
	c := Config{ProfileID: "tgi-completions"}
	for _, tc := range []struct {
		fields string
		valid  bool
	}{
		{`"prompt":"hello"`, true},
		{`"prompt":["one","two"]`, true},
		{`"prompt":"hello","stream":true,"stream_options":{"include_usage":true}`, true},
		{`"prompt":["one","two"],"stream":true`, false},
		{`"prompt":[1,2]`, false},
		{`"prompt":"hello","n":1`, false},
		{`"prompt":"hello","echo":false`, false},
		{`"prompt":"hello","best_of":1`, false},
		{`"prompt":"hello","logprobs":0`, false},
		{`"prompt":"hello","max_tokens":4294967296`, false},
		{`"prompt":"hello","seed":-1`, false},
		{`"prompt":"hello","stop":"end"`, false},
	} {
		doc, err := oif.ParseJSON([]byte(`{"model":"route",`+tc.fields+`}`), oif.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		if err = c.ValidateCompletionRequest(doc); (err == nil) != tc.valid {
			t.Errorf("%s: %v", tc.fields, err)
		}
	}
	p, err := LookupProfile("tgi-completions", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err = validateDefaultSet(p, "generation", DefaultSet{Dialect: "openai-completions", Values: map[string]json.RawMessage{"n": json.RawMessage(`2`)}}); err == nil {
		t.Fatal("TGI accepted an ignored provider default")
	}
}

func TestLegacyCompletionDefaultsRespectNativeTypes(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		valid       bool
	}{
		{"logprobs", "3", true}, {"logprobs", "true", false}, {"echo", "true", true}, {"echo", `"true"`, false}, {"suffix", `"text"`, false}, {"prompt", `"text"`, false},
	} {
		err := validateDialectDefaultValue("openai-completions", "generation", tc.name, json.RawMessage(tc.value))
		if (err == nil) != tc.valid {
			t.Errorf("%s=%s: %v", tc.name, tc.value, err)
		}
	}
}

func TestLegacyCompletionPublishedDefaultSchemas(t *testing.T) {
	profile, err := LookupProfile("openai-completions", "1")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Values struct {
				Properties map[string]struct {
					Type    []string `json:"type"`
					Minimum *int     `json:"minimum"`
					Maximum *int     `json:"maximum"`
				} `json:"properties"`
			} `json:"values"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(profile.DefaultSchemas["generation"], &schema); err != nil {
		t.Fatal(err)
	}
	for field, kind := range map[string]string{"logprobs": "integer", "best_of": "integer", "seed": "integer", "echo": "boolean", "logit_bias": "object"} {
		value := schema.Properties.Values.Properties[field]
		if len(value.Type) != 2 || value.Type[0] != kind || value.Type[1] != "null" {
			t.Errorf("%s schema: %+v", field, value)
		}
	}
	logprobs := schema.Properties.Values.Properties["logprobs"]
	if logprobs.Minimum == nil || *logprobs.Minimum != 0 || logprobs.Maximum == nil || *logprobs.Maximum != 5 {
		t.Fatal("logprobs range is not 0..5")
	}
	if minimum := schema.Properties.Values.Properties["max_tokens"].Minimum; minimum == nil || *minimum != 0 {
		t.Fatal("max_tokens must admit zero")
	}
}
