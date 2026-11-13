package connectors

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

const (
	vertexRegion  = "us-central1"
	vertexProject = "fixture-project"
	awsRegion     = "us-east-1"

	vertexLocation = "https://us-central1-aiplatform.googleapis.com/v1/projects/fixture-project/locations/us-central1"
	bedrockRuntime = "https://bedrock-runtime.us-east-1.amazonaws.com"
)

func direct(kind, profile, endpoint string) Config {
	return Config{Kind: kind, ProfileID: profile, ProfileRevision: ProfileRevision, AuthMode: AuthAPIKey, Endpoint: endpoint}
}

func azure(profile string) Config {
	return Config{
		Kind: KindAzureOpenAI, ProfileID: profile, ProfileRevision: ProfileRevision, AuthMode: AuthAPIKey,
		Endpoint: "https://fixture.openai.azure.com", APIVersion: "2024-10-21",
		Models: map[string]json.RawMessage{"gpt": json.RawMessage(`{"deployment":"dep-1"}`)},
	}
}

func vertex(profile string) Config {
	return Config{
		Kind: KindVertexAI, ProfileID: profile, ProfileRevision: ProfileRevision, AuthMode: AuthADC,
		CloudRegion: vertexRegion, CloudProject: vertexProject,
		Endpoint: DefaultProfileEndpoint(KindVertexAI, profile, vertexRegion, vertexProject),
	}
}

func amazon(kind, profile string) Config {
	return Config{
		Kind: kind, ProfileID: profile, ProfileRevision: ProfileRevision, AuthMode: AuthStatic,
		CloudRegion: awsRegion, Endpoint: DefaultProfileEndpoint(kind, profile, awsRegion, ""),
	}
}

func dialect(t *testing.T, id string) func(Config, string) (string, error) {
	t.Helper()
	d, ok := operationregistry.Lookup(id)
	if !ok {
		t.Fatalf("operation dialect %q is not registered", id)
	}
	return func(c Config, model string) (string, error) { return c.OperationURL(d, model) }
}

type address func() (string, error)

func TestConfigAddressesEachUpstreamAtTheURLItsHostingDocuments(t *testing.T) {
	t.Parallel()
	openAI := direct(KindOpenAI, "openai-chat", "https://openai.example/v1")
	responses := direct(KindOpenAI, "openai-responses", "https://openai.example/v1")
	compatible := direct(KindOpenAICompatible, "compatible-chat", "https://compatible.example/v1")
	anthropic := direct(KindAnthropic, "anthropic-messages", "https://anthropic.example")
	gemini := direct(KindGemini, "gemini-generation", "https://gemini.example/v1beta")
	bound := direct(KindGemini, "gemini-generation", "https://gemini.example/v1beta")
	bound.Bindings = map[string]Binding{"alias": {Model: "gemini-real"}}
	interactions := direct(KindGemini, "gemini-interactions", "https://gemini.example/v1beta")
	geminiLive := direct(KindGemini, "gemini-live", "https://gemini.example/v1beta")
	cohere := direct(KindOpenAICompatible, "cohere-v2", "https://cohere.example/compatibility/v1")
	xai := direct(KindOpenAICompatible, "xai-realtime", "https://xai.example/v1")
	xai.VendorID = "xai"
	azureChat := azure("azure-legacy-chat")
	azureResponses := azure("azure-legacy-responses")
	azureV1 := direct(KindAzureOpenAI, "azure-v1-chat", "https://fixture.openai.azure.com")
	search := direct(KindAzureAISearch, "azure-ai-search", "https://fixture.search.windows.net")
	search.APIVersion = "2025-09-01"
	vertexGemini := vertex("vertex-gemini")
	vertexAnthropic := vertex("vertex-anthropic")
	vertexOpenAI := vertex("vertex-openai")
	vertexLive := vertex("vertex-realtime")
	vertexLive.VendorID = "google-vertex"
	rag := vertex("vertex-rag-engine")
	vertexBatch := vertex("vertex-batch-prediction")
	converse := amazon(KindBedrock, "bedrock-converse")
	invoke := amazon(KindBedrock, "bedrock-invoke")
	anthropicInvoke := amazon(KindBedrock, "bedrock-anthropic-invoke")
	sonic := amazon(KindBedrock, "bedrock-realtime")
	sonic.VendorID = "amazon-bedrock"
	knowledge := amazon(KindBedrock, "bedrock-knowledge-bases")
	bedrockBatch := amazon(KindBedrock, "bedrock-batch-inference")
	sageMaker := amazon(KindSageMaker, "sagemaker-openai-chat")
	watsonx := Config{
		Kind: KindWatsonx, AuthMode: AuthIBMIAM, CloudRegion: "us-south", CloudProject: vertexProject,
		Endpoint: "https://us-south.ml.cloud.ibm.example",
	}
	arn := "arn:aws:bedrock:us-east-1::foundation-model/nova"
	corpus := "projects/fixture-project/locations/us-central1/ragCorpora/123"

	cases := map[string]struct {
		url  address
		want string
	}{
		"openai chat":                             {func() (string, error) { return openAI.URL(wire.Chat, "gpt", false) }, "https://openai.example/v1/chat/completions"},
		"openai responses":                        {func() (string, error) { return responses.URL(wire.Responses, "gpt", true) }, "https://openai.example/v1/responses"},
		"openai embeddings":                       {func() (string, error) { return openAI.URL(wire.Embeddings, "gpt", false) }, "https://openai.example/v1/embeddings"},
		"compatible chat":                         {func() (string, error) { return compatible.URL(wire.Chat, "llama", true) }, "https://compatible.example/v1/chat/completions"},
		"anthropic messages":                      {func() (string, error) { return anthropic.URL(wire.Anthropic, "claude", true) }, "https://anthropic.example/messages"},
		"anthropic token count":                   {func() (string, error) { return anthropic.URL(wire.AnthropicCount, "claude", false) }, "https://anthropic.example/messages/count_tokens"},
		"gemini generation":                       {func() (string, error) { return gemini.URL(wire.Gemini, "gemini-x", false) }, "https://gemini.example/v1beta/models/gemini-x:generateContent"},
		"gemini streaming":                        {func() (string, error) { return gemini.URL(wire.Gemini, "gemini-x", true) }, "https://gemini.example/v1beta/models/gemini-x:streamGenerateContent?alt=sse"},
		"gemini token count":                      {func() (string, error) { return gemini.URL(wire.GeminiCount, "gemini-x", false) }, "https://gemini.example/v1beta/models/gemini-x:countTokens"},
		"gemini bound model":                      {func() (string, error) { return bound.URL(wire.Gemini, "alias", false) }, "https://gemini.example/v1beta/models/gemini-real:generateContent"},
		"azure deployment bound to the model":     {func() (string, error) { return azureChat.URL(wire.Chat, "gpt", false) }, "https://fixture.openai.azure.com/openai/deployments/dep-1/chat/completions?api-version=2024-10-21"},
		"azure legacy responses":                  {func() (string, error) { return azureResponses.URL(wire.Responses, "gpt", false) }, "https://fixture.openai.azure.com/openai/responses?api-version=2024-10-21"},
		"azure v1 chat":                           {func() (string, error) { return azureV1.URL(wire.Chat, "gpt", false) }, "https://fixture.openai.azure.com/openai/v1/chat/completions"},
		"vertex gemini":                           {func() (string, error) { return vertexGemini.URL(wire.Gemini, "gemini-x", false) }, vertexLocation + "/publishers/google/models/gemini-x:generateContent"},
		"vertex gemini streaming":                 {func() (string, error) { return vertexGemini.URL(wire.Gemini, "gemini-x", true) }, vertexLocation + "/publishers/google/models/gemini-x:streamGenerateContent?alt=sse"},
		"vertex embeddings":                       {func() (string, error) { return vertexGemini.URL(wire.VertexEmbeddings, "embed-x", false) }, vertexLocation + "/publishers/google/models/embed-x:predict"},
		"vertex anthropic":                        {func() (string, error) { return vertexAnthropic.URL(wire.Anthropic, "claude", false) }, vertexLocation + "/publishers/anthropic/models/claude:rawPredict"},
		"vertex anthropic streaming":              {func() (string, error) { return vertexAnthropic.URL(wire.Anthropic, "claude", true) }, vertexLocation + "/publishers/anthropic/models/claude:streamRawPredict"},
		"vertex openai":                           {func() (string, error) { return vertexOpenAI.URL(wire.Chat, "meta/llama", false) }, vertexLocation + "/endpoints/openapi/chat/completions"},
		"bedrock converse":                        {func() (string, error) { return converse.URL(wire.Bedrock, "nova", false) }, bedrockRuntime + "/model/nova/converse"},
		"bedrock converse streaming":              {func() (string, error) { return converse.URL(wire.Bedrock, "nova", true) }, bedrockRuntime + "/model/nova/converse-stream"},
		"bedrock invoke":                          {func() (string, error) { return invoke.URL(wire.BedrockInvoke, "nova", false) }, bedrockRuntime + "/model/nova/invoke"},
		"bedrock invoke streaming":                {func() (string, error) { return invoke.URL(wire.BedrockInvoke, "nova", true) }, bedrockRuntime + "/model/nova/invoke-with-response-stream"},
		"bedrock anthropic invoke":                {func() (string, error) { return anthropicInvoke.URL(wire.Anthropic, "claude", true) }, bedrockRuntime + "/model/claude/invoke-with-response-stream"},
		"sagemaker endpoint":                      {func() (string, error) { return sageMaker.URL(wire.Chat, "endpoint", false) }, "https://runtime.sagemaker.us-east-1.amazonaws.com/endpoints/endpoint/openai/v1/chat/completions"},
		"sagemaker inference component":           {func() (string, error) { return sageMaker.URL(wire.Chat, "endpoint/component", false) }, "https://runtime.sagemaker.us-east-1.amazonaws.com/endpoints/endpoint/inference-components/component/openai/v1/chat/completions"},
		"watsonx chat":                            {func() (string, error) { return watsonx.URL(wire.Chat, "ibm/granite", false) }, "https://us-south.ml.cloud.ibm.example/ml/v1/text/chat?version=2025-10-25"},
		"watsonx chat streaming":                  {func() (string, error) { return watsonx.URL(wire.Chat, "ibm/granite", true) }, "https://us-south.ml.cloud.ibm.example/ml/v1/text/chat_stream?version=2025-10-25"},
		"cohere rerank":                           {func() (string, error) { return cohere.URL(wire.Rerank, "rerank-x", false) }, "https://cohere.example/compatibility/v1/rerank"},
		"cohere rerank operation":                 {func() (string, error) { return dialect(t, "cohere-rerank-v2")(cohere, "rerank-x") }, "https://cohere.example/compatibility/v1/rerank"},
		"bedrock token count escapes the model":   {func() (string, error) { return dialect(t, "bedrock-count-tokens")(converse, arn) }, bedrockRuntime + "/model/arn:aws:bedrock:us-east-1::foundation-model%2Fnova/count-tokens"},
		"vertex embeddings operation":             {func() (string, error) { return dialect(t, "vertex-embeddings")(vertexGemini, "embed-x") }, vertexLocation + "/publishers/google/models/embed-x:predict"},
		"openai media":                            {func() (string, error) { return openAI.MediaURL("images/generations", "gpt", nil) }, "https://openai.example/v1/images/generations"},
		"azure media on the bound deployment":     {func() (string, error) { return azureChat.MediaURL("images/generations", "gpt", url.Values{"n": {"1"}}) }, "https://fixture.openai.azure.com/openai/deployments/dep-1/images/generations?api-version=2024-10-21&n=1"},
		"openai resource":                         {func() (string, error) { return openAI.ResourceURL("gpt", "files", url.Values{"limit": {"2"}}) }, "https://openai.example/v1/files?limit=2"},
		"azure resource carries the api version":  {func() (string, error) { return azureChat.ResourceURL("gpt", "files", nil) }, "https://fixture.openai.azure.com/openai/files?api-version=2024-10-21"},
		"vertex resource under the publisher":     {func() (string, error) { return vertexGemini.ResourceURL("gemini-x", "cachedContents", nil) }, vertexLocation + "/publishers/google/cachedContents"},
		"vertex tuning jobs under the location":   {func() (string, error) { return vertexGemini.ResourceURL("gemini-x", "tuningJobs/1", nil) }, vertexLocation + "/tuningJobs/1"},
		"openai realtime socket":                  {func() (string, error) { return openAI.RealtimeURL("gpt") }, "wss://openai.example/v1/realtime?model=gpt"},
		"openai realtime calls":                   {func() (string, error) { return openAI.RealtimeCallsURL("gpt") }, "https://openai.example/v1/realtime/calls"},
		"openai realtime control socket":          {func() (string, error) { return openAI.RealtimeControlURL("gpt", "call_1") }, "wss://openai.example/v1/realtime?call_id=call_1"},
		"openai realtime hangup":                  {func() (string, error) { return openAI.RealtimeHangupURL("gpt", "call_1") }, "https://openai.example/v1/realtime/calls/call_1/hangup"},
		"azure realtime socket on the deployment": {func() (string, error) { return azureChat.RealtimeURL("gpt") }, "wss://fixture.openai.azure.com/openai/realtime?api-version=2024-10-21&deployment=dep-1"},
		"xai duplex socket":                       {func() (string, error) { return xai.DuplexURL("grok") }, "wss://xai.example/v1/realtime?model=grok"},
		"vertex duplex socket":                    {func() (string, error) { return vertexLive.DuplexURL("gemini-live") }, "wss://us-central1-aiplatform.googleapis.com/ws/google.cloud.aiplatform.v1.LlmBidiService/BidiGenerateContent"},
		"vertex duplex model resource":            {func() (string, error) { return vertexLive.DuplexBoundModel("gemini-live") }, "projects/fixture-project/locations/us-central1/publishers/google/models/gemini-live"},
		"gemini live socket":                      {geminiLive.GeminiLiveURL, "wss://gemini.example/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"},
		"bedrock duplex endpoint":                 {sonic.BedrockDuplexEndpoint, bedrockRuntime},
		"gemini interactions collection":          {func() (string, error) { return interactions.InteractionsURL("", "") }, "https://gemini.example/v1beta/interactions"},
		"gemini interaction":                      {func() (string, error) { return interactions.InteractionsURL("abc", "") }, "https://gemini.example/v1beta/interactions/abc"},
		"gemini interaction cancel":               {func() (string, error) { return interactions.InteractionsURL("abc", "cancel") }, "https://gemini.example/v1beta/interactions/abc/cancel"},
		"bedrock knowledge base retrieval":        {func() (string, error) { return knowledge.RetrievalURL("KB12345678") }, "https://bedrock-agent-runtime.us-east-1.amazonaws.com/knowledgebases/KB12345678/retrieve"},
		"vertex rag corpus retrieval":             {func() (string, error) { return rag.RetrievalURL(corpus) }, vertexLocation + ":retrieveContexts"},
		"azure search index retrieval":            {func() (string, error) { return search.RetrievalURL("idx") }, "https://fixture.search.windows.net/indexes/idx/docs/search?api-version=2025-09-01"},
		"azure search index probe":                {func() (string, error) { return search.RetrievalProbeURL("idx") }, "https://fixture.search.windows.net/indexes/idx?api-version=2025-09-01"},
		"vertex batch jobs":                       {func() (string, error) { return vertexBatch.VertexBatchJobsURL("", "") }, vertexLocation + "/batchPredictionJobs"},
		"vertex batch job":                        {func() (string, error) { return vertexBatch.VertexBatchJobsURL("job1", "") }, vertexLocation + "/batchPredictionJobs/job1"},
		"vertex batch job cancel":                 {func() (string, error) { return vertexBatch.VertexBatchJobsURL("job1", "cancel") }, vertexLocation + "/batchPredictionJobs/job1:cancel"},
		"bedrock batch jobs":                      {func() (string, error) { return bedrockBatch.BedrockBatchJobsURL("", "") }, "https://bedrock.us-east-1.amazonaws.com/model-invocation-jobs"},
		"bedrock batch job create":                {func() (string, error) { return bedrockBatch.BedrockBatchJobsURL("", "create") }, "https://bedrock.us-east-1.amazonaws.com/model-invocation-job"},
		"bedrock batch job stop":                  {func() (string, error) { return bedrockBatch.BedrockBatchJobsURL("job1", "stop") }, "https://bedrock.us-east-1.amazonaws.com/model-invocation-job/job1/stop"},
		"bedrock batch job id is escaped":         {func() (string, error) { return bedrockBatch.BedrockBatchJobsURL("job/1", "") }, "https://bedrock.us-east-1.amazonaws.com/model-invocation-job/job%2F1"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.url()
			if err != nil {
				t.Fatalf("address refused: %v", err)
			}
			if got != tc.want {
				t.Errorf("address = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestConfigRefusesAnAddressItsProfileDoesNotServe(t *testing.T) {
	t.Parallel()
	openAI := direct(KindOpenAI, "openai-chat", "https://openai.example/v1")
	stale := direct(KindOpenAI, "openai-chat", "https://openai.example/v1")
	stale.ProfileRevision = "0"
	anthropic := direct(KindAnthropic, "anthropic-messages", "https://anthropic.example")
	gemini := direct(KindGemini, "gemini-generation", "https://gemini.example/v1beta")
	interactions := direct(KindGemini, "gemini-interactions", "https://gemini.example/v1beta")
	azureChat := azure("azure-legacy-chat")
	vertexLive := vertex("vertex-realtime")
	vertexLive.VendorID = "google-vertex"
	rag := vertex("vertex-rag-engine")
	vertexBatch := vertex("vertex-batch-prediction")
	bedrockBatch := amazon(KindBedrock, "bedrock-batch-inference")
	sageMaker := amazon(KindSageMaker, "sagemaker-openai-chat")
	foreign := "projects/other/locations/us-central1"

	cases := map[string]address{
		"a wire the profile does not speak":    func() (string, error) { return openAI.URL(wire.Anthropic, "gpt", false) },
		"an unknown profile revision":          func() (string, error) { return stale.URL(wire.Chat, "gpt", false) },
		"a model without an azure deployment":  func() (string, error) { return azureChat.URL(wire.Chat, "unbound", false) },
		"a sagemaker wire other than chat":     func() (string, error) { return sageMaker.URL(wire.Responses, "endpoint", false) },
		"a resource path that leaves the base": func() (string, error) { return openAI.ResourceURL("gpt", "a/../b", nil) },
		"a caller query that names the api version": func() (string, error) {
			return azureChat.ResourceURL("gpt", "files", url.Values{"api-version": {"other"}})
		},
		"a media path that is absolute":             func() (string, error) { return openAI.MediaURL("/images/generations", "gpt", nil) },
		"a hangup for a call id with a path":        func() (string, error) { return openAI.RealtimeHangupURL("gpt", "a/b") },
		"realtime on a profile without it":          func() (string, error) { return anthropic.RealtimeURL("claude") },
		"a duplex socket on an http profile":        func() (string, error) { return openAI.DuplexURL("gpt") },
		"a duplex model in another project":         func() (string, error) { return vertexLive.DuplexBoundModel(foreign + "/publishers/google/models/x") },
		"interactions on a generation profile":      func() (string, error) { return gemini.InteractionsURL("", "") },
		"an interaction action that does not exist": func() (string, error) { return interactions.InteractionsURL("abc", "zap") },
		"a rag corpus in another project":           func() (string, error) { return rag.RetrievalURL(foreign + "/ragCorpora/123") },
		"retrieval on a generation profile":         func() (string, error) { return openAI.RetrievalURL("idx") },
		"vertex batch jobs on a bedrock provider":   func() (string, error) { return bedrockBatch.VertexBatchJobsURL("", "") },
		"a vertex batch action that does not exist": func() (string, error) { return vertexBatch.VertexBatchJobsURL("job1", "stop") },
	}
	for name, refused := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got, err := refused(); err == nil {
				t.Errorf("address = %q, want a refusal", got)
			}
		})
	}
}

func TestDefaultEndpointPlacesACloudProviderOnItsRegionalHost(t *testing.T) {
	t.Parallel()
	cases := map[string]struct{ kind, profile, region, want string }{
		"vertex region":              {KindVertexAI, "vertex-gemini", "us-central1", vertexLocation + "/publishers/google"},
		"vertex global":              {KindVertexAI, "vertex-gemini", "global", "https://aiplatform.googleapis.com/v1/projects/fixture-project/locations/global/publishers/google"},
		"vertex us multi-region":     {KindVertexAI, "vertex-gemini", "us", "https://aiplatform.us.rep.googleapis.com/v1/projects/fixture-project/locations/us/publishers/google"},
		"vertex eu multi-region":     {KindVertexAI, "vertex-gemini", "eu", "https://aiplatform.eu.rep.googleapis.com/v1/projects/fixture-project/locations/eu/publishers/google"},
		"vertex anthropic publisher": {KindVertexAI, "vertex-anthropic", "us-central1", vertexLocation + "/publishers/anthropic"},
		"vertex openai endpoint":     {KindVertexAI, "vertex-openai", "us-central1", vertexLocation + "/endpoints/openapi"},
		"vertex document ai":         {KindVertexAI, "vertex-document-processing-ocr", "us", "https://us-documentai.googleapis.com/v1/projects/fixture-project/locations/us"},
		"vertex rag engine":          {KindVertexAI, "vertex-rag-engine", "us-central1", vertexLocation},
		"vertex live":                {KindVertexAI, "vertex-realtime", "us-central1", vertexLocation + "/publishers/google"},
		"vertex without a profile":   {KindVertexAI, "", "us-central1", vertexLocation + "/publishers/google"},
		"bedrock runtime":            {KindBedrock, "bedrock-converse", "us-east-1", bedrockRuntime},
		"bedrock knowledge bases":    {KindBedrock, "bedrock-knowledge-bases", "us-east-1", "https://bedrock-agent-runtime.us-east-1.amazonaws.com"},
		"bedrock batch inference":    {KindBedrock, "bedrock-batch-inference", "us-east-1", bedrockRuntime},
		"bedrock in china":           {KindBedrock, "bedrock-converse", "cn-north-1", "https://bedrock-runtime.cn-north-1.amazonaws.com.cn"},
		"sagemaker runtime":          {KindSageMaker, "sagemaker-openai-chat", "us-east-1", "https://runtime.sagemaker.us-east-1.amazonaws.com"},
		"watsonx":                    {KindWatsonx, "", "us-south", "https://us-south.ml.cloud.ibm.com"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := DefaultProfileEndpoint(tc.kind, tc.profile, tc.region, vertexProject); got != tc.want {
				t.Errorf("endpoint = %q, want %q", got, tc.want)
			}
		})
	}
}
