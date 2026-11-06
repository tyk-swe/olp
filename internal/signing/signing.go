package signing

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"regexp"
	"slices"
	"strings"
)

var (
	ErrMalformedSignature = errors.New("the signature file is malformed")
	ErrUnknownKey         = errors.New("no trusted key signed the document")
	ErrInvalidSignature   = errors.New("a trusted key's signature does not verify")
)

const MaxSignatureFile = 64 << 10

var keyID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,62}$`)

type Key struct {
	ID     string
	Public ed25519.PublicKey
}

type Keyring struct{ keys []Key }

type File struct {
	Signatures []Entry `json:"signatures"`
}

type Entry struct {
	KeyID     string `json:"key_id"`
	Signature []byte `json:"signature"`
}

func NewKeyring(keys ...Key) (Keyring, error) {
	if i := slices.IndexFunc(keys, malformedKey); i >= 0 {
		return Keyring{}, fmt.Errorf("signing key %q is malformed", keys[i].ID)
	}
	sorted := slices.SortedFunc(mapped(keys, cloneKey), func(a, b Key) int { return strings.Compare(a.ID, b.ID) })
	if id, repeats := repeated(slices.Collect(mapped(sorted, keyIDOf))); repeats {
		return Keyring{}, fmt.Errorf("signing key %q is declared twice", id)
	}
	return Keyring{keys: sorted}, nil
}

func (k Keyring) IDs() []string {
	return slices.Collect(mapped(k.keys, keyIDOf))
}

func (k Keyring) Verify(document, signatures []byte) (string, error) {
	file, err := parse(signatures)
	if err != nil {
		return "", err
	}
	trusted := slices.Collect(filtered(file.Signatures, k.trusts))
	forged := func(entry Entry) bool {
		public, _ := k.lookup(entry.KeyID)
		return !ed25519.Verify(public, document, entry.Signature)
	}
	if i := slices.IndexFunc(trusted, forged); i >= 0 {
		return "", fmt.Errorf("%w: key %s", ErrInvalidSignature, trusted[i].KeyID)
	}
	if len(trusted) == 0 {
		return "", ErrUnknownKey
	}
	return trusted[0].KeyID, nil
}

func Sign(signatures []byte, id string, private ed25519.PrivateKey, document []byte) ([]byte, error) {
	if !keyID.MatchString(id) || len(private) != ed25519.PrivateKeySize {
		return nil, errors.New("the signing key or its identifier is malformed")
	}
	existing, err := existingEntries(signatures)
	if err != nil {
		return nil, err
	}
	others := slices.Collect(filtered(existing, func(e Entry) bool { return e.KeyID != id }))
	entries := slices.Concat(others, []Entry{{KeyID: id, Signature: ed25519.Sign(private, document)}})
	sorted := slices.SortedFunc(slices.Values(entries), func(a, b Entry) int { return strings.Compare(a.KeyID, b.KeyID) })
	encoded, err := json.MarshalIndent(File{Signatures: sorted}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func ParsePublicKey(encoded string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("the public key must be 32 base64-encoded bytes")
	}
	return ed25519.PublicKey(raw), nil
}

func ParseSeed(encoded string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) != ed25519.SeedSize {
		return nil, errors.New("the signing seed must be 32 base64-encoded bytes")
	}
	return ed25519.NewKeyFromSeed(raw), nil
}

func (k Keyring) lookup(id string) (ed25519.PublicKey, bool) {
	i, found := slices.BinarySearchFunc(k.keys, id, func(key Key, id string) int { return strings.Compare(key.ID, id) })
	if !found {
		return nil, false
	}
	return k.keys[i].Public, true
}

func (k Keyring) trusts(entry Entry) bool {
	_, ok := k.lookup(entry.KeyID)
	return ok
}

func existingEntries(signatures []byte) ([]Entry, error) {
	if len(signatures) == 0 {
		return nil, nil
	}
	file, err := parse(signatures)
	return file.Signatures, err
}

func parse(signatures []byte) (File, error) {
	if len(signatures) > MaxSignatureFile {
		return File{}, ErrMalformedSignature
	}
	var file File
	if err := json.Unmarshal(signatures, &file); err != nil || len(file.Signatures) == 0 {
		return File{}, ErrMalformedSignature
	}
	if slices.ContainsFunc(file.Signatures, malformedEntry) {
		return File{}, ErrMalformedSignature
	}
	if _, repeats := repeated(slices.Sorted(mapped(file.Signatures, func(e Entry) string { return e.KeyID }))); repeats {
		return File{}, ErrMalformedSignature
	}
	return file, nil
}

func malformedKey(key Key) bool {
	return !keyID.MatchString(key.ID) || len(key.Public) != ed25519.PublicKeySize
}

func malformedEntry(entry Entry) bool {
	return !keyID.MatchString(entry.KeyID) || len(entry.Signature) != ed25519.SignatureSize
}

func cloneKey(key Key) Key { return Key{ID: key.ID, Public: slices.Clone(key.Public)} }

func keyIDOf(key Key) string { return key.ID }

func repeated(sorted []string) (string, bool) {
	for i := range len(sorted) - 1 {
		if sorted[i] == sorted[i+1] {
			return sorted[i], true
		}
	}
	return "", false
}

func mapped[E, R any](s []E, f func(E) R) iter.Seq[R] {
	return func(yield func(R) bool) {
		for _, e := range s {
			if !yield(f(e)) {
				return
			}
		}
	}
}

func filtered[E any](s []E, keep func(E) bool) iter.Seq[E] {
	return func(yield func(E) bool) {
		for _, e := range s {
			if keep(e) && !yield(e) {
				return
			}
		}
	}
}
