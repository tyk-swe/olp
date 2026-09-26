package secrets

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordEncryptionRejectsTamperingAndMisbinding(t *testing.T) {
	ring, err := ParseRing([]byte(`{"active_version":1,"keys":[{"version":1,"key":"` + strings.Repeat("ab", 32) + `"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := ring.Seal("installation-a", "oidc_client", "record-a", []byte("private-value"))
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := ring.Open("installation-a", "oidc_client", "record-a", 1, ciphertext)
	if err != nil || string(recovered) != "private-value" {
		t.Fatal("round trip failed", err)
	}
	if bytes.Contains(ciphertext, []byte("private-value")) {
		t.Fatal("plaintext persisted")
	}
	for _, binding := range [][3]string{{"installation-b", "oidc_client", "record-a"}, {"installation-a", "mutation_replay", "record-a"}, {"installation-a", "oidc_client", "record-b"}} {
		if _, err := ring.Open(binding[0], binding[1], binding[2], 1, ciphertext); err == nil {
			t.Fatal("accepted misbound ciphertext", binding)
		}
	}
	ciphertext[len(ciphertext)-1] ^= 1
	if _, err = ring.Open("installation-a", "oidc_client", "record-a", 1, ciphertext); err == nil {
		t.Fatal("accepted tampered ciphertext")
	}
	for _, input := range []string{`{}`, `{"active_version":2,"keys":[{"version":1,"key":"` + strings.Repeat("ab", 32) + `"}]}`, `{"active_version":1,"keys":[{"version":1,"key":"invalid"}]}`} {
		if _, err := ParseRing([]byte(input)); err == nil {
			t.Fatal("accepted invalid ring")
		}
	}
}
func TestSecretPermissionsAndDomainSeparatedDigests(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("value"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0400, 0440, 0600, 0640, 0660, 0644, 0700} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		_, err := ReadFile(path)
		if (err == nil) != (mode == 0400 || mode == 0440 || mode == 0600 || mode == 0640) {
			t.Fatalf("permission %o: %v", mode, err)
		}
	}
	key := bytes.Repeat([]byte{1}, 32)
	a, b := NewAuthKey(key, "a"), NewAuthKey(key, "b")
	if bytes.Equal(a.Digest("session", "same"), a.Digest("api_key", "same")) || bytes.Equal(a.Digest("session", "same"), b.Digest("session", "same")) {
		t.Fatal("authentication domains collide")
	}
}

// Stored ciphertexts and digests are only readable while these authority labels
// stay fixed, so they are pinned against an independent computation.
func TestSecretAuthorityLabelsBindStoredRecords(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	ring, err := ParseRing([]byte(`{"active_version":1,"keys":[{"version":1,"key":"` + strings.Repeat("ab", 32) + `"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := ring.Seal("installation-a", "oidc_client", "record-a", []byte("private-value"))
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	associated, _ := json.Marshal([]string{"olp-secret-v1", "installation-a", "oidc_client", "record-a"})
	if plain, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], associated); err != nil || string(plain) != "private-value" {
		t.Fatalf("sealed record is not bound to the olp-secret-v1 authority: %v", err)
	}
	mac := hmac.New(sha256.New, key)
	labelled, _ := json.Marshal([]string{"olp-auth-v1", "installation-a", "session", "value"})
	mac.Write(labelled)
	if !bytes.Equal(NewAuthKey(key, "installation-a").Digest("session", "value"), mac.Sum(nil)) {
		t.Fatal("digest is not bound to the olp-auth-v1 authority")
	}
}

func TestPasswordsAreSaltedAndVerificationIsBounded(t *testing.T) {
	first, second := HashPassword("a long secure password"), HashPassword("a long secure password")
	if first == second || !VerifyPassword("a long secure password", first) || VerifyPassword("incorrect", first) {
		t.Fatal("password verification failed")
	}
	if VerifyPassword("a long secure password", strings.Replace(first, "m=65536", "m=999999999", 1)) {
		t.Fatal("accepted unbounded parameters")
	}
}

func TestMissingSecretFilePreservesClassificationWithoutPath(t *testing.T) {
	_, err := ReadFile(filepath.Join(t.TempDir(), "private-path-must-not-leak"))
	if !errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "private-path-must-not-leak") {
		t.Fatalf("missing-file classification or redaction failed: %v", err)
	}
}
