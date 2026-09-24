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
