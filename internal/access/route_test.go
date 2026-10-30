package access

import (
	"encoding/json"
	"net/http"
	"os"
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

func goldenTokenScopes(t *testing.T, names []string) operationSet {
	t.Helper()
	if len(names) == 0 {
		return 0
	}
	op, ok := ParseOperation(names[0])
	if !ok {
		t.Fatalf("unknown token scope %q", names[0])
	}
	return 1<<op | goldenTokenScopes(t, names[1:])
}

func TestCodexImportAuthorizationMatchesCanonicalMatrix(t *testing.T) {
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
	const pattern = "POST /api/v1/providers/{provider_id}/codex-import"
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
			principal := Principal{User: User{Role: caller.Role}, Kind: caller.Kind, AllProjects: caller.Global,
				creatorRole: caller.CreatorRole, scopes: goldenTokenScopes(t, caller.Scopes)}
			if admitted := requirements[pattern].Admits(principal) == nil; admitted != (row[index] == 'Y') {
				t.Fatalf("import authority disagrees with the canonical expectation, admitted=%t", admitted)
			}
		})
	}
}
