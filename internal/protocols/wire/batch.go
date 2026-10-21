package wire

// CanonicalBatchEndpoint selects the result dialect without changing the
// endpoint spelling sent to providers such as legacy Azure deployments.
func CanonicalBatchEndpoint(endpoint string) string {
	switch endpoint {
	case "/chat/completions", "/responses", "/completions", "/embeddings":
		return "/v1" + endpoint
	default:
		return endpoint
	}
}
