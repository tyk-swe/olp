package kernel

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/generationrefs"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
)

// generationResources is produced only after every reference has passed owner,
// current authority, historical connection and credential checks. Paths name
// dialect-owned positions; JSON supplied to tools is never walked as authority.
type generationResources struct {
	resolved     map[string]string
	bound        map[string]*resources.Resource
	cacheInput   int64
	unknownInput bool
}

func sameGenerationAffinity(a, b *resources.Resource) bool {
	return a != nil && b != nil && a.APIKeyID == b.APIKeyID && a.RouteSlug == b.RouteSlug && a.RouteRevisionID == b.RouteRevisionID && SamePin(a, b) && strings.TrimPrefix(ResourceModel(a), "models/") == strings.TrimPrefix(ResourceModel(b), "models/")
}

func (s *Server) bindGenerationReferences(ctx context.Context, x *Call) *fault.Error {
	parsed := x.generation.parsed
	refs, err := generationrefs.InRequest(parsed.OIF().Document(), parsed.Family)
	if err != nil {
		return fault.InvalidRequest("invalid_resource_reference", err.Error(), nil)
	}
	if len(refs) == 0 && x.previousResponse() == nil {
		return nil
	}
	if s.Deps.Resources == nil {
		return fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "Provider resource authority is unavailable.")
	}
	if len(refs) > 0 && !x.authority.Policy.AllowProviderState {
		return ProviderStateForbidden()
	}
	x.mode = "unary"
	if parsed.Stream {
		x.mode = "streaming"
	}
	bindings := &generationResources{resolved: map[string]string{}, bound: map[string]*resources.Resource{}}
	if prior := x.previousResponse(); prior != nil {
		p, route, failure := s.ResolveResource(ctx, x, x.authority, prior, "generation", RetainedNewWork)
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
		var durable *DurableDocument
		localID := ref.ID
		if ref.Kind == "context_cache" {
			// Gemini SDKs normalize resource names before generation, including
			// names returned by the gateway's owner-scoped cache lifecycle.
			localID = strings.TrimPrefix(localID, "cachedContents/")
		}
		if ref.Kind == resources.KindFile {
			resource, durable, err = s.ReadDurable(ctx, DurableKind(resources.KindFile, ref.ID), x.keyID, ref.ID)
		} else {
			resource, err = s.Deps.Resources.Get(ctx, ref.Kind, x.keyID, localID)
		}
		if err != nil || resource == nil || resource.RouteSlug != parsed.Route || RetainedUnsettled(resource.State) || resource.State == resources.StateDeleted || resource.ExpiresAt != nil && !resource.ExpiresAt.After(s.Now()) {
			return fault.InvalidRequest("invalid_resource_reference", "The resource is unavailable to this key and model.", &ref.Pointer)
		}
		if ref.Kind != resources.KindFile && (resource.ContractVersion == nil || *resource.ContractVersion != retainedapi.Revision) {
			return fault.InvalidRequest("resource_affinity", "The resource has no compatible native reference contract.", &ref.Pointer)
		}
		var p *Pin
		var route *routing.Route
		if durable != nil {
			p, route, _ = s.AdmitDurable(ctx, x, x.authority, resource, durable, "generation", RetainedNewWork)
		} else {
			p, route, _ = s.ResolveResource(ctx, x, x.authority, resource, "generation", RetainedNewWork)
		}
		if p == nil || x.pin != nil && !sameGenerationAffinity(x.pin, resource) {
			return fault.InvalidRequest("resource_affinity", "Generation references must share one authorized model, connection and credential.", &ref.Pointer)
		}
		if ref.Kind == "context_cache" {
			tokens, valid := generationCacheInput(resource, p)
			if !valid {
				return fault.InvalidRequest("resource_affinity", "Recreate this cache with a qualified model, expiry and no cached tools.", &ref.Pointer)
			}
			bindings.cacheInput = tokens
		} else {
			// Files, history and retrieved passages have no authoritative input
			// token bound. A finite output limit cannot bound those input costs.
			bindings.unknownInput = true
			if failure := s.generationReferenceBudget(x, p, route, bindings); failure != nil {
				return failure
			}
		}
		if failure := x.bindGenerationServing(p, resource, ref.Pointer); failure != nil {
			return failure
		}
		if x.pin == nil {
			x.pin = resource
		}
		bindings.resolved[ref.Pointer] = resource.UpstreamID
		bindings.bound[ref.Kind+"\x00"+resource.UpstreamID] = resource
		changes = append(changes, oif.Change{Pointer: ref.Pointer, Value: QuotedResponseID(resource.UpstreamID), Origin: oif.ResourceBinding, Reason: "verified generation resource authority"})
		if ref.Kind == "conversation" {
			x.providerState = true
		}
	}
	next, err := parsed.WithResourceBindings(changes...)
	if err != nil {
		return RequestError(err)
	}
	x.generation.parsed, x.generation.resources = next, bindings
	return nil
}

func generationCacheInput(resource *resources.Resource, p *Pin) (int64, bool) {
	var metadata struct {
		Model         string `json:"model"`
		Tokens        *int64 `json:"total_token_count"`
		NoCachedTools bool   `json:"no_cached_tools"`
	}
	if json.Unmarshal(resource.Metadata, &metadata) != nil || p.Provider.Kind != "gemini" || resource.ExpiresAt == nil || !metadata.NoCachedTools || metadata.Model == "" || metadata.Tokens == nil || *metadata.Tokens < 0 || *metadata.Tokens > estimate.MaxTokens || strings.TrimPrefix(metadata.Model, "models/") != p.Provider.Connector().Model(p.Model) || !strings.HasPrefix(resource.UpstreamID, "cachedContents/") {
		return 0, false
	}
	return *metadata.Tokens, true
}

func (x *Call) bindGenerationServing(p *Pin, resource *resources.Resource, pointer string) *fault.Error {
	serving := p.Provider.Connector().ServingIdentity(p.Provider.ID, p.Provider.RevisionID, p.Model)
	if x.serving != nil && *x.serving != serving {
		return fault.InvalidRequest("resource_affinity", "Generation references require the same native serving contract.", &pointer)
	}
	x.serving, x.servingSlot, x.servingBinding = &serving, resource.SlotID, p.Model
	return nil
}

func (s *Server) generationReferenceBudget(x *Call, p *Pin, route *routing.Route, bindings *generationResources) *fault.Error {
	if bindings == nil || !bindings.unknownInput {
		return nil
	}
	authority := x.authority
	authority.Attribution = x.attribution
	return s.unboundedPinLimits(authority, p, route, p.Model)
}

// A connection pin can also belong to a file, conversation or vector store.
// Only an actual prior response supplies the response parent relationship.
func (x *Call) previousResponse() *resources.Resource {
	if x.pin != nil && (x.pin.Kind == resources.KindResponse || x.pin.Kind == resources.KindStrictResponse) {
		return x.pin
	}
	return nil
}

func (x *Call) resolvedGenerationResources() map[string]string {
	if x.generation.resources == nil {
		return nil
	}
	return x.generation.resources.resolved
}

func (x *Call) checkGenerationResourceDestination(provider *routing.Provider, model string, destination wire.Format, source oif.Document) error {
	refs, err := generationrefs.InRequest(source, destination)
	if err != nil {
		return &oif.Incompatibility{Code: "resource_affinity", Requirement: "native_resource_destination", Message: "The destination contains an invalid resource reference."}
	}
	resolved := x.resolvedGenerationResources()
	for _, ref := range refs {
		if resolved[ref.Pointer] != ref.ID {
			return &oif.Incompatibility{Code: "resource_affinity", Field: ref.Pointer, Requirement: "verified_resource_binding", Message: "Destination defaults cannot introduce unresolved provider resources."}
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
