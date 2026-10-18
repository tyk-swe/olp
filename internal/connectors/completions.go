package connectors

// Each native text-completion endpoint has an explicit profile. Generic
// OpenAI-compatible hosting alone never qualifies the legacy dialect.
func registerCompletionProfiles() {
	for _, vendor := range []string{"openai", "vllm", "tgi", "fireworks", "together"} {
		kind, hosting := "openai_compatible", "direct-compatible"
		if vendor == "openai" {
			kind, hosting = "openai", "direct-openai"
		}
		p := Profile{ID: vendor + "-completions", Revision: ProfileRevision, Label: vendor + " text completions", Kind: kind, Dialect: "openai-completions", DialectRevision: "m9-baseline-1", Hosting: hosting, Authentication: []string{"api_key", "headers", "none"}, Transport: "http", Operations: []string{"generation"}, OperationDialects: map[string]string{"generation": "openai-completions"}, SemanticHeaders: []string{}, QuerySettings: []string{}, Documentation: "https://platform.openai.com/docs/api-reference/completions", Strict: true}
		completeProfileMetadata(&p)
		profileRegistry = append(profileRegistry, p)
	}
}
