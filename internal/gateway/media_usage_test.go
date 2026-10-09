package gateway

import (
	"math"
	"testing"

	"github.com/tyk-swe/olp/internal/media"
)

func TestTranscriptionAccountingBoundsDurationWithoutChangingNativeEvidence(t *testing.T) {
	for _, test := range []struct {
		name    string
		seconds float64
		want    string
	}{
		{"exact", 12.5, "12.500000"},
		{"sub-microsecond", 1.0000001, "1.000000"},
		{"rounds up", 1.1234567, "1.123457"},
		{"zero", 0, "0.000000"},
		{"negative", -1, ""},
		{"out of range", 1e18, ""},
		{"not a number", math.NaN(), ""},
		{"infinite", math.Inf(1), ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			duration := test.seconds
			result := &media.Result{Kind: media.ResponseTranscription, Transcription: &media.TranscriptionResult{DurationSeconds: &duration}}
			usage := mediaUsage(nil, result)
			if test.want == "" {
				if usage != nil {
					t.Fatal("invalid duration became billable evidence")
				}
			} else if usage == nil || usage.MediaUnits == nil || *usage.MediaUnits != test.want {
				t.Fatalf("usage=%+v, want %s seconds", usage, test.want)
			}
			if math.Float64bits(duration) != math.Float64bits(test.seconds) {
				t.Fatal("native duration changed")
			}
		})
	}
}
