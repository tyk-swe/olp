// Package secrets owns secret input, domain-separated digests, and record-bound
// encryption. Errors deliberately contain no input or cryptographic material.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
)

func ReadFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot read secret file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || (info.Mode().Perm() != 0600 && info.Mode().Perm() != 0640) {
		return nil, errors.New("secret file must be regular with mode 0600 or 0640")
	}
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(data) > 65536 || len(strings.TrimSpace(string(data))) == 0 {
		return nil, errors.New("secret file must contain 1–65536 bytes")
	}
	return []byte(strings.TrimSpace(string(data))), nil
}

func DecodeKey(raw string) ([]byte, error) {
	if b, err := hex.DecodeString(raw); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(raw); err == nil && len(b) == 32 {
		return b, nil
	}
	return nil, errors.New("key must encode exactly 32 bytes as hex or base64")
}

type KeyRing struct {
	Active int
	keys   map[int][]byte
}

func (k *KeyRing) String() string { return "KeyRing([REDACTED])" }
func LoadRing(path string) (*KeyRing, error) {
	data, err := ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseRing(data)
}
func ParseRing(data []byte) (*KeyRing, error) {
	var doc struct {
		Active int `json:"active_version"`
		Keys   []struct {
			Version int    `json:"version"`
			Key     string `json:"key"`
		} `json:"keys"`
	}
	if json.Unmarshal(data, &doc) != nil || doc.Active < 1 || len(doc.Keys) < 1 || len(doc.Keys) > 32 {
		return nil, errors.New("invalid master key ring")
	}
	ring := &KeyRing{Active: doc.Active, keys: map[int][]byte{}}
	for _, entry := range doc.Keys {
		key, err := DecodeKey(entry.Key)
		if err != nil || entry.Version < 1 || ring.keys[entry.Version] != nil {
			return nil, errors.New("invalid master key entry")
		}
		ring.keys[entry.Version] = key
	}
	if ring.keys[ring.Active] == nil {
		return nil, errors.New("missing active master key")
	}
	return ring, nil
}
func (k *KeyRing) Has(version int) bool { return k.keys[version] != nil }
func (k *KeyRing) aead(version int) (cipher.AEAD, error) {
	b := k.keys[version]
	if b == nil {
		return nil, errors.New("required master key is missing")
	}
	block, err := aes.NewCipher(b)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func aad(installation, purpose, id string) []byte {
	b, _ := json.Marshal([]string{"olp-go-secret-v1", installation, purpose, id})
	return b
}
func (k *KeyRing) Seal(installation, purpose, id string, data []byte) ([]byte, error) {
	a, err := k.aead(k.Active)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, a.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	return a.Seal(nonce, nonce, data, aad(installation, purpose, id)), nil
}
func (k *KeyRing) Open(installation, purpose, id string, version int, data []byte) ([]byte, error) {
	a, err := k.aead(version)
	if err != nil {
		return nil, err
	}
	if len(data) < a.NonceSize()+a.Overhead() {
		return nil, errors.New("invalid encrypted secret")
	}
	out, err := a.Open(nil, data[:a.NonceSize()], data[a.NonceSize():], aad(installation, purpose, id))
	if err != nil {
		return nil, errors.New("encrypted secret authentication failed")
	}
	return out, nil
}

type AuthKey struct {
	key          []byte
	installation string
}

func (a *AuthKey) String() string { return "AuthKey([REDACTED])" }
func NewAuthKey(key []byte, installation string) *AuthKey {
	return &AuthKey{key: append([]byte(nil), key...), installation: installation}
}
func (a *AuthKey) Digest(purpose, value string) []byte {
	h := hmac.New(sha256.New, a.key)
	b, _ := json.Marshal([]string{"olp-go-auth-v1", a.installation, purpose, value})
	h.Write(b)
	return h.Sum(nil)
}
func Token() string { return rand.Text() }
