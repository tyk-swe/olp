package secretstore

import (
	"context"
	"encoding/base64"
	"hash/crc32"
	"net/http"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/kms"
)

// Unwrap opens one data key under the process identity. Only a 256-bit result
// can enter the master key ring; service errors remain content-free.
func (r *Resolver) Unwrap(ctx context.Context, key WrappedKey) ([]byte, error) {
	if err := key.Validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var plaintext []byte
	switch key.Store {
	case "aws":
		ciphertext, err := base64.StdEncoding.Strict().DecodeString(key.Ciphertext)
		if err != nil || len(ciphertext) > 6144 {
			return nil, ErrUnavailable
		}
		config, err := r.awsConfig(ctx, key.Region)
		if err != nil {
			return nil, ErrUnavailable
		}
		output, err := kms.NewFromConfig(config).Decrypt(ctx, &kms.DecryptInput{KeyId: &key.KeyID, CiphertextBlob: ciphertext, EncryptionContext: key.Context})
		if err != nil {
			return nil, ErrUnavailable
		}
		plaintext = output.Plaintext
	case "gcp":
		ciphertext, err := base64.StdEncoding.Strict().DecodeString(key.Ciphertext)
		if err != nil {
			return nil, ErrUnavailable
		}
		token, err := r.googleToken(ctx)
		if err != nil {
			return nil, ErrUnavailable
		}
		var output struct {
			Plaintext string `json:"plaintext"`
			CRC       string `json:"plaintextCrc32c"`
			Verified  bool   `json:"verifiedCiphertextCrc32c"`
		}
		checksum := strconv.FormatUint(uint64(crc32.Checksum(ciphertext, crc32.MakeTable(crc32.Castagnoli))), 10)
		if r.json(ctx, http.MethodPost, "https://cloudkms.googleapis.com/v1/"+key.KeyID+":decrypt", bearer(token), map[string]string{"ciphertext": key.Ciphertext, "ciphertextCrc32c": checksum}, &output) != nil || !output.Verified {
			return nil, ErrUnavailable
		}
		plaintext, err = base64.StdEncoding.Strict().DecodeString(output.Plaintext)
		crc, crcErr := strconv.ParseUint(output.CRC, 10, 32)
		if err != nil || crcErr != nil || uint32(crc) != crc32.Checksum(plaintext, crc32.MakeTable(crc32.Castagnoli)) {
			clear(plaintext)
			return nil, ErrUnavailable
		}
	case "azure":
		u, err := azureResource(key.KeyID, "keys", true)
		if err != nil {
			return nil, ErrUnavailable
		}
		ciphertext, err := base64.RawURLEncoding.Strict().DecodeString(key.Ciphertext)
		if err != nil {
			return nil, ErrUnavailable
		}
		token, err := r.azureToken(ctx)
		if err != nil {
			return nil, ErrUnavailable
		}
		u.Path += "/decrypt"
		u.RawQuery = "api-version=7.4"
		var output struct {
			Value string `json:"value"`
			KeyID string `json:"kid"`
		}
		if r.json(ctx, http.MethodPost, u.String(), bearer(token), map[string]string{"alg": "RSA-OAEP-256", "value": base64.RawURLEncoding.EncodeToString(ciphertext)}, &output) != nil || output.KeyID != key.KeyID {
			return nil, ErrUnavailable
		}
		plaintext, err = base64.RawURLEncoding.Strict().DecodeString(output.Value)
		if err != nil {
			return nil, ErrUnavailable
		}
	case "vault":
		u, err := vaultResource(key.KeyID)
		if err != nil {
			return nil, ErrUnavailable
		}
		parts := strings.Split(u.Path, "/")
		if len(parts) < 5 || parts[len(parts)-2] != "keys" {
			return nil, ErrUnavailable
		}
		token, err := r.vaultToken(ctx, u)
		if err != nil {
			return nil, ErrUnavailable
		}
		parts[len(parts)-2] = "decrypt"
		u.Path = strings.Join(parts, "/")
		var output struct {
			Data struct {
				Plaintext string `json:"plaintext"`
			} `json:"data"`
		}
		if r.json(ctx, http.MethodPost, u.String(), vaultHeaders(token), map[string]string{"ciphertext": key.Ciphertext}, &output) != nil {
			return nil, ErrUnavailable
		}
		plaintext, err = base64.StdEncoding.Strict().DecodeString(output.Data.Plaintext)
		if err != nil {
			return nil, ErrUnavailable
		}
	}
	if len(plaintext) != 32 {
		clear(plaintext)
		return nil, ErrUnavailable
	}
	return plaintext, nil
}
