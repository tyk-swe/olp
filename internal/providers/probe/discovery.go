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
	"github.com/tyk-swe/olp/internal/connectors/providerconfig"
	"github.com/tyk-swe/olp/internal/domain/vendors"
	"github.com/tyk-swe/olp/internal/providers/connection"
)

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
	HasDisplay    bool
	Metadata      map[string]json.RawMessage
}

// A path that is an absolute URL replaces the endpoint. A listing with a cursor
// continues while a page holds the next cursor and, when it names a more
// field, while that field reports another page.
type modelListing struct {
	path, models, id, display, trimIDPrefix string
	cursor, more, parameter                 string
}

func listingFor(t Target) (modelListing, bool, error) {
	if t.Connection.Transport().RetrievalBackend() != "" {
		return modelListing{}, false, nil
	}
	switch t.Connection.Document().Kind {
	case connectors.KindAzureOpenAI, connectors.KindVertexAI:
		return modelListing{}, false, nil
	case connectors.KindPlugin:
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
		listing.trimIDPrefix = "models/"
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

type Listing struct {
	Models   []DiscoveredModel
	Complete bool
	Detail   string
}

func (p *Prober) ListCompleteModelFacts(ctx context.Context, t Target) (Listing, error) {
	return p.listModelFacts(ctx, t, true)
}

func (p *Prober) ListModelFacts(ctx context.Context, t Target) ([]DiscoveredModel, error) {
	listing, err := p.listModelFacts(ctx, t, false)
	return listing.Models, err
}

func (p *Prober) listModelFacts(ctx context.Context, t Target, strict bool) (Listing, error) {
	listing, discovery, err := listingFor(t)
	if err != nil {
		return Listing{}, err
	}
	if !discovery {
		return p.probeDeclaredModels(ctx, t)
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	out := []DiscoveredModel{}
	seen, cursors := map[string]bool{}, map[string]bool{}
	path := listing.path
	for {
		status, body, err := p.call(ctx, t, http.MethodGet, path, nil)
		if err != nil {
			return Listing{}, err
		}
		if status != http.StatusOK {
			return Listing{}, statusError(t, status, body)
		}
		var page map[string]json.RawMessage
		if json.Unmarshal(body, &page) != nil {
			return Listing{}, &Failure{Reason: ProviderProtocolError, Detail: "Invalid model discovery response."}
		}
		var items []map[string]json.RawMessage
		if json.Unmarshal(page[listing.models], &items) != nil || items == nil {
			return Listing{}, &Failure{Reason: ProviderProtocolError, Detail: "Discovery response is missing its model array."}
		}
		for _, item := range items {
			var id, display string
			if strict {
				rawID, hasID := item[listing.id]
				if !hasID || json.Unmarshal(rawID, &id) != nil {
					return Listing{}, &Failure{Reason: ProviderProtocolError, Detail: "Discovery response holds a model without a valid identifier."}
				}
				if rawDisplay, hasDisplay := item[listing.display]; hasDisplay && json.Unmarshal(rawDisplay, &display) != nil {
					return Listing{}, &Failure{Reason: ProviderProtocolError, Detail: "Discovery response holds a model without a valid display name."}
				}
			} else {
				_ = json.Unmarshal(item[listing.id], &id)
				_ = json.Unmarshal(item[listing.display], &display)
			}
			name := strings.TrimPrefix(id, listing.trimIDPrefix)
			if strict && connection.ValidModelName("model", name) != nil {
				return Listing{}, &Failure{Reason: ProviderProtocolError, Detail: "Discovery response holds a model without a valid identifier."}
			}
			if connection.ValidModelName("model", name) != nil || seen[name] {
				continue
			}
			seen[name] = true
			hasDisplay := display != ""
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
			out = append(out, DiscoveredModel{Name: name, Display: display, HasDisplay: hasDisplay, Metadata: facts})
			if len(out) == 2000 && !strict {
				return Listing{Models: out}, nil
			}
			if len(out) > 2000 {
				return Listing{Models: out[:2000], Detail: "The model listing exceeds the 2,000 model bound, so completeness cannot be proved."}, nil
			}
		}
		if strict {
			cursor, final, incomplete, err := listing.strictNext(page)
			if err != nil {
				return Listing{}, err
			}
			if final {
				return Listing{Models: out, Complete: incomplete == "", Detail: incomplete}, nil
			}
			if len(cursor) > 2048 || cursors[cursor] {
				return Listing{}, &Failure{Reason: ProviderProtocolError, Detail: "Invalid repeated discovery continuation."}
			}
			cursors[cursor] = true
			path = listing.path + "?" + url.Values{listing.parameter: []string{cursor}}.Encode()
			continue
		}
		cursor, err := listing.next(page)
		if err != nil || cursor == "" {
			return Listing{Models: out}, err
		}
		if len(cursor) > 2048 || cursors[cursor] {
			return Listing{}, &Failure{Reason: ProviderProtocolError, Detail: "Invalid repeated discovery continuation."}
		}
		cursors[cursor] = true
		path = listing.path + "?" + url.Values{listing.parameter: []string{cursor}}.Encode()
	}
}

func (p *Prober) probeDeclaredModels(ctx context.Context, t Target) (Listing, error) {
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
		return Listing{}, &Failure{Reason: ModelRequired, Detail: "Declare a model before probing this vendor."}
	}
	out := []DiscoveredModel{}
	previous := ""
	for _, name := range names {
		if name == previous {
			continue
		}
		previous = name
		tuple := DefaultTuple(t.Connection.Transport())
		err := p.Certify(ctx, t, name, tuple)
		if err != nil && t.Connection.Document().Kind == connectors.KindAzureOpenAI {
			tuple.Operation = "embeddings"
			err = p.Certify(ctx, t, name, tuple)
		}
		if err != nil {
			return Listing{}, err
		}
		out = append(out, DiscoveredModel{Name: name, Display: name})
	}
	return Listing{Models: out, Detail: "This provider has no model listing that can prove completeness."}, nil
}

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

func (l modelListing) strictNext(page map[string]json.RawMessage) (cursor string, final bool, incomplete string, err error) {
	if l.more == "" || l.cursor == "" || l.parameter == "" {
		return "", true, "The model listing does not state whether another page follows, so completeness cannot be proved.", nil
	}
	rawMore, ok := page[l.more]
	if !ok {
		return "", true, "The model listing is missing its pagination field, so completeness cannot be proved.", nil
	}
	var decoded any
	if json.Unmarshal(rawMore, &decoded) != nil {
		return "", true, "The model listing holds a malformed pagination field, so completeness cannot be proved.", nil
	}
	more, isBool := decoded.(bool)
	if !isBool {
		return "", true, "The model listing holds a malformed pagination field, so completeness cannot be proved.", nil
	}
	if !more {
		return "", true, "", nil
	}
	rawCursor, ok := page[l.cursor]
	if !ok || json.Unmarshal(rawCursor, &cursor) != nil || cursor == "" {
		return "", false, "", &Failure{Reason: ProviderProtocolError, Detail: "Discovery continuation is missing its cursor."}
	}
	return cursor, false, "", nil
}
