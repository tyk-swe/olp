package connectors

import (
	"slices"
	"testing"
)

func TestBuiltinProfilesComposeEveryCataloguePart(t *testing.T) {
	first := builtinProfiles()
	second := builtinProfiles()
	ids := func(profiles []Profile) []string {
		out := make([]string, 0, len(profiles))
		for _, profile := range profiles {
			out = append(out, profile.ID+"@"+profile.Revision)
		}
		return out
	}
	if !slices.Equal(ids(first), ids(second)) {
		t.Fatal("building the catalogue twice gave different profiles")
	}
	byID := map[string]Profile{}
	for _, profile := range first {
		if _, duplicate := byID[profile.ID]; duplicate {
			t.Fatalf("profile %q is built twice", profile.ID)
		}
		byID[profile.ID] = profile
	}
	for _, id := range []string{"openai-chat", "cohere-v2", "openai-completions", "groq-files-batches"} {
		if _, ok := byID[id]; !ok {
			t.Fatalf("catalogue is missing %q", id)
		}
	}
	openAI := byID["openai-chat"]
	if openAI.Transport != "http" || openAI.Revision != ProfileRevision || !openAI.Strict ||
		!slices.Contains(openAI.Operations, "batch") || !slices.Contains(openAI.Operations, "realtime") ||
		!slices.Contains(openAI.SemanticHeaders, "Openai-Beta") {
		t.Fatalf("openai-chat was not enriched as before: %+v", openAI)
	}
	first[0].ID = "mutated"
	if got := ids(builtinProfiles()); slices.Contains(got, "mutated@"+ProfileRevision) {
		t.Fatal("mutating one built catalogue changed the next build")
	}
}
