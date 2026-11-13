package connectors

import (
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/vendors"
)

type offer struct {
	surface    string
	modes      []string
	kinds      []Kind
	vendors    []string
	vendorKind bool
}

var (
	surfaces    = []string{"openai", "anthropic", "gemini", "bedrock", "native"}
	modes       = []string{"unary", "streaming", "async", "realtime", "webrtc"}
	unary       = []string{"unary"}
	generating  = []string{"unary", "streaming"}
	reviewed    = []Kind{KindOpenAI, KindOpenAICompatible, KindAnthropic, KindGemini, KindVertexAI, KindAzureOpenAI, KindBedrock}
	openAIKinds = []Kind{KindOpenAI, KindAzureOpenAI, KindOpenAICompatible}

	openAIUnary      = []offer{{surface: "openai", modes: unary, kinds: openAIKinds}}
	openAIGenerating = []offer{{surface: "openai", modes: generating, kinds: openAIKinds}}
	vendorUnary      = []offer{{surface: "openai", modes: unary, vendorKind: true}}

	nativeCloudVendors = map[Kind]string{KindVertexAI: "google-vertex", KindBedrock: "amazon-bedrock"}
)

var offers = map[string][]offer{
	"generation":      {{modes: generating}},
	"token_count":     {{modes: unary}},
	"bedrock_invoke":  {{surface: "bedrock", modes: generating, kinds: []Kind{KindBedrock}}},
	"embeddings":      {{surface: "openai", modes: unary, kinds: slices.Concat(openAIKinds, []Kind{KindGemini, KindVertexAI, KindBedrock})}},
	"rerank":          {{surface: "openai", modes: unary, kinds: []Kind{KindOpenAICompatible, KindBedrock}, vendorKind: true}},
	"ocr":             vendorUnary,
	"search":          vendorUnary,
	"moderation":      openAIUnary,
	"image_variation": openAIUnary,
	"translation":     openAIUnary,
	"image_generation": {
		{surface: "openai", modes: generating, kinds: openAIKinds},
		{surface: "openai", modes: unary, kinds: []Kind{KindVertexAI, KindBedrock}},
	},
	"image_edit":    openAIGenerating,
	"speech":        openAIGenerating,
	"transcription": openAIGenerating,
	"video_create":  {{surface: "openai", modes: []string{"async"}, kinds: openAIKinds}},
	"video_list":    openAIUnary,
	"video_get":     openAIUnary,
	"video_content": openAIUnary,
	"video_delete":  openAIUnary,
	"realtime": {
		{surface: "openai", modes: []string{"webrtc"}, kinds: []Kind{KindOpenAI}},
		{surface: "openai", modes: []string{"realtime"}, kinds: []Kind{KindOpenAI, KindAzureOpenAI}},
	},
	"batch": {
		{surface: "anthropic", modes: unary, kinds: []Kind{KindAnthropic}},
		{surface: "gemini", modes: unary, kinds: []Kind{KindGemini}},
		{surface: "openai", modes: unary, kinds: []Kind{KindGemini}, vendors: []string{"google"}},
		{surface: "openai", modes: unary, kinds: []Kind{KindOpenAI, KindAzureOpenAI}},
		{surface: "openai", modes: unary, kinds: []Kind{KindOpenAICompatible}, vendors: filesBatchesVendors()},
	},
}

func (o offer) serves(kind Kind, vendor, surface, mode string) bool {
	if o.surface != "" && o.surface != surface || !slices.Contains(o.modes, mode) {
		return false
	}
	if o.kinds != nil && !slices.Contains(o.kinds, kind) || o.vendors != nil && !slices.Contains(o.vendors, vendor) {
		return false
	}
	if !o.vendorKind {
		return true
	}
	connector, known := vendors.Kind(vendor)
	return known && Kind(connector) == kind
}

func Supports(kind Kind, vendor, operation, surface, mode string) bool {
	if retainedapi.Operation(operation) {
		return (operation != "agent_resource" || vendor == "google") && retainedapi.Supports(string(kind), operation, surface, mode)
	}
	if !slices.Contains(surfaces, surface) {
		return false
	}
	generates := operation == "generation" && slices.Contains(generating, mode)
	if surface == "native" {
		cloud, hosted := nativeCloudVendors[kind]
		switch operation {
		case "batch":
			return mode == "unary" && hosted && cloud == vendor
		case "realtime":
			return mode == "realtime" && (kind == KindOpenAICompatible && vendor == "xai" || hosted && cloud == vendor) && vendors.Serves(vendor, operation)
		case "generation":
			return generates && (kind == KindPlugin || kind == KindOpenAICompatible && (vendors.Speaks(vendor, "mistral-fim") || vendors.Speaks(vendor, "cohere-chat-v2")))
		}
	}
	if !slices.Contains(modes, mode) {
		return false
	}
	switch kind {
	case KindPlugin:
		return generates && surface != "bedrock"
	case KindSageMaker:
		return generates && surface == "openai" && vendors.Serves(vendor, operation)
	case KindWatsonx:
		return generates && surface != "bedrock" && vendors.Serves(vendor, operation)
	}
	if !slices.Contains(reviewed, kind) {
		return false
	}
	if kind == KindOpenAICompatible && (surface != "openai" || operation == "generation" && !vendors.Speaks(vendor, "openai-chat") && !vendors.Speaks(vendor, "openai-responses")) {
		return false
	}
	if surface == "bedrock" && kind != KindBedrock {
		return false
	}
	if (mode == "realtime" || mode == "webrtc") && (surface != "openai" || operation != "realtime") {
		return false
	}
	if !vendors.Serves(vendor, operation) {
		return false
	}
	if vendors.MediaWire(vendor, operation) != "" {
		connector, _ := vendors.Kind(vendor)
		creates := operation == "video_create" && mode == "async"
		return Kind(connector) == kind && surface == "openai" && (mode == "unary" || creates)
	}
	return slices.ContainsFunc(offers[operation], func(o offer) bool { return o.serves(kind, vendor, surface, mode) })
}

var (
	retrievalKinds          = []Kind{KindBedrock, KindVertexAI, KindAzureAISearch}
	withoutRetainedFamilies = []string{
		"gemini-live", "gemini-interactions", vertexBatchProfile, BedrockBatchProfile,
		"mistral-fim", "xai-realtime", "vertex-realtime", "bedrock-realtime",
	}
)

func (c Config) Supports(operation, surface, mode string) bool {
	switch {
	case c.RetrievalBackend() != "":
		return operation == "retrieval" && surface == "openai" && mode == "unary"
	case operation == "retrieval" && slices.Contains(retrievalKinds, c.Kind):
		return false
	case operation == "agent_resource":
		return c.selects("gemini-agents") && retainedapi.Supports(string(c.Kind), operation, surface, mode)
	case retainedapi.Operation(operation):
		return !slices.Contains(withoutRetainedFamilies, c.ProfileID) && retainedapi.Supports(string(c.Kind), operation, surface, mode)
	case operation == "batch":
		return mode == "unary" && c.servesBatches(surface)
	case c.ProfileID == "" && c.Kind != KindPlugin:
		return Supports(c.Kind, c.VendorID, operation, surface, mode)
	}
	p, err := c.profile()
	if err != nil || !slices.Contains(p.Operations, operation) {
		return false
	}
	if c.Kind == KindPlugin {
		return Supports(c.Kind, c.VendorID, operation, surface, mode)
	}
	return c.servesThrough(p, operation, surface, mode)
}

func (c Config) selects(profileID string) bool {
	p, err := c.profile()
	return err == nil && p.ID == profileID
}

func (c Config) servesBatches(surface string) bool {
	switch surface {
	case "native":
		return c.Kind == KindVertexAI && c.selects(vertexBatchProfile) || c.Kind == KindBedrock && c.selects(BedrockBatchProfile)
	case "openai":
		return c.SupportsFilesBatches()
	case "anthropic":
		return c.Kind == KindAnthropic && (c.ProfileID == "" || c.selects("anthropic-messages"))
	case "gemini":
		return c.Kind == KindGemini && (c.ProfileID == "" || c.selects("gemini-generation"))
	}
	return false
}

func (c Config) servesThrough(p Profile, operation, surface, mode string) bool {
	if family, ok := generationFamily(p.OperationDialect("generation")); ok && family.Surface() == "native" && operation == "generation" {
		return surface == "native" && (mode == "unary" || mode == "streaming") && vendors.Serves(c.VendorID, operation)
	}
	switch p.Dialect {
	case "gemini-interactions":
		return operation == "generation" && surface == "gemini" && (mode == "unary" || mode == "streaming")
	case "gemini-live":
		return operation == "realtime" && surface == "gemini" && mode == "realtime"
	case "xai-realtime", "vertex-realtime", "bedrock-realtime":
		return operation == "realtime" && surface == "native" && mode == "realtime"
	}
	codec, ok := operationregistry.Lookup(p.OperationDialect(operation))
	if ok && codec.Operation.ID == operation {
		if operationregistry.Catalog().SupportsTarget(codec.Identity, surface, mode) {
			return true
		}
		if _, dedicated := operationregistry.Lookup(p.Dialect); dedicated {
			return false
		}
	}
	return Supports(c.Kind, c.VendorID, operation, surface, mode)
}

func (c Config) SupportsRetainedResponses() bool {
	switch c.Kind {
	case KindAzureOpenAI:
		return true
	case KindOpenAI:
		endpoint := strings.TrimSuffix(strings.TrimSpace(c.Endpoint), "/")
		return endpoint == "" || endpoint == "https://api.openai.com/v1"
	}
	return false
}

func (c Config) SupportsFilesBatches() bool {
	switch c.Kind {
	case KindAzureOpenAI:
		return true
	case KindOpenAI:
		return c.SupportsRetainedResponses()
	case KindOpenAICompatible:
		_, ok := c.filesBatchesService()
		return ok
	case KindGemini:
		return c.ProfileID == "" && c.ProfileRevision == "" || c.selects("gemini-generation")
	}
	return false
}

func (c Config) filesBatchesService() (filesBatchesService, bool) {
	i := slices.IndexFunc(filesBatchesServices, func(s filesBatchesService) bool {
		return s.profile() == c.ProfileID && s.vendor == c.VendorID
	})
	if i < 0 || c.Kind != KindOpenAICompatible || c.ProfileRevision != ProfileRevision {
		return filesBatchesService{}, false
	}
	return filesBatchesServices[i], true
}
