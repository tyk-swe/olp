// Package mediacontract owns the strict native media contract. It deliberately
// has no dependency on the media transport or the gateway's Attempt owner.
package mediacontract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
)

const maxDocumentBytes = 64 << 20

// IsMediaOperation identifies the operations owned by the existing media
// service, including resource operations that cannot yet claim strict identity.
func IsMediaOperation(op string) bool {
	return slices.Contains([]string{
		"image_generation", "image_edit", "image_variation", "speech", "transcription", "translation",
		"video_create", "video_list", "video_get", "video_content", "video_delete",
	}, op)
}

type Config struct {
	Provider                                 connectors.Config
	ProviderID, RevisionID, Model, Operation string
	Policy                                   *contentpolicy.Policy
}

type Template struct {
	op, model string
	profile   connectors.Profile
	defaults  map[string]json.RawMessage
	origins   map[string]oif.Origin
	policy    *contentpolicy.Compiled
	serving   oif.ServingIdentity
}

func reject(code, field, requirement, message string) error {
	return operations.Error(code, field, requirement, message)
}

// Compile qualifies direct native media operations. The video create contract
// has a durable local resource mapping; list remains an owner-scoped local
// collection and cannot claim equivalence with a provider project-wide list.
func Compile(c Config) (*Template, error) {
	p, err := c.Provider.Profile()
	if err != nil || c.Provider.ValidateProfile() != nil {
		return nil, reject("target_capability", "/profile", "explicit_profile", "Strict media requires a registered versioned profile.")
	}
	if !slices.Contains([]string{"image_generation", "image_edit", "image_variation", "speech", "transcription", "translation", "video_create", "video_get", "video_content", "video_delete"}, c.Operation) ||
		!slices.Contains(p.Operations, c.Operation) ||
		!slices.Contains([]string{"direct-openai", "direct-compatible", "azure-v1", "azure-deployment"}, p.Hosting) {
		return nil, reject("target_capability", "/operation", "native_media_contract", "This media operation and hosting combination has no strict native contract.")
	}
	if strings.HasPrefix(c.Operation, "video_") && !slices.Contains([]string{"direct-openai", "direct-compatible"}, p.Hosting) {
		return nil, reject("target_capability", "/profile", "video_hosting", "This video hosting has no qualified native lifecycle contract.")
	}
	if !connectors.ModelValid(c.Provider.Kind, c.Provider.Model(c.Model)) {
		return nil, reject("resource_affinity", "/model", "serving_binding", "The configured media model is invalid.")
	}
	defaults, provenance, err := c.Provider.DefaultsFor(c.Operation, c.Model)
	if err != nil {
		return nil, reject("target_capability", "/defaults", "native_defaults", "The media defaults are invalid.")
	}
	if err := contentpolicy.Validate(c.Policy); err != nil {
		return nil, err
	}
	if c.Policy != nil {
		for _, rule := range c.Policy.Rules {
			if rule.Action != contentpolicy.ActionBlock {
				return nil, reject("policy_conflict", "/content_policy", "semantic_preservation", "Strict media policies cannot mutate the native request or result.")
			}
			if rule.Phase == contentpolicy.PhaseOutput || c.Operation != "image_generation" && c.Operation != "speech" {
				return nil, reject("policy_conflict", "/content_policy", "inspectable_media", "This media operation has no complete content-policy inspection contract.")
			}
		}
	}
	policy, err := contentpolicy.Compile(c.Policy)
	if err != nil {
		return nil, err
	}
	binding := c.Provider.Bindings[c.Model]
	serving := oif.ServingIdentity{ProviderID: c.ProviderID, RevisionID: c.RevisionID, Model: c.Provider.Model(c.Model), ProfileID: p.ID, ProfileRevision: p.Revision, PrincipalID: binding.PrincipalID, Snapshot: binding.Snapshot, Region: c.Provider.CloudRegion, ResourceScope: binding.ResourceScope}
	if binding.Region != "" {
		serving.Region = binding.Region
	}
	origins := map[string]oif.Origin{}
	for _, item := range provenance {
		for name := range defaults {
			if item.Pointer != oif.Pointer("", name) {
				continue
			}
			origins[name] = oif.ProviderDefault
			if strings.HasPrefix(item.Source, "binding_default") {
				origins[name] = oif.ModelDefault
			}
			break
		}
	}
	return &Template{op: c.Operation, model: c.Provider.Model(c.Model), profile: p, defaults: defaults, origins: origins, policy: policy, serving: serving}, nil
}

func (t *Template) Serving() oif.ServingIdentity { return t.serving }

// Descriptor gives retained resource calls the same operation-owned result
// identity as the request path, including durable effects and submission.
func (t *Template) Descriptor(mode string) oif.Descriptor { return t.descriptor(mode) }

// Part is an operation-owned view of one outbound multipart member. Blob is
// already authorized and staged by the media spool; this type grants no read.
type Part struct {
	Name        string
	Text        *string
	Blob        oif.BlobReference
	Filename    string
	ContentType string
	Raw         bool
}

type Input struct {
	Route       string
	Mode        string
	Source      oif.Document // complete immutable caller JSON, when JSON is used
	JSON        []byte       // exact candidate-specific outbound JSON
	Parts       []Part       // exact outbound multipart order
	CallerParts []Part       // accepted caller order before admitted overlays
}

type Bound struct {
	Descriptor oif.Descriptor
	Source     oif.Request
	Effective  oif.Request
	Receipt    oif.Receipt
	Parts      []Part
	Assets     []oif.BlobRequest
}

func (t *Template) receipt(d oif.Descriptor, dispositions []oif.Disposition) oif.Receipt {
	lifetime, submission, effects := "request", "immediate", []string{"inference"}
	class := "native_identity"
	if strings.HasPrefix(t.op, "video_") {
		lifetime, effects, class = "durable", []string{"inference", "resource_mutation"}, "qualified_translation"
		if t.op == "video_create" {
			submission = "queued"
		}
	}
	return oif.Receipt{Class: class, Operation: t.op, SourceDialect: d.Dialect.ID, TargetDialect: d.Dialect.ID,
		ProfileID: t.profile.ID, ProfileRevision: t.profile.Revision, Serving: t.serving, Dispositions: dispositions,
		Evidence:    []string{"media/native-openai/1", t.profile.Documentation},
		Obligations: oif.Obligations{Delivery: d.Execution.Delivery, Lifetime: lifetime, Submission: submission, Continuation: "none", Effects: effects, Retry: "before_dispatch_or_definitive_rejection", MaxBodyBytes: maxDocumentBytes, MaxEventBytes: 1 << 20, RejectAmbiguousFailover: true, GuardResults: true}}
}

func (t *Template) descriptor(mode string) oif.Descriptor {
	delivery := "unary"
	lifetime, submission, effects := "request", "immediate", []string{"inference"}
	if mode == "streaming" {
		delivery = "incremental"
	}
	if strings.HasPrefix(t.op, "video_") {
		lifetime, effects = "durable", []string{"inference", "resource_mutation"}
		if t.op == "video_create" {
			submission = "queued"
		}
	}
	return oif.Descriptor{
		Operation: oif.Identity{ID: t.op, Revision: "1"},
		Dialect:   oif.Identity{ID: t.profile.OperationDialect(t.op), Revision: t.profile.Revision},
		Profile:   oif.Identity{ID: t.profile.ID, Revision: t.profile.Revision},
		Execution: oif.Execution{Delivery: delivery, Lifetime: lifetime, Submission: submission, Effects: effects},
	}
}

// Bind checks the candidate's actual encoded body, not detached typed request
// fields. Untouched JSON values retain their exact lexical form and order;
// only the registered model identity and absent-only defaults may differ.
func (t *Template) Bind(in Input) (Bound, error) {
	d := t.descriptor(in.Mode)
	if in.Mode != "unary" && in.Mode != "streaming" && !(t.op == "video_create" && in.Mode == "async") {
		return Bound{}, reject("target_capability", "/mode", "media_delivery", "The media delivery mode has no native contract.")
	}
	if t.op == "video_create" && in.Mode != "async" {
		return Bound{}, reject("target_capability", "/mode", "video_submission", "Video creation requires an asynchronous durable job.")
	}
	if len(in.JSON) != 0 {
		if !in.Source.Valid() || len(in.Parts) != 0 {
			return Bound{}, reject("target_capability", "/request", "native_media_source", "The native JSON source is missing or mixed with multipart input.")
		}
		if t.op != "image_generation" && t.op != "speech" {
			return Bound{}, reject("target_capability", "/request", "native_media_source", "This operation requires multipart input.")
		}
		return t.bindJSON(d, in)
	}
	if in.Source.Valid() || len(in.Parts) == 0 || t.op == "image_generation" || t.op == "speech" {
		return Bound{}, reject("target_capability", "/request", "native_media_source", "This media operation has no matching source representation.")
	}
	return t.bindMultipart(d, in)
}

func (t *Template) bindJSON(d oif.Descriptor, in Input) (Bound, error) {
	effective, err := oif.ParseJSON(in.JSON, oif.Limits{MaxBytes: maxDocumentBytes})
	if err != nil || in.Source.Root().Kind() != oif.Object || effective.Root().Kind() != oif.Object {
		return Bound{}, reject("target_capability", "/request", "unambiguous_media_json", "The native media JSON is malformed or ambiguous.")
	}
	model, ok := in.Source.Root().Lookup("model")
	route, valid := model.Text()
	if !ok || !valid || route != in.Route {
		return Bound{}, reject("resource_affinity", "/model", "route_identity", "The media model must name the selected route.")
	}
	effectiveModel, ok := effective.Root().Lookup("model")
	upstreamModel, valid := effectiveModel.Text()
	if !ok || !valid || upstreamModel != t.model {
		return Bound{}, reject("resource_affinity", "/model", "serving_binding", "The outbound model differs from the selected serving model.")
	}
	previous := -1
	for _, member := range in.Source.Root().Members() {
		found := false
		for index, outbound := range effective.Root().Members() {
			if outbound.Name != member.Name {
				continue
			}
			found = true
			if index <= previous {
				return Bound{}, reject("target_capability", "/request", "source_order", "Media source member order changed.")
			}
			previous = index
			if member.Name != "model" && member.Value.Raw() != outbound.Value.Raw() {
				return Bound{}, reject("target_capability", "/request", "source_identity", "A native media field changed without an admitted overlay.")
			}
			break
		}
		if !found {
			return Bound{}, reject("target_capability", "/request", "source_identity", "A native media field was dropped.")
		}
	}
	for _, member := range effective.Root().Members() {
		if _, existed := in.Source.Root().Lookup(member.Name); existed {
			continue
		}
		if !bytes.Equal([]byte(member.Value.Raw()), t.defaults[member.Name]) {
			return Bound{}, reject("target_capability", "/defaults", "absent_only_default", "An outbound media field has no admitted default.")
		}
	}
	source, _ := oif.NewRequest(d, in.Source)
	modelJSON, _ := json.Marshal(t.model)
	changes := []oif.Change{{Pointer: "/model", Value: string(modelJSON), Origin: oif.IdentityBinding, Reason: "selected serving model"}}
	dispositions := []oif.Disposition{{Field: "/request", Disposition: "preserved", Rule: "native_source_identity", Evidence: "media/native-openai/1"}, {Field: "/model", Disposition: "bound", Rule: "serving_model", Evidence: "media/native-openai/1"}}
	for _, member := range effective.Root().Members() {
		if _, existed := in.Source.Root().Lookup(member.Name); existed {
			continue
		}
		origin := t.origins[member.Name]
		if origin == "" {
			origin = oif.ProviderDefault
		}
		changes = append(changes, oif.Change{Pointer: oif.Pointer("", member.Name), Value: member.Value.Raw(), Origin: origin, Reason: "declared absent-only media default"})
		dispositions = append(dispositions, oif.Disposition{Field: oif.Pointer("", member.Name), Disposition: "introduced", Rule: string(origin), Evidence: "media/native-openai/1"})
	}
	effectiveRequest, err := source.WithChanges(changes...)
	if err != nil || effectiveRequest.Document().Raw() != effective.Raw() {
		return Bound{}, reject("target_capability", "/request", "exact_media_overlay", "The native media request differs from its admitted source overlays.")
	}
	if err := t.inspectJSONInput(effective); err != nil {
		return Bound{}, err
	}
	return Bound{Descriptor: d, Source: source, Effective: effectiveRequest, Receipt: t.receipt(d, dispositions)}, nil
}

func (t *Template) bindMultipart(d oif.Descriptor, in Input) (Bound, error) {
	if len(in.CallerParts) == 0 || len(in.Parts) < len(in.CallerParts) {
		return Bound{}, reject("target_capability", "/request", "multipart_source", "The ordered caller multipart source is missing.")
	}
	seen := map[string]bool{}
	for index, caller := range in.CallerParts {
		outbound := in.Parts[index]
		seen[caller.Name] = true
		if caller.Name != outbound.Name || caller.Blob != outbound.Blob || caller.Filename != outbound.Filename || caller.ContentType != outbound.ContentType || caller.Raw != outbound.Raw || (caller.Text == nil) != (outbound.Text == nil) {
			return Bound{}, reject("target_capability", "/request", "source_order", "A caller multipart member changed position or identity.")
		}
		if caller.Text != nil {
			if caller.Name == "model" {
				if *caller.Text != in.Route || *outbound.Text != t.model {
					return Bound{}, reject("resource_affinity", "/model", "serving_binding", "The multipart model binding is invalid.")
				}
			} else if *caller.Text != *outbound.Text {
				return Bound{}, reject("target_capability", "/request", "source_identity", "A caller multipart text value changed.")
			}
		}
	}
	var defaultParts []Part
	for _, name := range slices.Sorted(maps.Keys(t.defaults)) {
		if seen[name] || seen[name+"[]"] {
			continue
		}
		defaultParts = append(defaultParts, multipartDefaultParts(name, t.defaults[name])...)
	}
	if len(defaultParts) != len(in.Parts)-len(in.CallerParts) {
		return Bound{}, reject("target_capability", "/defaults", "absent_only_default", "Multipart defaults are incomplete or unexpected.")
	}
	for i, want := range defaultParts {
		got := in.Parts[len(in.CallerParts)+i]
		if got.Name != want.Name || got.Text == nil || *got.Text != *want.Text || got.Blob.Valid() || got.Raw != want.Raw {
			return Bound{}, reject("target_capability", "/defaults", "absent_only_default", "A multipart default changed its native value.")
		}
	}
	modelSeen, fileCount, maskCount, imageCount := false, 0, 0, 0
	parts := make([]Part, len(in.Parts))
	copy(parts, in.Parts)
	for _, part := range parts {
		if part.Name == "" || (part.Text == nil) == !part.Blob.Valid() {
			return Bound{}, reject("target_capability", "/request", "multipart_member", "A multipart member has no unambiguous text or staged blob.")
		}
		if part.Name == "model" {
			if modelSeen || part.Text == nil || *part.Text != t.model {
				return Bound{}, reject("resource_affinity", "/model", "serving_binding", "Multipart model binding is invalid.")
			}
			modelSeen = true
		}
		if part.Blob.Valid() {
			if part.ContentType == "" {
				return Bound{}, reject("target_capability", "/request", "asset_media_type", "Strict multipart cannot introduce a missing file Content-Type.")
			}
			fileCount++
			switch t.op {
			case "image_edit":
				if part.Name == "mask" {
					maskCount++
				} else if part.Name != "image" && !strings.HasPrefix(part.Name, "image[") {
					return Bound{}, reject("target_capability", "/request", "image_input", "Image edit file field is not qualified.")
				} else {
					imageCount++
				}
			case "image_variation":
				if part.Name != "image" {
					return Bound{}, reject("target_capability", "/request", "image_input", "Image variation file field is not qualified.")
				}
				imageCount++
			case "transcription", "translation":
				if part.Name != "file" {
					return Bound{}, reject("target_capability", "/request", "audio_input", "Audio file field is not qualified.")
				}
			case "video_create":
				if part.Name != "input_reference" || !strings.HasPrefix(part.ContentType, "image/") {
					return Bound{}, reject("target_capability", "/request", "video_reference", "Video creation requires one original image reference.")
				}
			}
		}
	}
	if !modelSeen || t.op != "video_create" && fileCount == 0 || maskCount > 1 || t.op == "image_edit" && imageCount == 0 {
		return Bound{}, reject("target_capability", "/request", "media_assets", "The multipart source is incomplete.")
	}
	if t.op != "image_edit" && t.op != "video_create" && fileCount != 1 || t.op == "video_create" && fileCount > 1 {
		return Bound{}, reject("target_capability", "/request", "media_assets", "The operation requires exactly one staged source asset.")
	}
	bound := Bound{Parts: parts}
	dispositions := []oif.Disposition{{Field: "/request", Disposition: "preserved", Rule: "ordered_multipart_source", Evidence: "media/native-openai/1"}, {Field: "/model", Disposition: "bound", Rule: "serving_model", Evidence: "media/native-openai/1"}}
	for i, part := range in.CallerParts {
		if part.Blob.Valid() {
			dispositions = append(dispositions, oif.Disposition{Field: fmt.Sprintf("/parts/%d", i), Disposition: "preserved", Rule: "staged_blob_identity", Evidence: "media/native-openai/1"})
		}
	}
	for _, part := range defaultParts {
		origin := t.origins[strings.TrimSuffix(part.Name, "[]")]
		if origin == "" {
			origin = oif.ProviderDefault
		}
		dispositions = append(dispositions, oif.Disposition{Field: "/parts/" + part.Name, Disposition: "introduced", Rule: string(origin), Evidence: "media/native-openai/1"})
	}
	for _, part := range parts {
		if !part.Blob.Valid() {
			continue
		}
		d.Assets = append(d.Assets, oif.Asset{ID: part.Blob.ID(), Digest: part.Blob.Digest(), MediaType: part.Blob.MediaType(), Size: part.Blob.Size()})
	}
	bound.Descriptor = d
	bound.Receipt = t.receipt(d, dispositions)
	for _, part := range parts {
		if !part.Blob.Valid() {
			continue
		}
		asset, err := oif.NewBlobRequest(d, part.Blob)
		if err != nil {
			return Bound{}, err
		}
		bound.Assets = append(bound.Assets, asset)
	}
	return bound, nil
}

func multipartDefaultParts(name string, raw json.RawMessage) []Part {
	if slices.Contains([]string{"include", "timestamp_granularities", "known_speaker_names", "known_speaker_references"}, name) {
		var values []string
		if json.Unmarshal(raw, &values) != nil {
			return nil
		}
		out := make([]Part, 0, len(values))
		for _, value := range values {
			text := value
			out = append(out, Part{Name: name + "[]", Text: &text})
		}
		return out
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []Part{{Name: name, Text: &text}}
	}
	var compact bytes.Buffer
	if json.Compact(&compact, raw) != nil {
		return nil
	}
	text = compact.String()
	return []Part{{Name: name, Text: &text, Raw: true}}
}

func (t *Template) inspectJSONInput(doc oif.Document) error {
	if t.policy == nil || !t.policy.HasInput() {
		return nil
	}
	known := map[string]bool{"model": true, "prompt": true, "input": true, "voice": true, "n": true, "size": true, "quality": true, "response_format": true, "style": true, "user": true, "background": true, "moderation": true, "output_compression": true, "output_format": true, "partial_images": true, "stream": true, "instructions": true, "speed": true, "stream_format": true}
	for _, member := range doc.Root().Members() {
		if !known[member.Name] {
			return reject("policy_conflict", "/request", "inspectable_native_option", "A native media option has no qualified content-policy coverage.")
		}
		if member.Name == "model" {
			continue
		}
		value, ok := member.Value.Text()
		if !ok {
			continue
		}
		for _, rule := range t.policy.Input {
			if rule.Re.MatchString(value) {
				return reject("content_policy_blocked", "/request", "content_policy", "The effective native media request was blocked.")
			}
		}
	}
	return nil
}

// JSONResult retains the complete bounded native source, including unknown
// categories and exact numeric lexemes, before the legacy typed projection.
func (t *Template) JSONResult(d oif.Descriptor, source oif.Document) (oif.Result, error) {
	if !source.Valid() || source.Root().Kind() != oif.Object {
		return oif.Result{}, reject("protocol_violation", "/result", "media_result", "The native media result is not a JSON object.")
	}
	if t.op == "image_generation" || t.op == "image_edit" || t.op == "image_variation" {
		data, ok := source.Root().Lookup("data")
		if !ok || data.Kind() != oif.Array {
			return oif.Result{}, reject("protocol_violation", "/result/data", "image_set", "The image result has no image set.")
		}
	}
	if t.op == "video_create" || t.op == "video_get" {
		id, idOK := source.Root().Lookup("id")
		model, modelOK := source.Root().Lookup("model")
		kind, kindOK := source.Root().Lookup("object")
		status, statusOK := source.Root().Lookup("status")
		upstreamID, idText := id.Text()
		modelName, modelText := model.Text()
		objectName, kindText := kind.Text()
		state, statusText := status.Text()
		if !idOK || !idText || upstreamID == "" || !modelOK || !modelText || modelName != t.model ||
			!kindOK || !kindText || objectName != "video" || !statusOK || !statusText ||
			!slices.Contains([]string{"queued", "in_progress", "completed", "failed"}, state) {
			return oif.Result{}, reject("protocol_violation", "/result", "video_job_identity", "The native video job identity or status is invalid.")
		}
	}
	if t.op == "video_delete" {
		kind, kindOK := source.Root().Lookup("object")
		deleted, deletedOK := source.Root().Lookup("deleted")
		name, textOK := kind.Text()
		if !kindOK || !textOK || name != "video.deleted" || !deletedOK || deleted.Raw() != "true" {
			return oif.Result{}, reject("protocol_violation", "/result", "video_delete_receipt", "The native video deletion receipt is invalid.")
		}
	}
	return oif.NewResult(d, source, oif.Complete)
}

// Event retains one bounded native SSE document and its framing. The caller
// owns stream backpressure, terminal detection and truthful incomplete status.
func (t *Template) Event(d oif.Descriptor, data string, event string, sequence uint64) (oif.Event, error) {
	doc, err := oif.ParseJSON([]byte(data), oif.Limits{MaxBytes: maxDocumentBytes})
	if err != nil || doc.Root().Kind() != oif.Object {
		return oif.Event{}, reject("protocol_violation", "/event", "media_event", "The native media event is malformed or ambiguous.")
	}
	kind, ok := doc.Root().Lookup("type")
	name, valid := kind.Text()
	if !ok || !valid || name == "" || event != "" && event != name {
		return oif.Event{}, reject("protocol_violation", "/event/type", "media_event", "The native media event type is inconsistent.")
	}
	return oif.NewEvent(d, doc, name, sequence)
}

func (t *Template) String() string {
	return fmt.Sprintf("%s/%s@%s", t.op, t.profile.ID, t.profile.Revision)
}
