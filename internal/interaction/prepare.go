package interaction

import (
	"bytes"
	"golang.org/x/net/http/httpguts"
	"maps"
	"net/http"
	"net/textproto"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations/generation"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// BindRequest is the compatibility edge for callers still holding a legacy
// parsed request; the registered dialect lifts it to the neutral source
// contract before admission.
func (t *Template) BindRequest(request *openai.Request, context Context) (*Plan, error) {
	if request == nil {
		return nil, incompatible("target_capability", "/operation", "operation_contract", "This template admits generation requests only.")
	}
	// The legacy adapter's own claimed identity must agree with the immutable
	// envelope it produced; a mutated or misreporting adapter never reaches a
	// plan.
	descriptor := request.OIF().Descriptor()
	expected := openai.Descriptor(request.Family, request.Stream)
	if descriptor.Operation != expected.Operation || descriptor.Dialect != expected.Dialect || descriptor.Execution.Delivery != expected.Execution.Delivery {
		return nil, incompatible("target_capability", "/request", "source_identity_consistency", "The request adapter disagrees with its immutable source contract.")
	}
	return t.Bind(protocols.SourceOf(request), context)
}

// Bind admits one caller generation source against this target's registered
// dialect contract and qualified mappings. The source descriptor selects the
// registered source dialect; the profile's operation dialect selects the
// registered target. No provider family switch participates in admission.
func (t *Template) Bind(source generation.Source, context Context) (*Plan, error) {
	if !source.Request.Document().Valid() || source.Request.Descriptor().Operation != generation.Contract() {
		return nil, incompatible("target_capability", "/operation", "operation_contract", "This template admits generation requests only.")
	}
	sourceDialect, ok := t.registry.Dialect(source.Request.Descriptor().Dialect)
	if !ok {
		return nil, incompatible("target_capability", "/dialect", "registered_dialect", "The source dialect has no registered generation contract.")
	}
	expected := generation.Descriptor(sourceDialect.Identity, source.Stream)
	descriptor := source.Request.Descriptor()
	if descriptor.Operation != expected.Operation || descriptor.Dialect != expected.Dialect || descriptor.Execution.Delivery != expected.Execution.Delivery {
		return nil, incompatible("target_capability", "/request", "source_identity_consistency", "The request adapter disagrees with its immutable source contract.")
	}
	if sourceDialect.ValidateSource != nil {
		if err := sourceDialect.ValidateSource(source); err != nil {
			return nil, err
		}
	}
	if source.Stream && !sourceDialect.Streaming {
		return nil, incompatible("target_capability", "/stream", "source_delivery", "The source dialect has no incremental delivery contract.")
	}
	if source.Request.Document().Len() > t.config.MaxBodyBytes {
		return nil, incompatible("target_capability", "/request", "bounded_buffering", "The request exceeds the compiled body limit.")
	}
	if context.RequiredServing != nil && *context.RequiredServing != t.serving {
		return nil, incompatible("resource_affinity", "/resources", "serving_affinity", "The resolved resource belongs to a different serving identity.")
	}
	target := t.target
	stream := source.Stream
	config, headerReceipt, err := t.bindSemantic(source, sourceDialect, target, context)
	if err != nil {
		return nil, err
	}
	native := descriptor.Dialect == target.Identity
	receipt := Receipt{Class: NativeIdentity, Operation: "generation", SourceDialect: descriptor.Dialect.ID, TargetDialect: target.Identity.ID, ProfileID: t.profile.ID, ProfileRevision: t.profile.Revision, Serving: t.serving, Evidence: []string{evidenceNative}}
	receipt.Obligations = Obligations{Delivery: "unary", Lifetime: "request", Submission: "immediate", Effects: []string{"inference"}, Continuation: "client_native_history", Retry: "same_serving_before_send_or_definitive_rejection", MaxBodyBytes: t.config.MaxBodyBytes, MaxEventBytes: t.config.MaxEventBytes, RejectAmbiguousFailover: true, GuardResults: true}
	if stream {
		receipt.Obligations.Delivery = "incremental"
	}
	var prepared oif.Prepared
	var mapping *generation.Mapping
	if native {
		prepared, err = t.prepareNative(source, sourceDialect, target, &receipt)
	} else {
		receipt.Class, receipt.Evidence = QualifiedInteraction, nil
		registered, ok := t.registry.Mapping(sourceDialect.Identity, target.Identity, context.ContinuationVersion)
		if !ok {
			return nil, qualifiedMappingError(context.ContinuationVersion)
		}
		mapping = &registered
		receipt.Evidence = []string{mapping.Evidence}
		if mapping.ClientContract == "" {
			receipt.Obligations.Continuation = "stateless_text_history"
		}
		lowered, err := mapping.Lower(generation.LowerInput{Source: source, Model: t.serving.Model, Defaults: t.defaults, Hosting: t.profile.Hosting, MaxBodyBytes: t.config.MaxBodyBytes, Continuation: context.Continuation, DurableContinuation: context.DurableContinuation, AllowProviderState: context.AllowProviderState})
		if err != nil {
			return nil, err
		}
		destination := generation.Descriptor(target.Identity, stream)
		prepared, err = oif.PrepareDestination(source.Request, destination, lowered.Document, oif.QualifiedMapping, orEvidence(lowered.Evidence))
		if err != nil {
			return nil, err
		}
		receipt.Dispositions = append(receipt.Dispositions, lowered.Dispositions...)
		mergeObligations(&receipt.Obligations, lowered.Obligations)
	}
	if err != nil {
		return nil, err
	}
	effective := prepared.Document()
	if target.Assets != nil {
		if err := target.Assets(effective); err != nil {
			return nil, err
		}
	}
	if target.Effects != nil {
		if err := target.Effects(effective, generation.StateInput{AllowProviderState: context.AllowProviderState, RetainedResponses: context.RetainedResponses, RequiredServing: context.RequiredServing}, &receipt.Obligations); err != nil {
			return nil, err
		}
	}
	if t.policy != nil && t.policy.HasOutput() && (stream || receipt.Obligations.Lifetime != "request" || slices.Contains(receipt.Obligations.Effects, "resource_read") || target.ResultCoverage == nil) {
		return nil, incompatible("policy_conflict", "/content_policy", "output_inspection", "The output policy requires a stateless buffered unary interaction.")
	}
	if t.policy != nil && t.policy.HasOutput() {
		if err := target.RequestCoverage(effective); err != nil {
			return nil, err
		}
	}
	if t.policy != nil && len(t.policy.Input) > 0 {
		if target.InputCoverage == nil {
			return nil, incompatible("policy_conflict", "/content_policy", "input_inspection", "The input policy cannot inspect this dialect's native controls.")
		}
		if err := target.InputCoverage(effective); err != nil {
			return nil, err
		}
	}
	// Hosting wrappers may only perform the reviewed address/model/API-revision
	// binding. Semantic preparation is already complete and its document retained.
	if t.profile.Hosting == "vertex-anthropic" || t.profile.Hosting == "bedrock-anthropic-invoke" {
		wrapped, err := config.WrapBodyDialect(effective.Bytes(), target.Identity)
		if err != nil {
			return nil, incompatible("target_capability", "/profile", "hosting_wrapper", "The hosting wrapper conflicts with native request requirements.")
		}
		if !bytes.Equal(wrapped, effective.Bytes()) {
			document, err := oif.ParseJSON(wrapped, oif.Limits{MaxBytes: t.config.MaxBodyBytes})
			if err != nil {
				return nil, incompatible("target_capability", "/request", "bounded_buffering", "The hosted invocation exceeds its compiled bounds.")
			}
			hosted, err := oif.PrepareDestination(source.Request, prepared.Descriptor(), document, oif.IdentityBinding, "qualified hosting model and native API revision binding")
			if err != nil {
				return nil, err
			}
			prepared = hosted.WithProvenance(prepared.Provenance()...)
			receipt.Dispositions = append(receipt.Dispositions, Disposition{Field: "/profile", Disposition: "introduced", Rule: "hosting_identity_binding", Evidence: evidenceNative})
		}
	}
	prepared = prepared.WithProfile(oif.Identity{ID: t.profile.ID, Revision: t.profile.Revision})
	receipt.Dispositions = append(receipt.Dispositions, headerReceipt...)
	receipt.Dispositions = compactDispositions(receipt.Dispositions)
	return &Plan{template: t, config: config, prepared: prepared, effective: effective, source: source, target: target, mapping: mapping, stream: stream, route: source.Route, receipt: receipt}, nil
}

// qualifiedMappingError preserves the admission failures callers observed when
// no qualified mapping serves the requested client contract.
func qualifiedMappingError(contract string) error {
	if contract != "" {
		return incompatible("state_carrier", "/client_contract", "qualified_continuation_version", "The selected client continuation contract cannot preserve this interaction.")
	}
	return incompatible("target_capability", "/profile", "qualified_mapping", "No qualified mapping exists for this source and target dialect pair.")
}

func orEvidence(evidence string) string {
	if evidence == "" {
		return "qualified generation mapping"
	}
	return evidence
}

// mergeObligations applies the obligation fields a qualified lowering declared.
// Present fields replace the defaults; boolean and list obligations accumulate.
func mergeObligations(base *Obligations, declared *Obligations) {
	if declared == nil {
		return
	}
	if declared.Delivery != "" {
		base.Delivery = declared.Delivery
	}
	if declared.Lifetime != "" {
		base.Lifetime = declared.Lifetime
	}
	if declared.Submission != "" {
		base.Submission = declared.Submission
	}
	if declared.Continuation != "" {
		base.Continuation = declared.Continuation
	}
	if declared.Retry != "" {
		base.Retry = declared.Retry
	}
	if declared.Actionability != "" {
		base.Actionability = declared.Actionability
	}
	if declared.MaxBodyBytes != 0 {
		base.MaxBodyBytes = declared.MaxBodyBytes
	}
	if declared.MaxEventBytes != 0 {
		base.MaxEventBytes = declared.MaxEventBytes
	}
	if declared.MaxContinuationBytes != 0 {
		base.MaxContinuationBytes = declared.MaxContinuationBytes
	}
	base.RejectAmbiguousFailover = base.RejectAmbiguousFailover || declared.RejectAmbiguousFailover
	base.GuardResults = base.GuardResults || declared.GuardResults
	base.Effects = append(base.Effects, declared.Effects...)
}

func (t *Template) prepareNative(source generation.Source, sourceDialect, target generation.Dialect, receipt *Receipt) (oif.Prepared, error) {
	for _, entry := range source.Request.Provenance() {
		if entry.Origin == oif.ExplicitTransform || entry.Origin == oif.LegacyMapping {
			return oif.Prepared{}, incompatible("policy_conflict", "/request", "semantic_preservation", "Strict execution cannot consume a semantically transformed source.")
		}
	}
	changes, err := target.IdentityChanges(source, t.serving.Model)
	if err != nil {
		return oif.Prepared{}, incompatible("resource_affinity", "/request", "authorized_identity_overlays", "The source contains changes outside its registered native identity contract.")
	}
	prepared, err := t.registry.PrepareIdentity(source.Request, source.Request.Descriptor(), changes)
	if err != nil {
		return oif.Prepared{}, incompatible("resource_affinity", "/request", "authorized_identity_overlays", "The source contains changes outside its registered native identity contract.")
	}
	for _, member := range source.Request.Document().Root().Members() {
		disposition, rule := "preserved", "native_source_identity"
		if member.Name == "model" {
			disposition, rule = "bound", "published_model_binding"
		}
		if member.Name == "stream_options" && source.Stream && target.TransportOverlay == "stream_options" {
			disposition, rule = "transport_bound", "native_usage_observation"
		}
		receipt.Dispositions = append(receipt.Dispositions, Disposition{Field: safeField(member.Name), Disposition: disposition, Rule: rule, Evidence: evidenceNative})
	}
	for _, entry := range prepared.Provenance() {
		receipt.Dispositions = append(receipt.Dispositions, Disposition{Field: entry.Pointer, Disposition: "bound", Rule: string(entry.Origin), Evidence: evidenceNative})
	}
	overlay := []oif.Change{}
	for _, name := range slices.Sorted(maps.Keys(t.defaults)) {
		if _, present := source.Request.Document().Root().Lookup(name); present {
			continue
		}
		raw := t.defaults[name]
		origin := oif.ProviderDefault
		rule := "provider_default"
		for _, entry := range t.origins {
			if entry.Pointer == oif.Pointer("", name) {
				rule = entry.Source
				if strings.HasPrefix(rule, "binding_default") {
					origin = oif.ModelDefault
				}
				break
			}
		}
		overlay = append(overlay, oif.Change{Pointer: oif.Pointer("", name), Value: string(raw), Origin: origin, Reason: "declared absent-only native operation default"})
		receipt.Dispositions = append(receipt.Dispositions, Disposition{Field: safeField(name), Disposition: "introduced", Rule: rule, Evidence: evidenceNative})
	}
	if len(overlay) > 0 {
		document, err := oif.Apply(source.Request.Document(), overlay)
		if err != nil {
			return oif.Prepared{}, incompatible("target_capability", "/defaults", "native_default_overlay", "Native defaults exceed the source bounds or conflict with its structure.")
		}
		// Defaults are resolved against caller presence before transport usage
		// overlays, so an added stream_options object cannot hide a default. A
		// fresh source envelope keeps default provenance out of identity
		// admission; the defaults were already admitted as absent-only.
		defaulted, err := oif.NewRequest(source.Request.Descriptor(), document)
		if err != nil {
			return oif.Prepared{}, incompatible("target_capability", "/defaults", "native_default_overlay", "The declared default cannot satisfy native identity and transport obligations.")
		}
		boundChanges, err := target.IdentityChanges(generation.Source{Request: defaulted, Route: source.Route, Stream: source.Stream, IncludeUsage: source.IncludeUsage}, t.serving.Model)
		if err != nil {
			return oif.Prepared{}, incompatible("target_capability", "/defaults", "native_default_overlay", "The declared default cannot satisfy native identity and transport obligations.")
		}
		bound, err := t.registry.PrepareIdentity(defaulted, source.Request.Descriptor(), boundChanges)
		if err != nil {
			return oif.Prepared{}, incompatible("target_capability", "/defaults", "native_default_overlay", "The declared default cannot satisfy native identity and transport obligations.")
		}
		next, err := oif.PrepareDestination(source.Request, prepared.Descriptor(), bound.Document(), oif.ProviderDefault, "qualified native omission defaults; source members remain authoritative")
		if err != nil {
			return oif.Prepared{}, err
		}
		next = next.WithProvenance(bound.Provenance()...)
		for _, change := range overlay {
			next = next.WithProvenance(oif.Provenance{Pointer: change.Pointer, Origin: change.Origin, Reason: change.Reason})
		}
		prepared = next
	}
	if target.Admit != nil {
		if err := target.Admit(prepared.Document()); err != nil {
			return oif.Prepared{}, err
		}
	}
	// The bound stream mode is request-owned, including controls introduced by
	// a future/default extension; defaults may never select another execution path.
	if prepared.Document().Len() > t.config.MaxBodyBytes {
		return oif.Prepared{}, incompatible("target_capability", "/defaults", "bounded_buffering", "The effective request exceeds the compiled body limit.")
	}
	return prepared, nil
}

func (t *Template) bindSemantic(source generation.Source, sourceDialect, target generation.Dialect, context Context) (connectors.Config, []Disposition, error) {
	config := t.config.Provider
	config.SemanticHeaders = maps.Clone(config.SemanticHeaders)
	config.QuerySettings = maps.Clone(config.QuerySettings)
	if config.SemanticHeaders == nil {
		config.SemanticHeaders = map[string]string{}
	}
	if config.QuerySettings == nil {
		config.QuerySettings = map[string]string{}
	}
	receipts := []Disposition{}
	known := map[string]bool{"Anthropic-Version": true, "Anthropic-Beta": true, "Openai-Beta": true}
	native := source.Request.Descriptor().Dialect == target.Identity
	seen := map[string]bool{}
	for name, values := range context.Headers {
		name = textproto.CanonicalMIMEHeaderKey(name)
		if name == "Idempotency-Key" || name == "X-Idempotency-Key" {
			return connectors.Config{}, nil, incompatible("target_capability", "/headers", "upstream_idempotency", "The caller requested an upstream idempotency contract that this profile has not qualified.")
		}
		if slices.Contains([]string{"Openai-Organization", "Openai-Project", "X-Goog-User-Project", "X-Goog-Request-Params", "X-Ms-Region", "X-Ms-Routing-Name"}, name) {
			return connectors.Config{}, nil, incompatible("resource_affinity", "/headers", "serving_header", "Caller serving or tenant headers cannot override the published serving identity.")
		}
		if name == "Openai-Version" || name == "Api-Version" || strings.HasPrefix(name, "X-Amzn-Bedrock-") {
			return connectors.Config{}, nil, incompatible("target_capability", "/headers", "native_semantic_header", "A native semantic header has no admitted mapping in this profile.")
		}
		if !known[name] {
			continue
		}
		if seen[name] || len(values) != 1 || !native {
			return connectors.Config{}, nil, incompatible("target_capability", "/headers", "semantic_header", "A caller semantic header has no unambiguous mapping in the selected profile.")
		}
		if len(values[0]) > 2048 || !httpguts.ValidHeaderFieldValue(values[0]) {
			return connectors.Config{}, nil, incompatible("target_capability", "/headers", "semantic_configuration", "Caller semantic settings are malformed or outside the profile revision.")
		}
		seen[name] = true
		if handled, err := t.profile.BindIngressSemanticHeader(name, values[0]); handled {
			if err != nil {
				return connectors.Config{}, nil, incompatible("target_capability", "/headers/Anthropic-Version", "hosting_api_revision", "The caller API revision has no qualified binding to the selected hosting profile.")
			}
			receipts = append(receipts, Disposition{Field: "/headers/Anthropic-Version", Disposition: "mapped", Rule: "hosting_api_revision", Evidence: evidenceNative})
			continue
		}
		if !slices.Contains(t.profile.SemanticHeaders, name) || name == "Anthropic-Version" && values[0] != t.profile.DialectRevision {
			return connectors.Config{}, nil, incompatible("target_capability", "/headers", "semantic_header", "A caller semantic header has no unambiguous mapping in the selected profile.")
		}
		for configured, value := range config.SemanticHeaders {
			if strings.EqualFold(configured, name) {
				if value != values[0] {
					return connectors.Config{}, nil, incompatible("target_capability", "/headers", "semantic_header_conflict", "A caller semantic header conflicts with the published profile.")
				}
				delete(config.SemanticHeaders, configured)
			}
		}
		config.SemanticHeaders[name] = values[0]
		receipts = append(receipts, Disposition{Field: "/headers/" + name, Disposition: "preserved", Rule: "caller_semantic_header", Evidence: evidenceNative})
	}
	for name, values := range context.Query {
		// A dialect-declared delivery selector (for example Gemini's alt=sse)
		// chooses transport at ingress; it never becomes a configurable semantic
		// query override on the provider URL.
		if sourceDialect.DeliveryKey != "" && name == sourceDialect.DeliveryKey && source.Stream && len(values) == 1 && values[0] == sourceDialect.DeliveryValue {
			continue
		}
		if len(values) != 1 || !native || !slices.Contains(t.profile.QuerySettings, name) {
			return connectors.Config{}, nil, incompatible("target_capability", "/query", "semantic_query", "A caller query setting has no mapping in the selected profile.")
		}
		if len(values[0]) > 2048 || !httpguts.ValidHeaderFieldValue(values[0]) || name == "$xgafv" && values[0] != "1" && values[0] != "2" || name == "api-version" && values[0] != config.APIVersion {
			return connectors.Config{}, nil, incompatible("target_capability", "/query", "semantic_configuration", "Caller semantic settings are malformed or outside the profile revision.")
		}
		if value, present := config.QuerySettings[name]; present && value != values[0] {
			return connectors.Config{}, nil, incompatible("target_capability", "/query", "semantic_query_conflict", "A caller query setting conflicts with the published profile.")
		}
		config.QuerySettings[name] = values[0]
		receipts = append(receipts, Disposition{Field: "/query/" + name, Disposition: "preserved", Rule: "caller_semantic_query", Evidence: evidenceNative})
	}
	for name := range t.config.Provider.SemanticHeaders {
		if !seen[http.CanonicalHeaderKey(name)] {
			receipts = append(receipts, Disposition{Field: "/headers/" + http.CanonicalHeaderKey(name), Disposition: "introduced", Rule: "profile_semantic_header", Evidence: evidenceNative})
		}
	}
	for name := range t.config.Provider.QuerySettings {
		if _, present := context.Query[name]; !present {
			receipts = append(receipts, Disposition{Field: "/query/" + name, Disposition: "introduced", Rule: "profile_semantic_query", Evidence: evidenceNative})
		}
	}
	if t.profile.Hosting == "direct-anthropic" && !seen["Anthropic-Version"] {
		receipts = append(receipts, Disposition{Field: "/headers/Anthropic-Version", Disposition: "introduced", Rule: "profile_api_revision", Evidence: evidenceNative})
	}
	return config, receipts, nil
}

// Only schema-owned names are observable receipt fields. Arbitrary native
// extension names may themselves contain sensitive caller data.
func safeField(name string) string {
	return generation.SafeField(name)
}

func compactDispositions(input []Disposition) []Disposition {
	out := make([]Disposition, 0, min(len(input), 128))
	seen := map[Disposition]bool{}
	for _, entry := range input {
		if !seen[entry] {
			seen[entry] = true
			out = append(out, entry)
		}
	}
	return out
}
