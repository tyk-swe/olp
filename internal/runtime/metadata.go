package runtime

import (
	"errors"
	"strings"
	"time"
)

// ModelMetadata keeps absent facts distinct from affirmative declarations.
// Privacy declarations always name their evidence and observation time.
type ModelMetadata struct {
	CanonicalModel      *string    `json:"canonical_model"`
	InputModalities     []string   `json:"input_modalities"`
	OutputModalities    []string   `json:"output_modalities"`
	ContextLength       *int64     `json:"context_length"`
	MaxOutputTokens     *int64     `json:"max_output_tokens"`
	SupportedParameters *[]string  `json:"supported_parameters"`
	Quantization        *string    `json:"quantization"`
	Region              *string    `json:"region"`
	DataCollection      *bool      `json:"data_collection"`
	ZeroDataRetention   *bool      `json:"zero_data_retention"`
	Deployment          *string    `json:"deployment"`
	Source              *string    `json:"source"`
	ObservedAt          *time.Time `json:"observed_at"`
}

func (m ModelMetadata) Validate() error {
	if m.ContextLength != nil && *m.ContextLength < 1 || m.MaxOutputTokens != nil && *m.MaxOutputTokens < 1 {
		return errors.New("model token limits must be positive")
	}
	if m.SupportedParameters != nil && len(*m.SupportedParameters) > 128 {
		return errors.New("use at most 128 supported parameters")
	}
	if m.Deployment != nil && (*m.Deployment == "" || len(*m.Deployment) > 200 || strings.ContainsAny(*m.Deployment, "\r\n\x00")) {
		return errors.New("invalid model deployment")
	}
	if (m.DataCollection != nil || m.ZeroDataRetention != nil) && (m.Source == nil || strings.TrimSpace(*m.Source) == "" || m.ObservedAt == nil) {
		return errors.New("privacy declarations require a source and observation time")
	}
	return nil
}
