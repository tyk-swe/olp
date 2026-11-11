package pricebook

import (
	"encoding/json"
	"time"
)

// quoteInput is what POST /api/v1/pricing/quote prices: one attempt's usage
// against a provider's price, discount and consumer rate card, exactly as
// accounting would record it.
type quoteInput struct {
	ProviderID string          `json:"provider_id"`
	Model      string          `json:"model"`
	Operation  string          `json:"operation"`
	Usage      json.RawMessage `json:"usage"`
	StartedAt  *time.Time      `json:"started_at"`
	Successful *bool           `json:"successful"`
	ProjectID  *string         `json:"project_id"`
}
