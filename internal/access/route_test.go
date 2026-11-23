package access

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/tyk-swe/olp/internal/access/authz"
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

func goldenTokenScopes(t *testing.T, names []string) []authz.Operation {
	t.Helper()
	scopes := make([]authz.Operation, len(names))
	for i, name := range names {
		op, ok := authz.ParseOperation(name)
		if !ok {
			t.Fatalf("unknown token scope %q", name)
		}
		scopes[i] = op
	}
	return scopes
}

func TestCredentialImportAuthorizationMatchesCanonicalMatrix(t *testing.T) {
	raw, err := os.ReadFile("testdata/authorization.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var matrix struct {
		Archetypes []struct {
			Name        string   `json:"name"`
			Kind        string   `json:"kind"`
			Role        string   `json:"role"`
			CreatorRole string   `json:"creator_role"`
			Global      bool     `json:"global"`
			Scopes      []string `json:"scopes"`
		} `json:"archetypes"`
		Routes map[string]string `json:"routes"`
	}
	if err := json.Unmarshal(raw, &matrix); err != nil {
		t.Fatal(err)
	}
	for _, pattern := range []string{"POST /api/v1/providers/{provider_id}/codex-import", "POST /api/v1/providers/{provider_id}/claude-import", "POST /api/v1/providers/{provider_id}/antigravity-import"} {
		row, ok := matrix.Routes[pattern]
		if !ok || len(row) != len(matrix.Archetypes) {
			t.Fatal("the import route needs an authorization expectation for every caller")
		}
		requirements, err := ContractRequirements()
		if err != nil {
			t.Fatal(err)
		}
		for index, caller := range matrix.Archetypes {
			t.Run(caller.Name, func(t *testing.T) {
				principal := authz.Principal{User: authz.User{Role: caller.Role}, Kind: caller.Kind, AllProjects: caller.Global}.
					Delegated(goldenTokenScopes(t, caller.Scopes), caller.CreatorRole)
				if admitted := requirements[pattern].Admits(principal) == nil; admitted != (row[index] == 'Y') {
					t.Fatalf("import authority disagrees with the canonical expectation, admitted=%t", admitted)
				}
			})
		}
	}
}
