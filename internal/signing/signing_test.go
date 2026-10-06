package signing

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"strings"
	"testing"
)

func newKey(t *testing.T, id string) (Key, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return Key{ID: id, Public: public}, private
}

func TestSignedDocumentsVerifyByExactBytes(t *testing.T) {
	key, private := newKey(t, "release-a")
	ring := MustKeyring(key)
	document := []byte("{\n  \"api_version\": \"x\"\n}\n")
	signatures, err := Sign(nil, key.ID, private, document)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := ring.Verify(document, signatures); err != nil || id != key.ID {
		t.Fatalf("verify = %q, %v", id, err)
	}
	tampered := bytes.Replace(document, []byte("x"), []byte("y"), 1)
	if _, err := ring.Verify(tampered, signatures); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("tampered document: %v", err)
	}
	flipped := bytes.Clone(signatures)
	i := bytes.Index(flipped, []byte(`"signature": "`)) + len(`"signature": "`)
	flipped[i] ^= 'A' ^ 'B'
	if flipped[i] == signatures[i] {
		flipped[i] = 'C'
	}
	if _, err := ring.Verify(document, flipped); err == nil {
		t.Fatal("a tampered signature verified")
	}
}

func TestOnlyTrustedKeysCount(t *testing.T) {
	trustedKey, trustedPrivate := newKey(t, "release-a")
	strangerKey, strangerPrivate := newKey(t, "stranger")
	document := []byte("document")
	byStranger, _ := Sign(nil, strangerKey.ID, strangerPrivate, document)
	ring := MustKeyring(trustedKey)
	if _, err := ring.Verify(document, byStranger); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("stranger-only signature: %v", err)
	}
	both, _ := Sign(byStranger, trustedKey.ID, trustedPrivate, document)
	if id, err := ring.Verify(document, both); err != nil || id != trustedKey.ID {
		t.Fatalf("trusted beside stranger = %q, %v", id, err)
	}
	if _, err := MustKeyring().Verify(document, both); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("an empty keyring trusted a document: %v", err)
	}
}

// TestRotationKeepsBothSignatures signs with an outgoing and an incoming key,
// so installations trusting either verify the same document.
func TestRotationKeepsBothSignatures(t *testing.T) {
	outgoing, outgoingPrivate := newKey(t, "release-a")
	incoming, incomingPrivate := newKey(t, "release-b")
	document := []byte("document")
	signatures, _ := Sign(nil, outgoing.ID, outgoingPrivate, document)
	signatures, _ = Sign(signatures, incoming.ID, incomingPrivate, document)
	for _, ring := range []Keyring{MustKeyring(outgoing), MustKeyring(incoming), MustKeyring(outgoing, incoming)} {
		if _, err := ring.Verify(document, signatures); err != nil {
			t.Fatalf("%v: %v", ring.IDs(), err)
		}
	}
	resigned, _ := Sign(signatures, outgoing.ID, outgoingPrivate, []byte("changed"))
	if _, err := MustKeyring(outgoing, incoming).Verify([]byte("changed"), resigned); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("a stale signature by a trusted key was ignored: %v", err)
	}
}

func TestMalformedSignatureFilesAreRefused(t *testing.T) {
	key, _ := newKey(t, "release-a")
	ring := MustKeyring(key)
	for _, file := range []string{``, `{}`, `{"signatures":[]}`, `[]`, `{"signatures":[{"key_id":"release-a","signature":"AAAA"}]}`,
		`{"signatures":[{"key_id":"Bad Id","signature":"` + strings.Repeat("A", 88) + `"}]}`, strings.Repeat(" ", MaxSignatureFile+1)} {
		if _, err := ring.Verify([]byte("document"), []byte(file)); !errors.Is(err, ErrMalformedSignature) {
			t.Fatalf("%.40q: %v", file, err)
		}
	}
}

func TestKeyringsRefuseMalformedKeys(t *testing.T) {
	key, _ := newKey(t, "release-a")
	for _, keys := range [][]Key{{{ID: "x", Public: key.Public}}, {{ID: "release-a", Public: key.Public[:8]}}, {key, key}} {
		if _, err := NewKeyring(keys...); err == nil {
			t.Fatalf("accepted %v", keys)
		}
	}
}

func TestDevelopmentSeedMatchesTheDevelopmentKey(t *testing.T) {
	seed, err := os.ReadFile("devkey/" + DevelopmentKeyID + ".seed")
	if err != nil {
		t.Fatal(err)
	}
	private, err := ParseSeed(strings.TrimSpace(string(seed)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(private.Public().(ed25519.PublicKey), developmentKey.Public) {
		t.Fatal("the committed development seed does not match the development key")
	}
}

func TestBuildTrustFollowsItsChannel(t *testing.T) {
	if len(releaseKeys) == 0 {
		t.Fatal("release builds have no trusted signing key")
	}
	_, trustsDevelopment := Trusted().Public(DevelopmentKeyID)
	if trustsDevelopment != (Channel == "development") {
		t.Fatalf("%s build trusts the development key: %v", Channel, trustsDevelopment)
	}
	for _, key := range releaseKeys {
		if bytes.Equal(key.Public, developmentKey.Public) {
			t.Fatal("a release key trusts the public development seed")
		}
		if _, ok := Trusted().Public(key.ID); !ok {
			t.Fatalf("%s build does not trust release key %s", Channel, key.ID)
		}
	}
}
