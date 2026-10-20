package connectors

func registerFilesBatchesProfiles() {
	for _, item := range []struct {
		vendor        string
		operations    []string
		documentation string
	}{
		{"groq", []string{"generation", "batch"}, "https://console.groq.com/docs/api-reference"},
		{"together", []string{"generation", "batch"}, "https://docs.together.ai/reference/batch-create"},
		{"mistral", []string{"generation", "embeddings", "batch"}, "https://docs.mistral.ai/api/endpoint/batch"},
	} {
		profile := Profile{
			ID: item.vendor + "-files-batches", Revision: ProfileRevision,
			Label: item.vendor + " OpenAI-compatible files and batches",
			Kind:  "openai_compatible", Dialect: "openai-chat", DialectRevision: "unversioned-2026-09-22",
			Hosting: "direct-compatible", Authentication: []string{"api_key"}, Transport: "http",
			Operations: item.operations, SemanticHeaders: []string{}, QuerySettings: []string{},
			Documentation: item.documentation,
		}
		completeProfileMetadata(&profile)
		profileRegistry = append(profileRegistry, profile)
	}
}
