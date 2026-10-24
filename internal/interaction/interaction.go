// Package interaction compiles and binds strict operation contracts. It owns
// semantic admission; live authority, dispatch, retry and accounting remain with
// their existing owners. No transformed codec establishes a strict plan.
package interaction

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/oif"
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
	Headers            http.Header
	Query              url.Values
	AllowProviderState bool
	RequiredServing    *oif.ServingIdentity
	// ResolvedResources contains only live gateway-verified dialect positions.
	ResolvedResources   map[string]string
	ContinuationVersion string
	DurableContinuation bool
	RetainedResponses   bool
	Continuation        *Continuation
}

func incompatible(code, field, requirement, message string) *oif.Incompatibility {
	return &oif.Incompatibility{Code: code, Field: field, Requirement: requirement, Message: message}
}

type Template struct {
	config   Config
	profile  connectors.Profile
	wire     wire.Format
	policy   *contentpolicy.Compiled
	defaults map[string]json.RawMessage
	origins  []connectors.DefaultProvenance
	serving  oif.ServingIdentity
}
type Plan struct {
	template         *Template
	config           connectors.Config
	prepared         oif.Prepared
	effective        oif.Document
	effectiveRequest *wire.Request
	sourceFamily     wire.Format
	stream           bool
	route            string
	receipt          oif.Receipt
}

// Compile snapshots all caller-owned configuration. A template is immutable and
// can be shared by the bounded published runtime across requests.
func Compile(config Config) (*Template, error) {
	provider := config.Provider.Clone()
	if provider.ProfileID == "" {
		return nil, incompatible("target_capability", "/profile", "explicit_profile", "Strict execution requires an explicit versioned provider profile.")
	}
	if err := provider.ValidateProfile(); err != nil {
		return nil, incompatible("target_capability", "/profile", "profile_configuration", "The provider profile composition or defaults are incompatible.")
	}
	if !provider.ValidModel(provider.Model(config.Model)) {
		return nil, incompatible("resource_affinity", "/model", "serving_binding", "The configured serving model is invalid.")
	}
	profile, _ := provider.Profile()
	if !profile.Strict {
		return nil, incompatible("target_capability", "/profile", "strict_profile", "This provider profile changes more than authorization, address and headers, or its plugin carries its traffic.")
	}
	if !slices.Contains(profile.Operations, "generation") {
		return nil, incompatible("target_capability", "/operation", "operation_contract", "This profile has no strict generation contract.")
	}
	encodedWire, err := provider.TargetFamily(wire.Chat)
	if err != nil {
		return nil, incompatible("target_capability", "/profile", "generation_dialect", "The profile has no implemented generation dialect.")
	}
	policy := config.Policy.Clone()
	if contentpolicy.Validate(policy) != nil {
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
	serving := provider.ServingIdentity(config.ProviderID, config.RevisionID, config.Model)
	return &Template{config: config, profile: profile, wire: encodedWire, policy: compiled, defaults: defaults, origins: origins, serving: serving}, nil
}
func (p *Plan) Prepared() oif.Prepared    { return p.prepared }
func (p *Plan) Body() []byte              { return p.prepared.Document().Bytes() }
func (p *Plan) Wire() wire.Format         { return p.template.wire }
func (p *Plan) Config() connectors.Config { return p.config.Clone() }
func (p *Plan) EffectiveRequest() *wire.Request {
	return p.effectiveRequest
}
func (p *Plan) Receipt() oif.Receipt {
	out := p.receipt
	out.Dispositions = slices.Clone(out.Dispositions)
	out.Evidence = slices.Clone(out.Evidence)
	out.Obligations.Effects = slices.Clone(out.Obligations.Effects)
	return out
}
func (p *Plan) Serving() oif.ServingIdentity { return p.receipt.Serving }
func (p *Plan) Obligations() oif.Obligations {
	out := p.receipt.Obligations
	out.Effects = slices.Clone(out.Effects)
	return out
}
