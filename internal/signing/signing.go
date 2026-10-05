// Package signing verifies the detached Ed25519 signatures of the documents a
// release ships: the reference catalog and the plugin index. A signature
// covers the exact bytes of its document, so anyone can verify it without
// reproducing an encoding.
//
// A signature file may carry one signature per key, so a document can be
// signed by an outgoing and an incoming key while installations rotate.
package signing

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

var (
	// ErrMalformedSignature reports a signature file that is not one.
	ErrMalformedSignature = errors.New("the signature file is malformed")
	// ErrUnknownKey reports a document signed by no trusted key.
	ErrUnknownKey = errors.New("no trusted key signed the document")
	// ErrInvalidSignature reports a trusted key's signature that does not
	// verify: the document or its signature was altered.
	ErrInvalidSignature = errors.New("a trusted key's signature does not verify")
)

// MaxSignatureFile bounds a signature file.
const MaxSignatureFile = 64 << 10

var keyID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,62}$`)

// Key is a named Ed25519 verification key.
type Key struct {
	ID     string
	Public ed25519.PublicKey
}

// Keyring is a set of trusted verification keys by identifier.
type Keyring struct{ keys map[string]ed25519.PublicKey }

// NewKeyring builds a keyring. An empty keyring trusts nothing.
func NewKeyring(keys ...Key) (Keyring, error) {
	ring := Keyring{keys: make(map[string]ed25519.PublicKey, len(keys))}
	for _, key := range keys {
		if !keyID.MatchString(key.ID) || len(key.Public) != ed25519.PublicKeySize {
			return Keyring{}, fmt.Errorf("signing key %q is malformed", key.ID)
		}
		if _, duplicate := ring.keys[key.ID]; duplicate {
			return Keyring{}, fmt.Errorf("signing key %q is declared twice", key.ID)
		}
		ring.keys[key.ID] = slices.Clone(key.Public)
	}
	return ring, nil
}

// MustKeyring is NewKeyring for keys compiled into the binary.
func MustKeyring(keys ...Key) Keyring {
	ring, err := NewKeyring(keys...)
	if err != nil {
		panic(err)
	}
	return ring
}

// IDs are the identifiers of the trusted keys, sorted.
func (k Keyring) IDs() []string {
	ids := make([]string, 0, len(k.keys))
	for id := range k.keys {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// Public returns a trusted key.
func (k Keyring) Public(id string) (ed25519.PublicKey, bool) {
	key, ok := k.keys[id]
	return slices.Clone(key), ok
}

// File is a detached signature file.
type File struct {
	Signatures []Entry `json:"signatures"`
}

// Entry is one key's signature; Signature is base64 in JSON.
type Entry struct {
	KeyID     string `json:"key_id"`
	Signature []byte `json:"signature"`
}

// Verify checks a document against its signature file and returns the
// trusted key that verified it. Signatures by keys outside the keyring are
// ignored, at least one trusted key must have signed, and every trusted key's
// signature must verify.
func (k Keyring) Verify(document, signatures []byte) (string, error) {
	file, err := parse(signatures)
	if err != nil {
		return "", err
	}
	verified := ""
	for _, entry := range file.Signatures {
		public, trusted := k.keys[entry.KeyID]
		if !trusted {
			continue
		}
		if !ed25519.Verify(public, document, entry.Signature) {
			return "", fmt.Errorf("%w: key %s", ErrInvalidSignature, entry.KeyID)
		}
		if verified == "" {
			verified = entry.KeyID
		}
	}
	if verified == "" {
		return "", ErrUnknownKey
	}
	return verified, nil
}

// Sign adds or replaces one key's signature of document in a signature file,
// keeping the other keys' signatures. An empty file starts a new one.
func Sign(signatures []byte, id string, private ed25519.PrivateKey, document []byte) ([]byte, error) {
	if !keyID.MatchString(id) || len(private) != ed25519.PrivateKeySize {
		return nil, errors.New("the signing key or its identifier is malformed")
	}
	file := File{}
	if len(signatures) > 0 {
		parsed, err := parse(signatures)
		if err != nil {
			return nil, err
		}
		file = parsed
	}
	file.Signatures = slices.DeleteFunc(file.Signatures, func(e Entry) bool { return e.KeyID == id })
	file.Signatures = append(file.Signatures, Entry{KeyID: id, Signature: ed25519.Sign(private, document)})
	slices.SortFunc(file.Signatures, func(a, b Entry) int { return strings.Compare(a.KeyID, b.KeyID) })
	encoded, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

// ParsePublicKey decodes a base64 Ed25519 public key.
func ParsePublicKey(encoded string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("the public key must be 32 base64-encoded bytes")
	}
	return ed25519.PublicKey(raw), nil
}

// ParseSeed decodes a base64 Ed25519 seed into its private key.
func ParseSeed(encoded string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) != ed25519.SeedSize {
		return nil, errors.New("the signing seed must be 32 base64-encoded bytes")
	}
	return ed25519.NewKeyFromSeed(raw), nil
}

func parse(signatures []byte) (File, error) {
	if len(signatures) > MaxSignatureFile {
		return File{}, ErrMalformedSignature
	}
	var file File
	if err := json.Unmarshal(signatures, &file); err != nil || len(file.Signatures) == 0 {
		return File{}, ErrMalformedSignature
	}
	seen := map[string]bool{}
	for _, entry := range file.Signatures {
		if !keyID.MatchString(entry.KeyID) || len(entry.Signature) != ed25519.SignatureSize || seen[entry.KeyID] {
			return File{}, ErrMalformedSignature
		}
		seen[entry.KeyID] = true
	}
	return file, nil
}
