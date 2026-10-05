package estimate

import "testing"

func TestFamilyOf(t *testing.T) {
	for family, models := range map[Family][]string{
		FamilyOpenAIO200k: {
			"gpt-4o", "gpt-4o-mini", "gpt-4o-2024-08-06", "GPT-4o", "  gpt-4o  ", "gpt-4o-audio-preview",
			"gpt-4.1", "gpt-4.1-mini", "gpt-4.1-nano-2025-04-14", "gpt-4.5-preview",
			"gpt-5", "gpt-5-mini", "gpt-5.1", "gpt-5.2-codex", "gpt-5-pro",
			"o1", "o1-mini", "o1-preview", "o3", "o3-mini", "o3-deep-research", "o4-mini", "o4-mini-deep-research",
			"chatgpt-4o-latest", "gpt-oss-120b", "gpt-oss-20b",
			// Provider and routing prefixes, and variant suffixes, are not part of the model.
			"openai/gpt-4o", "azure/gpt-4o-mini", "openrouter/openai/gpt-oss-120b", "gpt-4o:online",
			// A fine-tune uses its base model's encoding, including bases tiktoken's
			// own ft:gpt-4 prefix would send to cl100k_base.
			"ft:gpt-4o-mini-2024-07-18:acme::abc123", "ft:gpt-4o:acme::abc123", "ft:gpt-4.1-mini-2025-04-14:acme::abc123",
		},
		FamilyOpenAICL100k: {
			"gpt-4", "gpt-4-turbo", "gpt-4-turbo-preview", "gpt-4-0613", "gpt-4-32k", "gpt-4-vision-preview",
			"gpt-3.5-turbo", "gpt-3.5-turbo-0125", "gpt-3.5-turbo-instruct", "gpt-3.5",
			"gpt-35-turbo", "gpt-35-turbo-16k",
			"text-embedding-3-small", "text-embedding-3-large", "text-embedding-ada-002",
			"davinci-002", "babbage-002",
			"ft:gpt-3.5-turbo-0125:acme::abc123", "ft:gpt-4-0613:acme::abc123", "ft:davinci-002:acme::abc123",
		},
		FamilyAnthropic: {
			"claude-3-5-sonnet-20241022", "claude-sonnet-4-5", "claude-opus-4-1-20250805", "Claude-3-Opus", "claude",
			"claude-3-haiku@20240307",
			"anthropic.claude-3-sonnet-20240229-v1:0", "us.anthropic.claude-sonnet-4-20250514-v1:0",
			"anthropic/claude-3.5-sonnet:beta",
			"arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.anthropic.claude-3-7-sonnet-20250219-v1:0",
		},
		FamilyGemini: {
			"gemini-2.5-pro", "gemini-1.5-flash-002", "gemini-embedding-001",
			"models/gemini-2.0-flash", "publishers/google/models/gemini-pro", "google/gemini-2.5-flash",
		},
		FamilyOther: {
			"", " ", "llama-3.1-70b-instruct", "meta-llama/Llama-3.3-70B-Instruct", "mistral-large-latest",
			"deepseek-chat", "qwen2.5-coder-32b", "command-r-plus", "sonar-pro", "grok-4", "gemma-2-9b", "llama3:8b",
			// Names that only contain a recognized name: a deployment's name is its
			// owner's, and says nothing about what it runs.
			"my-gpt4o-deployment", "prod-gpt-4o", "gpt4o", "gpt", "gpt-3", "o", "o2", "o4",
			// The encodings the estimator does not carry.
			"text-davinci-003", "davinci", "gpt-2", "code-davinci-002",
			// Parts, not substrings.
			"claudette", "claudeclaude", "notgemini", "geminis",
			"text-embedding-004",
		},
	} {
		for _, model := range models {
			if got := FamilyOf(model); got != family {
				t.Errorf("FamilyOf(%q) = %q, want %q", model, got, family)
			}
		}
	}
}

// TestUnknownModelsNeverUseATokenizer is the registry's safety rule: only a
// recognized model gets an encoding, and everything else is counted by the
// heuristic, whatever it is called.
func TestUnknownModelsNeverUseATokenizer(t *testing.T) {
	for _, model := range []string{"", "my-deployment", "prod-gpt-4o", "llama-3", "mistral-large", "text-davinci-003"} {
		c := ForModel(model)
		if c.encoding != nil {
			t.Errorf("%q resolved to the %s encoding", model, c.encoding.Name())
		}
		if _, provenance := c.Count("some prompt text"); provenance != ProvenanceHeuristic {
			t.Errorf("%q: provenance %q, want %q", model, provenance, ProvenanceHeuristic)
		}
	}
}

func TestForModelPicksTheEncoding(t *testing.T) {
	for model, want := range map[string]*Encoding{
		"gpt-4o": O200kBase, "o3-mini": O200kBase, "gpt-5": O200kBase, "openai/gpt-oss-120b": O200kBase,
		"gpt-4": CL100kBase, "gpt-3.5-turbo": CL100kBase, "text-embedding-3-small": CL100kBase,
		"claude-sonnet-4-5": nil, "gemini-2.5-pro": nil, "llama-3": nil,
	} {
		if got := ForModel(model).encoding; got != want {
			t.Errorf("ForModel(%q) uses %v, want %v", model, got, want)
		}
	}
	if got := ForModel("gpt-4o").Family(); got != FamilyOpenAIO200k {
		t.Errorf("family = %q", got)
	}
	var zero Counter
	if zero.Family() != FamilyOther {
		t.Errorf("the zero counter's family is %q, want %q", zero.Family(), FamilyOther)
	}
	if n, provenance := zero.Count("hello"); n != 2 || provenance != ProvenanceHeuristic {
		t.Errorf("the zero counter counts hello as %d, %s", n, provenance)
	}
}

// TestProvenanceNamesAreTheStoredStrings pins the strings usage records keep,
// which the usage package validates as plain strings.
func TestProvenanceNamesAreTheStoredStrings(t *testing.T) {
	for got, want := range map[string]string{
		string(ProvenanceTokenizer):  "tokenizer",
		string(ProvenanceCalibrated): "calibrated",
		string(ProvenanceHeuristic):  "heuristic",
		string(FamilyOpenAIO200k):    "openai-o200k",
		string(FamilyOpenAICL100k):   "openai-cl100k",
		string(FamilyAnthropic):      "anthropic",
		string(FamilyGemini):         "gemini",
		string(FamilyOther):          "other",
	} {
		if got != want {
			t.Errorf("constant is %q, want %q", got, want)
		}
	}
}

// TestCatalogFactorsCalibrateOnlyFamiliesWithoutATokenizer keeps a measured
// factor from ever scaling a family the estimator counts exactly.
func TestCatalogFactorsCalibrateOnlyFamiliesWithoutATokenizer(t *testing.T) {
	for family, factor := range familyFactors() {
		if family != FamilyAnthropic && family != FamilyGemini && family != FamilyOther || factor <= 0 {
			t.Fatalf("the catalog calibrates %s by %v", family, factor)
		}
	}
	for _, model := range []string{"claude-sonnet-4-5", "gemini-2.5-pro", "mystery-model"} {
		counter := ForModel(model)
		if counter.factor != familyFactors()[counter.Family()] {
			t.Fatalf("%s counts with factor %v, the catalog says %v", model, counter.factor, familyFactors()[counter.Family()])
		}
	}
}
