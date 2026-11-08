package serve

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
)

type bindingDigest struct {
	Ref    promotion.Ref `json:"ref"`
	SHA256 string        `json:"sha256"`
}

type fingerprint struct {
	DocumentDigest string                              `json:"document_digest"`
	ExpectedDigest *string                             `json:"expected_digest"`
	SecretBindings []bindingDigest                     `json:"secret_bindings"`
	CodeBindings   *contract.ConfigurationCodeBindings `json:"code_bindings,omitempty"`
}

func (d desired) fingerprint() (fingerprint, error) {
	secrets, err := promotion.TryMap(slices.Sorted(maps.Keys(d.bindings)), func(ref promotion.Ref) (bindingDigest, error) {
		sum, err := credentialDigest(d.bindings[ref])
		return bindingDigest{Ref: ref, SHA256: sum}, err
	})
	if err != nil {
		return fingerprint{}, err
	}
	return fingerprint{DocumentDigest: d.digest, ExpectedDigest: d.expected, SecretBindings: secrets, CodeBindings: d.codeBindings}, nil
}

func credentialDigest(credential promotion.Credential) (string, error) {
	material, err := credentialMaterial(credential)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(material)
	return hex.EncodeToString(sum[:]), nil
}

func credentialMaterial(credential promotion.Credential) ([]byte, error) {
	switch c := credential.(type) {
	case promotion.External:
		reference, err := json.Marshal(c.Reference)
		return append([]byte("external:"), reference...), err
	case promotion.Sealed:
		return []byte("sealed:" + c.Secret), nil
	}
	return nil, fmt.Errorf("unknown credential %T", credential)
}
