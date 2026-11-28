package access

import (
	"net/http"
	"testing"
)

func TestPresentedInferenceKeyShapes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		want   bool
	}{
		{"api key", "Bearer olp_abc123_secret", true},
		{"scheme case", "bearer olp_abc123_secret", true},
		{"padded token", "Bearer   olp_abc123_secret  ", true},
		{"jwt shape", "Bearer eyJhbGciOiJSUzI1NiJ9.eyJpc3MiOiJ4In0.c2ln", true},
		{"missing header", "", false},
		{"other scheme", "Basic olp_abc123_secret", false},
		{"bare scheme", "Bearer", false},
		{"empty token", "Bearer    ", false},
		{"opaque token", "Bearer session-token", false},
		{"single dot", "Bearer a.b", false},
		{"empty segments", "Bearer ..", false},
		{"leading dot", "Bearer .a.b", false},
		{"trailing dot", "Bearer a.b.", false},
		{"hollow middle", "Bearer a..b", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := http.NewRequest("GET", "https://example.test/", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			if got := presentedInferenceKey(r); got != tc.want {
				t.Fatalf("presentedInferenceKey(%q) = %v, want %v", tc.header, got, tc.want)
			}
		})
	}
}

func TestCredentialImportAuthorizationMatchesCanonicalMatrix(t *testing.T) {
	matrix := readAuthorizationGolden(t)
	requirements, err := ContractRequirements()
	if err != nil {
		t.Fatal(err)
	}
	for _, pattern := range []string{"POST /api/v1/providers/{provider_id}/codex-import", "POST /api/v1/providers/{provider_id}/claude-import", "POST /api/v1/providers/{provider_id}/antigravity-import"} {
		row, ok := matrix.Routes[pattern]
		if !ok || len(row) != len(matrix.Archetypes) {
			t.Fatal("the import route needs an authorization expectation for every caller")
		}
		for index, caller := range matrix.Archetypes {
			t.Run(caller.Name, func(t *testing.T) {
				principal := caller.principal(t)
				if admitted := requirements[pattern].Admits(principal) == nil; admitted != (row[index] == 'Y') {
					t.Fatalf("import authority disagrees with the canonical expectation, admitted=%t", admitted)
				}
			})
		}
	}
}
