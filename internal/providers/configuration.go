package providers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// Limits is the connection quota configuration persisted for distributed limits enforcement.
type Limits struct {
	MaxConcurrency    *int64 `json:"max_concurrency"`
	RequestsPerMinute *int64 `json:"requests_per_minute"`
	TokensPerMinute   *int64 `json:"tokens_per_minute"`
}

// Options carries connector options and per-model metadata.
type Options struct {
	Network           *egress.ConnectionOptions        `json:"network,omitempty"`
	SemanticHeaders   map[string]string                `json:"semantic_headers,omitempty"`
	QuerySettings     map[string]string                `json:"query_settings,omitempty"`
	OperationDefaults map[string]connectors.DefaultSet `json:"operation_defaults,omitempty"`
	Bindings          map[string]connectors.Binding    `json:"bindings,omitempty"`
	CredentialHeaders []string                         `json:"credential_headers"`
	Limits            *Limits                          `json:"limits"`
	Models            map[string]json.RawMessage       `json:"models"`
	ParameterDefaults map[string]json.RawMessage       `json:"parameter_defaults"`
	VendorID          *string                          `json:"vendor_id"`
}

// Configuration is the stored connection configuration; it is the contract's
// ProviderConfiguration verbatim.
type Configuration struct {
	ProviderID      string   `json:"-"`
	ProfileID       string   `json:"profile_id,omitempty"`
	ProfileRevision string   `json:"profile_revision,omitempty"`
	ProbeModels     []string `json:"-"`
	Kind            string   `json:"kind"`
	AuthMode        string   `json:"auth_mode"`
	Endpoint        *string  `json:"endpoint"`
	CloudRegion     *string  `json:"cloud_region"`
	CloudProject    *string  `json:"cloud_project"`
	Deployment      *string  `json:"deployment"`
	APIVersion      *string  `json:"api_version"`
	Options         Options  `json:"options"`
}

// normalize applies defaults and canonical forms so equal configurations
// compare equal and every stored document is complete.
func (c *Configuration) Normalize() {
	if c.Endpoint == nil || *c.Endpoint == "" {
		if endpoint := connectors.DefaultEndpoint(c.Kind, value(c.CloudRegion), value(c.CloudProject)); endpoint != "" {
			if c.ProfileID == "vertex-anthropic" {
				endpoint = strings.TrimSuffix(endpoint, "/google") + "/anthropic"
			}
			c.Endpoint = new(endpoint)
		}
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
		c.Options.VendorID = new(defaultVendor(c.Kind))
	}
}

// VendorMissing reports whether the configuration omits its vendor id.
func (c *Configuration) VendorMissing() bool {
	return c.Options.VendorID == nil || *c.Options.VendorID == ""
}

// validate rejects configurations this gateway cannot serve.
func (c *Configuration) Validate(policy *egress.Policy) error {
	options, err := json.Marshal(c.Options)
	if err != nil || len(options) > 1<<20 {
		return access.Invalid("configuration.options", "Connection options must fit within 1 MiB")
	}
	if c.Options.Network != nil && c.Options.Network.CredentialID != "" {
		id, err := uuid.Parse(c.Options.Network.CredentialID)
		if err != nil || id.String() != c.Options.Network.CredentialID {
			return access.Invalid("configuration.options.network.credential_id", "Use a canonical network credential UUID.")
		}
	}
	kind := kindByName(c.Kind)
	if kind == nil {
		return access.Fail(422, "provider_kind_unavailable", "Unknown provider connector kind.")
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
		if h == "" || !validHeaderName(h) || reservedHeader(h) {
			return access.Invalid("configuration.options.credential_headers", "Use valid header names other than hop-by-hop or framing headers.")
		}
	}
	if c.AuthMode == AuthHeaders && len(c.Options.CredentialHeaders) == 0 {
		return access.Invalid("configuration.options.credential_headers", "Name at least one header that carries the credential.")
	}
	if c.AuthMode != AuthHeaders && len(c.Options.CredentialHeaders) != 0 {
		return access.Invalid("configuration.options.credential_headers", "Credential headers apply only to the headers authentication mode.")
	}
	if err := c.transport().Validate(policy); err != nil {
		return access.Invalid("configuration", err.Error())
	}
	if kind, ok := VendorKind(value(c.Options.VendorID)); !ok || kind != c.Kind {
		return access.Invalid("configuration.options.vendor_id", "Vendor does not support this connector")
	}
	if len(c.Options.Models) > 2000 {
		return access.Invalid("configuration.options.models", "Use at most 2000 model metadata entries.")
	}
	for name, raw := range c.Options.Models {
		if len(name) == 0 || len(name) > 200 {
			return access.Invalid("configuration.options.models", "Model names must be 1–200 characters.")
		}
		var metadata map[string]any
		if err := json.Unmarshal(raw, &metadata); err != nil || metadata == nil {
			return access.Invalid("configuration.options.models", "Model metadata must be an object.")
		}
		if err := validateMetadata(raw); err != nil {
			return access.Invalid("configuration.options.models", err.Error())
		}
	}
	if len(c.Options.ParameterDefaults) > 64 {
		return access.Invalid("configuration.options.parameter_defaults", "Use at most 64 parameter defaults.")
	}
	if c.ProfileID != "" && len(c.Options.ParameterDefaults) != 0 {
		return access.Invalid("configuration.options.parameter_defaults", "Explicit profiles use operation_defaults; legacy parameter_defaults cannot be mixed.")
	}
	if err := openai.ValidateDefaults(c.Options.ParameterDefaults); err != nil {
		return access.Invalid("configuration.options.parameter_defaults", err.Error())
	}
	if c.Options.Limits != nil {
		if !ValidQuota(*c.Options.Limits) {
			return access.Invalid("configuration.options.limits", "Use positive limits: requests and concurrency at most 2147483647, tokens at most 9007199254740991.")
		}
	}
	return nil
}

// validQuota uses the same integer bounds as the shared limiter and reference
// contract. Absence, not zero, disables a dimension.
func ValidQuota(q Limits) bool {
	for _, bound := range []struct {
		value *int64
		max   int64
	}{{q.RequestsPerMinute, 2147483647}, {q.MaxConcurrency, 2147483647}, {q.TokensPerMinute, limits.MaxCounter}} {
		if bound.value != nil && (*bound.value < 1 || *bound.value > bound.max) {
			return false
		}
	}
	return true
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
func (c *Configuration) CredentialRequired() bool { return connectors.SecretRequired(c.AuthMode) }

// transportFingerprint identifies everything that affects how the gateway
// reaches the upstream. Certification evidence is retained only while it is
// unchanged.
func (c *Configuration) transportFingerprint() string {
	h := sha256.New()
	parts := []any{c.Kind, c.AuthMode, c.Endpoint, c.CloudRegion, c.CloudProject, c.Deployment, c.APIVersion, c.Options.CredentialHeaders, c.Options.ParameterDefaults, c.Options.Models, c.Options.VendorID}
	if c.ProfileID != "" {
		parts = append(parts, c.ProfileID, c.ProfileRevision, c.Options.SemanticHeaders, c.Options.QuerySettings, c.Options.OperationDefaults, c.Options.Bindings)
	}
	if c.Options.Network != nil {
		parts = append(parts, c.Options.Network)
	}
	encoded, _ := json.Marshal(parts)
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

func value(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
func (c *Configuration) transport() connectors.Config {
	return connectors.Config{Network: c.Options.Network, ProfileID: c.ProfileID, ProfileRevision: c.ProfileRevision, SemanticHeaders: c.Options.SemanticHeaders, QuerySettings: c.Options.QuerySettings, OperationDefaults: c.Options.OperationDefaults, Bindings: c.Options.Bindings, Kind: c.Kind, AuthMode: c.AuthMode, Endpoint: value(c.Endpoint), CloudRegion: value(c.CloudRegion), CloudProject: value(c.CloudProject), Deployment: value(c.Deployment), APIVersion: value(c.APIVersion), VendorID: value(c.Options.VendorID), CredentialHeaders: c.Options.CredentialHeaders, Models: c.Options.Models}
}
func reservedHeader(h string) bool {
	h = strings.ToLower(h)
	if strings.HasPrefix(h, "x-olp-") {
		return true
	}
	switch h {
	case "host", "content-length", "transfer-encoding", "connection", "cookie", "proxy-authorization", "proxy-connection", "upgrade", "te", "trailer", "content-type", "content-encoding", "accept", "traceparent", "tracestate", "x-request-id":
		return true
	}
	return false
}
