// Package operationplan compiles strict unary contracts without a generation
// representation. Live authority, retries and accounting remain gateway-owned.
package operationplan

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/operations"
)

type Config struct {
	Provider                                 connectors.Config
	ProviderID, RevisionID, Model, Operation string
	Policy                                   *contentpolicy.Policy
	MaxBodyBytes                             int
}
type Context struct {
	Headers               http.Header
	Query                 url.Values
	Route, ClientContract string
}
type Template struct {
	config   Config
	profile  connectors.Profile
	codec    operations.Dialect
	defaults map[string]json.RawMessage
	origins  []connectors.DefaultProvenance
	policy   *contentpolicy.Compiled
	serving  oif.ServingIdentity
	inputs   map[string]bool
}
type Plan struct {
	template          *Template
	config            connectors.Config
	source, effective oif.Request
	view              oif.View
	prepared          oif.Prepared
	receipt           oif.Receipt
	mapping           *operations.Mapping
	route             string
	estimate          int64
}
type Result struct {
	Envelope  oif.Result
	View      oif.View
	Body      []byte
	Usage     *operations.Usage
	Decisions []contentpolicy.Decision
}

func fail(code, field, requirement, message string) error {
	return operations.Error(code, field, requirement, message)
}
func cloneConfig(c connectors.Config) connectors.Config {
	raw, _ := json.Marshal(c)
	var out connectors.Config
	_ = json.Unmarshal(raw, &out)
	return out
}
func Compile(config Config) (*Template, error) {
	config.Provider = cloneConfig(config.Provider)
	p, err := config.Provider.Profile()
	if err != nil || config.Provider.ValidateProfile() != nil {
		return nil, fail("target_capability", "/profile", "explicit_profile", "A registered versioned operation profile is required.")
	}
	codec, ok := operationregistry.Lookup(p.OperationDialect(config.Operation))
	if !ok || !slices.Contains(p.Operations, config.Operation) || codec.Operation.ID != config.Operation {
		return nil, fail("target_capability", "/operation", "operation_contract", "The profile has no registered unary operation contract.")
	}
	if !connectors.ModelValid(config.Provider.Kind, config.Provider.Model(config.Model)) {
		return nil, fail("resource_affinity", "/model", "serving_binding", "The configured serving model is invalid.")
	}
	defaults, origins, err := config.Provider.DefaultsFor(config.Operation, config.Model)
	if err != nil {
		return nil, fail("target_capability", "/defaults", "native_defaults", "The operation defaults are invalid.")
	}
	if contentpolicy.Validate(config.Policy) != nil {
		return nil, fail("policy_conflict", "/content_policy", "valid_policy", "The content policy is invalid.")
	}
	if config.Policy != nil {
		for _, rule := range config.Policy.Rules {
			if rule.Action != contentpolicy.ActionBlock {
				return nil, fail("policy_conflict", "/content_policy", "semantic_preservation", "Strict operation plans require non-mutating content policies.")
			}
		}
	}
	policy, err := contentpolicy.Compile(config.Policy)
	if err != nil {
		return nil, err
	}
	if config.MaxBodyBytes == 0 {
		config.MaxBodyBytes = 64 << 20
	}
	if config.MaxBodyBytes < 1 || config.MaxBodyBytes > 64<<20 {
		return nil, fail("target_capability", "/limits", "bounded_buffering", "The operation response limit is invalid.")
	}
	binding := config.Provider.Bindings[config.Model]
	serving := oif.ServingIdentity{ProviderID: config.ProviderID, RevisionID: config.RevisionID, Model: config.Provider.Model(config.Model), ProfileID: p.ID, ProfileRevision: p.Revision, PrincipalID: binding.PrincipalID, Snapshot: binding.Snapshot, Region: config.Provider.CloudRegion, ResourceScope: binding.ResourceScope}
	if binding.Region != "" {
		serving.Region = binding.Region
	}
	var schema struct {
		Required []string `json:"required"`
	}
	_ = json.Unmarshal(codec.RequestSchema, &schema)
	inputs := map[string]bool{"model": true}
	for _, name := range schema.Required {
		inputs[name] = true
	}
	return &Template{config: config, profile: p, codec: codec, defaults: defaults, origins: origins, policy: policy, serving: serving, inputs: inputs}, nil
}
func Parse(dialect string, body []byte, maxBytes int) (oif.Request, error) {
	d, ok := operationregistry.Lookup(dialect)
	if !ok {
		return oif.Request{}, fail("target_capability", "/dialect", "registered_dialect", "The native dialect is not registered.")
	}
	doc, err := oif.ParseJSON(body, oif.Limits{MaxBytes: maxBytes})
	if err != nil {
		return oif.Request{}, err
	}
	return oif.NewRequest(oif.Descriptor{Operation: d.Operation, Dialect: d.Identity, Execution: oif.Execution{Delivery: "unary", Lifetime: "request", Submission: "immediate", Effects: []string{"inference"}}}, doc)
}
func (t *Template) Bind(source oif.Request, ctx Context) (*Plan, error) {
	if ctx.ClientContract != "" && ctx.ClientContract != operations.RawVectorClient {
		return nil, fail("state_carrier", "/client_contract", "registered_client_contract", "The requested client contract is not registered.")
	}
	sourceCodec, ok := operationregistry.Lookup(source.Descriptor().Dialect.ID)
	if !ok || sourceCodec.Identity != source.Descriptor().Dialect || source.Descriptor().Operation != t.codec.Operation {
		return nil, fail("target_capability", "/operation", "matching_operation", "Source and target operation contracts differ.")
	}
	if len(source.Provenance()) != 0 {
		return nil, fail("policy_conflict", "/request", "source_identity", "Strict operations require the original caller source.")
	}
	if sourceCodec.ValidateRoute != nil {
		if err := sourceCodec.ValidateRoute(source, ctx.Route); err != nil {
			return nil, err
		}
	}
	sourceView, err := sourceCodec.Request(source)
	if err != nil {
		return nil, err
	}
	if model := operations.Member(source.Document().Root(), "model"); model.Kind() != oif.Absent {
		text, ok := model.Text()
		if !ok || strings.TrimPrefix(text, "models/") != strings.TrimPrefix(ctx.Route, "models/") {
			return nil, fail("resource_affinity", "/model", "route_identity", "The native model must agree with the selected route.")
		}
	}
	config, semantic, err := t.bindSemantic(ctx, sourceCodec.Identity == t.codec.Identity)
	if err != nil {
		return nil, err
	}
	class := "native_identity"
	document := source.Document()
	var mapping *operations.Mapping
	var provenance []oif.Provenance
	if sourceCodec.Identity != t.codec.Identity {
		m, ok := operationregistry.Default.Mapping(sourceCodec.Identity, t.codec.Identity)
		if !ok {
			return nil, fail("target_capability", "/dialect", "qualified_mapping", "No complete qualified mapping links these operation dialects.")
		}
		document, provenance, err = m.Lower(source, sourceView)
		if err != nil {
			return nil, err
		}
		mapping = &m
		class = "qualified_interaction"
	}
	descriptor := source.Descriptor()
	descriptor.Dialect = t.codec.Identity
	descriptor.Profile = oif.Identity{ID: t.profile.ID, Revision: t.profile.Revision}
	effective, _ := oif.NewRequest(descriptor, document)
	changes := []oif.Change{}
	dispositions := []oif.Disposition{{Field: "/request", Disposition: "preserved", Rule: "native_source_identity", Evidence: t.codec.Evidence}}
	for _, name := range slices.Sorted(maps.Keys(t.defaults)) {
		if _, present := document.Root().Lookup(name); present {
			continue
		}
		origin := oif.ProviderDefault
		rule := "provider_default"
		for _, p := range t.origins {
			if p.Pointer == operations.Pointer("", name) {
				rule = p.Source
				if strings.HasPrefix(rule, "binding_default") {
					origin = oif.ModelDefault
				}
				break
			}
		}
		changes = append(changes, oif.Change{Pointer: operations.Pointer("", name), Value: string(t.defaults[name]), Origin: origin, Reason: "declared absent-only operation default"})
		dispositions = append(dispositions, oif.Disposition{Field: operations.Pointer("", name), Disposition: "introduced", Rule: rule, Evidence: t.codec.Evidence})
	}
	effective, err = effective.WithChanges(changes...)
	if err != nil {
		return nil, err
	}
	// Model hooks return only identity changes. The codec owns nested model paths.
	if t.codec.BindModel != nil {
		bound, err := t.codec.BindModel(effective.Document(), t.serving.Model)
		if err != nil {
			return nil, err
		}
		for _, change := range bound {
			if change.Origin != oif.IdentityBinding || change.Remove {
				return nil, fail("target_capability", "/model", "identity_binding", "The registered model overlay is invalid.")
			}
		}
		effective, err = effective.WithChanges(bound...)
		if err != nil {
			return nil, err
		}
	}
	if mapping != nil && mapping.ValidateEffective != nil {
		if err := mapping.ValidateEffective(source, effective); err != nil {
			return nil, err
		}
	}
	view, err := t.codec.Request(effective)
	if err != nil {
		return nil, err
	}
	for _, pair := range []struct {
		codec operations.Dialect
		view  oif.View
	}{{sourceCodec, sourceView}, {t.codec, view}} {
		if pair.codec.RequiredClient != nil {
			required := pair.codec.RequiredClient(pair.view)
			if required != "" && ctx.ClientContract != required {
				return nil, fail("state_carrier", "/client_contract", "raw_vector_storage", "This native vector storage requires the explicit versioned raw-storage client contract.")
			}
		}
	}
	origin := oif.IdentityBinding
	reason := "registered native model binding and omission defaults"
	if mapping != nil {
		origin = oif.QualifiedMapping
		reason = mapping.Evidence
	}
	prepared, err := oif.PrepareDestination(source, descriptor, effective.Document(), origin, reason)
	if err != nil {
		return nil, err
	}
	prepared = prepared.WithProvenance(effective.Provenance()...).WithProvenance(provenance...)
	evidence := []string{t.codec.Evidence}
	if mapping != nil {
		evidence = append(evidence, mapping.Evidence)
	}
	receipt := oif.Receipt{Class: class, Operation: t.codec.Operation.ID, SourceDialect: sourceCodec.Identity.ID, TargetDialect: t.codec.Identity.ID, ProfileID: t.profile.ID, ProfileRevision: t.profile.Revision, Serving: t.serving, Dispositions: append(dispositions, semantic...), Evidence: evidence, Obligations: oif.Obligations{Delivery: "unary", Lifetime: "request", Submission: "immediate", Continuation: "none", Effects: []string{"inference"}, Retry: "before_dispatch_or_definitive_rejection", MaxBodyBytes: t.config.MaxBodyBytes, RejectAmbiguousFailover: true, GuardResults: true}}
	var estimate int64
	if sourceCodec.Estimate != nil {
		estimate = sourceCodec.Estimate(sourceView)
	}
	if t.codec.Estimate != nil {
		estimate = max(estimate, t.codec.Estimate(view))
	}
	return &Plan{template: t, config: config, source: source, effective: effective, view: view, prepared: prepared, receipt: receipt, mapping: mapping, route: ctx.Route, estimate: estimate}, nil
}
func (p *Plan) Prepared() oif.Prepared       { return p.prepared }
func (p *Plan) Body() []byte                 { return p.prepared.Document().Bytes() }
func (p *Plan) Config() connectors.Config    { return cloneConfig(p.config) }
func (p *Plan) Serving() oif.ServingIdentity { return p.receipt.Serving }
func (p *Plan) Estimate() int64              { return p.estimate }
func (p *Plan) Parameters() []string {
	out := []string{}
	for _, member := range p.effective.Document().Root().Members() {
		if !p.template.inputs[member.Name] {
			out = append(out, member.Name)
		}
	}
	return out
}
func (p *Plan) Receipt() oif.Receipt {
	out := p.receipt
	out.Dispositions = slices.Clone(out.Dispositions)
	out.Evidence = slices.Clone(out.Evidence)
	out.Obligations.Effects = slices.Clone(out.Obligations.Effects)
	return out
}
func (p *Plan) EffectiveRequest() oif.Request { return p.effective }
func (p *Plan) Endpoint() (string, error) {
	return p.config.OperationURL(p.template.codec, p.template.config.Model)
}
func (p *Plan) Decode(body []byte) (Result, error) {
	doc, err := oif.ParseJSON(body, oif.Limits{MaxBytes: p.template.config.MaxBodyBytes})
	if err != nil {
		return Result{}, operations.Violation("/result", "Native result is malformed or exceeds its declared bounds.")
	}
	envelope, _ := oif.NewResult(p.prepared.Descriptor(), doc, oif.Complete)
	view, err := p.template.codec.Result(p.effective, envelope)
	if err != nil {
		return Result{}, err
	}
	var usage *operations.Usage
	if p.template.codec.Usage != nil {
		usage = p.template.codec.Usage(view)
	}
	decisions, err := p.checkOutput(envelope)
	if err != nil {
		return Result{Envelope: envelope, View: view, Usage: usage, Decisions: decisions}, err
	}
	output := doc
	if p.mapping != nil {
		output, err = p.mapping.Project(p.source, envelope, view, p.route)
	} else if p.template.codec.BindResultModel != nil {
		var changes []oif.Change
		changes, err = p.template.codec.BindResultModel(doc, p.route)
		if err == nil {
			output, err = oif.Apply(doc, changes)
		}
	}
	if err != nil {
		return Result{Envelope: envelope, View: view, Usage: usage, Decisions: decisions}, err
	}
	return Result{Envelope: envelope, View: view, Body: output.Bytes(), Usage: usage, Decisions: decisions}, nil
}
