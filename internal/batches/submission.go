package batches

import (
	"encoding/json"
	"fmt"
)

type Submission interface {
	Source() string
	submissionType() string
}

type FilesSubmission struct {
	InputFileID      string          `json:"input_file_id"`
	LocalInputFileID string          `json:"local_input_file_id"`
	Endpoint         string          `json:"endpoint"`
	CompletionWindow string          `json:"completion_window"`
	ClientMetadata   json.RawMessage `json:"client_metadata,omitempty"`
}

type GeminiManagedSubmission struct {
	GeminiFile       string          `json:"gemini_file"`
	LocalInputFileID string          `json:"local_input_file_id"`
	Endpoint         string          `json:"endpoint"`
	Metadata         json.RawMessage `json:"gemini_metadata,omitempty"`
}

type GeminiFileSubmission struct {
	GeminiFile       string `json:"gemini_file"`
	LocalInputFileID string `json:"local_input_file_id"`
}

type GeminiInlineSubmission struct{}

type VertexSubmission struct {
	Label string `json:"vertex_label"`
}

type BedrockSubmission struct {
	ClientRequestToken string          `json:"bedrock_client_request_token"`
	CreateBody         json.RawMessage `json:"create_body"`
}

type AnthropicSubmission struct{}

func (s FilesSubmission) Source() string         { return s.InputFileID }
func (s GeminiManagedSubmission) Source() string { return s.GeminiFile }
func (s GeminiFileSubmission) Source() string    { return s.GeminiFile }
func (GeminiInlineSubmission) Source() string    { return "" }
func (VertexSubmission) Source() string          { return "" }
func (BedrockSubmission) Source() string         { return "" }
func (AnthropicSubmission) Source() string       { return "" }

func (FilesSubmission) submissionType() string         { return "files" }
func (GeminiManagedSubmission) submissionType() string { return "gemini_managed" }
func (GeminiFileSubmission) submissionType() string    { return "gemini_file" }
func (GeminiInlineSubmission) submissionType() string  { return "gemini_inline" }
func (VertexSubmission) submissionType() string        { return "vertex" }
func (BedrockSubmission) submissionType() string       { return "bedrock" }
func (AnthropicSubmission) submissionType() string     { return "anthropic" }

type submissionEnvelope[T any] struct {
	Type     string `json:"type"`
	Identity T      `json:"identity"`
}

func encodeSubmission(submission Submission) ([]byte, error) {
	return json.Marshal(submissionEnvelope[Submission]{Type: submission.submissionType(), Identity: submission})
}

func decodeSubmission(raw []byte) (Submission, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var envelope submissionEnvelope[json.RawMessage]
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("decode batch submission: %w", err)
	}
	switch envelope.Type {
	case "files":
		return decodeIdentity[FilesSubmission](envelope.Identity)
	case "gemini_managed":
		return decodeIdentity[GeminiManagedSubmission](envelope.Identity)
	case "gemini_file":
		return decodeIdentity[GeminiFileSubmission](envelope.Identity)
	case "gemini_inline":
		return GeminiInlineSubmission{}, nil
	case "vertex":
		return decodeIdentity[VertexSubmission](envelope.Identity)
	case "bedrock":
		return decodeIdentity[BedrockSubmission](envelope.Identity)
	case "anthropic":
		return AnthropicSubmission{}, nil
	default:
		return nil, fmt.Errorf("unknown batch submission type %q", envelope.Type)
	}
}

func decodeIdentity[T Submission](raw json.RawMessage) (Submission, error) {
	var identity T
	if err := json.Unmarshal(raw, &identity); err != nil {
		return nil, fmt.Errorf("decode batch submission identity: %w", err)
	}
	return identity, nil
}
