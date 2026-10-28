package secrets

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPPrivateKeyPreservesSigningIdentityAndRejectsOtherMaterial(t *testing.T) {
	publicKey, privateKey, generationErr := ed25519.GenerateKey(rand.Reader)
	if generationErr != nil {
		t.Fatal(generationErr)
	}
	privateDER, encodingErr := x509.MarshalPKCS8PrivateKey(privateKey)
	if encodingErr != nil {
		t.Fatal(encodingErr)
	}
	material := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	original := bytes.Clone(material)
	parsed, parsingErr := MCPPrivateKey(material)
	if parsingErr != nil {
		t.Fatal(parsingErr)
	}
	message := []byte("delegated tool identity")
	if !ed25519.Verify(publicKey, message, ed25519.Sign(parsed, message)) {
		t.Fatal("parsed key changed the signing identity")
	}
	if !bytes.Equal(material, original) {
		t.Fatal("parsing changed the caller's key material")
	}
	otherKey, otherGenerationErr := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if otherGenerationErr != nil {
		t.Fatal(otherGenerationErr)
	}
	otherDER, otherEncodingErr := x509.MarshalPKCS8PrivateKey(otherKey)
	if otherEncodingErr != nil {
		t.Fatal(otherEncodingErr)
	}
	for _, test := range []struct {
		name     string
		material []byte
	}{
		{name: "missing", material: nil},
		{name: "public key", material: pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicKey})},
		{name: "invalid private key", material: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("invalid")})},
		{name: "different algorithm", material: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: otherDER})},
		{name: "multiple keys", material: bytes.Repeat(material, 2)},
		{name: "oversized", material: bytes.Repeat(material, 100)},
	} {
		t.Run(test.name, func(t *testing.T) {
			key, err := MCPPrivateKey(test.material)
			if err == nil || len(key) != 0 {
				t.Fatal("invalid material produced a signing key")
			}
		})
	}
}

func ringJSON(active int, keys string) []byte {
	return fmt.Appendf(nil, `{"active_version":%d,"keys":[%s]}`, active, keys)
}

func keyEntry(version int, hexPair string) string {
	return fmt.Sprintf(`{"version":%d,"key":%q}`, version, strings.Repeat(hexPair, 32))
}

func TestDecodeKeyAcceptsHexAndBase64(t *testing.T) {
	raw := strings.Repeat("ab", 32)
	decoded, err := DecodeKey(raw)
	if err != nil || hex.EncodeToString(decoded) != raw {
		t.Fatalf("hex key: %v", err)
	}
	encoded := base64.StdEncoding.EncodeToString(decoded)
	roundTrip, err := DecodeKey(encoded)
	if err != nil || hex.EncodeToString(roundTrip) != raw {
		t.Fatalf("base64 key: %v", err)
	}
	for _, bad := range []string{"", "ab", raw + "00", encoded[:len(encoded)-2], strings.Repeat("zz", 32)} {
		if _, err := DecodeKey(bad); err == nil {
			t.Fatalf("decoded invalid key %q", bad)
		}
	}
}

func TestReadFileEnforcesModeAndContent(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	got, err := ReadFile(write("ok", "  secret-value \n", 0o600))
	if err != nil || string(got) != "secret-value" {
		t.Fatalf("valid secret file: %q, %v", got, err)
	}
	for name, path := range map[string]string{
		"missing":     filepath.Join(dir, "absent"),
		"world read":  write("wide", "secret-value", 0o644),
		"empty":       write("empty", "  \n", 0o600),
		"oversized":   write("big", strings.Repeat("x", 65537), 0o600),
		"a directory": dir,
	} {
		if _, err := ReadFile(path); err == nil {
			t.Fatalf("read %s secret file without error", name)
		}
	}
}

func TestParseRingRejectsInvalidDocuments(t *testing.T) {
	documents := map[string][]byte{
		"not json":          []byte(`{`),
		"trailing data":     append(ringJSON(1, keyEntry(1, "01")), ' ', '{', '}'),
		"unknown field":     []byte(`{"active_version":1,"bogus":true,"keys":[` + keyEntry(1, "01") + `]}`),
		"no keys":           ringJSON(1, ""),
		"missing active":    ringJSON(2, keyEntry(1, "01")),
		"duplicate version": ringJSON(1, keyEntry(1, "01")+","+keyEntry(1, "02")),
		"version zero":      ringJSON(1, keyEntry(0, "01")),
		"short key":         []byte(`{"active_version":1,"keys":[{"version":1,"key":"abcd"}]}`),
		"no key material":   []byte(`{"active_version":1,"keys":[{"version":1}]}`),
	}
	for name, document := range documents {
		if _, err := ParseRing(document); err == nil {
			t.Fatalf("parsed %s ring without error", name)
		}
	}
}

func TestRingOpensCiphertextFromRetiredKeyVersion(t *testing.T) {
	old, err := ParseRing(ringJSON(1, keyEntry(1, "01")))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := old.Seal("install-a", ProviderCredential, "record-1", []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := ParseRing(ringJSON(2, keyEntry(1, "01")+","+keyEntry(2, "02")))
	if err != nil {
		t.Fatal(err)
	}
	if !rotated.Has(1) || !rotated.Has(2) || rotated.Has(3) {
		t.Fatal("ring reports the wrong key versions")
	}
	opened, err := rotated.Open("install-a", ProviderCredential, "record-1", 1, sealed)
	if err != nil || string(opened) != "payload" {
		t.Fatalf("retired-version ciphertext: %q, %v", opened, err)
	}
	if _, err := rotated.Open("install-a", ProviderCredential, "record-1", 2, sealed); err == nil {
		t.Fatal("version 1 ciphertext opened under version 2")
	}
	fresh, err := rotated.Seal("install-a", ProviderCredential, "record-1", []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rotated.Open("install-a", ProviderCredential, "record-1", 1, fresh); err == nil {
		t.Fatal("active-version ciphertext opened under retired version")
	}
	if _, err := rotated.Open("install-a", ProviderCredential, "record-1", 99, sealed); err == nil {
		t.Fatal("ciphertext opened under a version the ring does not hold")
	}
	if _, err := rotated.Open("install-a", ProviderCredential, "record-1", 1, []byte("short")); err == nil {
		t.Fatal("truncated ciphertext opened")
	}
}
