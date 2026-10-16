package signing

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
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
