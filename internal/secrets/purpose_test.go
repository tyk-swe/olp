package secrets

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Purpose names are bound into every stored digest and ciphertext, so renaming
// one silently orphans what the installation stores. The lists are pinned.
func TestPurposeNamesNeverChange(t *testing.T) {
	var digests, seals []string
	for _, purpose := range DigestPurposes() {
		digests = append(digests, purpose.String())
	}
	for _, purpose := range SealPurposes() {
		seals = append(seals, purpose.String())
	}
	if want := []string{"api_key", "management_token", "session", "recent_auth", "csrf", "oidc_state", "oidc_cookie", "invitation", "admission", "mutation", "installation"}; !slices.Equal(digests, want) {
		t.Fatalf("digest purposes %v, want %v", digests, want)
	}
	if want := []string{"provider_credential", "provider_continuation", "notification_secret", "mutation_replay", "oidc_client", "oidc_flow", "media_job_source"}; !slices.Equal(seals, want) {
		t.Fatalf("seal purposes %v, want %v", seals, want)
	}
	for _, names := range [][]string{digests, seals} {
		if len(slices.Compact(slices.Sorted(slices.Values(names)))) != len(names) {
			t.Fatal("purpose names must be unique", names)
		}
	}
	for _, purpose := range SealPurposes() {
		if parsed, ok := ParseSealPurpose(purpose.String()); !ok || parsed != purpose {
			t.Fatalf("%s does not parse back", purpose)
		}
		if value, err := purpose.Value(); err != nil || value != purpose.String() {
			t.Fatalf("%s binds as %v", purpose, value)
		}
	}
	if _, ok := ParseSealPurpose("unknown"); ok {
		t.Fatal("parsed an unknown purpose")
	}
}

// SQL strings are invisible to the purpose types, so queries bind a purpose
// as a parameter instead of spelling one.
func TestQueriesBindPurposes(t *testing.T) {
	literal := regexp.MustCompile(`purpose\s*(=|IN)\s*\(?'`)
	sources, err := filepath.Glob("../*/*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range sources {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if literal.Match(source) {
			t.Errorf("%s spells a secret purpose in SQL; bind the secrets purpose instead", name)
		}
	}
}

var update = flag.Bool("update", false, "rewrite docs/security.md from the declared purposes")

func TestSecurityDocumentListsEveryPurpose(t *testing.T) {
	var table strings.Builder
	table.WriteString("| Kind | Purposes |\n| --- | --- |\n")
	for _, row := range []struct {
		kind  string
		names []string
	}{
		{"Digest", func() (names []string) {
			for _, purpose := range DigestPurposes() {
				names = append(names, "`"+purpose.String()+"`")
			}
			return names
		}()},
		{"Seal", func() (names []string) {
			for _, purpose := range SealPurposes() {
				names = append(names, "`"+purpose.String()+"`")
			}
			return names
		}()},
	} {
		table.WriteString("| " + row.kind + " | " + strings.Join(row.names, ", ") + " |\n")
	}
	path := "../../docs/security.md"
	document, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	start, end := "<!-- purposes -->\n", "<!-- /purposes -->"
	i, j := strings.Index(string(document), start), strings.Index(string(document), end)
	if i < 0 || j < i {
		t.Fatal("docs/security.md has no purposes region")
	}
	if string(document[i+len(start):j]) == table.String() {
		return
	}
	if !*update {
		t.Fatal("docs/security.md lists purposes differently from purpose.go; rerun with -update")
	}
	updated := string(document[:i+len(start)]) + table.String() + string(document[j:])
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
}
