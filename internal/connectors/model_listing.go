package connectors

import (
	"cmp"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/vendors"
	"github.com/tyk-swe/olp/plugin/abi"
)

// A path that is an absolute URL replaces the endpoint. A listing with a cursor
// continues while a page holds the next cursor and, when it names a more
// field, while that field reports another page.
type ModelListing struct {
	Path, Models, ID, Display, TrimIDPrefix string
	Cursor, More, Parameter                 string
}

var openAIModelListing = ModelListing{Path: "/models", Models: "data", ID: "id", Display: "display_name"}

var modelListings = map[Kind]ModelListing{
	KindAnthropic: {Path: "/models", Models: "data", ID: "id", Display: "display_name", Cursor: "last_id", More: "has_more", Parameter: "after_id"},
	KindGemini:    {Path: "/models", Models: "models", ID: "name", Display: "displayName", TrimIDPrefix: "models/", Cursor: "nextPageToken", Parameter: "pageToken"},
	KindWatsonx:   {Models: "resources", ID: "model_id", Display: "label"},
	KindBedrock:   {Path: "/foundation-models", Models: "modelSummaries", ID: "modelId", Display: "modelName"},
}

var unlistedKinds = []Kind{KindAzureOpenAI, KindVertexAI}

func (l ModelListing) at(path string) ModelListing {
	return ModelListing{Path: path, Models: l.Models, ID: l.ID, Display: l.Display, TrimIDPrefix: l.TrimIDPrefix, Cursor: l.Cursor, More: l.More, Parameter: l.Parameter}
}

func (c Config) ModelListing() (ModelListing, bool, error) {
	if c.RetrievalBackend() != "" || slices.Contains(unlistedKinds, c.Kind) {
		return ModelListing{}, false, nil
	}
	if c.Kind == KindPlugin {
		listing, ok := c.pluginModelListing()
		return listing, ok, nil
	}
	if contract, ok := vendors.Lookup(c.VendorID); ok && !contract.Discovery {
		return ModelListing{}, false, nil
	}
	listing, ok := modelListings[c.Kind]
	if !ok {
		return openAIModelListing, true, nil
	}
	switch c.Kind {
	case KindWatsonx:
		// The chat-capable foundation models, in one page of the most the
		// listing returns.
		return listing.at("/ml/v1/foundation_model_specs?version=" + WatsonxVersion(c.APIVersion) + "&filters=function_text_chat&limit=200"), true, nil
	case KindBedrock:
		if c.Endpoint != DefaultEndpoint(c.Kind, c.CloudRegion, "") {
			return listing, true, nil
		}
		endpoint, err := BedrockEndpoint(c.CloudRegion, true)
		if err != nil {
			return ModelListing{}, false, err
		}
		return listing.at(endpoint + listing.Path), true, nil
	}
	return listing, true, nil
}

func (c Config) pluginModelListing() (ModelListing, bool) {
	if c.Plugin == nil {
		return ModelListing{}, false
	}
	declared, ok := c.Plugin.Discovery()
	if !ok {
		return ModelListing{}, false
	}
	pagination := cmp.Or(declared.Pagination, &abi.Pagination{})
	// Keep the rendered address's escaped option segments. The caller validates
	// absolute URLs without rebuilding their path from a normalized base.
	return ModelListing{
		Path: strings.TrimRight(c.Endpoint, "/") + declared.Path, Models: declared.Models, ID: declared.ID,
		Cursor: pagination.Cursor, More: pagination.More, Parameter: pagination.Parameter,
	}, true
}
