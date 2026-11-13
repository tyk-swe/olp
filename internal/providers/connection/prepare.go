package connection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/limits"
	pluginidentity "github.com/tyk-swe/olp/internal/plugins/identity"
	"github.com/tyk-swe/olp/internal/pricingpolicy"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/providerconfig"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/vendors"
)

const (
	maxOptionsBytes      = 1 << 20
	maxCredentialHeaders = 16
	maxModelEntries      = 2000
	maxModelNameLength   = 200
	maxParameterDefaults = 64
)

type PluginSource interface {
	Profile(ctx context.Context, digest, profileID string) (*connectors.PluginProfile, error)
}

type Policy struct {
	Egress  *egress.Policy
	Plugins PluginSource
}

type Prepared struct {
	document Configuration
	plugin   *connectors.PluginProfile
}

func Prepare(ctx context.Context, policy Policy, input Configuration) (Prepared, error) {
	document := input.Normalized()
	plugin, err := pin(ctx, policy.Plugins, document)
	if err != nil {
		return Prepared{}, err
	}
	validated, err := validate(policy.Egress, Prepared{document.addressedBy(plugin), plugin})
	if err != nil {
		return Prepared{}, err
	}
	encoded, err := json.Marshal(validated.document)
	if err != nil {
		return Prepared{}, err
	}
	stored, err := Decode(encoded)
	if err != nil {
		return Prepared{}, err
	}
	return Prepared{stored, plugin}, nil
}

func Load(raw, installed []byte) (Prepared, error) {
	document, err := Decode(raw)
	if err != nil {
		return Prepared{}, err
	}
	if document.Kind != connectors.KindPlugin || installed == nil {
		return Prepared{document: document}, nil
	}
	plugin, err := connectors.DecodePluginProfile(document.ProfileRevision, installed, document.ProfileID)
	if err != nil {
		return Prepared{}, err
	}
	return Prepared{document, plugin}, nil
}

// A published plugin connection is served with the plugin its revision pins.
func ValidateUnpinned(policy *egress.Policy, input Configuration) (Configuration, error) {
	if input.Kind == connectors.KindPlugin {
		return input.Normalized(), nil
	}
	prepared, err := Prepare(context.Background(), Policy{Egress: policy}, input)
	return prepared.Document(), err
}

func (p Prepared) Document() Configuration { return p.document }

func (p Prepared) Transport() connectors.Config { return p.document.connector(p.plugin) }

func (p Prepared) WithNetworkCredential(credentialID string) Prepared {
	return Prepared{p.document.WithNetworkCredential(credentialID), p.plugin}
}

func (p Prepared) MarshalJSON() ([]byte, error) { return json.Marshal(p.document) }

func pin(ctx context.Context, plugins PluginSource, c Configuration) (*connectors.PluginProfile, error) {
	if c.Kind != connectors.KindPlugin {
		return nil, nil
	}
	if c.ProfileID == "" || !pluginidentity.ValidDigest(c.ProfileRevision) {
		return nil, invalid("configuration.profile_revision", "Choose a plugin profile: its ID, and the plugin's digest as the profile revision.")
	}
	return plugins.Profile(ctx, c.ProfileRevision, c.ProfileID)
}

func (c Configuration) addressedBy(plugin *connectors.PluginProfile) Configuration {
	if plugin == nil {
		return c
	}
	addressed := c
	addressed.Endpoint = new(plugin.Address(c.Options.PluginOptions))
	return addressed
}

type rule func(Prepared) error

func validate(policy *egress.Policy, p Prepared) (Prepared, error) {
	if err := firstViolation(p, credentialSource, optionsFit, canonicalNetworkCredential, knownKind, pinnedPlugin, pluginOptions, offeredAuthMode, endpointGiven); err != nil {
		return Prepared{}, err
	}
	scoped, err := withStorageScopes(p)
	if err != nil {
		return Prepared{}, err
	}
	if err := firstViolation(scoped, allowedEndpoint(policy), credentialHeaders, servableTransport(policy), servingVendor, modelMetadata, parameterDefaults); err != nil {
		return Prepared{}, err
	}
	committed, err := withNormalCommitment(scoped)
	if err != nil {
		return Prepared{}, err
	}
	if err := firstViolation(committed, healthProbe, quota); err != nil {
		return Prepared{}, err
	}
	return committed, nil
}

func firstViolation(p Prepared, rules ...rule) error {
	for _, check := range rules {
		if err := check(p); err != nil {
			return err
		}
	}
	return nil
}

func invalid(field, message string) error {
	return &routing.ValidationError{Field: field, Message: message}
}

func credentialSource(p Prepared) error {
	if err := connectors.ValidateCredentialSource(p.document.CredentialSource, p.document.Kind, p.document.AuthMode); err != nil {
		return invalid("configuration.credential_source", err.Error())
	}
	return nil
}

func optionsFit(p Prepared) error {
	options, err := json.Marshal(p.document.Options)
	if err != nil || len(options) > maxOptionsBytes {
		return invalid("configuration.options", fmt.Sprintf("Connection options must fit within %d MiB", maxOptionsBytes>>20))
	}
	return nil
}

func canonicalNetworkCredential(p Prepared) error {
	network := p.document.Options.Network
	if network == nil || network.CredentialID == "" {
		return nil
	}
	if id, err := uuid.Parse(network.CredentialID); err != nil || id.String() != network.CredentialID {
		return invalid("configuration.options.network.credential_id", "Use a canonical network credential UUID.")
	}
	return nil
}

func knownKind(p Prepared) error {
	if _, ok := LookupKind(p.document.Kind); !ok {
		return &routing.Refusal{Code: "provider_kind_unavailable", Message: "Unknown provider connector kind."}
	}
	return nil
}

func pinnedPlugin(p Prepared) error {
	if p.document.Kind == connectors.KindPlugin && p.plugin == nil {
		return invalid("configuration.profile_revision", "Choose a profile of an installed, approved plugin, with the plugin's digest as the profile revision.")
	}
	return nil
}

func pluginOptions(p Prepared) error {
	if p.plugin == nil {
		if len(p.document.Options.PluginOptions) != 0 {
			return invalid("configuration.options.plugin_options", "Only plugin providers have plugin options.")
		}
		return nil
	}
	err := p.plugin.ValidateOptions(p.document.Options.PluginOptions)
	if refusal, ok := errors.AsType[*connectors.OptionError](err); ok {
		return invalid("configuration.options.plugin_options."+refusal.Option, refusal.Message)
	}
	return err
}

func offeredAuthMode(p Prepared) error {
	if kind, _ := LookupKind(p.document.Kind); !kind.Offers(p.document.AuthMode) {
		return invalid("configuration.auth_mode", "This authentication mode is not available for the selected kind.")
	}
	return nil
}

func endpointGiven(p Prepared) error {
	if p.document.Endpoint == nil || *p.document.Endpoint == "" {
		return invalid("configuration.endpoint", "Provide the base URL of the API.")
	}
	return nil
}

func withStorageScopes(p Prepared) (Prepared, error) {
	c := p.document
	batchStorage, err := connectors.NormalizeBatchStorage(c.Kind, c.ProfileID, c.BatchStorage)
	if err != nil {
		return Prepared{}, invalid("configuration.batch_storage", err.Error())
	}
	tuningPrefixes, err := connectors.NormalizeTuningDatasetPrefixes(c.Kind, c.Options.TuningDatasetURIPrefixes)
	if err != nil {
		return Prepared{}, invalid("configuration.options.tuning_dataset_uri_prefixes", err.Error())
	}
	documentPrefixes, err := connectors.NormalizeDocumentURIPrefixes(c.Kind, c.Options.DocumentURIPrefixes)
	if err != nil {
		return Prepared{}, invalid("configuration.options.document_uri_prefixes", err.Error())
	}
	scoped := c
	scoped.BatchStorage = batchStorage
	scoped.Options.TuningDatasetURIPrefixes = tuningPrefixes
	scoped.Options.DocumentURIPrefixes = documentPrefixes
	return Prepared{scoped, p.plugin}, nil
}

func allowedEndpoint(policy *egress.Policy) rule {
	return func(p Prepared) error {
		if _, err := policy.ValidateEndpoint(*p.document.Endpoint); err != nil {
			return invalid("configuration.endpoint", err.Error())
		}
		return nil
	}
}

func credentialHeaders(p Prepared) error {
	headers := p.document.Options.CredentialHeaders
	headersMode := p.document.AuthMode == connectors.AuthHeaders
	switch {
	case len(headers) > maxCredentialHeaders:
		return invalid("configuration.options.credential_headers", fmt.Sprintf("Use at most %d credential headers.", maxCredentialHeaders))
	case slices.ContainsFunc(headers, func(h string) bool { return !connectors.ConfigurableHeader(h) }):
		return invalid("configuration.options.credential_headers", "Use valid header names other than hop-by-hop or framing headers.")
	case headersMode && len(headers) == 0:
		return invalid("configuration.options.credential_headers", "Name at least one header that carries the credential.")
	case !headersMode && len(headers) != 0:
		return invalid("configuration.options.credential_headers", "Credential headers apply only to the headers authentication mode.")
	}
	return nil
}

func servableTransport(policy *egress.Policy) rule {
	return func(p Prepared) error {
		if err := p.Transport().Validate(policy); err != nil {
			return invalid("configuration", err.Error())
		}
		return nil
	}
}

func servingVendor(p Prepared) error {
	if p.document.Kind == connectors.KindPlugin {
		if !vendorMissing(p.document.Options.VendorID) {
			return invalid("configuration.options.vendor_id", "A plugin provider has no vendor.")
		}
		return nil
	}
	if kind, ok := vendors.Kind(providerconfig.Value(p.document.Options.VendorID)); !ok || connectors.Kind(kind) != p.document.Kind {
		return invalid("configuration.options.vendor_id", "Vendor does not support this connector")
	}
	return nil
}

func modelMetadata(p Prepared) error {
	models := p.document.Options.Models
	if len(models) > maxModelEntries {
		return invalid("configuration.options.models", fmt.Sprintf("Use at most %d model metadata entries.", maxModelEntries))
	}
	for name, raw := range models {
		if len(name) == 0 || len(name) > maxModelNameLength {
			return invalid("configuration.options.models", fmt.Sprintf("Model names must be 1–%d characters.", maxModelNameLength))
		}
		if _, err := metadataObject(raw); err != nil {
			return invalid("configuration.options.models", "Model metadata must be an object.")
		}
		if err := ValidateMetadata(raw); err != nil {
			return invalid("configuration.options.models", err.Error())
		}
	}
	return nil
}

func parameterDefaults(p Prepared) error {
	defaults := p.document.Options.ParameterDefaults
	if len(defaults) > maxParameterDefaults {
		return invalid("configuration.options.parameter_defaults", fmt.Sprintf("Use at most %d parameter defaults.", maxParameterDefaults))
	}
	if p.document.ProfileID != "" && len(defaults) != 0 {
		return invalid("configuration.options.parameter_defaults", "Explicit profiles use operation_defaults; parameter_defaults belong to Automatic providers.")
	}
	if err := openai.ValidateDefaults(defaults); err != nil {
		return invalid("configuration.options.parameter_defaults", err.Error())
	}
	return nil
}

func withNormalCommitment(p Prepared) (Prepared, error) {
	commitment := p.document.Options.Commitment
	if commitment == nil {
		return p, nil
	}
	amount, valid := pricingpolicy.NormalizeAmount(commitment.Amount)
	if !valid || strings.Trim(amount, "0.") == "" {
		return Prepared{}, invalid("configuration.options.commitment.amount",
			"A commitment amount is a positive decimal with at most 12 integer and 12 fractional digits.")
	}
	if !routing.ValidCommitmentPeriod(commitment.Period) {
		return Prepared{}, invalid("configuration.options.commitment.period", `A commitment period is "hour", "day" or "month".`)
	}
	committed := p.document
	committed.Options.Commitment = &routing.Commitment{ID: commitment.ID, EffectiveAt: commitment.EffectiveAt, EndsAt: commitment.EndsAt, Amount: amount, Period: commitment.Period}
	return Prepared{committed, p.plugin}, nil
}

func healthProbe(p Prepared) error {
	if p.document.Options.HealthProbe == nil {
		return nil
	}
	return p.document.Options.HealthProbe.Validate("configuration.options.health_probe")
}

func quota(p Prepared) error {
	if p.document.Options.Limits == nil {
		return nil
	}
	return ValidLimits("configuration.options.limits", *p.document.Options.Limits)
}

func ValidLimits(field string, q Limits) error {
	if !validQuota(q) {
		return invalid(field, fmt.Sprintf("Use positive limits: requests and concurrency at most %d, tokens at most %d.", math.MaxInt32, limits.MaxCounter))
	}
	if err := q.Supply.Validate(field); err != nil {
		return err
	}
	if q.Shared() && q.RequestsPerMinute == nil && q.TokensPerMinute == nil && q.MaxConcurrency == nil {
		return invalid(field+".priority_shares", "Priority shares divide a quota; declare a request, token or concurrency limit.")
	}
	return nil
}

func validQuota(q Limits) bool {
	return limits.ValidRequestLimit(q.RequestsPerMinute) && limits.ValidRequestLimit(q.MaxConcurrency) && limits.ValidTokenLimit(q.TokensPerMinute)
}

func ValidModelName(field, value string) error {
	if len(value) == 0 || len(value) > maxModelNameLength {
		return invalid(field, fmt.Sprintf("Use 1–%d characters.", maxModelNameLength))
	}
	if strings.ContainsFunc(value, unicode.IsControl) {
		return invalid(field, "Control characters are not allowed.")
	}
	return nil
}
