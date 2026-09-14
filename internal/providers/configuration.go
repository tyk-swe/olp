package providers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// Limits is the connection quota configuration persisted for M4 enforcement.
type Limits struct {
	MaxConcurrency    *int64 `json:"max_concurrency"`
	RequestsPerMinute *int64 `json:"requests_per_minute"`
	TokensPerMinute   *int64 `json:"tokens_per_minute"`
}

// Options carries connector options and per-model metadata.
type Options struct {
	CredentialHeaders []string                   `json:"credential_headers"`
	Limits            *Limits                    `json:"limits"`
	Models            map[string]json.RawMessage `json:"models"`
	ParameterDefaults map[string]json.RawMessage `json:"parameter_defaults"`
	VendorID          *string                    `json:"vendor_id"`
}

// Configuration is the stored connection configuration; it is the contract's
// ProviderConfiguration verbatim.
type Configuration struct {
	Kind         string  `json:"kind"`
	AuthMode     string  `json:"auth_mode"`
	Endpoint     *string `json:"endpoint"`
	CloudRegion  *string `json:"cloud_region"`
	CloudProject *string `json:"cloud_project"`
	Deployment   *string `json:"deployment"`
	APIVersion   *string `json:"api_version"`
	Options      Options `json:"options"`
}

// normalize applies defaults and canonical forms so equal configurations
// compare equal and every stored document is complete.
func (c *Configuration) normalize() {
	if c.Kind == KindOpenAI && (c.Endpoint == nil || *c.Endpoint == "") {
		c.Endpoint = ptr(DefaultOpenAIEndpoint)
	}
	if c.Options.CredentialHeaders == nil {
		c.Options.CredentialHeaders = []string{}
	}
	for i, h := range c.Options.CredentialHeaders {
		c.Options.CredentialHeaders[i] = textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(h))
	}
	if c.Options.Models == nil {
		c.Options.Models = map[string]json.RawMessage{}
	}
	if c.Options.ParameterDefaults == nil {
		c.Options.ParameterDefaults = map[string]json.RawMessage{}
	}
	if c.VendorMissing() {
		c.Options.VendorID = ptr(c.Kind)
	}
}

// VendorMissing reports whether the configuration omits its vendor id.
func (c *Configuration) VendorMissing() bool {
	return c.Options.VendorID == nil || *c.Options.VendorID == ""
}

// validate rejects configurations this gateway cannot serve.
func (c *Configuration) validate(policy *egress.Policy) error {
	kind := kindByName(c.Kind)
	if kind == nil {
		return access.Fail(422, "provider_kind_unavailable", "Only openai and openai_compatible connections are available in this release.")
	}
	modeAllowed := false
	for _, m := range kind.AuthModes {
		modeAllowed = modeAllowed || m.Mode == c.AuthMode
	}
	if !modeAllowed {
		return access.Invalid("configuration.auth_mode", "This authentication mode is not available for the selected kind.")
	}
	if c.Endpoint == nil || *c.Endpoint == "" {
		return access.Invalid("configuration.endpoint", "Provide the base URL of the API.")
	}
	if _, err := policy.ValidateEndpoint(*c.Endpoint); err != nil {
		return access.Invalid("configuration.endpoint", err.Error())
	}
	if len(c.Options.CredentialHeaders) > 16 {
		return access.Invalid("configuration.options.credential_headers", "Use at most 16 credential headers.")
	}
	for _, h := range c.Options.CredentialHeaders {
		if h == "" || !validHeaderName(h) || h == "Host" || h == "Content-Length" || h == "Transfer-Encoding" || h == "Connection" {
			return access.Invalid("configuration.options.credential_headers", "Use valid header names other than hop-by-hop or framing headers.")
		}
	}
	if c.AuthMode == AuthHeaders && len(c.Options.CredentialHeaders) == 0 {
		return access.Invalid("configuration.options.credential_headers", "Name at least one header that carries the credential.")
	}
	if c.AuthMode != AuthHeaders && len(c.Options.CredentialHeaders) != 0 {
		return access.Invalid("configuration.options.credential_headers", "Credential headers apply only to the headers authentication mode.")
	}
	for _, field := range []struct {
		name  string
		value *string
	}{{"cloud_region", c.CloudRegion}, {"cloud_project", c.CloudProject}, {"deployment", c.Deployment}, {"api_version", c.APIVersion}} {
		if field.value != nil && *field.value != "" {
			return access.Invalid("configuration."+field.name, "This field does not apply to OpenAI-compatible connections.")
		}
	}
	if len(c.Options.Models) > 2000 {
		return access.Invalid("configuration.options.models", "Use at most 2000 model metadata entries.")
	}
	for name, raw := range c.Options.Models {
		if len(name) == 0 || len(name) > 200 {
			return access.Invalid("configuration.options.models", "Model names must be 1–200 characters.")
		}
		var metadata map[string]any
		if err := json.Unmarshal(raw, &metadata); err != nil {
			return access.Invalid("configuration.options.models", "Model metadata must be an object.")
		}
	}
	if len(c.Options.ParameterDefaults) > 64 {
		return access.Invalid("configuration.options.parameter_defaults", "Use at most 64 parameter defaults.")
	}
	if err := openai.ValidateDefaults(c.Options.ParameterDefaults); err != nil {
		return access.Invalid("configuration.options.parameter_defaults", err.Error())
	}
	if c.Options.Limits != nil {
		for _, v := range []*int64{c.Options.Limits.MaxConcurrency, c.Options.Limits.RequestsPerMinute, c.Options.Limits.TokensPerMinute} {
			if v != nil && *v < 0 {
				return access.Invalid("configuration.options.limits", "Limits must be zero or positive.")
			}
		}
	}
	return nil
}

func validHeaderName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, r := range name {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// credentialRequired reports whether the auth mode needs a secret.
func (c *Configuration) credentialRequired() bool { return c.AuthMode != AuthNone }

// transportFingerprint identifies everything that affects how the gateway
// reaches the upstream. Certification evidence is retained only while it is
// unchanged.
func (c *Configuration) transportFingerprint() string {
	h := sha256.New()
	encoded, _ := json.Marshal([]any{c.Kind, c.AuthMode, c.Endpoint, c.CloudRegion, c.CloudProject, c.Deployment, c.APIVersion, c.Options.CredentialHeaders})
	h.Write(encoded)
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// applyCredential adds the credential to an upstream request.
func (c *Configuration) applyCredential(req *http.Request, credential []byte) error {
	switch c.AuthMode {
	case AuthAPIKey:
		req.Header.Set("Authorization", "Bearer "+string(credential))
	case AuthHeaders:
		return egress.ApplyCredentialHeaders(req.Header, c.Options.CredentialHeaders, credential)
	}
	return nil
}
