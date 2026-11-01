package secrets

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
)

func MCPPrivateKey(material []byte) (ed25519.PrivateKey, error) {
	if len(material) > 8192 {
		return nil, errors.New("signing key exceeds size limit")
	}
	block, rest := pem.Decode(material)
	if block == nil || block.Type != "PRIVATE KEY" || len(rest) != 0 {
		return nil, errors.New("use a PKCS8 Ed25519 private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	clear(block.Bytes)
	if err != nil {
		return nil, errors.New("invalid PKCS8 private key")
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("use an Ed25519 private key")
	}
	return key, nil
}
