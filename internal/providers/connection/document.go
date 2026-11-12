package connection

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net/textproto"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/providerconfig"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/vendors"
)

type Limits struct {
	MaxConcurrency    *int64 `json:"max_concurrency"`
	RequestsPerMinute *int64 `json:"requests_per_minute"`
	TokensPerMinute   *int64 `json:"tokens_per_minute"`
	routing.Supply
}

type Options struct {
	TuningDatasetURIPrefixes []string                         `json:"tuning_dataset_uri_prefixes,omitempty"`
	DocumentURIPrefixes      []string                         `json:"document_uri_prefixes,omitempty"`
	Network                  *egress.ConnectionOptions        `json:"network,omitempty"`
	SemanticHeaders          map[string]string                `json:"semantic_headers,omitempty"`
	QuerySettings            map[string]string                `json:"query_settings,omitempty"`
	OperationDefaults        map[string]connectors.DefaultSet `json:"operation_defaults,omitempty"`
	Bindings                 map[string]connectors.Binding    `json:"bindings,omitempty"`
	CredentialHeaders        []string                         `json:"credential_headers"`
	Limits                   *Limits                          `json:"limits"`
	Models                   map[string]json.RawMessage       `json:"models"`
	ParameterDefaults        map[string]json.RawMessage       `json:"parameter_defaults"`
	VendorID                 *string                          `json:"vendor_id"`
	// Commitment is the provisioned capacity the connection pays for: priced
	// attempts on it bill nothing extra and share the amount across each UTC
	// calendar period by list-cost weight.
	Commitment    *routing.Commitment `json:"commitment,omitempty"`
	PluginOptions map[string]string   `json:"plugin_options,omitempty"`
	// HealthProbe opts the connection into active health probing, which
	// sends billable synthetic requests.
	HealthProbe *routing.HealthProbe `json:"health_probe,omitempty"`
}

// Configuration is the contract's ProviderConfiguration verbatim. A plugin
// provider pins its plugin's digest as the profile revision.
type Configuration struct {
	providerconfig.Identity[*string]
	Options Options `json:"options"`
}

func Decode(raw []byte) (Configuration, error) {
	var decoded Configuration
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return Configuration{}, err
	}
	return decoded.Normalized(), nil
}

func (c Configuration) Normalized() Configuration {
	normalized := c
	normalized.Endpoint = c.defaultedEndpoint()
	normalized.Options = c.Options.normalized(c.Kind)
	return normalized
}

func (c Configuration) defaultedEndpoint() *string {
	if c.Endpoint != nil && *c.Endpoint != "" {
		return c.Endpoint
	}
	endpoint := connectors.DefaultProfileEndpoint(c.Kind, c.ProfileID, providerconfig.Value(c.CloudRegion), providerconfig.Value(c.CloudProject))
	if endpoint == "" {
		return c.Endpoint
	}
	return new(endpoint)
}

func (o Options) normalized(kind string) Options {
	normalized := o
	normalized.CredentialHeaders = canonicalHeaders(o.CredentialHeaders)
	normalized.Models = orEmpty(o.Models)
	normalized.ParameterDefaults = orEmpty(o.ParameterDefaults)
	normalized.PluginOptions = setOptions(o.PluginOptions)
	normalized.VendorID = defaultVendor(kind, o.VendorID)
	return normalized
}

func canonicalHeaders(headers []string) []string {
	canonical := slices.Collect(func(yield func(string) bool) {
		for _, h := range headers {
			if !yield(textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(h))) {
				return
			}
		}
	})
	if canonical == nil {
		return []string{}
	}
	return canonical
}

func orEmpty[V any](values map[string]V) map[string]V {
	if values == nil {
		return map[string]V{}
	}
	return values
}

func setOptions(options map[string]string) map[string]string {
	if options == nil {
		return nil
	}
	return maps.Collect(func(yield func(string, string) bool) {
		for key, value := range options {
			if value != "" && !yield(key, value) {
				return
			}
		}
	})
}

// No vendor list price applies to a plugin provider.
func defaultVendor(kind string, vendorID *string) *string {
	switch {
	case !vendorMissing(vendorID):
		return vendorID
	case kind == connectors.KindPlugin:
		return nil
	default:
		return new(vendors.DefaultFor(kind))
	}
}

func vendorMissing(vendorID *string) bool { return vendorID == nil || *vendorID == "" }

func (c Configuration) PluginDigest() string {
	if c.Kind != connectors.KindPlugin {
		return ""
	}
	return c.ProfileRevision
}

func (c Configuration) CredentialRequired() bool { return connectors.SecretRequired(c.AuthMode) }

// Only grant enrollment creates the credential versions of a grant provider.
func (c Configuration) Grant() bool { return c.AuthMode == connectors.AuthGrant }

// A grant serves only the plugin build and profile that enrolled it, because a
// plugin gets only its own grant and a build's name is only what its manifest
// claims. Moving a provider to another build takes a new enrollment per slot.
func (c Configuration) Authenticates(pluginDigest, profileID string) bool {
	if c.Grant() {
		return profileID != "" && pluginDigest != "" && pluginDigest == c.ProfileRevision && profileID == c.ProfileID
	}
	return pluginDigest == "" && profileID == ""
}

// Certification evidence is kept only while the fingerprint is unchanged.
func (c Configuration) Fingerprint() string {
	parts := slices.Concat([]any{c.Kind, c.AuthMode, c.Endpoint, c.CloudRegion, c.CloudProject, c.Deployment, c.APIVersion, c.Options.CredentialHeaders, c.Options.ParameterDefaults, c.Options.Models, c.Options.VendorID},
		when(c.ProfileID != "", c.ProfileID, c.ProfileRevision, c.Options.SemanticHeaders, c.Options.QuerySettings, c.Options.OperationDefaults, c.Options.Bindings),
		when(c.Kind == connectors.KindPlugin, c.Options.PluginOptions),
		when(c.Options.Network != nil, c.Options.Network),
		when(c.BatchStorage != nil, c.BatchStorage),
		when(len(c.Options.TuningDatasetURIPrefixes) > 0, c.Options.TuningDatasetURIPrefixes),
		when(len(c.Options.DocumentURIPrefixes) > 0, c.Options.DocumentURIPrefixes))
	encoded, _ := json.Marshal(parts)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])[:fingerprintLength]
}

const fingerprintLength = 32

func when(included bool, parts ...any) []any {
	if !included {
		return nil
	}
	return parts
}

func (c Configuration) Serving() (routing.Configuration, error) {
	encoded, err := json.Marshal(c)
	if err != nil {
		return routing.Configuration{}, err
	}
	var served routing.Configuration
	if err := json.Unmarshal(encoded, &served); err != nil {
		return routing.Configuration{}, err
	}
	return served, nil
}

func FromProvider(p routing.Provider) Configuration {
	return Configuration{
		Identity: providerconfig.Identity[*string]{
			CredentialSource: p.CredentialSource, ProfileID: p.ProfileID, ProfileRevision: p.ProfileRevision, Kind: p.Kind, AuthMode: p.AuthMode,
			Endpoint: optional(p.Endpoint), CloudRegion: optional(p.CloudRegion), CloudProject: optional(p.CloudProject), BatchStorage: p.BatchStorage,
			Deployment: optional(p.Deployment), APIVersion: optional(p.APIVersion),
		},
		Options: Options{
			TuningDatasetURIPrefixes: p.TuningDatasetURIPrefixes, DocumentURIPrefixes: p.DocumentURIPrefixes, Network: p.Network,
			SemanticHeaders: p.SemanticHeaders, QuerySettings: p.QuerySettings, OperationDefaults: p.OperationDefaults, Bindings: p.Bindings,
			CredentialHeaders: append([]string{}, p.CredentialHeaders...), Limits: limitsOf(p.Limits), Models: maps.Clone(p.Models),
			ParameterDefaults: maps.Clone(p.ParameterDefaults), VendorID: optional(p.VendorID), Commitment: p.Commitment,
			PluginOptions: p.PluginOptions, HealthProbe: p.HealthProbe,
		},
	}
}

func limitsOf(limits *routing.Limits) *Limits {
	if limits == nil {
		return nil
	}
	return &Limits{MaxConcurrency: limits.MaxConcurrency, RequestsPerMinute: limits.RequestsPerMinute, TokensPerMinute: limits.TokensPerMinute, Supply: limits.Supply}
}

func optional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func (c Configuration) WithNetworkCredential(credentialID string) Configuration {
	if c.Options.Network == nil {
		return c
	}
	network := *c.Options.Network
	network.CredentialID = credentialID
	configured := c
	configured.Options.Network = &network
	return configured
}

func (c Configuration) connector(plugin *connectors.PluginProfile) connectors.Config {
	return c.Identity.Connector(providerconfig.Options{
		Network: c.Options.Network, SemanticHeaders: c.Options.SemanticHeaders, QuerySettings: c.Options.QuerySettings,
		OperationDefaults: c.Options.OperationDefaults, Bindings: c.Options.Bindings, PluginOptions: c.Options.PluginOptions,
		TuningDatasetURIPrefixes: c.Options.TuningDatasetURIPrefixes, DocumentURIPrefixes: c.Options.DocumentURIPrefixes,
		VendorID: providerconfig.Value(c.Options.VendorID), CredentialHeaders: c.Options.CredentialHeaders, Models: c.Options.Models,
	}, plugin, "", "")
}
