package signing

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
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

func keyring(t *testing.T, keys ...Key) Keyring {
	t.Helper()
	ring, err := NewKeyring(keys...)
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

func signed(t *testing.T, signatures []byte, id string, private ed25519.PrivateKey, document []byte) []byte {
	t.Helper()
	file, err := Sign(signatures, id, private, document)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func TestOnlyTrustedKeysCount(t *testing.T) {
	trustedKey, trustedPrivate := newKey(t, "release-a")
	strangerKey, strangerPrivate := newKey(t, "stranger")
	document := []byte("document")
	byStranger := signed(t, nil, strangerKey.ID, strangerPrivate, document)
	ring := keyring(t, trustedKey)
	if _, err := ring.Verify(document, byStranger); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("stranger-only signature: %v", err)
	}
	both := signed(t, byStranger, trustedKey.ID, trustedPrivate, document)
	if id, err := ring.Verify(document, both); err != nil || id != trustedKey.ID {
		t.Fatalf("trusted beside stranger = %q, %v", id, err)
	}
	if _, err := keyring(t).Verify(document, both); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("an empty keyring trusted a document: %v", err)
	}
}

func TestRotationRequiresEveryTrustedSignature(t *testing.T) {
	outgoing, outgoingPrivate := newKey(t, "release-a")
	incoming, incomingPrivate := newKey(t, "release-b")
	ring := keyring(t, incoming, outgoing)
	document := []byte("document")
	byBoth := signed(t, signed(t, nil, outgoing.ID, outgoingPrivate, []byte("previous release")), incoming.ID, incomingPrivate, document)
	resigned := signed(t, byBoth, outgoing.ID, outgoingPrivate, document)
	if id, err := ring.Verify(document, resigned); err != nil || id != outgoing.ID {
		t.Fatalf("re-signed by both keys = %q, %v", id, err)
	}
	var file File
	if err := json.Unmarshal(resigned, &file); err != nil || len(file.Signatures) != 2 {
		t.Fatalf("re-signing duplicated or dropped an entry: %s", resigned)
	}
	file.Signatures[1].Signature[0] ^= 1
	tampered, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ring.Verify(document, tampered); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("a valid signature hid a forged trusted one: %v", err)
	}
}
