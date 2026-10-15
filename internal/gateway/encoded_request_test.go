package gateway

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/providerinvoke"
	"github.com/tyk-swe/olp/internal/routing"
)

func TestAnAttemptIsSentTheBodyPlanningMadeIfItIsStillTheRequest(t *testing.T) {
	parsed, err := openai.Parse(wire.Chat, []byte(`{"model":"team-chat","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	p := &routing.Provider{ID: uuid.NewString(), RevisionID: uuid.NewString(), Kind: "openai", ParameterDefaults: map[string]json.RawMessage{"temperature": json.RawMessage(`0.2`)}}
	x := &call{generation: generationCall{parsed: parsed}}
	if err := x.encodes(p, p.Connector(), "first", "gpt-4o"); err != nil {
		t.Fatal(err)
	}
	record := x.targetPreparation(p, "first", "gpt-4o")
	invocation, err := providerinvoke.Prepare(parsed, p.Connector(), "gpt-4o", p.ParameterDefaults)
	expected, format := invocation.Prepared.Document().Bytes(), invocation.Wire
	if err != nil {
		t.Fatal(err)
	}
	body, got, _, err := x.encoding(p, p.Connector(), "first", "gpt-4o")
	if err != nil || got != format || !bytes.Equal(body, expected) {
		t.Fatalf("prepared body changed: %s %v", body, err)
	}
	record.encoding.body = []byte("made by planning")
	body, _, _, err = x.encoding(p, p.Connector(), "first", "gpt-4o")
	if err != nil || string(body) != "made by planning" {
		t.Fatal("dispatch did not reuse prepared body", err)
	}
	revised := *p
	revised.RevisionID = uuid.NewString()
	for _, tc := range []struct {
		target, model string
		provider      *routing.Provider
	}{{"second", "gpt-4o", p}, {"first", "gpt-4o-mini", p}, {"first", "gpt-4o", &revised}} {
		body, _, _, err = x.encoding(tc.provider, tc.provider.Connector(), tc.target, tc.model)
		invocation, prepareErr := providerinvoke.Prepare(parsed, tc.provider.Connector(), tc.model, tc.provider.ParameterDefaults)
		expected := invocation.Prepared.Document().Bytes()
		if err != nil || prepareErr != nil || !bytes.Equal(body, expected) {
			t.Fatal("target identity reused another preparation", err, prepareErr)
		}
	}
	next, err := parsed.WithField("user", json.RawMessage(`"someone"`))
	if err != nil {
		t.Fatal(err)
	}
	x.generation.parsed = next
	body, _, _, err = x.encoding(p, p.Connector(), "first", "gpt-4o")
	if err != nil || !bytes.Contains(body, []byte(`"user":"someone"`)) {
		t.Fatal("changed source reused old preparation", err)
	}
	if bytes.Contains(parsed.Field("user"), []byte("someone")) {
		t.Fatal("transform mutated source")
	}
}

func TestPlanningKeepsNoBodyForATargetThatCannotTakeTheRequest(t *testing.T) {
	parsed, err := openai.Parse(wire.Chat, []byte(`{"model":"team-chat","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	p := &routing.Provider{ID: "p", Kind: "openai", ProfileID: "missing-profile"}
	x := &call{generation: generationCall{parsed: parsed}}
	if err := x.encodes(p, p.Connector(), "target", "model"); err == nil {
		t.Fatal("invalid profile admitted")
	}
	for _, record := range x.targets {
		if record.heavy() {
			t.Fatal("failed preparation retained a body")
		}
	}
}

func TestPlanningKeepsAFewBodiesAtMost(t *testing.T) {
	parsed, err := openai.Parse(wire.Chat, []byte(`{"model":"team-chat","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	x := &call{generation: generationCall{parsed: parsed}}
	p := &routing.Provider{ID: uuid.NewString(), RevisionID: uuid.NewString(), Kind: "openai"}
	for range keptEncodings + 3 {
		if err := x.encodes(p, p.Connector(), uuid.NewString(), "gpt-4o"); err != nil {
			t.Fatal(err)
		}
	}
	retained := 0
	for _, record := range x.targets {
		if record.heavy() {
			retained++
		}
	}
	if retained != keptEncodings {
		t.Fatalf("retained %d plans, want %d", retained, keptEncodings)
	}
}

func TestRetentionPolicyAppliesToNativeDestinationAcrossEncodingPaths(t *testing.T) {
	for _, kind := range []string{"openai", "anthropic", "gemini", "vertex_ai", "bedrock"} {
		t.Run(kind, func(t *testing.T) {
			parsed, err := openai.Parse(wire.Responses, []byte(`{"model":"team-chat","input":"private prompt","max_output_tokens":32}`))
			if err != nil {
				t.Fatal(err)
			}
			p := &routing.Provider{ID: kind, Kind: kind}
			x := &call{generation: generationCall{parsed: parsed}}
			if err := x.encodes(p, p.Connector(), "target", "model"); err != nil {
				t.Fatal(err)
			}
			for _, evict := range []bool{false, true} {
				if evict {
					x.targetPreparation(p, "target", "model").discard(false)
				}
				body, format, _, err := x.encoding(p, p.Connector(), "target", "model")
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(body, &fields); err != nil {
					t.Fatal(err)
				}
				if format == wire.Responses {
					if string(fields["store"]) != "false" {
						t.Fatalf("retention allowed: %s", body)
					}
				} else if fields["store"] != nil {
					t.Fatalf("unsupported retention field: %s", body)
				}
			}
			if parsed.Field("store") != nil {
				t.Fatal("destination policy mutated source")
			}
		})
	}
}

func TestRetentionPolicyPreservesOptInAndProviderDefaults(t *testing.T) {
	for _, allow := range []bool{false, true} {
		for _, field := range []string{"", `,"store":null`, `,"store":false`, `,"store":true`} {
			parsed, err := openai.Parse(wire.Responses, []byte(`{"model":"team-chat","input":"private prompt","max_output_tokens":32`+field+`}`))
			if err != nil {
				t.Fatal(err)
			}
			p := &routing.Provider{ID: "provider", Kind: "openai", ParameterDefaults: map[string]json.RawMessage{"temperature": json.RawMessage(`0.2`)}}
			x := &call{generation: generationCall{parsed: parsed}}
			x.authority.Policy.AllowProviderState = allow
			prepared, err := x.preparedProvider(p, "target", "model")
			if err != nil {
				t.Fatal(err)
			}
			body := prepared.invocation.Prepared.Document().Bytes()
			if allow {
				original, err := providerinvoke.Prepare(parsed, p.Connector(), "model", p.ParameterDefaults)
				if err != nil || !bytes.Equal(body, original.Prepared.Document().Bytes()) {
					t.Fatal("state-enabled request changed", err)
				}
			} else {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(body, &fields); err != nil || string(fields["store"]) != "false" {
					t.Fatalf("retention allowed: %s %v", body, err)
				}
			}
		}
	}
}
