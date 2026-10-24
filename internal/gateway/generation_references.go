package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
)

type generationReference struct{ pointer, kind, id string }

// generationResources is produced only after every reference has passed owner,
// current authority, historical connection and credential checks. Paths name
// dialect-owned positions; JSON supplied to tools is never walked as authority.
type generationResources struct {
	resolved     map[string]string
	bound        map[string]*resources.Resource
	cacheInput   int64
	unknownInput bool
}

func generationReferences(source oif.Document, family wire.Format) ([]generationReference, error) {
	var out []generationReference
	add := func(pointer, kind string, value oif.Value) error {
		if value.Kind() == oif.Absent || value.Kind() == oif.Null {
			return nil
		}
		id, ok := value.Text()
		if !ok || id == "" {
			return errors.New("resource reference must be a nonempty string")
		}
		out = append(out, generationReference{pointer, kind, id})
		return nil
	}
	root := source.Root()
	if family == wire.Gemini || family == wire.GeminiStream {
		checkParts := func(parts oif.Value) error {
			for _, part := range parts.Elements() {
				fileData, _ := part.Lookup("fileData")
				fileURI, _ := fileData.Lookup("fileUri")
				uri, valid := fileURI.Text()
				if !valid || uri == "" {
					continue
				}
				parsed, _ := url.Parse(uri)
				providerFile := strings.HasPrefix(strings.TrimPrefix(uri, "/"), "files/")
				if parsed != nil {
					providerFile = providerFile || strings.EqualFold(strings.TrimSuffix(parsed.Hostname(), "."), "generativelanguage.googleapis.com") || strings.EqualFold(parsed.Scheme, "gs") || strings.EqualFold(parsed.Scheme, "s3")
				}
				if providerFile {
					return errors.New("provider-managed Gemini file references have no qualified generation ownership contract")
				}
			}
			return nil
		}
		contents, _ := root.Lookup("contents")
		for _, content := range contents.Elements() {
			parts, _ := content.Lookup("parts")
			if err := checkParts(parts); err != nil {
				return nil, err
			}
		}
		system, _ := root.Lookup("systemInstruction")
		parts, _ := system.Lookup("parts")
		if err := checkParts(parts); err != nil {
			return nil, err
		}
		cache, _ := root.Lookup("cachedContent")
		if err := add("/cachedContent", "context_cache", cache); err != nil {
			return nil, err
		}
		return out, nil
	}
	if family != wire.Responses {
		return nil, nil
	}
	conversation, present := root.Lookup("conversation")
	if present && conversation.Kind() != oif.Null {
		path := "/conversation"
		if conversation.Kind() == oif.Object {
			conversation, _ = conversation.Lookup("id")
			path += "/id"
			if conversation.Kind() == oif.Absent || conversation.Kind() == oif.Null {
				return nil, errors.New("conversation needs an identifier")
			}
		}
		if err := add(path, "conversation", conversation); err != nil {
			return nil, err
		}
	}
	input, _ := root.Lookup("input")
	filePart := func(part oif.Value, path string) error {
		kind, _ := part.Lookup("type")
		name, _ := kind.Text()
		if name == "input_file" || name == "input_image" || name == "computer_screenshot" {
			value, _ := part.Lookup("file_id")
			return add(path+"/file_id", resources.KindFile, value)
		}
		return nil
	}
	for i, item := range input.Elements() {
		kind, _ := item.Lookup("type")
		if name, _ := kind.Text(); name == "item_reference" {
			return nil, errors.New("item_reference is not qualified for generation")
		}
		if name, _ := kind.Text(); name == "code_interpreter_call" {
			return nil, errors.New("container execution is outside the container lifecycle and file-access contract")
		}
		path := "/input/" + strconv.Itoa(i)
		if err := filePart(item, path); err != nil {
			return nil, err
		}
		name, _ := kind.Text()
		field := "content"
		if name == "function_call_output" || name == "custom_tool_call_output" || name == "computer_call_output" {
			field = "output"
		} else if name != "" && name != "message" {
			continue
		}
		content, _ := item.Lookup(field)
		if content.Kind() == oif.Object {
			if err := filePart(content, path+"/"+field); err != nil {
				return nil, err
			}
		}
		for j, part := range content.Elements() {
			if err := filePart(part, path+"/"+field+"/"+strconv.Itoa(j)); err != nil {
				return nil, err
			}
		}
	}
	tools, _ := root.Lookup("tools")
	for i, tool := range tools.Elements() {
		kind, _ := tool.Lookup("type")
		if name, _ := kind.Text(); name == "code_interpreter" {
			return nil, errors.New("container execution is outside the container lifecycle and file-access contract")
		}
		if name, _ := kind.Text(); name != "file_search" {
			continue
		}
		ids, present := tool.Lookup("vector_store_ids")
		if !present || ids.Kind() != oif.Array || len(ids.Elements()) == 0 {
			return nil, errors.New("file_search requires owned vector stores")
		}
		for j, id := range ids.Elements() {
			if err := add("/tools/"+strconv.Itoa(i)+"/vector_store_ids/"+strconv.Itoa(j), "vector_store", id); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func sameGenerationAffinity(a, b *resources.Resource) bool {
	return a != nil && b != nil && a.APIKeyID == b.APIKeyID && a.RouteSlug == b.RouteSlug && a.RouteRevisionID == b.RouteRevisionID && samePin(a, b) && strings.TrimPrefix(resourceModel(a), "models/") == strings.TrimPrefix(resourceModel(b), "models/")
}

func (s *Server) bindGenerationReferences(ctx context.Context, x *call) *Error {
	parsed := x.generation.parsed
	refs, err := generationReferences(parsed.OIF().Document(), parsed.Family)
	if err != nil {
		return invalidRequest("invalid_resource_reference", err.Error(), nil)
	}
	if len(refs) == 0 && x.previousResponse() == nil {
		return nil
	}
	if s.Resources == nil {
		return serverError(http.StatusServiceUnavailable, "provider_state_unavailable", "Provider resource authority is unavailable.")
	}
	if len(refs) > 0 && !x.authority.Policy.AllowProviderState {
		return providerStateForbidden()
	}
	x.mode = "unary"
	if parsed.Stream {
		x.mode = "streaming"
	}
	bindings := &generationResources{resolved: map[string]string{}, bound: map[string]*resources.Resource{}}
	if prior := x.previousResponse(); prior != nil {
		p, route, failure := s.resolveResource(ctx, x, x.authority, prior, "generation", retainedNewWork)
		if failure != nil {
			return failure
		}
		bindings.unknownInput = true
		if failure := s.generationReferenceBudget(x, p, route, bindings); failure != nil {
			return failure
		}
		if failure := x.bindGenerationServing(p, prior, "/previous_response_id"); failure != nil {
			return failure
		}
		bindings.resolved["/previous_response_id"] = prior.UpstreamID
		bindings.bound[prior.Kind+"\x00"+prior.UpstreamID] = prior
	}
	var changes []oif.Change
	for _, ref := range refs {
		var resource *resources.Resource
		var durable *durableDocument
		localID := ref.id
		if ref.kind == "context_cache" {
			// Gemini SDKs normalize resource names before generation, including
			// names returned by the gateway's owner-scoped cache lifecycle.
			localID = strings.TrimPrefix(localID, "cachedContents/")
		}
		if ref.kind == resources.KindFile {
			resource, durable, err = s.readDurable(ctx, durableKind(resources.KindFile, ref.id), x.keyID, ref.id)
		} else {
			resource, err = s.Resources.Get(ctx, ref.kind, x.keyID, localID)
		}
		if err != nil || resource == nil || resource.RouteSlug != parsed.Route || resource.State == "submitting" || resource.State == "submission_unknown" || resource.State == "not_accepted" || resource.State == resources.StateDeleted || resource.ExpiresAt != nil && !resource.ExpiresAt.After(s.now()) {
			return invalidRequest("invalid_resource_reference", "The resource is unavailable to this key and model.", &ref.pointer)
		}
		if ref.kind != resources.KindFile && (resource.ContractVersion == nil || *resource.ContractVersion != retainedapi.Revision) {
			return invalidRequest("resource_affinity", "The resource has no compatible native reference contract.", &ref.pointer)
		}
		var p *pin
		var route *routing.Route
		if durable != nil {
			p, route, _ = s.admitDurable(ctx, x, x.authority, resource, durable, "generation", retainedNewWork)
		} else {
			p, route, _ = s.resolveResource(ctx, x, x.authority, resource, "generation", retainedNewWork)
		}
		if p == nil || x.pin != nil && !sameGenerationAffinity(x.pin, resource) {
			return invalidRequest("resource_affinity", "Generation references must share one authorized model, connection and credential.", &ref.pointer)
		}
		if ref.kind == "context_cache" {
			var metadata struct {
				Model  string `json:"model"`
				Tokens *int64 `json:"total_token_count"`
			}
			_ = json.Unmarshal(resource.Metadata, &metadata)
			if p.provider.Kind != "gemini" || resource.ExpiresAt == nil || metadata.Model == "" || metadata.Tokens == nil || *metadata.Tokens < 0 || *metadata.Tokens > estimate.MaxTokens || strings.TrimPrefix(metadata.Model, "models/") != p.provider.Connector().Model(p.model) || !strings.HasPrefix(resource.UpstreamID, "cachedContents/") {
				return invalidRequest("resource_affinity", "The context cache has no valid model and expiry binding.", &ref.pointer)
			}
			bindings.cacheInput = *metadata.Tokens
		} else {
			// Files, history and retrieved passages have no authoritative input
			// token bound. A finite output limit cannot bound those input costs.
			bindings.unknownInput = true
			if failure := s.generationReferenceBudget(x, p, route, bindings); failure != nil {
				return failure
			}
		}
		if failure := x.bindGenerationServing(p, resource, ref.pointer); failure != nil {
			return failure
		}
		if x.pin == nil {
			x.pin = resource
		}
		bindings.resolved[ref.pointer] = resource.UpstreamID
		bindings.bound[ref.kind+"\x00"+resource.UpstreamID] = resource
		changes = append(changes, oif.Change{Pointer: ref.pointer, Value: quotedResponseID(resource.UpstreamID), Origin: oif.ResourceBinding, Reason: "verified generation resource authority"})
		if ref.kind == "conversation" {
			x.providerState = true
		}
	}
	next, err := parsed.WithResourceBindings(changes...)
	if err != nil {
		return requestError(err)
	}
	x.generation.parsed, x.generation.resources = next, bindings
	return nil
}

func (x *call) bindGenerationServing(p *pin, resource *resources.Resource, pointer string) *Error {
	serving := p.provider.Connector().ServingIdentity(p.provider.ID, p.provider.RevisionID, p.model)
	if x.serving != nil && *x.serving != serving {
		return invalidRequest("resource_affinity", "Generation references require the same native serving contract.", &pointer)
	}
	x.serving, x.servingSlot, x.servingBinding = &serving, resource.SlotID, p.model
	return nil
}

func (s *Server) generationReferenceBudget(x *call, p *pin, route *routing.Route, bindings *generationResources) *Error {
	if bindings == nil || !bindings.unknownInput {
		return nil
	}
	authority := x.authority
	authority.Attribution = x.attribution
	if s.Runtime == nil {
		return unboundedWorkLimits(authority, &p.provider, &p.slot, route)
	}
	return s.unboundedPinLimits(authority, p, route)
}

// A connection pin can also belong to a file, conversation or vector store.
// Only an actual prior response supplies the response parent relationship.
func (x *call) previousResponse() *resources.Resource {
	if x.pin != nil && (x.pin.Kind == resources.KindResponse || x.pin.Kind == resources.KindStrictResponse) {
		return x.pin
	}
	return nil
}

func (x *call) resolvedGenerationResources() map[string]string {
	if x.generation.resources == nil {
		return nil
	}
	return x.generation.resources.resolved
}

func (x *call) checkGenerationResourceDestination(provider *routing.Provider, model string, destination wire.Format, source oif.Document) error {
	refs, err := generationReferences(source, destination)
	if err != nil {
		return &oif.Incompatibility{Code: "resource_affinity", Requirement: "native_resource_destination", Message: "The destination contains an invalid resource reference."}
	}
	resolved := x.resolvedGenerationResources()
	for _, ref := range refs {
		if resolved[ref.pointer] != ref.id {
			return &oif.Incompatibility{Code: "resource_affinity", Field: ref.pointer, Requirement: "verified_resource_binding", Message: "Destination defaults cannot introduce unresolved provider resources."}
		}
	}
	if previous, present := source.Lookup("/previous_response_id"); present && previous.Kind() != oif.Null {
		id, valid := previous.Text()
		if !valid || id == "" || resolved["/previous_response_id"] != id {
			return &oif.Incompatibility{Code: "resource_affinity", Field: "/previous_response_id", Requirement: "verified_resource_binding", Message: "The destination has no verified prior response authority."}
		}
	}
	if x.generation.resources == nil {
		return nil
	}
	nativeFamily := x.generation.parsed.Family
	if nativeFamily == wire.GeminiStream {
		nativeFamily = wire.Gemini
	}
	if destination != nativeFamily || x.serving == nil || *x.serving != provider.Connector().ServingIdentity(provider.ID, provider.RevisionID, model) {
		return &oif.Incompatibility{Code: "resource_affinity", Requirement: "native_resource_destination", Message: "Provider resources require their verified native model and dialect."}
	}
	for pointer, id := range x.generation.resources.resolved {
		value, present := source.Lookup(pointer)
		actual, ok := value.Text()
		if !present || !ok || actual != id {
			return &oif.Incompatibility{Code: "resource_affinity", Field: pointer, Requirement: "preserved_resource_binding", Message: "The destination changed a verified resource reference."}
		}
	}
	return nil
}
