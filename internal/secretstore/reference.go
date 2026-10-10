// Package secretstore resolves version-pinned external secrets and unwraps data
// keys. Every outbound request, including workload identity exchanges, uses the
// installation's provider egress policy. Errors contain no service response.
package secretstore

import (
	"errors"
	"regexp"
	"strings"
)

var ErrUnavailable = errors.New("external secret service is unavailable")

// Reference is immutable credential metadata. Version aliases such as latest
// are deliberately unsupported: activation pins the exact store version.
type Reference struct {
	Store    string `json:"store"`
	SecretID string `json:"secret_id"`
	Version  string `json:"version"`
	Region   string `json:"region,omitempty"`
	Field    string `json:"field,omitempty"`
}

func (Reference) String() string { return "SecretReference([REDACTED])" }

// WrappedKey carries ciphertext and public key identity, never a plaintext key.
type WrappedKey struct {
	Store      string            `json:"store"`
	KeyID      string            `json:"key_id"`
	Ciphertext string            `json:"ciphertext"`
	Region     string            `json:"region,omitempty"`
	Context    map[string]string `json:"context,omitempty"`
}

func (WrappedKey) String() string { return "WrappedKey([REDACTED])" }

var component = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
var versionID = regexp.MustCompile(`^[A-Za-z0-9-]{32,64}$`)
var numericVersion = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
var awsRegion = regexp.MustCompile(`^[a-z]{2}(?:-[a-z]+)+-[0-9]+$`)

func clean(value string, max int) bool {
	return value != "" && len(value) <= max && !strings.ContainsAny(value, "\x00\r\n\t") && strings.TrimSpace(value) == value
}

func (r Reference) Validate() error {
	if !clean(r.SecretID, 2048) || !clean(r.Version, 128) || len(r.Field) > 128 || strings.ContainsAny(r.Field, "\x00\r\n") {
		return errors.New("invalid external credential reference")
	}
	switch r.Store {
	case "aws":
		if !awsRegion.MatchString(r.Region) || !versionID.MatchString(r.Version) || r.Field != "" {
			return errors.New("AWS references require a region and immutable version ID")
		}
	case "gcp":
		parts := strings.Split(r.SecretID, "/")
		if len(parts) != 4 || parts[0] != "projects" || parts[2] != "secrets" || !component.MatchString(parts[1]) || !component.MatchString(parts[3]) || !numericVersion.MatchString(r.Version) || r.Region != "" || r.Field != "" {
			return errors.New("Google references require a project secret and numeric version")
		}
	case "azure":
		if _, err := azureResource(r.SecretID, "secrets", false); err != nil || !component.MatchString(r.Version) || len(r.Version) != 32 || r.Region != "" || r.Field != "" {
			return errors.New("Azure references require a versioned vault secret")
		}
	case "vault":
		if _, err := vaultResource(r.SecretID); err != nil || !numericVersion.MatchString(r.Version) || !clean(r.Field, 128) || r.Region != "" {
			return errors.New("Vault KV v2 references require a version and field")
		}
	default:
		return errors.New("unsupported external credential store")
	}
	return nil
}

func (k WrappedKey) Validate() error {
	if !clean(k.KeyID, 2048) || !clean(k.Ciphertext, 32768) || len(k.Context) > 32 {
		return errors.New("invalid wrapped master key")
	}
	for name, value := range k.Context {
		if !clean(name, 256) || !clean(value, 1024) {
			return errors.New("invalid master key encryption context")
		}
	}
	if k.Store != "aws" && len(k.Context) > 0 {
		return errors.New("encryption context is supported only by AWS KMS")
	}
	switch k.Store {
	case "aws":
		if !awsRegion.MatchString(k.Region) {
			return errors.New("AWS KMS requires a region")
		}
	case "gcp":
		parts := strings.Split(k.KeyID, "/")
		if len(parts) != 8 || parts[0] != "projects" || parts[2] != "locations" || parts[4] != "keyRings" || parts[6] != "cryptoKeys" || k.Region != "" {
			return errors.New("invalid Google KMS key")
		}
		for _, i := range []int{1, 3, 5, 7} {
			if !component.MatchString(parts[i]) {
				return errors.New("invalid Google KMS key")
			}
		}
	case "azure":
		if _, err := azureResource(k.KeyID, "keys", true); err != nil || k.Region != "" {
			return errors.New("invalid versioned Azure vault key")
		}
	case "vault":
		resource, err := vaultResource(k.KeyID)
		if err != nil {
			return errors.New("invalid Vault Transit key")
		}
		parts := strings.Split(resource.Path, "/")
		if len(parts) < 5 || parts[len(parts)-2] != "keys" || k.Region != "" || !strings.HasPrefix(k.Ciphertext, "vault:v") {
			return errors.New("invalid Vault Transit key")
		}
	default:
		return errors.New("unsupported master key wrapping service")
	}
	return nil
}
