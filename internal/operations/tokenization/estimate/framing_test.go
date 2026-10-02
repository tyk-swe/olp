package estimate

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/tests/fixtures"
)

// framingFixture is a chat conversation counted by the OpenAI Cookbook's
// num_tokens_from_messages on top of tiktoken, by generate.py.
type framingFixture struct {
	Name     string              `json:"name"`
	Model    string              `json:"model"`
	Encoding string              `json:"encoding"`
	Messages []map[string]string `json:"messages"`
	Tokens   int64               `json:"tokens"`
}

// TestFramingReproducesTheCookbookCounts counts each conversation the way a
// request walker will: every field's value as a segment of its own, plus the
// framing, and expects the cookbook formula's number.
func TestFramingReproducesTheCookbookCounts(t *testing.T) {
	data, err := fixtures.Files.ReadFile("tokens/framing.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []framingFixture
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 16 {
		t.Fatalf("%d framing fixtures, want the full set", len(cases))
	}
	for _, f := range cases {
		t.Run(f.Name, func(t *testing.T) {
			c := ForModel(f.Model)
			if c.encoding == nil || c.encoding.Name() != f.Encoding {
				t.Fatalf("%s resolves to %v, the fixture counted with %s", f.Model, c.encoding, f.Encoding)
			}
			m := c.Meter()
			names := 0
			for _, message := range f.Messages {
				for field, value := range message {
					m.Add(value)
					if field == "name" {
						names++
					}
				}
			}
			text, provenance := m.Total()
			if provenance != ProvenanceTokenizer {
				t.Fatalf("provenance %s", provenance)
			}
			if got := text + c.Framing().Overhead(len(f.Messages), names); got != f.Tokens {
				t.Fatalf("%d tokens of text and framing, the cookbook formula counts %d", got, f.Tokens)
			}
		})
	}
}

func TestFraming(t *testing.T) {
	want := Framing{PerMessage: 3, PerName: 1, Reply: 3}
	for _, model := range []string{"gpt-4o", "gpt-4.1", "o3", "gpt-4", "gpt-3.5-turbo", "ft:gpt-4o:acme::x"} {
		if got := ForModel(model).Framing(); got != want {
			t.Errorf("%s: framing %+v, want %+v", model, got, want)
		}
	}
	for _, model := range []string{"claude-sonnet-4-5", "gemini-2.5-pro", "llama-3", "my-deployment", ""} {
		if got := ForModel(model).Framing(); got != (Framing{}) {
			t.Errorf("%s: framing %+v, want none", model, got)
		}
	}
	for _, tc := range []struct {
		messages, names int
		want            int64
	}{
		{0, 0, 0}, {-1, 0, 0}, {1, 0, 6}, {3, 0, 12}, {3, 1, 13}, {4, 4, 19},
	} {
		if got := want.Overhead(tc.messages, tc.names); got != tc.want {
			t.Errorf("Overhead(%d, %d) = %d, want %d", tc.messages, tc.names, got, tc.want)
		}
	}
	if got := (Framing{}).Overhead(5, 2); got != 0 {
		t.Errorf("no framing has overhead %d", got)
	}
}
