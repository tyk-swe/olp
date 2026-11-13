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
	kinds      []string
	vendors    []string
	vendorKind bool
}

var (
	surfaces    = []string{"openai", "anthropic", "gemini", "bedrock", "native"}
	modes       = []string{"unary", "streaming", "async", "realtime", "webrtc"}
	unary       = []string{"unary"}
	generating  = []string{"unary", "streaming"}
	reviewed    = []string{KindOpenAI, KindOpenAICompatible, KindAnthropic, KindGemini, KindVertexAI, KindAzureOpenAI, KindBedrock}
	openAIKinds = []string{KindOpenAI, KindAzureOpenAI, KindOpenAICompatible}

	openAIUnary      = []offer{{surface: "openai", modes: unary, kinds: openAIKinds}}
	openAIGenerating = []offer{{surface: "openai", modes: generating, kinds: openAIKinds}}
	vendorUnary      = []offer{{surface: "openai", modes: unary, vendorKind: true}}

	nativeCloudVendors = map[string]string{KindVertexAI: "google-vertex", KindBedrock: "amazon-bedrock"}
)

var offers = map[string][]offer{
	"generation":      {{modes: generating}},
	"token_count":     {{modes: unary}},
	"bedrock_invoke":  {{surface: "bedrock", modes: generating, kinds: []string{KindBedrock}}},
	"embeddings":      {{surface: "openai", modes: unary, kinds: slices.Concat(openAIKinds, []string{KindGemini, KindVertexAI, KindBedrock})}},
	"rerank":          {{surface: "openai", modes: unary, kinds: []string{KindOpenAICompatible, KindBedrock}, vendorKind: true}},
	"ocr":             vendorUnary,
	"search":          vendorUnary,
	"moderation":      openAIUnary,
	"image_variation": openAIUnary,
	"translation":     openAIUnary,
	"image_generation": {
		{surface: "openai", modes: generating, kinds: openAIKinds},
		{surface: "openai", modes: unary, kinds: []string{KindVertexAI, KindBedrock}},
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
		{surface: "openai", modes: []string{"webrtc"}, kinds: []string{KindOpenAI}},
		{surface: "openai", modes: []string{"realtime"}, kinds: []string{KindOpenAI, KindAzureOpenAI}},
	},
	"batch": {
		{surface: "anthropic", modes: unary, kinds: []string{KindAnthropic}},
		{surface: "gemini", modes: unary, kinds: []string{KindGemini}},
		{surface: "openai", modes: unary, kinds: []string{KindGemini}, vendors: []string{"google"}},
		{surface: "openai", modes: unary, kinds: []string{KindOpenAI, KindAzureOpenAI}},
		{surface: "openai", modes: unary, kinds: []string{KindOpenAICompatible}, vendors: filesBatchesVendors()},
	},
}

func (o offer) serves(kind, vendor, surface, mode string) bool {
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
	return known && connector == kind
}

func Supports(kind, vendor, operation, surface, mode string) bool {
	if retainedapi.Operation(operation) {
		return (operation != "agent_resource" || vendor == "google") && retainedapi.Supports(kind, operation, surface, mode)
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
		return connector == kind && surface == "openai" && (mode == "unary" || creates)
	}
	return slices.ContainsFunc(offers[operation], func(o offer) bool { return o.serves(kind, vendor, surface, mode) })
}

func (c Config) Supports(operation, surface, mode string) bool {
	if c.RetrievalBackend() != "" {
		return operation == "retrieval" && surface == "openai" && mode == "unary"
	}
	if operation == "retrieval" && (c.Kind == KindBedrock || c.Kind == KindVertexAI || c.Kind == KindAzureAISearch) {
		return false
	}
	if retainedapi.Operation(operation) {
		if operation == "agent_resource" {
			return retainedapi.Supports(c.Kind, operation, surface, mode) && c.ProfileID == "gemini-agents" && c.ProfileRevision == ProfileRevision
		}
		switch c.ProfileID {
		case "gemini-live", "gemini-interactions", "vertex-batch-prediction", "bedrock-batch-inference", "mistral-fim", "xai-realtime", "vertex-realtime", "bedrock-realtime":
			return false
		}
		return retainedapi.Supports(c.Kind, operation, surface, mode)
	}
	if operation == "batch" {
		if surface == "native" {
			return mode == "unary" && (c.Kind == KindVertexAI && c.ProfileID == vertexBatchProfile && c.ProfileRevision == ProfileRevision && c.Hosting() == "vertex-google" ||
				c.Kind == KindBedrock && c.ProfileID == BedrockBatchProfile && c.ProfileRevision == ProfileRevision && c.Hosting() == "bedrock-batch-inference")
		}
		switch surface {
		case "openai":
			return c.SupportsFilesBatches() && mode == "unary"
		case "anthropic":
			return c.Kind == KindAnthropic && mode == "unary" &&
				(c.ProfileID == "" || c.ProfileID == "anthropic-messages" && c.Hosting() == "direct-anthropic")
		case "gemini":
			return c.Kind == KindGemini && mode == "unary" &&
				(c.ProfileID == "" || c.ProfileID == "gemini-generation" && c.Hosting() == "direct-gemini")
		}
		return false
	}
	if c.Kind == KindPlugin {
		p, err := c.profile()
		return err == nil && slices.Contains(p.Operations, operation) && Supports(c.Kind, c.VendorID, operation, surface, mode)
	}
	if c.ProfileID != "" {
		if p, err := c.profile(); err == nil {
			if family, ok := generationFamily(p.OperationDialect("generation")); ok && family.Surface() == "native" && operation == "generation" {
				return surface == "native" && (mode == "unary" || mode == "streaming") && slices.Contains(p.Operations, operation) && vendors.Serves(c.VendorID, operation)
			}
			switch p.Dialect {
			case "gemini-interactions":
				return operation == "generation" && surface == "gemini" && (mode == "unary" || mode == "streaming")
			case "gemini-live":
				return operation == "realtime" && surface == "gemini" && mode == "realtime"
			case "xai-realtime", "vertex-realtime", "bedrock-realtime":
				return operation == "realtime" && surface == "native" && mode == "realtime"
			}
			if codec, ok := operationregistry.Lookup(p.OperationDialect(operation)); ok && codec.Operation.ID == operation && slices.Contains(p.Operations, operation) {
				if operationregistry.Catalog().SupportsTarget(codec.Identity, surface, mode) {
					return true
				}
				if _, dedicated := operationregistry.Lookup(p.Dialect); dedicated {
					return false
				}
				return Supports(c.Kind, c.VendorID, operation, surface, mode)
			}
		}
	}

	if !Supports(c.Kind, c.VendorID, operation, surface, mode) {
		return false
	}
	if c.ProfileID == "" {
		return true
	}
	p, err := c.profile()
	return err == nil && slices.Contains(p.Operations, operation)
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
		if c.ProfileID == "" {
			return c.ProfileRevision == "" && c.Hosting() == ""
		}
		return c.ProfileID == "gemini-generation" && c.ProfileRevision == ProfileRevision && c.Hosting() == "direct-gemini"
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
