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
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func (t *Template) Bind(request *openai.Request, context Context) (*Plan, error) {
	if request == nil || !request.OIF().Document().Valid() || request.Family.Operation() != "generation" {
		return nil, incompatible("target_capability", "/operation", "operation_contract", "This template admits generation requests only.")
	}
	descriptor := request.OIF().Descriptor()
	expected := openai.Descriptor(request.Family, request.Stream)
	if descriptor.Operation != expected.Operation || descriptor.Dialect != expected.Dialect || descriptor.Execution.Delivery != expected.Execution.Delivery {
		return nil, incompatible("target_capability", "/request", "source_identity_consistency", "The request adapter disagrees with its immutable source contract.")
	}
	if request.Family == openai.FamilyChat || request.Family == openai.FamilyResponses || request.Family == openai.FamilyAnthropic {
		stream := member(request.OIF().Document().Root(), "stream")
		if (stream.Raw() == "true") != request.Stream {
			return nil, incompatible("target_capability", "/stream", "source_delivery_consistency", "The selected delivery mode disagrees with the native source.")
		}
	}
	if request.OIF().Document().Len() > t.config.MaxBodyBytes {
		return nil, incompatible("target_capability", "/request", "bounded_buffering", "The request exceeds the compiled body limit.")
	}
	if context.RequiredServing != nil && *context.RequiredServing != t.serving {
		return nil, incompatible("resource_affinity", "/resources", "serving_affinity", "The resolved resource belongs to a different serving identity.")
	}
	config, headerReceipt, err := t.bindSemantic(request, context)
	if err != nil {
		return nil, err
	}
	native := request.OIF().Descriptor().Dialect == openai.Descriptor(t.wire, request.Stream).Dialect
	receipt := Receipt{Class: NativeIdentity, Operation: "generation", SourceDialect: request.OIF().Descriptor().Dialect.ID, TargetDialect: openai.Descriptor(t.wire, request.Stream).Dialect.ID, ProfileID: t.profile.ID, ProfileRevision: t.profile.Revision, Serving: t.serving, Evidence: []string{evidenceNative}}
	receipt.Obligations = Obligations{Delivery: "unary", Lifetime: "request", Submission: "immediate", Effects: []string{"inference"}, Continuation: "client_native_history", Retry: "same_serving_before_send_or_definitive_rejection", MaxBodyBytes: t.config.MaxBodyBytes, MaxEventBytes: t.config.MaxEventBytes, RejectAmbiguousFailover: true, GuardResults: true}
	if request.Stream {
		receipt.Obligations.Delivery = "incremental"
	}
	var prepared oif.Prepared
	if native {
		prepared, err = t.prepareNative(request, &receipt)
	} else {
		receipt.Class, receipt.Evidence = QualifiedInteraction, []string{evidenceText}
		receipt.Obligations.Continuation = "stateless_text_history"
		prepared, err = t.prepareText(request, &receipt)
	}
	if err != nil {
		return nil, err
	}
	effective := prepared.Document()
	if err := checkAssetResources(effective, t.wire); err != nil {
		return nil, err
	}
	if err := checkState(effective, t.wire, context, &receipt.Obligations); err != nil {
		return nil, err
	}
	if t.policy != nil && t.policy.HasOutput() && (request.Stream || receipt.Obligations.Lifetime != "request" || slices.Contains(receipt.Obligations.Effects, "resource_read") || t.wire == openai.FamilyBedrock) {
		return nil, incompatible("policy_conflict", "/content_policy", "output_inspection", "The output policy requires a stateless buffered unary interaction.")
	}
	if t.policy != nil && t.policy.HasOutput() {
		if err := outputRequestCoverage(effective); err != nil {
			return nil, err
		}
	}
	if t.policy != nil && len(t.policy.Input) > 0 {
		if err := checkInputCoverage(t.wire, effective); err != nil {
			return nil, err
		}
	}
	// Hosting wrappers may only perform the reviewed address/model/API-revision
	// binding. Semantic preparation is already complete and its document retained.
	if t.profile.Hosting == "vertex-anthropic" || t.profile.Hosting == "bedrock-anthropic-invoke" {
		wrapped, err := config.WrapBody(effective.Bytes(), t.wire)
		if err != nil {
			return nil, incompatible("target_capability", "/profile", "hosting_wrapper", "The hosting wrapper conflicts with native request requirements.")
		}
		if !bytes.Equal(wrapped, effective.Bytes()) {
			document, err := oif.ParseJSON(wrapped, oif.Limits{MaxBytes: t.config.MaxBodyBytes})
			if err != nil {
				return nil, incompatible("target_capability", "/request", "bounded_buffering", "The hosted invocation exceeds its compiled bounds.")
			}
			hosted, err := oif.PrepareDestination(request.OIF(), prepared.Descriptor(), document, oif.IdentityBinding, "qualified hosting model and native API revision binding")
			if err != nil {
				return nil, err
			}
			prepared = hosted.WithProvenance(prepared.Provenance()...)
			receipt.Dispositions = append(receipt.Dispositions, Disposition{"/profile", "introduced", "hosting_identity_binding", evidenceNative})
		}
	}
	prepared = prepared.WithProfile(oif.Identity{ID: t.profile.ID, Revision: t.profile.Revision})
	receipt.Dispositions = append(receipt.Dispositions, headerReceipt...)
	receipt.Dispositions = compactDispositions(receipt.Dispositions)
	return &Plan{template: t, config: config, prepared: prepared, effective: effective, sourceFamily: request.Family, stream: request.Stream, route: request.Route, receipt: receipt}, nil
}

func (t *Template) prepareNative(request *openai.Request, receipt *Receipt) (oif.Prepared, error) {
	for _, entry := range request.OIF().Provenance() {
		if entry.Origin == oif.ExplicitTransform || entry.Origin == oif.LegacyMapping {
			return oif.Prepared{}, incompatible("policy_conflict", "/request", "semantic_preservation", "Strict execution cannot consume a semantically transformed source.")
		}
	}
	prepared, err := protocols.PrepareIdentity(request, t.serving.Model)
	if err != nil {
		return oif.Prepared{}, incompatible("resource_affinity", "/request", "authorized_identity_overlays", "The source contains changes outside its registered native identity contract.")
	}
	for _, member := range request.OIF().Document().Root().Members() {
		disposition, rule := "preserved", "native_source_identity"
		if member.Name == "model" {
			disposition, rule = "bound", "published_model_binding"
		}
		if member.Name == "stream_options" && request.Stream && request.Family == openai.FamilyChat {
			disposition, rule = "transport_bound", "native_usage_observation"
		}
		receipt.Dispositions = append(receipt.Dispositions, Disposition{safeField(member.Name), disposition, rule, evidenceNative})
	}
	for _, entry := range prepared.Provenance() {
		receipt.Dispositions = append(receipt.Dispositions, Disposition{entry.Pointer, "bound", string(entry.Origin), evidenceNative})
	}
	changes := []oif.Change{}
	for _, name := range slices.Sorted(maps.Keys(t.defaults)) {
		if _, present := request.OIF().Document().Root().Lookup(name); present {
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
		changes = append(changes, oif.Change{Pointer: oif.Pointer("", name), Value: string(raw), Origin: origin, Reason: "declared absent-only native operation default"})
		receipt.Dispositions = append(receipt.Dispositions, Disposition{safeField(name), "introduced", rule, evidenceNative})
	}
	if len(changes) > 0 {
		document, err := oif.Apply(request.OIF().Document(), changes)
		if err != nil {
			return oif.Prepared{}, incompatible("target_capability", "/defaults", "native_default_overlay", "Native defaults exceed the source bounds or conflict with its structure.")
		}
		// Defaults are resolved against caller presence before transport usage
		// overlays, so an added stream_options object cannot hide a default.
		native := openai.NewSourceEnvelope(request.Family, request.Route, request.Stream, document)
		bound, err := protocols.PrepareIdentity(native, t.serving.Model)
		if err != nil {
			return oif.Prepared{}, incompatible("target_capability", "/defaults", "native_default_overlay", "The declared default cannot satisfy native identity and transport obligations.")
		}
		next, err := oif.PrepareDestination(request.OIF(), prepared.Descriptor(), bound.Document(), oif.ProviderDefault, "qualified native omission defaults; source members remain authoritative")
		if err != nil {
			return oif.Prepared{}, err
		}
		next = next.WithProvenance(bound.Provenance()...)
		for _, change := range changes {
			next = next.WithProvenance(oif.Provenance{Pointer: change.Pointer, Origin: change.Origin, Reason: change.Reason})
		}
		prepared = next
	}
	if t.wire == openai.FamilyChat {
		_, legacyCap := prepared.Document().Root().Lookup("max_tokens")
		_, completionCap := prepared.Document().Root().Lookup("max_completion_tokens")
		if legacyCap && completionCap {
			return oif.Prepared{}, incompatible("reasoning_budget", "/max_completion_tokens", "conflicting_budget_scopes", "The effective request combines token controls with distinct scopes; no precedence or equivalence is qualified.")
		}
	}
	// The bound stream mode is request-owned, including controls introduced by
	// a future/default extension; defaults may never select another execution path.
	if prepared.Document().Len() > t.config.MaxBodyBytes {
		return oif.Prepared{}, incompatible("target_capability", "/defaults", "bounded_buffering", "The effective request exceeds the compiled body limit.")
	}
	return prepared, nil
}

func (t *Template) bindSemantic(request *openai.Request, context Context) (connectors.Config, []Disposition, error) {
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
	native := request.OIF().Descriptor().Dialect == openai.Descriptor(t.wire, request.Stream).Dialect
	seen := map[string]bool{}
	for name, values := range context.Headers {
		name = textproto.CanonicalMIMEHeaderKey(name)
		if slices.Contains([]string{"Openai-Organization", "Openai-Project", "X-Goog-User-Project", "X-Goog-Request-Params", "X-Ms-Region", "X-Ms-Routing-Name"}, name) {
			return connectors.Config{}, nil, incompatible("resource_affinity", "/headers", "serving_header", "Caller serving or tenant headers cannot override the published serving identity.")
		}
		if name == "Openai-Version" || name == "Api-Version" || strings.HasPrefix(name, "X-Amzn-Bedrock-") {
			return connectors.Config{}, nil, incompatible("target_capability", "/headers", "native_semantic_header", "A native semantic header has no admitted mapping in this profile.")
		}
		if !known[name] {
			continue
		}
		if seen[name] || len(values) != 1 || !native || !slices.Contains(t.profile.SemanticHeaders, name) {
			return connectors.Config{}, nil, incompatible("target_capability", "/headers", "semantic_header", "A caller semantic header has no unambiguous mapping in the selected profile.")
		}
		if len(values[0]) > 2048 || !httpguts.ValidHeaderFieldValue(values[0]) || name == "Anthropic-Version" && values[0] != t.profile.DialectRevision {
			return connectors.Config{}, nil, incompatible("target_capability", "/headers", "semantic_configuration", "Caller semantic settings are malformed or outside the profile revision.")
		}
		seen[name] = true
		for configured, value := range config.SemanticHeaders {
			if strings.EqualFold(configured, name) {
				if value != values[0] {
					return connectors.Config{}, nil, incompatible("target_capability", "/headers", "semantic_header_conflict", "A caller semantic header conflicts with the published profile.")
				}
				delete(config.SemanticHeaders, configured)
			}
		}
		config.SemanticHeaders[name] = values[0]
		receipts = append(receipts, Disposition{"/headers/" + name, "preserved", "caller_semantic_header", evidenceNative})
	}
	for name, values := range context.Query {
		// Gemini chooses SSE at the ingress path. This selector never becomes a
		// configurable semantic query override on the provider URL.
		if name == "alt" && request.Family == openai.FamilyGeminiStream && len(values) == 1 && values[0] == "sse" {
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
		receipts = append(receipts, Disposition{"/query/" + name, "preserved", "caller_semantic_query", evidenceNative})
	}
	for name := range t.config.Provider.SemanticHeaders {
		if !seen[http.CanonicalHeaderKey(name)] {
			receipts = append(receipts, Disposition{"/headers/" + http.CanonicalHeaderKey(name), "introduced", "profile_semantic_header", evidenceNative})
		}
	}
	for name := range t.config.Provider.QuerySettings {
		if _, present := context.Query[name]; !present {
			receipts = append(receipts, Disposition{"/query/" + name, "introduced", "profile_semantic_query", evidenceNative})
		}
	}
	if t.profile.Hosting == "direct-anthropic" && !seen["Anthropic-Version"] {
		receipts = append(receipts, Disposition{"/headers/Anthropic-Version", "introduced", "profile_api_revision", evidenceNative})
	}
	return config, receipts, nil
}

func checkState(document oif.Document, wire openai.Family, context Context, obligations *Obligations) error {
	root := document.Root()
	if background, _ := root.Lookup("background"); background.Raw() == "true" {
		return incompatible("state_carrier", "/background", "durable_lifecycle", "This strict runner does not admit background delivery before durable lifecycle qualification.")
	}
	retained := member(root, "store").Raw() == "true"
	if wire == openai.FamilyResponses {
		store, present := root.Lookup("store")
		retained = !present || store.Kind() == oif.Null || store.Raw() != "false"
	}
	referenced := false
	for _, name := range []string{"previous_response_id", "conversation", "cachedContent"} {
		value, present := root.Lookup(name)
		if !present || value.Kind() == oif.Null {
			continue
		}
		referenced = true
		if name == "conversation" {
			retained = true
		}
		if context.RequiredServing == nil {
			return incompatible("resource_affinity", "/"+name, "resolved_resource_affinity", "Provider resources require resolved historical serving authority.")
		}
	}
	if (retained || referenced) && !context.AllowProviderState {
		return incompatible("policy_conflict", "/store", "provider_state_authorization", "The native invocation retains or reads provider state but the caller does not permit it.")
	}
	if retained || referenced {
		return incompatible("state_carrier", "/resources", "historical_resource_contract", "Provider-retained strict continuation requires a qualified historical serving and resource reconstruction contract.")
	}
	tools, err := declaredClientTools(root, wire)
	if err != nil {
		return err
	}
	if tools {
		obligations.Effects = append(obligations.Effects, "client_tool_call")
	}
	return nil
}
func declaredClientTools(root oif.Value, wire openai.Family) (bool, error) {
	client := false
	reject := func() (bool, error) {
		return false, incompatible("state_carrier", "/tools", "hosted_tool_effects", "Provider-hosted or unregistered tool effects require a separately qualified lifecycle contract.")
	}
	if tools, present := root.Lookup("tools"); present && tools.Kind() != oif.Null {
		if tools.Kind() != oif.Array {
			return reject()
		}
		for _, tool := range tools.Elements() {
			kind := valueText(member(tool, "type"))
			switch wire {
			case openai.FamilyChat, openai.FamilyResponses:
				if kind != "function" {
					return reject()
				}
			case openai.FamilyAnthropic:
				if kind != "" && kind != "custom" || member(tool, "name").Kind() != oif.String || member(tool, "input_schema").Kind() != oif.Object {
					return reject()
				}
			case openai.FamilyGemini:
				if !onlyMembers(tool, "functionDeclarations") || member(tool, "functionDeclarations").Kind() != oif.Array {
					return reject()
				}
			default:
				return reject()
			}
			client = true
		}
	}
	if config, present := root.Lookup("toolConfig"); present && config.Kind() != oif.Null {
		if wire == openai.FamilyBedrock {
			if tools, present := config.Lookup("tools"); present {
				if tools.Kind() != oif.Array {
					return reject()
				}
				for _, tool := range tools.Elements() {
					if !onlyMembers(tool, "toolSpec") || member(tool, "toolSpec").Kind() != oif.Object {
						return reject()
					}
					client = true
				}
			}
		}
	}
	var history func(oif.Value) bool
	history = func(value oif.Value) bool {
		for _, field := range value.Members() {
			if slices.Contains([]string{"tool_calls", "function_call", "functionCall", "functionResponse", "toolUse", "toolResult"}, field.Name) {
				return true
			}
			if field.Name == "type" && slices.Contains([]string{"tool_use", "tool_result", "function_call", "function_call_output"}, valueText(field.Value)) {
				return true
			}
			if history(field.Value) {
				return true
			}
		}
		for _, element := range value.Elements() {
			if history(element) {
				return true
			}
		}
		return false
	}
	for _, name := range []string{"messages", "contents", "input"} {
		if history(member(root, name)) {
			client = true
		}
	}
	return client, nil
}

// Only schema-owned names are observable receipt fields. Arbitrary native
// extension names may themselves contain sensitive caller data.
func safeField(name string) string {
	if slices.Contains(strings.Fields("model messages input contents system systemInstruction instructions tools toolConfig tool_choice max_tokens max_completion_tokens max_output_tokens temperature top_p top_k n stream stream_options response_format text reasoning reasoning_effort thinking output_config generationConfig inferenceConfig additionalModelRequestFields additionalModelResponseFieldPaths safetySettings stop stop_sequences seed logprobs top_logprobs presence_penalty frequency_penalty user metadata store previous_response_id conversation cachedContent background"), name) {
		return oif.Pointer("", name)
	}
	return "/$native"
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
