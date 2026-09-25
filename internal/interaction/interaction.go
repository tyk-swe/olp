// Package interaction compiles and binds strict operation contracts. It owns
// semantic admission; live authority, dispatch, retry and accounting remain with
// their existing owners. No legacy encoder establishes a strict plan.
//
// Generation dialects are resolved through the operation-owned registry: the
// planner never switches on provider families. A dialect registration owns its
// grammar, admission, delivery and effects contracts; a mapping qualifies one
// actual source/target pair.
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
	"github.com/tyk-swe/olp/internal/operations/generation"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

const (
	NativeIdentity       = "native_identity"
	QualifiedInteraction = "qualified_interaction"
	evidenceNative       = "codec-fixture/native-source-conservation/v1"
)

// ContinuationV1 names the negotiated chat→anthropic tool contract for
// compatibility with stored resources and tests.
const ContinuationV1 = protocols.ChatAnthropicToolsV1

type Config struct {
	Provider                      connectors.Config
	ProviderID, RevisionID, Model string
	Policy                        *contentpolicy.Policy
	MaxBodyBytes, MaxEventBytes   int
	// Registry resolves generation dialect identities and qualified mappings;
	// nil uses the built-in registrations. Runtime configuration supplies
	// registered values only — no caller may install codecs here.
	Registry *generation.Registry
}
type Context struct {
	Headers             http.Header
	Query               url.Values
	AllowProviderState  bool
	AllowHostedTools    bool
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

// Continuation and Delivery alias the operation-owned contract types so stored
// resources and recovery endpoints share one wire shape.
type Continuation = generation.Continuation
type Delivery = generation.Delivery

// NativeTerminal and ContinuationActions alias the carrier's committed
// terminal observation and explicit actionability claim records.
type NativeTerminal = generation.NativeTerminal
type ContinuationActions = generation.ContinuationActions

// Accounting aliases the projection's separated resource counters.
type Accounting = generation.Accounting

// ContinuationFrame wraps projected client JSON as one SSE data frame.
func ContinuationFrame(raw []byte) []byte { return generation.ContinuationFrame(raw) }

// ContinuationActionable marks a projected client frame carrying an actionable
// tool-call delta.
func ContinuationActionable(frame []byte) bool { return generation.ContinuationActionable(frame) }

// SameSource verifies an invocation for delivery replay with exact numeric
// lexemes and ordered arrays, allowing only insignificant JSON object order.
func SameSource(a, b []byte) bool { return generation.SameSource(a, b) }

type Template struct {
	config   Config
	profile  connectors.Profile
	target   generation.Dialect
	registry *generation.Registry
	policy   *contentpolicy.Compiled
	defaults map[string]json.RawMessage
	origins  []connectors.DefaultProvenance
	serving  ServingIdentity
}
type Plan struct {
	template  *Template
	config    connectors.Config
	prepared  oif.Prepared
	effective oif.Document
	source    generation.Source
	target    generation.Dialect
	mapping   *generation.Mapping
	stream    bool
	route     string
	receipt   Receipt
	// hosted records the provider-hosted tool families admitted at bind time.
	// Result, event and policy guards use it to bound what the provider may
	// emit and what the caller may observe.
	hosted []string
	// assets records the resource-authority bindings admitted at dialect-owned
	// file positions. The caller's local IDs never reached the provider.
	assets []AssetBinding
}

// Compile snapshots all caller-owned configuration. A template is immutable and
// can be shared by the bounded published runtime across requests.
func Compile(config Config) (*Template, error) {
	registry := config.Registry
	if registry == nil {
		registry = defaultRegistry()
	}
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
	target, ok := registry.DialectLabel(profile.OperationDialect("generation"))
	if !ok || target.Operation != generation.Contract() {
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
	return &Template{config: config, profile: profile, target: target, registry: registry, policy: compiled, defaults: defaults, origins: origins, serving: serving}, nil
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
func (p *Plan) Config() connectors.Config { out, _ := copyConfig(p.config); return out }

// Wire is the compatibility view of the target dialect's checked-in hosting
// path; strict dispatch treats it as addressing only, never semantics.
func (p *Plan) Wire() openai.Family { return openai.Family(p.target.Address.LegacyPath) }

// TargetDialect is the registered destination contract this plan serves.
func (p *Plan) TargetDialect() generation.Dialect { return p.target }

// Source is the admitted caller source the plan bound.
func (p *Plan) Source() generation.Source { return p.source }

// Stream reports the bound incremental delivery selection.
func (p *Plan) Stream() bool { return p.stream }

// Effective is the admitted effective native request document.
func (p *Plan) Effective() oif.Document { return p.effective }

// Estimate is the dialect-owned reservation shape of the effective request.
func (p *Plan) Estimate() generation.Estimate { return p.target.Estimate(p.effective) }

// OutputLimit is the largest named output bound the effective request allowed.
func (p *Plan) OutputLimit() *int64 { return p.target.Estimate(p.effective).Output }

// Parameters are the effective request's routing-relevant control names.
func (p *Plan) Parameters() []string { return p.target.Parameters(p.effective) }

// ClientContract names the negotiated client contract this plan serves; empty
// for native or stateless-qualified interactions.
func (p *Plan) ClientContract() string {
	if p.mapping == nil {
		return ""
	}
	return p.mapping.ClientContract
}

// EffectiveRequest is the legacy envelope view of the effective request for
// compatibility callers that still consume *openai.Request.
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

// Hosted returns the provider-hosted tool families this plan admitted at bind
// time, bounded by the caller's authorization and the bound profile's
// qualified lifecycle contracts. Admission is a planner fact: it records what
// provider-hosted work the contract covers, never an executed effect.
func (p *Plan) Hosted() []string { return slices.Clone(p.hosted) }

func (p *Plan) Obligations() Obligations {
	out := p.receipt.Obligations
	out.Effects = slices.Clone(out.Effects)
	return out
}
