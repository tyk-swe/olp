// Package interaction compiles and binds strict operation contracts. It owns
// semantic admission; live authority, dispatch, retry and accounting remain with
// their existing owners. No legacy encoder establishes a strict plan.
package interaction

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"slices"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

const (
	NativeIdentity       = "native_identity"
	QualifiedInteraction = "qualified_interaction"
	evidenceNative       = "codec-fixture/native-source-conservation/v1"
	evidenceText         = "codec-fixture/chat-anthropic-stateless-text/v1"
)

type Config struct {
	Provider                      connectors.Config
	ProviderID, RevisionID, Model string
	Policy                        *contentpolicy.Policy
	MaxBodyBytes, MaxEventBytes   int
}
type Context struct {
	Headers             http.Header
	Query               url.Values
	AllowProviderState  bool
	RequiredServing     *ServingIdentity
	ContinuationVersion string
	DurableContinuation bool
	RetainedResponses   bool
	Continuation        *Continuation
	// ResolvedAssets binds caller-referenced local resource IDs to verified
	// provider objects. Every entry is produced by the existing resource
	// authority before binding; request bodies can only name map keys and can
	// never assert ownership, native identity, or serving scope themselves.
	ResolvedAssets map[string]AssetBinding
}
type ServingIdentity = oif.ServingIdentity
type Error = oif.Incompatibility

// AssetBinding is one verified provider-resource binding supplied by the
// resource authority. A binding carries the authority's own record: the
// owner-scoped local ID, the provider-native identity, the admitted upload
// purpose, the serving identity established at commit, and the byte identity
// of the accepted upload.
type AssetBinding struct {
	LocalID   string
	Kind      string
	NativeID  string
	Purpose   string
	Serving   ServingIdentity
	Digest    string
	MediaType string
	Size      int64
}

func incompatible(code, field, requirement, message string) *Error {
	return &Error{Code: code, Field: field, Requirement: requirement, Message: message}
}

type Disposition = oif.Disposition
type Obligations = oif.Obligations
type Receipt = oif.Receipt

type Template struct {
	config   Config
	profile  connectors.Profile
	wire     openai.Family
	policy   *contentpolicy.Compiled
	defaults map[string]json.RawMessage
	origins  []connectors.DefaultProvenance
	serving  ServingIdentity
}
type Plan struct {
	template     *Template
	config       connectors.Config
	prepared     oif.Prepared
	effective    oif.Document
	sourceFamily openai.Family
	stream       bool
	route        string
	receipt      Receipt
	assets       []AssetBinding
}

// Compile snapshots all caller-owned configuration. A template is immutable and
// can be shared by the bounded published runtime across requests.
func Compile(config Config) (*Template, error) {
	provider, err := copyConfig(config.Provider)
	if err != nil {
		return nil, incompatible("target_capability", "/profile", "profile_configuration", "The provider configuration cannot form a strict template.")
	}
	if provider.ProfileID == "" {
		return nil, incompatible("target_capability", "/profile", "explicit_profile", "Strict execution requires an explicit versioned provider profile.")
	}
	if err := provider.ValidateProfile(); err != nil {
		return nil, incompatible("target_capability", "/profile", "profile_configuration", "The provider profile composition or defaults are incompatible.")
	}
	if !connectors.ModelValid(provider.Kind, provider.Model(config.Model)) {
		return nil, incompatible("resource_affinity", "/model", "serving_binding", "The configured serving model is invalid.")
	}
	profile, _ := provider.Profile()
	if !slices.Contains(profile.Operations, "generation") {
		return nil, incompatible("target_capability", "/operation", "operation_contract", "This profile has no strict generation contract.")
	}
	wire, err := provider.TargetFamily(openai.FamilyChat)
	if err != nil {
		return nil, incompatible("target_capability", "/profile", "generation_dialect", "The profile has no implemented generation dialect.")
	}
	policy, err := copyPolicy(config.Policy)
	if err != nil || contentpolicy.Validate(policy) != nil {
		return nil, incompatible("policy_conflict", "/content_policy", "valid_policy", "The content policy is invalid.")
	}
	if policy != nil {
		for _, rule := range policy.Rules {
			if rule.Action != contentpolicy.ActionBlock {
				return nil, incompatible("policy_conflict", "/content_policy", "semantic_preservation", "Strict execution cannot apply semantic mutation policies.")
			}
		}
	}
	compiled, err := contentpolicy.Compile(policy)
	if err != nil {
		return nil, incompatible("policy_conflict", "/content_policy", "valid_policy", "The content policy cannot be compiled.")
	}
	defaults, origins, err := provider.DefaultsFor("generation", config.Model)
	if err != nil {
		return nil, incompatible("target_capability", "/defaults", "default_collision", "Configured native defaults collide or lack an operation schema.")
	}
	if config.MaxBodyBytes == 0 {
		config.MaxBodyBytes = 64 << 20
	}
	if config.MaxEventBytes == 0 {
		config.MaxEventBytes = 8 << 20
	}
	if config.MaxBodyBytes < 1 || config.MaxBodyBytes > 64<<20 || config.MaxEventBytes < 1 || config.MaxEventBytes > 64<<20 {
		return nil, incompatible("target_capability", "/limits", "bounded_buffering", "Strict body and event limits must be positive and bounded.")
	}
	config.Provider, config.Policy = provider, policy
	binding := provider.Bindings[config.Model]
	serving := ServingIdentity{ProviderID: config.ProviderID, RevisionID: config.RevisionID, Model: provider.Model(config.Model), ProfileID: provider.ProfileID, ProfileRevision: provider.ProfileRevision, PrincipalID: binding.PrincipalID, Snapshot: binding.Snapshot, Region: provider.CloudRegion, ResourceScope: binding.ResourceScope}
	if binding.Region != "" {
		serving.Region = binding.Region
	}
	return &Template{config: config, profile: profile, wire: wire, policy: compiled, defaults: defaults, origins: origins, serving: serving}, nil
}

// Serving is the strict serving identity this template binds plans to. The
// resource authority records it when a managed provider asset is committed so
// later generation plans can require serving affinity without re-resolving
// configuration.
func (t *Template) Serving() ServingIdentity { return t.serving }
func copyConfig(config connectors.Config) (connectors.Config, error) {
	out := config
	out.SemanticHeaders = maps.Clone(config.SemanticHeaders)
	out.QuerySettings = maps.Clone(config.QuerySettings)
	out.CredentialHeaders = slices.Clone(config.CredentialHeaders)
	out.Models = copyValues(config.Models)
	out.OperationDefaults = copyDefaults(config.OperationDefaults)
	if config.Bindings != nil {
		out.Bindings = make(map[string]connectors.Binding, len(config.Bindings))
		for name, binding := range config.Bindings {
			binding.Defaults = copyDefaults(binding.Defaults)
			out.Bindings[name] = binding
		}
	}
	if config.Network != nil {
		network := *config.Network
		network.ConnectTimeoutMS = copyPointer(network.ConnectTimeoutMS)
		network.TLSHandshakeTimeoutMS = copyPointer(network.TLSHandshakeTimeoutMS)
		network.ResponseHeaderTimeoutMS = copyPointer(network.ResponseHeaderTimeoutMS)
		network.IdleConnTimeoutMS = copyPointer(network.IdleConnTimeoutMS)
		network.MaxIdleConns = copyPointer(network.MaxIdleConns)
		network.MaxIdleConnsPerHost = copyPointer(network.MaxIdleConnsPerHost)
		network.MaxConnsPerHost = copyPointer(network.MaxConnsPerHost)
		out.Network = &network
	}
	return out, nil
}
func copyValues(values map[string]json.RawMessage) map[string]json.RawMessage {
	if values == nil {
		return nil
	}
	out := make(map[string]json.RawMessage, len(values))
	for name, value := range values {
		out[name] = bytes.Clone(value)
	}
	return out
}
func copyDefaults(values map[string]connectors.DefaultSet) map[string]connectors.DefaultSet {
	if values == nil {
		return nil
	}
	out := make(map[string]connectors.DefaultSet, len(values))
	for name, value := range values {
		value.Values = copyValues(value.Values)
		value.NativeOptions = copyValues(value.NativeOptions)
		out[name] = value
	}
	return out
}
func copyPointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}

func copyPolicy(policy *contentpolicy.Policy) (*contentpolicy.Policy, error) {
	if policy == nil {
		return nil, nil
	}
	raw, err := json.Marshal(policy)
	if err != nil {
		return nil, err
	}
	return contentpolicy.Decode(raw)
}
func (p *Plan) Prepared() oif.Prepared    { return p.prepared }
func (p *Plan) Body() []byte              { return p.prepared.Document().Bytes() }
func (p *Plan) Wire() openai.Family       { return p.template.wire }
func (p *Plan) Config() connectors.Config { out, _ := copyConfig(p.config); return out }
func (p *Plan) EffectiveRequest() *openai.Request {
	return openai.NewSourceEnvelope(p.Wire(), p.route, p.stream, p.effective)
}
func (p *Plan) Receipt() Receipt {
	out := p.receipt
	out.Dispositions = slices.Clone(out.Dispositions)
	out.Evidence = slices.Clone(out.Evidence)
	out.Obligations.Effects = slices.Clone(out.Obligations.Effects)
	return out
}
func (p *Plan) Serving() ServingIdentity { return p.receipt.Serving }

// Assets returns the resource-authority bindings this plan admitted at
// dialect-owned file positions. The caller's IDs never reached the provider;
// each entry records the verified local identity bound to a native ID.
func (p *Plan) Assets() []AssetBinding { return slices.Clone(p.assets) }
func (p *Plan) Obligations() Obligations {
	out := p.receipt.Obligations
	out.Effects = slices.Clone(out.Effects)
	return out
}
