package media

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/vendors"
	"github.com/tyk-swe/olp/tests/fixtures"
)

// TestEveryVendorWireHasItsCodec checks that the media wires vendor contracts
// name and the registered codecs agree, operation for operation.
func TestEveryVendorWireHasItsCodec(t *testing.T) {
	named := map[string][]string{}
	for _, contract := range vendors.All() {
		for operation, wire := range contract.MediaWires {
			named[wire] = append(named[wire], operation)
		}
	}
	for wire := range named {
		slices.Sort(named[wire])
		named[wire] = slices.Compact(named[wire])
	}
	registered := Wires()
	if !maps.EqualFunc(named, registered, slices.Equal) {
		t.Fatalf("contracts name wires %v; codecs serve %v", named, registered)
	}
}

// mediaProbe is the smallest valid request of each media operation.
func mediaProbe(operation string) *Request {
	r := &Request{Op: operation, Route: "route", Prompt: "A small blue square.", Input: "Hello.", Voice: "alloy"}
	if operation == OpTranscription || operation == OpTranslation {
		r.File = &Part{}
	}
	if operation == OpImageEdit {
		r.Images = []Part{{}}
	}
	return r
}

// TestVendorMediaEndpointsAreDocumented addresses each media operation a
// preset serves as its codec does, and compares it with the endpoint its
// reviewed evidence documents.
func TestVendorMediaEndpointsAreDocumented(t *testing.T) {
	for _, contract := range vendors.All() {
		if contract.Preset == nil {
			continue
		}
		raw, err := fixtures.Files.ReadFile("vendors/" + contract.ID + "/contract.json")
		if err != nil {
			t.Fatal(err)
		}
		var evidence struct {
			Endpoints map[string]string `json:"endpoints"`
			// MediaModels are the models media probes name, by operation,
			// where the vendor reads more than its identity from one.
			MediaModels map[string]string `json:"media_models"`
		}
		if err := json.Unmarshal(raw, &evidence); err != nil {
			t.Fatal(err)
		}
		config := connectors.Config{Kind: contract.Connector, AuthMode: contract.Preset.AuthMode, Endpoint: contract.Endpoint, VendorID: contract.ID}
		for _, operation := range contract.Operations {
			if !slices.Contains([]string{OpImageGeneration, OpImageEdit, OpSpeech, OpTranscription, OpTranslation, OpVideoCreate}, operation) {
				continue
			}
			model := "model"
			if named := evidence.MediaModels[operation]; named != "" {
				model = named
			}
			call, _, failure := EncodeConfigured(mediaProbe(operation), config, model)
			if failure != nil {
				t.Fatalf("%s %s: %s", contract.ID, operation, failure.Message)
			}
			got, err := config.MediaURL(call.Path, model, call.Query)
			if want := evidence.Endpoints[operation]; err != nil || got != want {
				t.Fatalf("%s %s is addressed at %s (%v); the documentation says %s", contract.ID, operation, got, err, want)
			}
		}
	}
}
