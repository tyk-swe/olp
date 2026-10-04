package gateway

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/providerinvoke"
	"github.com/tyk-swe/olp/internal/runtime"
)

// Planning asks every target whether it can take the request as the caller sent
// it by encoding the request for it, and the attempt that serves the request
// encodes it again to send it. The attempt is given the body planning made when
// that is the body it would make, and makes one when it is not.
func TestAnAttemptIsSentTheBodyPlanningMadeIfItIsStillTheRequest(t *testing.T) {
	parsed, err := openai.Parse(openai.FamilyChat, []byte(`{"model":"team-chat","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	newProvider := func() *runtime.Provider {
		return &runtime.Provider{ID: uuid.NewString(), RevisionID: uuid.NewString(), Kind: "openai", ParameterDefaults: map[string]json.RawMessage{"temperature": json.RawMessage(`0.2`)}}
	}
	provider, other := newProvider(), newProvider()
	cfg := provider.Connector()
	x := &execution{parsed: parsed}

	if err := x.encodes(provider, cfg, "gpt-4o"); err != nil {
		t.Fatal(err)
	}
	if err := x.encodes(other, other.Connector(), "gpt-4o"); err != nil {
		t.Fatal(err)
	}
	want, wire, err := providerinvoke.Encode(parsed, cfg, "gpt-4o", provider.ParameterDefaults)
	if err != nil {
		t.Fatal(err)
	}
	kept := x.takeEncoded()
	if len(kept) != 2 || x.encoded != nil || x.takeEncoded() != nil {
		t.Fatalf("planning kept %d bodies and left %d behind", len(kept), len(x.encoded))
	}
	if body, got, err := x.encoding(kept, provider, cfg, "gpt-4o"); err != nil || got != wire || !bytes.Equal(body, want) {
		t.Fatalf("the attempt was sent %s in %s (%v), want %s in %s", body, got, err, want, wire)
	}

	// What shows that the attempt is sent the body that was kept and does not
	// make it again is a body that only planning could have made.
	key := encodedKey(provider, "gpt-4o")
	kept[key] = encodedRequest{source: kept[key].source, body: []byte("made by planning"), wire: wire}
	if body, _, _ := x.encoding(kept, provider, cfg, "gpt-4o"); string(body) != "made by planning" {
		t.Fatalf("the attempt made its own body, %s", body)
	}
	// A body is only the right one for the provider revision and model it was made
	// for, and for the request as it still is: for any other the attempt makes its
	// own.
	revised := *provider
	revised.RevisionID = uuid.NewString()
	for _, tc := range []struct {
		name     string
		provider *runtime.Provider
		model    string
		change   func()
	}{
		{"another model", provider, "gpt-4o-mini", nil},
		{"another revision", &revised, "gpt-4o", nil},
		{"a request changed since", provider, "gpt-4o", func() { parsed.SetField("user", json.RawMessage(`"someone"`)) }},
	} {
		if tc.change != nil {
			tc.change()
		}
		cfg := tc.provider.Connector()
		body, wire, err := x.encoding(kept, tc.provider, cfg, tc.model)
		fresh, freshWire, freshErr := providerinvoke.Encode(parsed, cfg, tc.model, tc.provider.ParameterDefaults)
		if err != nil || freshErr != nil || wire != freshWire || !bytes.Equal(body, fresh) {
			t.Errorf("%s: the attempt was sent %s (%v), want %s", tc.name, body, err, fresh)
		}
	}
}

// A target that cannot take the request keeps no body, and says why.
func TestPlanningKeepsNoBodyForATargetThatCannotTakeTheRequest(t *testing.T) {
	parsed, err := openai.Parse(openai.FamilyChat, []byte(`{"model":"team-chat","messages":[{"role":"user","content":"hello"}],"response_format":{"type":"json_object"},"n":2}`))
	if err != nil {
		t.Fatal(err)
	}
	provider := &runtime.Provider{ID: uuid.NewString(), RevisionID: uuid.NewString(), Kind: "anthropic"}
	x := &execution{parsed: parsed}
	if err := x.encodes(provider, provider.Connector(), "claude-sonnet-4-5"); err == nil {
		t.Skip("this request translates to Anthropic; the test needs one that does not")
	}
	if x.encoded != nil {
		t.Fatalf("a target that cannot take the request kept %d bodies", len(x.encoded))
	}
}

// A route of many targets does not make a request hold a copy of itself for each
// of them while planning waits for an attempt.
func TestPlanningKeepsAFewBodiesAtMost(t *testing.T) {
	parsed, err := openai.Parse(openai.FamilyChat, []byte(`{"model":"team-chat","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	x := &execution{parsed: parsed}
	for range keptEncodings + 3 {
		provider := &runtime.Provider{ID: uuid.NewString(), RevisionID: uuid.NewString(), Kind: "openai"}
		if err := x.encodes(provider, provider.Connector(), "gpt-4o"); err != nil {
			t.Fatal(err)
		}
	}
	if kept := x.takeEncoded(); len(kept) != keptEncodings {
		t.Fatalf("planning kept %d bodies, want %d", len(kept), keptEncodings)
	}
}
