package probe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/providerconfig"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/vendors"
)

// ListModels asks the upstream for its model identifiers.
func (p *Prober) ListModels(ctx context.Context, t Target) ([]string, error) {
	models, err := p.ListModelFacts(ctx, t)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, m := range models {
		ids = append(ids, m.Name)
	}
	return ids, nil
}

type DiscoveredModel struct {
	Name, Display string
	Metadata      map[string]json.RawMessage
}

// A modelListing is how an upstream lists its models: a GET of path, which
// extends the endpoint unless it is an absolute URL, answered with a JSON
// object whose models field holds an array of model objects, each with its ID
// in its id field and display name in its display field. A listing with a
// cursor continues while a page holds the next page's cursor in that field,
// and, when it names a more field, reports there that another page follows.
// The next request sends the cursor back in the parameter query parameter.
type modelListing struct {
	path, models, id, display string
	cursor, more, parameter   string
}

// listingFor returns how the configured upstream lists its models, or false
// when it has no listing and operators declare its models.
func listingFor(t Target) (modelListing, bool, error) {
	if t.Connection.Transport().RetrievalBackend() != "" {
		return modelListing{}, false, nil
	}
	switch t.Connection.Document().Kind {
	case connectors.KindAzureOpenAI, connectors.KindVertexAI:
		return modelListing{}, false, nil
	case connectors.KindPlugin:
		// A plugin profile declares the listing, if its upstream has one.
		if t.Connection.Transport().Plugin == nil {
			return modelListing{}, false, nil
		}
		declared, ok := t.Connection.Transport().Plugin.Discovery()
		if !ok {
			return modelListing{}, false, nil
		}
		// Keep the rendered address's escaped option segments. call validates
		// absolute URLs without rebuilding their path from a normalized base.
		listing := modelListing{path: strings.TrimRight(providerconfig.Value(t.Connection.Document().Endpoint), "/") + declared.Path, models: declared.Models, id: declared.ID}
		if pagination := declared.Pagination; pagination != nil {
			listing.cursor, listing.more, listing.parameter = pagination.Cursor, pagination.More, pagination.Parameter
		}
		return listing, true, nil
	}
	if contract, ok := vendors.Lookup(providerconfig.Value(t.Connection.Document().Options.VendorID)); ok && !contract.Discovery {
		return modelListing{}, false, nil
	}
	listing := modelListing{path: "/models", models: "data", id: "id", display: "display_name"}
	switch t.Connection.Document().Kind {
	case connectors.KindAnthropic:
		listing.cursor, listing.more, listing.parameter = "last_id", "has_more", "after_id"
	case connectors.KindGemini:
		listing.models, listing.id, listing.display = "models", "name", "displayName"
		listing.cursor, listing.parameter = "nextPageToken", "pageToken"
	case connectors.KindWatsonx:
		// The chat-capable foundation models, in one page of the most the
		// listing returns.
		listing.path = "/ml/v1/foundation_model_specs?version=" + connectors.WatsonxVersion(providerconfig.Value(t.Connection.Document().APIVersion)) + "&filters=function_text_chat&limit=200"
		listing.models, listing.id, listing.display = "resources", "model_id", "label"
	case connectors.KindBedrock:
		listing.path, listing.models, listing.id, listing.display = "/foundation-models", "modelSummaries", "modelId", "modelName"
		if providerconfig.Value(t.Connection.Document().Endpoint) == connectors.DefaultEndpoint(t.Connection.Document().Kind, providerconfig.Value(t.Connection.Document().CloudRegion), "") {
			endpoint, err := connectors.BedrockEndpoint(providerconfig.Value(t.Connection.Document().CloudRegion), true)
			if err != nil {
				return modelListing{}, false, err
			}
			listing.path = endpoint + "/foundation-models"
		}
	}
	return listing, true, nil
}

func (p *Prober) ListModelFacts(ctx context.Context, t Target) ([]DiscoveredModel, error) {
	listing, discovery, err := listingFor(t)
	if err != nil {
		return nil, err
	}
	if !discovery {
		// Each declared model is its own bounded upstream probe.
		ctx, cancel := Budget(ctx)
		defer cancel()
		names := append([]string{}, t.ProbeModels...)
		if t.Connection.Document().Kind == connectors.KindAzureOpenAI {
			names = append(names, providerconfig.Value(t.Connection.Document().Deployment))
		}
		for name := range t.Connection.Document().Options.Models {
			names = append(names, name)
		}
		for name := range t.Connection.Document().Options.Bindings {
			names = append(names, name)
		}
		slices.Sort(names)
		if len(names) == 0 {
			return nil, &Failure{Reason: ModelRequired, Detail: "Declare a model before probing this vendor."}
		}
		out := []DiscoveredModel{}
		previous := ""
		for _, name := range names {
			if name == previous {
				continue
			}
			previous = name
			tuple := DefaultTuple(t.Connection.Transport())
			err := p.Certify(ctx, t, name, tuple, BodyLimit)
			if err != nil && t.Connection.Document().Kind == connectors.KindAzureOpenAI {
				tuple.Operation = "embeddings"
				err = p.Certify(ctx, t, name, tuple, BodyLimit)
			}
			if err != nil {
				return nil, err
			}
			out = append(out, DiscoveredModel{Name: name, Display: name})
		}
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	out := []DiscoveredModel{}
	seen, cursors := map[string]bool{}, map[string]bool{}
	path := listing.path
	for {
		status, body, err := p.call(ctx, t, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, statusError(t, status, body)
		}
		var page map[string]json.RawMessage
		if json.Unmarshal(body, &page) != nil {
			return nil, &Failure{Reason: ProviderProtocolError, Detail: "Invalid model discovery response."}
		}
		var items []map[string]json.RawMessage
		if json.Unmarshal(page[listing.models], &items) != nil || items == nil {
			return nil, &Failure{Reason: ProviderProtocolError, Detail: "Discovery response is missing its model array."}
		}
		for _, item := range items {
			var name, display string
			_ = json.Unmarshal(item[listing.id], &name)
			_ = json.Unmarshal(item[listing.display], &display)
			name = strings.TrimPrefix(name, "models/")
			if connection.ValidModelName("model", name) != nil || seen[name] {
				continue
			}
			seen[name] = true
			if display == "" {
				display = name
			}
			facts := map[string]json.RawMessage{}
			for from, to := range map[string]string{"inputTokenLimit": "context_length", "outputTokenLimit": "max_output_tokens", "inputModalities": "input_modalities", "outputModalities": "output_modalities"} {
				if v, ok := item[from]; ok {
					facts[to] = v
				}
			}
			if len(facts) > 0 {
				facts["source"], _ = json.Marshal(providerconfig.Value(t.Connection.Document().Endpoint))
				facts["observed_at"], _ = json.Marshal(time.Now().UTC())
			}
			out = append(out, DiscoveredModel{Name: name, Display: display, Metadata: facts})
			if len(out) == 2000 {
				return out, nil
			}
		}
		cursor, err := listing.next(page)
		if err != nil || cursor == "" {
			return out, err
		}
		if len(cursor) > 2048 || cursors[cursor] {
			return nil, &Failure{Reason: ProviderProtocolError, Detail: "Invalid repeated discovery continuation."}
		}
		cursors[cursor] = true
		path = listing.path + "?" + url.Values{listing.parameter: []string{cursor}}.Encode()
	}
}

// next returns the cursor of the page after page, or "" when page is the last.
func (l modelListing) next(page map[string]json.RawMessage) (string, error) {
	if l.cursor == "" {
		return "", nil
	}
	if l.more != "" {
		var more bool
		_ = json.Unmarshal(page[l.more], &more)
		if !more {
			return "", nil
		}
	}
	var cursor string
	_ = json.Unmarshal(page[l.cursor], &cursor)
	if cursor == "" && l.more != "" {
		return "", &Failure{Reason: ProviderProtocolError, Detail: "Discovery continuation is missing its cursor."}
	}
	return cursor, nil
}
