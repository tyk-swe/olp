package openai

import (
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/sse"
)

// Descriptor adapts historical family names to independent operation/dialect
// identities. Wire-v1 describes the checked-in codec contract, not a claim
// that a provider's moving API or model alias is immutable.
var wireDialects = map[Family]string{
	FamilyChat: "openai-chat", FamilyResponses: "openai-responses", FamilyInputTokens: "openai-input-tokens",
	FamilyAnthropic: "anthropic-messages", FamilyAnthropicCount: "anthropic-count-tokens",
	FamilyGemini: "gemini-generate-content", FamilyGeminiStream: "gemini-generate-content", FamilyGeminiCount: "gemini-count-tokens",
	FamilyEmbeddings: "openai-embeddings", FamilyModeration: "openai-moderation", FamilyRerank: "rerank",
	FamilyGeminiEmbeddings: "gemini-embeddings", FamilyGeminiEmbeddingsBatch: "gemini-embeddings-batch",
	FamilyVertexEmbeddings: "vertex-embeddings", FamilyBedrockEmbeddings: "bedrock-embeddings",
	FamilyBedrock: "bedrock-converse", "bedrock_count": "bedrock-count-tokens",
}

func Descriptor(family Family, stream bool) oif.Descriptor {
	dialect := wireDialects[family]
	if dialect == "" {
		dialect = string(family)
	}
	delivery := "unary"
	if stream {
		delivery = "incremental"
	}
	operation := family.Operation()
	if family == "bedrock_count" {
		operation = "token_count"
	}
	return oif.Descriptor{Operation: oif.Identity{ID: operation, Revision: "1"}, Dialect: oif.Identity{ID: dialect, Revision: "wire-v1"}, Profile: oif.Identity{ID: "legacy", Revision: "1"}, Execution: oif.Execution{Delivery: delivery, Lifetime: "request", Submission: "immediate", Effects: []string{"inference"}}}
}

// LiftEvent is shared by the legacy stream adapters. Framing remains owned by
// the transport decoder; JSON ambiguity is rejected before dialect projection.
func LiftEvent(family Family, data, name string, sequence uint64, maxBytes int) (oif.Event, error) {
	d := Descriptor(family, true)
	if data == "[DONE]" {
		return oif.ControlEvent(d, name, data, sequence), nil
	}
	source, err := oif.ParseJSON([]byte(data), oif.Limits{MaxBytes: maxBytes})
	if err != nil {
		return oif.Event{}, &ProtocolError{Detail: err.Error()}
	}
	return oif.NewEvent(d, source, name, sequence)
}

func LiftSSE(family Family, frame sse.Frame, sequence uint64, maxBytes int) (oif.Event, error) {
	name := ""
	if frame.Event != nil {
		name = *frame.Event
	}
	event, err := LiftEvent(family, frame.Data, name, sequence, maxBytes)
	if err != nil {
		return event, err
	}
	framing := oif.Framing{}
	if frame.ID != nil {
		framing.ID = *frame.ID
		framing.HasID = true
	}
	if frame.RetryMS != nil {
		framing.RetryMillis = *frame.RetryMS
		framing.HasRetry = true
	}
	return event.WithFraming(framing), nil
}

func liftResult(family Family, body []byte) (oif.Result, error) {
	doc, err := oif.ParseJSON(body, oif.Limits{})
	if err != nil {
		return oif.Result{}, &ProtocolError{Detail: err.Error()}
	}
	return oif.NewResult(Descriptor(family, false), doc, oif.Complete)
}
