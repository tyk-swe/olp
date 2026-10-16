package secrets

import (
	"flag"
	"io/fs"
	"path/filepath"
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
	if want := []string{"cache_key", "saml_state", "saml_cookie", "saml_assertion", "mfa_challenge", "mfa_recovery", "api_key", "workload_identity", "end_user", "management_token", "session", "recent_auth", "csrf", "oidc_state", "oidc_cookie", "invitation", "admission", "mutation", "installation"}; !slices.Equal(digests, want) {
		t.Fatalf("digest purposes %v, want %v", digests, want)
	}
	if want := []string{"saml_key", "saml_flow", "mfa_totp", "mfa_webauthn", "provider_credential", "provider_continuation", "notification_secret", "mutation_replay", "oidc_client", "oidc_flow", "media_job_source", "provider_grant_refresh", "grant_enrollment", "sink_credential", "mcp_credential", "billing_credential", "guardrail_credential", "response_cache"}; !slices.Equal(seals, want) {
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

var update = flag.Bool("update", false, "rewrite docs/security.md from the declared purposes")

// productionSources lists every non-test Go source under internal/ and cmd/,
// at any depth.
func productionSources(t *testing.T) []string {
	t.Helper()
	var sources []string
	for _, root := range []string{"..", "../../cmd"} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && !entry.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				sources = append(sources, path)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(sources) < 100 {
		t.Fatalf("found only %d sources", len(sources))
	}
	return sources
}
