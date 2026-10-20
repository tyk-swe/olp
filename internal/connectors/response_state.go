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
	switch c.Kind {
	case "azure_openai":
		return true
	case "openai":
		return c.SupportsRetainedResponses()
	case "openai_compatible":
		if c.ProfileRevision != ProfileRevision {
			return false
		}
		switch c.ProfileID {
		case "groq-files-batches":
			return c.VendorID == "groq"
		case "together-files-batches":
			return c.VendorID == "together"
		case "mistral-files-batches":
			return c.VendorID == "mistral"
		}
	}
	return false
}

// FilesBatchesAdapter names the reviewed resource lifecycle implementation.
func (c Config) FilesBatchesAdapter() string {
	if !c.SupportsFilesBatches() {
		return ""
	}
	switch c.ProfileID {
	case "together-files-batches":
		return "together"
	case "mistral-files-batches":
		return "mistral"
	default:
		return "openai"
	}
}
