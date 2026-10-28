package secrets

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestDigestPurposesSeparateDomains(t *testing.T) {
	key := NewAuthKey(bytes.Repeat([]byte{7}, 32), "install-a")
	seen := map[string]DigestPurpose{}
	for _, purpose := range DigestPurposes() {
		digest := key.Digest(purpose, "token-value")
		if again := key.Digest(purpose, "token-value"); !bytes.Equal(again, digest) {
			t.Fatalf("%s digest is not stable across calls", purpose)
		}
		if other, dup := seen[string(digest)]; dup {
			t.Fatalf("%s and %s produce the same digest for one value", purpose, other)
		}
		seen[string(digest)] = purpose
		if changed := key.Digest(purpose, "other-value"); bytes.Equal(changed, digest) {
			t.Fatalf("%s digest ignores the value", purpose)
		}
	}
	otherInstall := NewAuthKey(bytes.Repeat([]byte{7}, 32), "install-b")
	for _, purpose := range DigestPurposes() {
		if bytes.Equal(otherInstall.Digest(purpose, "token-value"), key.Digest(purpose, "token-value")) {
			t.Fatalf("%s digest ignores the installation", purpose)
		}
	}
	if len(slices.Compact(slices.Sorted(slices.Values(purposeNames())))) != len(DigestPurposes())+len(SealPurposes()) {
		t.Fatal("purpose names must be unique")
	}
}

func purposeNames() []string {
	names := make([]string, 0, len(DigestPurposes())+len(SealPurposes()))
	for _, purpose := range DigestPurposes() {
		names = append(names, purpose.String())
	}
	for _, purpose := range SealPurposes() {
		names = append(names, purpose.String())
	}
	return names
}

func TestSealPurposesBindCiphertext(t *testing.T) {
	ring, err := ParseRing([]byte(fmt.Sprintf(`{"active_version":1,"keys":[{"version":1,"key":%q}]}`, strings.Repeat("01", 32))))
	if err != nil {
		t.Fatal(err)
	}
	purposes := SealPurposes()
	for i, purpose := range purposes {
		other := purposes[(i+1)%len(purposes)]
		sealed, err := ring.Seal("install-a", purpose, "record-1", []byte("payload"))
		if err != nil {
			t.Fatalf("seal for %s: %v", purpose, err)
		}
		opened, err := ring.Open("install-a", purpose, "record-1", ring.Active, sealed)
		if err != nil || string(opened) != "payload" {
			t.Fatalf("open for %s: %v, %q", purpose, err, opened)
		}
		for name, attempt := range map[string]func() ([]byte, error){
			"other purpose":      func() ([]byte, error) { return ring.Open("install-a", other, "record-1", ring.Active, sealed) },
			"other installation": func() ([]byte, error) { return ring.Open("install-b", purpose, "record-1", ring.Active, sealed) },
			"other record":       func() ([]byte, error) { return ring.Open("install-a", purpose, "record-2", ring.Active, sealed) },
		} {
			if _, err := attempt(); err == nil {
				t.Fatalf("%s ciphertext opened under %s", purpose, name)
			}
		}
		tampered := slices.Clone(sealed)
		tampered[len(tampered)-1] ^= 1
		if _, err := ring.Open("install-a", purpose, "record-1", ring.Active, tampered); err == nil {
			t.Fatalf("%s tampered ciphertext opened", purpose)
		}
		if parsed, ok := ParseSealPurpose(purpose.String()); !ok || parsed != purpose {
			t.Fatalf("stored name of %s does not parse back", purpose)
		}
		if value, err := purpose.Value(); err != nil || value != purpose.String() {
			t.Fatalf("%s binds as %v, want its stored name", purpose, value)
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
