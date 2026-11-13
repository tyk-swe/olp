package connectors

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/operations"
)

const (
	ProfileRevision           = "1"
	anthropicMessagesRevision = "2023-06-01"
	unversionedDialect        = "unversioned-2026-09-22"
)

var (
	keyHeadersOrNone = []string{AuthAPIKey, AuthHeaders, AuthNone}
	keyOnly          = []string{AuthAPIKey}
	azureIdentities  = []string{AuthAPIKey, AuthAzureDefault, AuthAzureClientSecret}
	googleIdentities = []string{AuthADC, AuthServiceAccount}
	awsIdentities    = []string{AuthStatic, AuthDefaultChain}

	openAIFamilyOperations = []string{
		"generation", "token_count", "embeddings", "moderation", "image_generation", "image_edit", "image_variation",
		"speech", "transcription", "translation", "video_create", "video_list", "video_get", "video_content", "video_delete",
	}
	openAIOperations     = slices.Concat(openAIFamilyOperations, []string{"batch", "realtime", "fine_tuning", "vector_store", "retrieval", "conversation", "container", "eval"})
	compatibleOperations = slices.Concat(openAIFamilyOperations, []string{"rerank"})
	azureOperations      = slices.Concat(openAIFamilyOperations, []string{"batch", "realtime", "fine_tuning", "vector_store", "retrieval", "container"})

	openAIModeration = map[string]string{"moderation": "openai-moderation"}
	openAIBeta       = []string{"Openai-Beta"}
	apiVersion       = []string{"api-version"}
	xgafv            = []string{"$xgafv"}
)

const (
	openAIGuide       = "https://developers.openai.com/api/docs/guides/migrate-to-responses"
	azureGuide        = "https://learn.microsoft.com/en-us/azure/foundry/openai/api-version-lifecycle"
	geminiGuide       = "https://ai.google.dev/api/generate-content"
	bedrockInvokeDocs = "https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_InvokeModel.html"
)

var hostedProfiles = []Profile{
	{
		ID: "bedrock-knowledge-bases", Label: "Bedrock Knowledge Bases retrieval", Kind: KindBedrock,
		Dialect: "bedrock-knowledge-bases", DialectRevision: "2023-09-30", Hosting: "bedrock-knowledge-bases",
		Authentication: awsIdentities, Operations: []string{"retrieval"},
		Documentation: "https://docs.aws.amazon.com/bedrock/latest/APIReference/API_agent-runtime_Retrieve.html",
	},
	{
		ID: "vertex-rag-engine", Label: "Vertex RAG Engine retrieval", Kind: KindVertexAI,
		Dialect: "vertex-rag-engine", DialectRevision: "v1", Hosting: "vertex-rag-engine",
		Authentication: googleIdentities, Operations: []string{"retrieval"},
		Documentation: "https://cloud.google.com/vertex-ai/generative-ai/docs/reference/rest/v1/projects.locations/retrieveContexts",
	},
	{
		ID: "azure-ai-search", Label: "Azure AI Search retrieval", Kind: KindAzureAISearch,
		Dialect: "azure-ai-search", DialectRevision: "2025-09-01", Hosting: "azure-ai-search",
		Authentication: azureIdentities, Operations: []string{"retrieval"},
		Documentation: "https://learn.microsoft.com/en-us/rest/api/searchservice/documents/search-post?view=rest-searchservice-2025-09-01",
	},
	{
		ID: "openai-chat", Label: "OpenAI Chat Completions", Kind: KindOpenAI, Dialect: "openai-chat", Hosting: "direct-openai",
		Authentication: keyHeadersOrNone, Operations: openAIOperations, OperationDialects: openAIModeration,
		SemanticHeaders: openAIBeta, Documentation: openAIGuide, Strict: true,
	},
	{
		ID: "openai-responses", Label: "OpenAI Responses", Kind: KindOpenAI, Dialect: "openai-responses", Hosting: "direct-openai",
		Authentication: keyHeadersOrNone, Operations: openAIOperations, OperationDialects: openAIModeration,
		SemanticHeaders: openAIBeta, Documentation: openAIGuide, Strict: true,
	},
	{
		ID: "compatible-chat", Label: "Compatible Chat Completions", Kind: KindOpenAICompatible, Dialect: "openai-chat", Hosting: "direct-compatible",
		Authentication: keyHeadersOrNone, Operations: compatibleOperations, OperationDialects: openAIModeration,
		SemanticHeaders: openAIBeta, Documentation: openAIGuide, Strict: true,
	},
	{
		ID: "compatible-responses", Label: "Compatible Responses", Kind: KindOpenAICompatible, Dialect: "openai-responses", Hosting: "direct-compatible",
		Authentication: keyHeadersOrNone, Operations: compatibleOperations, OperationDialects: openAIModeration,
		SemanticHeaders: openAIBeta, Documentation: openAIGuide, Strict: true,
	},
	{
		ID: "anthropic-messages", Label: "Anthropic Messages", Kind: KindAnthropic,
		Dialect: "anthropic-messages", DialectRevision: anthropicMessagesRevision, Hosting: "direct-anthropic",
		Authentication: keyHeadersOrNone, Operations: []string{"generation", "token_count", "skill", "batch"},
		SemanticHeaders: []string{"Anthropic-Version", "Anthropic-Beta"},
		Documentation:   "https://platform.claude.com/docs/en/api/versioning", Strict: true,
	},
	{
		ID: "gemini-generation", Label: "Gemini GenerateContent", Kind: KindGemini,
		Dialect: "gemini-generate-content", DialectRevision: "v1beta", Hosting: "direct-gemini",
		Authentication: keyHeadersOrNone, Operations: []string{"generation", "token_count", "embeddings", "context_cache", "batch"},
		QuerySettings: xgafv, Documentation: geminiGuide, Strict: true,
	},
	{
		ID: "gemini-interactions", Label: "Gemini Interactions", Kind: KindGemini,
		Dialect: "gemini-interactions", DialectRevision: "v1beta", Hosting: "direct-gemini-interactions",
		Authentication: keyOnly, Operations: []string{"generation"},
		QuerySettings: xgafv, Documentation: "https://ai.google.dev/gemini-api/docs/interactions-overview", Strict: true,
	},
	{
		ID: "gemini-agents", Label: "Gemini managed agents", Kind: KindGemini,
		Dialect: "gemini-agents", DialectRevision: "v1beta", Hosting: "direct-gemini-interactions",
		Authentication: keyOnly, Operations: []string{"agent_resource"},
		QuerySettings: xgafv, Documentation: "https://pkg.go.dev/google.golang.org/genai/interactions#Agents", Strict: true,
	},
	{
		ID: "gemini-live", Label: "Gemini Live", Kind: KindGemini,
		Dialect: "gemini-live", DialectRevision: "v1beta", Hosting: "direct-gemini-live",
		Authentication: keyOnly, Transport: "websocket", Operations: []string{"realtime"},
		QuerySettings: xgafv, Documentation: "https://ai.google.dev/api/live", Strict: true,
	},
	{
		ID: "xai-realtime", Label: "xAI Realtime", Kind: KindOpenAICompatible,
		Dialect: "xai-realtime", DialectRevision: "1", Hosting: "xai-realtime",
		Authentication: keyOnly, Transport: "websocket", Operations: []string{"realtime"},
		Documentation: "https://docs.x.ai/developers/model-capabilities/voice", Strict: true,
	},
	{
		ID: "vertex-realtime", Label: "Vertex AI Live", Kind: KindVertexAI,
		Dialect: "vertex-realtime", DialectRevision: "v1", Hosting: "vertex-live",
		Authentication: googleIdentities, Transport: "websocket", Operations: []string{"realtime"},
		Documentation: "https://docs.cloud.google.com/vertex-ai/generative-ai/docs/live-api/start-manage-session", Strict: true,
	},
	{
		ID: "bedrock-realtime", Label: "Bedrock Nova Sonic", Kind: KindBedrock,
		Dialect: "bedrock-realtime", DialectRevision: "v1", Hosting: "bedrock-nova-sonic",
		Authentication: awsIdentities, Transport: "bedrock-bidi", Operations: []string{"realtime"},
		Documentation: "https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_InvokeModelWithBidirectionalStream.html", Strict: true,
	},
	{
		ID: "azure-legacy-chat", Label: "Azure deployment Chat Completions", Kind: KindAzureOpenAI, Dialect: "openai-chat", Hosting: "azure-deployment",
		Authentication: azureIdentities, Operations: azureOperations, OperationDialects: openAIModeration,
		SemanticHeaders: openAIBeta, QuerySettings: apiVersion, Documentation: azureGuide, Strict: true,
	},
	{
		ID: "azure-legacy-responses", Label: "Azure legacy Responses", Kind: KindAzureOpenAI, Dialect: "openai-responses", Hosting: "azure-responses-legacy",
		Authentication: azureIdentities, Operations: azureOperations, OperationDialects: openAIModeration,
		SemanticHeaders: openAIBeta, QuerySettings: apiVersion, Documentation: azureGuide, Strict: true,
	},
	{
		ID: "azure-v1-chat", Label: "Azure v1 Chat Completions", Kind: KindAzureOpenAI,
		Dialect: "openai-chat", DialectRevision: "v1", Hosting: "azure-v1",
		Authentication: azureIdentities, Operations: azureOperations, OperationDialects: openAIModeration,
		SemanticHeaders: openAIBeta, Documentation: azureGuide, Strict: true,
	},
	{
		ID: "azure-v1-responses", Label: "Azure v1 Responses", Kind: KindAzureOpenAI,
		Dialect: "openai-responses", DialectRevision: "v1", Hosting: "azure-v1",
		Authentication: azureIdentities, Operations: azureOperations, OperationDialects: openAIModeration,
		SemanticHeaders: openAIBeta, Documentation: azureGuide, Strict: true,
	},
	{
		ID: "vertex-gemini", Label: "Vertex Google publisher", Kind: KindVertexAI,
		Dialect: "gemini-generate-content", DialectRevision: "v1", Hosting: "vertex-google",
		Authentication: googleIdentities, Operations: []string{"generation", "token_count", "embeddings", "fine_tuning", "image_generation"},
		QuerySettings: xgafv, Documentation: geminiGuide, Strict: true,
	},
	{
		ID: "vertex-batch-prediction", Label: "Vertex batch prediction", Kind: KindVertexAI,
		Dialect: "vertex-batch-prediction", DialectRevision: "v1", Hosting: "vertex-google",
		Authentication: googleIdentities, Operations: []string{"batch"}, QuerySettings: xgafv,
		Documentation: "https://cloud.google.com/vertex-ai/generative-ai/docs/model-reference/batch-prediction", Strict: true,
	},
	{
		ID: "vertex-document-processing-ocr", Label: "Google Cloud Document AI document processing", Kind: KindVertexAI,
		Dialect: "vertex-document-processing-ocr", DialectRevision: "v1", Hosting: "google-document-ai",
		Authentication: googleIdentities, Operations: []string{"ocr"},
		OperationDialects: map[string]string{"ocr": "vertex-document-processing-ocr"},
		Documentation:     "https://docs.cloud.google.com/document-ai/docs/reference/rest/v1/projects.locations.processors/process", Strict: true,
	},
	{
		ID: "bedrock-batch-inference", Label: "Bedrock batch inference", Kind: KindBedrock,
		Dialect: "bedrock-batch-inference", DialectRevision: "v1", Hosting: "bedrock-batch-inference",
		Authentication: awsIdentities, Operations: []string{"batch"},
		Documentation: "https://docs.aws.amazon.com/bedrock/latest/userguide/batch-inference.html", Strict: true,
	},
	{
		ID: "vertex-anthropic", Label: "Vertex Anthropic publisher", Kind: KindVertexAI,
		Dialect: "anthropic-messages", DialectRevision: "vertex-2023-10-16", Hosting: "vertex-anthropic",
		Authentication: googleIdentities, Operations: []string{"generation"},
		SemanticHeaders: []string{"Anthropic-Beta"}, QuerySettings: xgafv,
		Documentation: "https://platform.claude.com/docs/en/build-with-claude/claude-on-vertex-ai", Strict: true,
	},
	{
		ID: "vertex-openai", Label: "Vertex OpenAI-compatible Chat Completions", Kind: KindVertexAI,
		Dialect: "openai-chat", DialectRevision: "v1", Hosting: "vertex-openai",
		Authentication: googleIdentities, Operations: []string{"generation"},
		Documentation: "https://cloud.google.com/vertex-ai/generative-ai/docs/migrate/openai/overview", Strict: true,
	},
	{
		ID: "bedrock-converse", Label: "Bedrock Converse", Kind: KindBedrock, Dialect: "bedrock-converse", Hosting: "bedrock-converse",
		Authentication: awsIdentities, Operations: []string{"generation", "token_count", "embeddings", "image_generation"},
		Documentation: "https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_Converse.html", Strict: true,
	},
	{
		ID: "bedrock-anthropic-invoke", Label: "Bedrock Anthropic Invoke", Kind: KindBedrock,
		Dialect: "anthropic-messages", DialectRevision: "bedrock-2023-05-31", Hosting: "bedrock-anthropic-invoke",
		Authentication: awsIdentities, Operations: []string{"bedrock_invoke", "generation"},
		Documentation: bedrockInvokeDocs, Strict: true,
	},
	{
		ID: "bedrock-invoke", Label: "Bedrock model-specific Invoke", Kind: KindBedrock, Dialect: "bedrock-invoke", Hosting: "bedrock-invoke",
		Authentication: awsIdentities, Operations: []string{"bedrock_invoke"},
		Documentation: bedrockInvokeDocs, Strict: true,
	},
	{
		ID: "mistral-fim", Label: "Mistral fill-in-the-middle", Kind: KindOpenAICompatible,
		Dialect: "mistral-fim", DialectRevision: "v1", Hosting: "direct-compatible",
		Authentication: keyOnly, Operations: []string{"generation"},
		Documentation: "https://docs.mistral.ai/api/endpoint/fim", Strict: true,
	},
	{
		ID: "sagemaker-openai-chat", Label: "SageMaker OpenAI-compatible Chat Completions", Kind: KindSageMaker,
		Dialect: "openai-chat", DialectRevision: "v1", Hosting: "sagemaker-openai",
		Authentication: awsIdentities, Operations: []string{"generation"},
		Documentation: "https://docs.aws.amazon.com/sagemaker/latest/dg/realtime-endpoints-openai-compatible.html", Strict: true,
	},
}

var (
	nativelyHostedDialects = []string{
		"anthropic-count-tokens", "gemini-count-tokens", "bedrock-count-tokens",
		"gemini-embeddings", "vertex-embeddings", "bedrock-embeddings", "vertex-document-processing-ocr",
	}
	openAIOperationDialects = []string{"openai-embeddings", "openai-moderation", "openai-input-tokens", "openai-decisions"}
	keyOnlyDialects         = []string{
		"cohere-embed-v2", "cohere-rerank-v2", "mistral-ocr", "azure-document-intelligence-ocr",
		"perplexity-search", "tavily-search", "exa-search", "parallel-search", "brave-search",
	}
	cohereDialects = []string{"cohere-embed-v2", "cohere-rerank-v2"}
)

func operationProfile(d operations.Dialect) Profile {
	p := Profile{
		ID: d.Identity.ID, Label: d.Label, Kind: KindOpenAICompatible, Dialect: d.Identity.ID, DialectRevision: d.Identity.Revision,
		Hosting: "direct-compatible", Authentication: keyHeadersOrNone, Operations: []string{d.Operation.ID},
		OperationDialects: map[string]string{d.Operation.ID: d.Identity.ID}, Documentation: d.Documentation, Strict: true,
	}
	switch {
	case slices.Contains(openAIOperationDialects, p.ID):
		p.Kind, p.Hosting = KindOpenAI, "direct-openai"
	case p.ID == "gemini-batch-embeddings":
		p.Kind, p.Hosting = KindGemini, "direct-gemini"
	case slices.Contains(cohereDialects, p.ID):
		p.Authentication, p.DialectRevision = keyOnly, "v2"
	case slices.Contains(keyOnlyDialects, p.ID):
		p.Authentication = keyOnly
	}
	return p
}

func operationProfiles() []Profile {
	var out []Profile
	for _, d := range operationregistry.Catalog().Dialects() {
		if !slices.Contains(nativelyHostedDialects, d.Identity.ID) {
			out = append(out, operationProfile(d))
		}
	}
	return out
}

var cohereV2 = Profile{
	ID: "cohere-v2", Label: "Cohere native v2", Kind: KindOpenAICompatible,
	Dialect: "cohere-embed-v2", DialectRevision: "v2", Hosting: "direct-compatible",
	Authentication: keyOnly, Operations: []string{"embeddings", "rerank", "generation"},
	OperationDialects: map[string]string{"embeddings": "cohere-embed-v2", "rerank": "cohere-rerank-v2", "generation": "cohere-chat-v2"},
	Documentation:     "https://docs.cohere.com/reference/embed", Strict: true,
}

func completionProfile(vendor, kind, hosting string) Profile {
	return Profile{
		ID: vendor + "-completions", Label: vendor + " text completions", Kind: kind,
		Dialect: "openai-completions", DialectRevision: "m9-baseline-1", Hosting: hosting,
		Authentication: keyHeadersOrNone, Operations: []string{"generation"},
		OperationDialects: map[string]string{"generation": "openai-completions"},
		Documentation:     "https://platform.openai.com/docs/api-reference/completions", Strict: true,
	}
}

var completionProfiles = []Profile{
	completionProfile("openai", KindOpenAI, "direct-openai"),
	completionProfile("vllm", KindOpenAICompatible, "direct-compatible"),
	completionProfile("tgi", KindOpenAICompatible, "direct-compatible"),
	completionProfile("fireworks", KindOpenAICompatible, "direct-compatible"),
	completionProfile("together", KindOpenAICompatible, "direct-compatible"),
}

type filesBatchesService struct {
	vendor, documentation string
	adapter               BatchAdapter
	operations            []string
}

var filesBatchesServices = []filesBatchesService{
	{vendor: "groq", documentation: "https://console.groq.com/docs/api-reference", adapter: BatchOpenAI, operations: []string{"generation", "batch"}},
	{vendor: "together", documentation: "https://docs.together.ai/reference/batch-create", adapter: BatchTogether, operations: []string{"generation", "batch"}},
	{vendor: "mistral", documentation: "https://docs.mistral.ai/api/endpoint/batch", adapter: BatchMistral, operations: []string{"generation", "embeddings", "batch"}},
}

func (s filesBatchesService) profile() string {
	return s.vendor + "-files-batches"
}

func filesBatchesVendors() []string {
	out := make([]string, len(filesBatchesServices))
	for i, service := range filesBatchesServices {
		out[i] = service.vendor
	}
	return out
}

func filesBatchesProfiles() []Profile {
	out := make([]Profile, len(filesBatchesServices))
	for i, service := range filesBatchesServices {
		out[i] = Profile{
			ID: service.profile(), Label: service.vendor + " OpenAI-compatible files and batches", Kind: KindOpenAICompatible,
			Dialect: "openai-chat", Hosting: "direct-compatible", Authentication: keyOnly, Operations: service.operations,
			Documentation: service.documentation, vendor: service.vendor,
		}
	}
	return out
}

var builtIns = builtIn(slices.Concat(hostedProfiles, operationProfiles(), []Profile{cohereV2}, completionProfiles, filesBatchesProfiles()))

func builtIn(declared []Profile) []Profile {
	out := make([]Profile, len(declared))
	for i, p := range declared {
		p.Revision = ProfileRevision
		if p.Transport == "" {
			p.Transport = "http"
		}
		if p.DialectRevision == "" {
			p.DialectRevision = unversionedDialect
		}
		if p.SemanticHeaders == nil {
			p.SemanticHeaders = []string{}
		}
		if p.QuerySettings == nil {
			p.QuerySettings = []string{}
		}
		out[i] = withOperationMetadata(p)
	}
	return out
}

func withOperationMetadata(p Profile) Profile {
	dialects := maps.Clone(p.OperationDialects)
	if dialects == nil {
		dialects = map[string]string{}
	}
	schemas := map[string]json.RawMessage{}
	for _, operation := range p.Operations {
		dialects[operation] = p.OperationDialect(operation)
		schemas[operation] = defaultSchema(p.ID, dialects[operation], operation)
	}
	p.OperationDialects, p.DefaultSchemas = dialects, schemas
	return p
}

func Profiles() []Profile {
	out := make([]Profile, len(builtIns))
	for i, p := range builtIns {
		out[i] = cloneProfile(p)
	}
	return out
}

func cloneProfile(p Profile) Profile {
	p.OperationDialects = maps.Clone(p.OperationDialects)
	schemas := make(map[string]json.RawMessage, len(p.DefaultSchemas))
	for name, schema := range p.DefaultSchemas {
		schemas[name] = bytes.Clone(schema)
	}
	p.DefaultSchemas = schemas
	p.OptionsSchema = bytes.Clone(p.OptionsSchema)
	p.Authentication = slices.Clone(p.Authentication)
	p.Operations = slices.Clone(p.Operations)
	p.SemanticHeaders = slices.Clone(p.SemanticHeaders)
	p.QuerySettings = slices.Clone(p.QuerySettings)
	if p.Plugin != nil {
		plugin := *p.Plugin
		p.Plugin = &plugin
	}
	return p
}

func builtInProfile(id, revision string) (Profile, error) {
	for _, p := range builtIns {
		if p.ID == id && p.Revision == revision {
			return p, nil
		}
	}
	return Profile{}, errors.New("unknown provider profile or unsupported profile revision")
}

func profileHosting(id string) string {
	for _, p := range builtIns {
		if p.ID == id {
			return p.Hosting
		}
	}
	return ""
}

func LookupProfile(id, revision string) (Profile, error) {
	p, err := builtInProfile(id, revision)
	if err != nil {
		return Profile{}, err
	}
	return cloneProfile(p), nil
}
