package runtime

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"
)

func withMetadata(s *Snapshot, providerID, model string, m ModelMetadata) {
	encoded, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	p := s.Providers[providerID]
	if p.Models == nil {
		p.Models = map[string]json.RawMessage{}
	}
	p.Models[model] = encoded
	s.Providers[providerID] = p
}

func TestEffectiveCapabilitiesHomogeneous(t *testing.T) {
	s, slug, ids := planningFixture()
	for _, id := range ids {
		withMetadata(&s, id, "wire-model", ModelMetadata{
			InputModalities:     []string{"text"},
			OutputModalities:    []string{"text"},
			ContextLength:       ptr(int64(8000)),
			MaxOutputTokens:     ptr(int64(4000)),
			SupportedParameters: &[]string{"temperature", "top_p"},
		})
	}
	got := EffectiveCapabilities(&s, s.Routes[slug])
	if !slices.Equal(got.Operations, []string{"generation"}) {
		t.Fatalf("operations %v", got.Operations)
	}
	if got.OperationSupport["generation"] != "guaranteed" {
		t.Fatalf("operation_support %v", got.OperationSupport)
	}
	if got.ContextLength == nil || *got.ContextLength != 8000 || got.MaxOutputTokens == nil || *got.MaxOutputTokens != 4000 {
		t.Fatalf("limits %+v", got)
	}
	if !slices.Equal(got.InputModalities, []string{"text"}) || !slices.Equal(got.OutputModalities, []string{"text"}) {
		t.Fatalf("modalities %+v", got)
	}
	if got.SupportedParameters == nil || !slices.Equal(*got.SupportedParameters, []string{"temperature", "top_p"}) {
		t.Fatalf("parameters %+v", got)
	}
	if len(got.Unknown) != 0 {
		t.Fatalf("unknown %v", got.Unknown)
	}
}

func TestEffectiveCapabilitiesHeterogeneousMinimaAndIntersections(t *testing.T) {
	s, slug, ids := planningFixture()
	withMetadata(&s, ids[0], "wire-model", ModelMetadata{
		InputModalities:     []string{"image", "text"},
		OutputModalities:    []string{"text"},
		ContextLength:       ptr(int64(8000)),
		MaxOutputTokens:     ptr(int64(4000)),
		SupportedParameters: &[]string{"seed", "temperature"},
	})
	withMetadata(&s, ids[1], "wire-model", ModelMetadata{
		InputModalities:     []string{"text"},
		OutputModalities:    []string{"text"},
		ContextLength:       ptr(int64(4000)),
		MaxOutputTokens:     ptr(int64(8000)),
		SupportedParameters: &[]string{"temperature", "top_p"},
	})
	withMetadata(&s, ids[2], "wire-model", ModelMetadata{
		InputModalities:     []string{"text"},
		OutputModalities:    []string{"text"},
		ContextLength:       ptr(int64(16000)),
		MaxOutputTokens:     ptr(int64(2000)),
		SupportedParameters: &[]string{"temperature"},
	})
	got := EffectiveCapabilities(&s, s.Routes[slug])
	if got.ContextLength == nil || *got.ContextLength != 4000 {
		t.Fatalf("context %v", got.ContextLength)
	}
	if got.MaxOutputTokens == nil || *got.MaxOutputTokens != 2000 {
		t.Fatalf("max output %v", got.MaxOutputTokens)
	}
	if !slices.Equal(got.InputModalities, []string{"text"}) || !slices.Equal(*got.SupportedParameters, []string{"temperature"}) {
		t.Fatalf("intersections %+v", got)
	}
	if len(got.Unknown) != 0 {
		t.Fatalf("unknown %v", got.Unknown)
	}
}

func TestEffectiveCapabilitiesUnknownFactsAreNamed(t *testing.T) {
	s, slug, ids := planningFixture()
	for _, id := range ids {
		withMetadata(&s, id, "wire-model", ModelMetadata{
			InputModalities:     []string{"text"},
			OutputModalities:    []string{"text"},
			ContextLength:       ptr(int64(8000)),
			MaxOutputTokens:     ptr(int64(4000)),
			SupportedParameters: &[]string{"temperature"},
		})
	}
	p := s.Providers[ids[1]]
	metadata := ModelMetadata{
		InputModalities:  []string{"text"},
		OutputModalities: []string{"text"},
		MaxOutputTokens:  ptr(int64(4000)),
	}
	encoded, _ := json.Marshal(metadata)
	p.Models["wire-model"] = encoded
	s.Providers[ids[1]] = p
	got := EffectiveCapabilities(&s, s.Routes[slug])
	if got.ContextLength != nil || !slices.Contains(got.Unknown, "context_length") {
		t.Fatalf("missing context fact must be unknown: %+v", got)
	}
	if got.MaxOutputTokens == nil || *got.MaxOutputTokens != 4000 {
		t.Fatalf("declared max output %v", got.MaxOutputTokens)
	}
}

func TestEffectiveCapabilitiesIgnoresUncertifiedAndDisabledTargets(t *testing.T) {
	s, slug, ids := planningFixture()
	for _, id := range ids {
		withMetadata(&s, id, "wire-model", ModelMetadata{
			InputModalities:     []string{"text"},
			OutputModalities:    []string{"text"},
			ContextLength:       ptr(int64(8000)),
			MaxOutputTokens:     ptr(int64(4000)),
			SupportedParameters: &[]string{"temperature"},
		})
	}
	disabled := s.Providers[ids[1]]
	disabled.Enabled = false
	s.Providers[ids[1]] = disabled
	uncertified := s.Providers[ids[2]]
	uncertified.Capabilities = []Capability{{Model: "wire-model", Operation: "embeddings", Surface: "openai", Mode: "unary"}}
	s.Providers[ids[2]] = uncertified
	got := EffectiveCapabilities(&s, s.Routes[slug])
	if got.ContextLength == nil || *got.ContextLength != 8000 || len(got.Unknown) != 0 {
		t.Fatalf("ineligible targets must not weaken guarantees: %+v", got)
	}
}

func TestEffectiveCapabilitiesOperationSupport(t *testing.T) {
	s, slug, ids := planningFixture()
	route := s.Routes[slug]
	route.Operations = []string{"generation", "embeddings", "token_count"}
	s.Routes[slug] = route
	p := s.Providers[ids[0]]
	p.Capabilities = append(p.Capabilities, Capability{Model: "wire-model", Operation: "embeddings", Surface: "openai", Mode: "unary"})
	s.Providers[ids[0]] = p
	got := EffectiveCapabilities(&s, s.Routes[slug])
	want := map[string]string{"generation": "guaranteed", "embeddings": "target_dependent", "token_count": "unknown"}
	if !maps.Equal(got.OperationSupport, want) {
		t.Fatalf("operation_support %v, want %v", got.OperationSupport, want)
	}
}

func TestEffectiveCapabilitiesWithoutTargetsNamesEverythingUnknown(t *testing.T) {
	s, slug, ids := planningFixture()
	for _, id := range ids {
		p := s.Providers[id]
		p.Enabled = false
		s.Providers[id] = p
	}
	got := EffectiveCapabilities(&s, s.Routes[slug])
	if !slices.Equal(got.Operations, []string{"generation"}) {
		t.Fatalf("operations %v", got.Operations)
	}
	if got.OperationSupport["generation"] != "unknown" {
		t.Fatalf("operation_support %v", got.OperationSupport)
	}
	want := []string{"context_length", "input_modalities", "max_output_tokens", "output_modalities", "supported_parameters"}
	if !slices.Equal(got.Unknown, want) {
		t.Fatalf("unknown %v, want %v", got.Unknown, want)
	}
	if got.ContextLength != nil || got.MaxOutputTokens != nil || got.SupportedParameters != nil || len(got.InputModalities) != 0 || len(got.OutputModalities) != 0 {
		t.Fatalf("facts must be empty: %+v", got)
	}
}
