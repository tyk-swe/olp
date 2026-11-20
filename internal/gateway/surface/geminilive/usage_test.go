package geminilive

import (
	"math"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/geminilifecycle"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

func TestModalitySharesSaturateOnOverflow(t *testing.T) {
	overflowing := &wire.Usage{}
	geminiLiveModality(overflowing, &geminilifecycle.LiveUsage{PromptDetails: []geminilifecycle.LiveUsageDetail{
		{Modality: "IMAGE", TokenCount: math.MaxInt64},
		{Modality: "VIDEO", TokenCount: math.MaxInt64},
	}})
	if overflowing.ImageInputTokens == nil || *overflowing.ImageInputTokens <= 0 {
		t.Errorf("overflowing image share = %v, want a saturated positive share", overflowing.ImageInputTokens)
	}
}
