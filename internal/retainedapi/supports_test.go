package retainedapi

import (
	"slices"
	"testing"
)

func TestSupportsFollowsDeclaredFamilies(t *testing.T) {
	surfaces := map[string]bool{}
	for _, family := range Families() {
		surfaces[family.Surface] = true
	}
	for _, family := range Families() {
		for _, provider := range family.Providers {
			if !Supports(provider, family.Operation, family.Surface, "unary") {
				t.Errorf("declared family %s on %s/%s is not selectable", family.Operation, provider, family.Surface)
			}
			if Supports(provider, family.Operation, family.Surface, "streaming") {
				t.Errorf("%s on %s/%s qualifies for streaming", family.Operation, provider, family.Surface)
			}
		}
		for surface := range surfaces {
			if surface == family.Surface {
				continue
			}
			for _, provider := range family.Providers {
				if Supports(provider, family.Operation, surface, "unary") {
					t.Errorf("%s on %s qualifies on undeclared surface %s", family.Operation, provider, surface)
				}
			}
		}
		if Supports("undeclared-kind", family.Operation, family.Surface, "unary") {
			t.Errorf("%s qualifies for a provider kind no family declares", family.Operation)
		}
	}
}

func TestSupportsSelectsOnlyDeclaredProviders(t *testing.T) {
	kinds := declaredKinds()
	for _, family := range Families() {
		for _, kind := range kinds {
			want := slices.Contains(family.Providers, kind)
			if got := Supports(kind, family.Operation, family.Surface, "unary"); got != want {
				t.Errorf("Supports(%q,%q,%q,unary)=%v, declared providers say %v", kind, family.Operation, family.Surface, got, want)
			}
		}
	}
}

func declaredKinds() []string {
	seen := map[string]bool{}
	var kinds []string
	for _, family := range Families() {
		for _, provider := range family.Providers {
			if !seen[provider] {
				seen[provider] = true
				kinds = append(kinds, provider)
			}
		}
	}
	return append(kinds, "undeclared-kind")
}

func TestSupportsRetrievalModeAndSurface(t *testing.T) {
	for _, kind := range declaredKinds() {
		if Supports(kind, "retrieval", "openai", "streaming") {
			t.Errorf("retrieval on %s qualifies for streaming", kind)
		}
		for _, family := range Families() {
			if family.Surface != "openai" && Supports(kind, "retrieval", family.Surface, "unary") {
				t.Errorf("retrieval on %s qualifies on surface %s", kind, family.Surface)
			}
		}
	}
	if Supports("undeclared-kind", "retrieval", "openai", "unary") {
		t.Fatal("retrieval qualifies for a kind no profile or family declares")
	}
	if Supports("openai", "not_an_operation", "openai", "unary") {
		t.Fatal("an undeclared operation qualifies")
	}
}

func TestEndpointsCoverDeclaredFamilies(t *testing.T) {
	ops := map[string]bool{}
	for _, endpoint := range Endpoints() {
		ops[endpoint.Operation] = true
		if !Operation(endpoint.Operation) {
			t.Fatalf("endpoint operation not declared: %s", endpoint.Operation)
		}
	}
	for _, family := range Families() {
		if !ops[family.Operation] {
			t.Fatalf("missing endpoints for declared family %s", family.Operation)
		}
	}
}
