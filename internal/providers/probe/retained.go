package probe

import (
	"context"
	"net/http"
	"net/url"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/retainedapi"
)

func (p *Prober) certifyRetained(ctx context.Context, t Target, model string, tuple connection.Capability) error {
	if tuple.Operation == "retrieval" && t.Connection.Transport().RetrievalBackend() != "" {
		return p.certifyRetrieval(ctx, t, model)
	}
	if tuple.Operation == "agent_resource" {
		return p.certifyAgentResources(ctx, t, model)
	}
	path, collection, identity := "vector_stores", "data", "id"
	query := url.Values{"limit": {"1"}}
	var header http.Header
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
		return p.certifyListedModel(ctx, t, model)
	case "container":
		path = "containers"
	case "eval":
		path = "evals"
	case "skill":
		path = "skills"
		query.Set("beta", "true")
		if t.Connection.Document().Kind == connectors.KindAnthropic {
			header = anthropicFilesBeta()
		}
	case "context_cache":
		path, collection, identity = "cachedContents", "cachedContents", "name"
		query = url.Values{"pageSize": {"1"}}
	case "retrieval", "vector_store":
		// Both operations read the same native store collection.
	default:
		return &Failure{Reason: CapabilityUnavailable, Detail: "This connector cannot certify the requested tuple."}
	}
	if !retainedapi.Supports(t.Connection.Document().Kind, tuple.Operation, tuple.Surface, tuple.Mode) {
		return &Failure{Reason: CapabilityUnavailable, Detail: "This connector cannot certify the requested tuple."}
	}
	endpoint, err := t.Connection.Transport().ResourceURL(model, path, query)
	if err != nil {
		return err
	}
	resp, err := p.do(ctx, t, request{method: http.MethodGet, path: endpoint, header: header})
	if err != nil {
		return err
	}
	status, body := resp.status, resp.body
	if status != http.StatusOK {
		return statusError(t, status, body)
	}
	source, err := oif.ParseJSON(body, oif.Limits{MaxBytes: 1 << 20})
	if err != nil || source.Root().Kind() != oif.Object {
		return &Failure{Reason: ProviderProtocolError, Detail: "The provider returned an invalid retained collection."}
	}
	items, present := source.Root().Lookup(collection)
	// Only Protobuf JSON (Gemini/Vertex) may omit an empty repeated
	// field; OpenAI and Anthropic collections must carry their array.
	if !present && (tuple.Surface == "openai" || tuple.Surface == "anthropic") {
		return &Failure{Reason: ProviderProtocolError, Detail: "The provider returned an invalid retained collection."}
	}
	if present && items.Kind() != oif.Array {
		return &Failure{Reason: ProviderProtocolError, Detail: "The provider returned an invalid retained collection."}
	}
	for _, item := range items.Elements() {
		id, _ := item.Lookup(identity)
		if value, ok := id.Text(); !ok || value == "" {
			return &Failure{Reason: ProviderProtocolError, Detail: "The provider returned a retained resource without an identity."}
		}
	}
	return nil
}

// Certification reads the native collection; it never creates a billable
// resource or infers support from a generic 404 response.
func (p *Prober) certifyAgentResources(ctx context.Context, t Target, model string) error {
	endpoint, err := t.Connection.Transport().ResourceURL(model, "agents", url.Values{"page_size": {"1"}})
	if err != nil {
		return err
	}
	status, body, err := p.call(ctx, t, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return statusError(t, status, body)
	}
	source, err := oif.ParseJSON(body, oif.Limits{MaxBytes: 1 << 20})
	if err != nil {
		return &Failure{Reason: ProviderProtocolError, Detail: "The provider returned an invalid agent collection."}
	}
	items, present := source.Root().Lookup("agents")
	if source.Root().Kind() != oif.Object || present && items.Kind() != oif.Array {
		return &Failure{Reason: ProviderProtocolError, Detail: "The provider returned an invalid agent collection."}
	}
	for _, item := range items.Elements() {
		id, _ := item.Lookup("id")
		value, ok := id.Text()
		if !ok || value == "" {
			return &Failure{Reason: ProviderProtocolError, Detail: "The provider returned an agent without an identity."}
		}
	}
	return nil
}
