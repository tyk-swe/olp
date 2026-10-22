package operations

// ContractRevision identifies the admission contract, not a codec revision or
// a claim that an endpoint is implemented. A declared family cannot install a
// dialect in Registry or make an unsupported provider tuple executable.
const ContractRevision = "execution-foundations-1"

// Contract describes the common obligations of an operation family. Endpoint
// profiles still own field-level codecs, SDK evidence and native lifecycles.
type Contract struct {
	Operation         string
	Revision          string
	Scope             string
	ProviderState     bool
	OwnerScopedIDs    bool
	DurableIfAsync    bool
	InspectionByCodec bool
}

// ContractFor returns a declaration only. Serving must separately resolve a
// qualified dialect/profile/mode; callers must never infer serving from this.
func ContractFor(operation string) (Contract, bool) {
	c := Contract{Operation: operation, Revision: ContractRevision, Scope: "inference", InspectionByCodec: true}
	switch operation {
	case "generation", "embeddings", "rerank", "classification", "tokenization", "decision", "search", "ocr", "image", "speech", "transcription", "translation", "video", "realtime", "passthrough":
		c.DurableIfAsync = true
	case "file", "batch":
		// Existing files/batches retain their original provider-state policy.
		c.OwnerScopedIDs, c.DurableIfAsync = true, true
	case "fine_tuning", "vector_store", "retrieval", "conversation", "container", "eval", "skill", "context_cache", "agent_resource":
		c.ProviderState, c.OwnerScopedIDs, c.DurableIfAsync = true, true, true
	default:
		return Contract{}, false
	}
	return c, true
}
