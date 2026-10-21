package secrets

import (
	"flag"
	"os"
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
	if want := []string{"cache_key", "saml_state", "saml_cookie", "saml_assertion", "mfa_challenge", "mfa_recovery", "api_key", "workload_identity", "end_user", "management_token", "session", "recent_auth", "csrf", "oidc_state", "oidc_cookie", "invitation", "admission", "mutation", "installation", "batch_submission", "batch_line"}; !slices.Equal(digests, want) {
		t.Fatalf("digest purposes %v, want %v", digests, want)
	}
	if want := []string{"saml_key", "saml_flow", "mfa_totp", "mfa_webauthn", "provider_credential", "provider_continuation", "notification_secret", "mutation_replay", "oidc_client", "oidc_flow", "media_job_source", "provider_grant_refresh", "grant_enrollment", "sink_credential", "mcp_credential", "billing_credential", "guardrail_credential", "response_cache", "provider_upload_session"}; !slices.Equal(seals, want) {
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

var update = flag.Bool("update", false, "rewrite docs/develop/security.md from the declared purposes")

func TestPurposeDocumentation(t *testing.T) {
	path := "../../docs/develop/security.md"
	document, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	startMarker, endMarker := "<!-- purposes -->\n", "<!-- /purposes -->"
	start := strings.Index(string(document), startMarker)
	if start < 0 {
		t.Fatal("docs/develop/security.md has no purposes region")
	}
	start += len(startMarker)
	end := strings.Index(string(document[start:]), endMarker)
	if end < 0 {
		t.Fatal("docs/develop/security.md has no closing purposes marker")
	}
	end += start
	var digests, seals []string
	for _, purpose := range DigestPurposes() {
		digests = append(digests, "`"+purpose.String()+"`")
	}
	for _, purpose := range SealPurposes() {
		seals = append(seals, "`"+purpose.String()+"`")
	}
	rendered := "| Kind | Purposes |\n| --- | --- |\n" +
		"| Digest | " + strings.Join(digests, ", ") + " |\n" +
		"| Seal | " + strings.Join(seals, ", ") + " |\n"
	current := string(document[start:end])
	if current == rendered {
		return
	}
	if !*update {
		t.Fatal("docs/develop/security.md purpose table differs from declared purposes; rerun with -update")
	}
	updated := append(append(append([]byte{}, document[:start]...), rendered...), document[end:]...)
	if err = os.WriteFile(path, updated, 0o644); err != nil {
		t.Fatal(err)
	}
}
