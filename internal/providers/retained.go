package providers

import (
	"context"
	"net/http"
	"net/url"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/retainedapi"
)

// certifyRetained proves a retained family with one authenticated,
// read-only collection request, mirroring the agent-resource probe.
func (s *Server) certifyRetained(ctx context.Context, cfg *Configuration, credential []byte, model string, tuple CapabilityInput) error {
	if tuple.Operation == "retrieval" && cfg.transport().RetrievalBackend() != "" {
		return s.certifyRetrieval(ctx, cfg, credential, model)
	}
	if tuple.Operation == "agent_resource" {
		return s.certifyAgentResources(ctx, cfg, credential, model)
	}
	path, collection, identity := "vector_stores", "data", "id"
	query := url.Values{"limit": {"1"}}
	switch tuple.Operation {
	case "fine_tuning":
		if tuple.Surface == "native" {
			path, collection, identity = "tuningJobs", "tuningJobs", "name"
			query = url.Values{"pageSize": {"1"}}
		} else {
			path = "fine_tuning/jobs"
		}
	case "conversation":
		// Conversations have no collection endpoint in the pinned SDK
		// or retained contract, so certification proves the credential
		// and model through the supported model listing instead.
		return s.certifyListedModel(ctx, cfg, credential, model)
	case "container":
		path = "containers"
	case "eval":
		path = "evals"
	case "skill":
		path = "skills"
		query.Set("beta", "true")
	case "context_cache":
		path, collection, identity = "cachedContents", "cachedContents", "name"
		query = url.Values{"pageSize": {"1"}}
	case "retrieval", "vector_store":
		// Both operations read the same native store collection.
	default:
		return &probeError{Code: "capability_unavailable", Detail: "This connector cannot certify the requested tuple."}
	}
	if !retainedapi.Supports(cfg.Kind, tuple.Operation, tuple.Surface, tuple.Mode) {
		return &probeError{Code: "capability_unavailable", Detail: "This connector cannot certify the requested tuple."}
	}
	endpoint, err := cfg.transport().ResourceURL(model, path, query)
	if err != nil {
		return err
	}
	status, body, err := s.call(ctx, cfg, credential, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return statusError(cfg, status, body)
	}
	source, err := oif.ParseJSON(body, oif.Limits{MaxBytes: 1 << 20})
	if err != nil || source.Root().Kind() != oif.Object {
		return &probeError{Code: "provider_protocol_error", Detail: "The provider returned an invalid retained collection."}
	}
	items, present := source.Root().Lookup(collection)
	// Only Protobuf JSON (Gemini/Vertex) may omit an empty repeated
	// field; OpenAI and Anthropic collections must carry their array.
	if !present && (tuple.Surface == "openai" || tuple.Surface == "anthropic") {
		return &probeError{Code: "provider_protocol_error", Detail: "The provider returned an invalid retained collection."}
	}
	if present && items.Kind() != oif.Array {
		return &probeError{Code: "provider_protocol_error", Detail: "The provider returned an invalid retained collection."}
	}
	for _, item := range items.Elements() {
		id, _ := item.Lookup(identity)
		if value, ok := id.Text(); !ok || value == "" {
			return &probeError{Code: "provider_protocol_error", Detail: "The provider returned a retained resource without an identity."}
		}
	}
	return nil
}

// Certification reads the native collection; it never creates a billable
// resource or infers support from a generic 404 response.
func (s *Server) certifyAgentResources(ctx context.Context, cfg *Configuration, credential []byte, model string) error {
	endpoint, err := cfg.transport().ResourceURL(model, "agents", url.Values{"page_size": {"1"}})
	if err != nil {
		return err
	}
	status, body, err := s.call(ctx, cfg, credential, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return statusError(cfg, status, body)
	}
	source, err := oif.ParseJSON(body, oif.Limits{MaxBytes: 1 << 20})
	if err != nil {
		return &probeError{Code: "provider_protocol_error", Detail: "The provider returned an invalid agent collection."}
	}
	items, present := source.Root().Lookup("agents")
	// Protobuf JSON can omit an empty repeated field.
	if source.Root().Kind() != oif.Object || present && items.Kind() != oif.Array {
		return &probeError{Code: "provider_protocol_error", Detail: "The provider returned an invalid agent collection."}
	}
	for _, item := range items.Elements() {
		id, _ := item.Lookup("id")
		value, ok := id.Text()
		if !ok || value == "" {
			return &probeError{Code: "provider_protocol_error", Detail: "The provider returned an agent without an identity."}
		}
	}
	return nil
}
