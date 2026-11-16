package connectors

import "testing"

func TestConfigSupportsOnlyTheOperationsItsProfileServes(t *testing.T) {
	t.Parallel()
	openAI := direct(KindOpenAI, "openai-chat", "https://api.openai.com/v1")
	proxied := direct(KindOpenAI, "openai-chat", "https://openai.example/v1")
	compatible := direct(KindOpenAICompatible, "compatible-chat", "https://compatible.example/v1")
	anthropic := direct(KindAnthropic, "anthropic-messages", "https://anthropic.example")
	gemini := direct(KindGemini, "gemini-generation", "https://gemini.example/v1beta")
	geminiLive := direct(KindGemini, "gemini-live", "https://gemini.example/v1beta")
	fim := direct(KindOpenAICompatible, "mistral-fim", "https://mistral.example/v1")
	cohere := direct(KindOpenAICompatible, "cohere-v2", "https://cohere.example/compatibility/v1")
	groqBatches := direct(KindOpenAICompatible, "groq-files-batches", "https://groq.example/openai/v1")
	groqBatches.VendorID = "groq"
	groq := Config{Kind: KindOpenAICompatible, VendorID: "groq", AuthMode: AuthAPIKey, Endpoint: "https://groq.example/openai/v1"}
	azureChat := azure("azure-legacy-chat")
	vertexGemini := vertex("vertex-gemini")
	vertexBatch := vertex("vertex-batch-prediction")
	converse := amazon(KindBedrock, "bedrock-converse")
	anthropicInvoke := amazon(KindBedrock, "bedrock-anthropic-invoke")
	knowledge := amazon(KindBedrock, "bedrock-knowledge-bases")
	sageMaker := amazon(KindSageMaker, "sagemaker-openai-chat")
	watsonx := Config{Kind: KindWatsonx, AuthMode: AuthIBMIAM, CloudRegion: "us-south", CloudProject: vertexProject}

	cases := map[string]struct {
		config                   Config
		operation, surface, mode string
		want                     bool
	}{
		"openai streams generation":                     {openAI, "generation", "openai", "streaming", true},
		"openai translates anthropic generation":        {openAI, "generation", "anthropic", "unary", true},
		"openai serves webrtc realtime":                 {openAI, "realtime", "openai", "webrtc", true},
		"openai retains conversations":                  {openAI, "conversation", "openai", "unary", true},
		"openai serves batches":                         {openAI, "batch", "openai", "unary", true},
		"openai behind another endpoint has no batches": {proxied, "batch", "openai", "unary", false},
		"openai has no rerank":                          {openAI, "rerank", "openai", "unary", false},
		"openai has no native generation":               {openAI, "generation", "native", "unary", false},
		"openai has no anthropic batches":               {openAI, "batch", "anthropic", "unary", false},
		"compatible reranks":                            {compatible, "rerank", "openai", "unary", true},
		"compatible has no batches":                     {compatible, "batch", "openai", "unary", false},
		"compatible has no realtime":                    {compatible, "realtime", "openai", "realtime", false},
		"compatible retains no conversations":           {compatible, "conversation", "openai", "unary", false},
		"anthropic serves its own batches":              {anthropic, "batch", "anthropic", "unary", true},
		"anthropic retains skills":                      {anthropic, "skill", "anthropic", "unary", true},
		"anthropic has no embeddings":                   {anthropic, "embeddings", "openai", "unary", false},
		"anthropic has no openai batches":               {anthropic, "batch", "openai", "unary", false},
		"gemini serves its own batches":                 {gemini, "batch", "gemini", "unary", true},
		"gemini serves openai batches":                  {gemini, "batch", "openai", "unary", true},
		"gemini retains context caches":                 {gemini, "context_cache", "gemini", "unary", true},
		"gemini has no image generation":                {gemini, "image_generation", "openai", "unary", false},
		"gemini live serves realtime":                   {geminiLive, "realtime", "gemini", "realtime", true},
		"gemini live has no generation":                 {geminiLive, "generation", "gemini", "unary", false},
		"azure serves websocket realtime":               {azureChat, "realtime", "openai", "realtime", true},
		"azure has no webrtc realtime":                  {azureChat, "realtime", "openai", "webrtc", false},
		"azure retains fine tuning":                     {azureChat, "fine_tuning", "openai", "unary", true},
		"azure retains no conversations":                {azureChat, "conversation", "openai", "unary", false},
		"vertex streams gemini generation":              {vertexGemini, "generation", "gemini", "streaming", true},
		"vertex retains native tuning":                  {vertexGemini, "fine_tuning", "native", "unary", true},
		"vertex generates images":                       {vertexGemini, "image_generation", "openai", "unary", true},
		"vertex generation has no batch":                {vertexGemini, "batch", "native", "unary", false},
		"vertex batch serves native batches":            {vertexBatch, "batch", "native", "unary", true},
		"vertex batch has no generation":                {vertexBatch, "generation", "openai", "unary", false},
		"bedrock streams converse":                      {converse, "generation", "bedrock", "streaming", true},
		"bedrock counts tokens":                         {converse, "token_count", "bedrock", "unary", true},
		"bedrock converse has no invoke":                {converse, "bedrock_invoke", "bedrock", "unary", false},
		"bedrock anthropic invoke streams invoke":       {anthropicInvoke, "bedrock_invoke", "bedrock", "streaming", true},
		"bedrock anthropic invoke has no token count":   {anthropicInvoke, "token_count", "openai", "unary", false},
		"bedrock knowledge bases retrieve":              {knowledge, "retrieval", "openai", "unary", true},
		"bedrock knowledge bases have no generation":    {knowledge, "generation", "openai", "unary", false},
		"sagemaker streams chat":                        {sageMaker, "generation", "openai", "streaming", true},
		"sagemaker has no anthropic generation":         {sageMaker, "generation", "anthropic", "unary", false},
		"mistral fim streams native generation":         {fim, "generation", "native", "streaming", true},
		"mistral fim has no openai generation":          {fim, "generation", "openai", "unary", false},
		"cohere reranks natively":                       {cohere, "rerank", "native", "unary", true},
		"cohere has no openai generation":               {cohere, "generation", "openai", "unary", false},
		"groq files and batches serve batches":          {groqBatches, "batch", "openai", "unary", true},
		"groq files and batches have no embeddings":     {groqBatches, "embeddings", "openai", "unary", false},
		"automatic groq transcribes":                    {groq, "transcription", "openai", "unary", true},
		"automatic groq has no embeddings":              {groq, "embeddings", "openai", "unary", false},
		"automatic watsonx translates anthropic":        {watsonx, "generation", "anthropic", "streaming", true},
		"automatic watsonx has no embeddings":           {watsonx, "embeddings", "openai", "unary", false},
		"no profile serves an unknown mode":             {openAI, "generation", "openai", "duplex", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := tc.config.Supports(tc.operation, tc.surface, tc.mode); got != tc.want {
				t.Errorf("Supports(%q, %q, %q) = %t, want %t", tc.operation, tc.surface, tc.mode, got, tc.want)
			}
		})
	}
}

func TestFilesAndBatchesUseTheAdapterOfTheProviderThatServesThem(t *testing.T) {
	t.Parallel()
	reviewed := func(profile, vendor string) Config {
		c := direct(KindOpenAICompatible, profile, "https://vendor.example/v1")
		c.VendorID = vendor
		return c
	}
	cases := map[string]struct {
		config Config
		want   BatchAdapter
		served bool
	}{
		"openai":                         {direct(KindOpenAI, "openai-chat", "https://api.openai.com/v1"), BatchOpenAI, true},
		"openai behind another endpoint": {direct(KindOpenAI, "openai-chat", "https://openai.example/v1"), "", false},
		"azure":                          {azure("azure-legacy-chat"), BatchOpenAI, true},
		"gemini":                         {direct(KindGemini, "gemini-generation", "https://gemini.example/v1beta"), BatchGeminiManaged, true},
		"groq":                           {reviewed("groq-files-batches", "groq"), BatchOpenAI, true},
		"together":                       {reviewed("together-files-batches", "together"), BatchTogether, true},
		"mistral":                        {reviewed("mistral-files-batches", "mistral"), BatchMistral, true},
		"a profile on another vendor":    {reviewed("groq-files-batches", "together"), "", false},
		"an unreviewed compatible":       {reviewed("compatible-chat", ""), "", false},
		"anthropic":                      {direct(KindAnthropic, "anthropic-messages", "https://anthropic.example"), "", false},
		"vertex generation":              {vertex("vertex-gemini"), "", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, served := tc.config.FilesBatchesAdapter()
			if got != tc.want || served != tc.served {
				t.Errorf("adapter = %q, %t, want %q, %t", got, served, tc.want, tc.served)
			}
		})
	}
}
