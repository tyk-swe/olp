package connectors

import "strings"

// SupportsRetainedResponses identifies the native resource-addressing contracts
// implemented by the existing Responses resource owner. A compatible JSON
// generation dialect alone does not establish provider resource lifecycle APIs.
func (c Config) SupportsRetainedResponses() bool {
	switch c.Kind {
	case "azure_openai":
		return true
	case "openai":
		endpoint := strings.TrimSuffix(strings.TrimSpace(c.Endpoint), "/")
		return endpoint == "" || endpoint == "https://api.openai.com/v1"
	default:
		return false
	}
}

// SupportsFilesBatches identifies provider configurations with qualified
// owner-scoped file and batch lifecycle endpoints.
func (c Config) SupportsFilesBatches() bool {
	return c.FilesBatchesAdapter() != ""
}

// FilesBatchesAdapter names the reviewed resource lifecycle implementation.
func (c Config) FilesBatchesAdapter() string {
	switch c.Kind {
	case "azure_openai":
		return "openai"
	case "openai":
		if c.SupportsRetainedResponses() {
			return "openai"
		}
	case "openai_compatible":
		if c.ProfileRevision != ProfileRevision {
			return ""
		}
		switch c.ProfileID {
		case "groq-files-batches":
			if c.VendorID == "groq" {
				return "openai"
			}
		case "together-files-batches":
			if c.VendorID == "together" {
				return "together"
			}
		case "mistral-files-batches":
			if c.VendorID == "mistral" {
				return "mistral"
			}
		}
	case "gemini":
		if c.ProfileID == "" {
			if c.ProfileRevision == "" && c.Hosting() == "" {
				return "gemini_managed"
			}
			return ""
		}
		if c.ProfileID == "gemini-generation" && c.ProfileRevision == ProfileRevision && c.Hosting() == "direct-gemini" {
			return "gemini_managed"
		}
	}
	return ""
}
