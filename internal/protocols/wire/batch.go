package wire

type BatchStage uint8

const (
	BatchUnknown BatchStage = iota
	BatchValidating
	BatchInProgress
	BatchFinalizing
	BatchCancelling
	BatchTerminal
)

func BatchStageOf(status string) BatchStage {
	switch status {
	case "validating":
		return BatchValidating
	case "in_progress":
		return BatchInProgress
	case "finalizing":
		return BatchFinalizing
	case "cancelling":
		return BatchCancelling
	case "completed", "failed", "expired", "cancelled":
		return BatchTerminal
	default:
		return BatchUnknown
	}
}

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

func BatchLineOperation(endpoint string) string {
	switch CanonicalBatchEndpoint(endpoint) {
	case "/v1/embeddings":
		return "embeddings"
	case "/v1/chat/completions", "/v1/responses", "/v1/completions":
		return "generation"
	default:
		return ""
	}
}
