package usage

import "github.com/tyk-swe/olp/internal/attribution"

const (
	AttributionHeader      = attribution.Header
	AttributionHeaderBytes = attribution.HeaderBytes
	AttributionMaxKeys     = attribution.MaxKeys
	AttributionMaxBytes    = attribution.MaxBytes
	AttributionKeyMax      = attribution.KeyMax
)

var (
	AttributionKeyPattern   = attribution.KeyPattern
	AttributionValuePattern = attribution.ValuePattern
)

type AttributionError = attribution.Error

func ValidateAttribution(labels map[string]string) error { return attribution.Validate(labels) }
func AttributionJSON(labels map[string]string) []byte    { return attribution.JSON(labels) }
