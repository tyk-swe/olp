package connectors

import ()

func vertexOpenAI() Config {
	return Config{Kind: "vertex_ai", AuthMode: "adc", ProfileID: "vertex-openai", ProfileRevision: ProfileRevision,
		CloudRegion: "us-central1", CloudProject: "acme-prod", VendorID: "google-vertex",
		Endpoint: DefaultProfileEndpoint("vertex_ai", "vertex-openai", "us-central1", "acme-prod")}
}
