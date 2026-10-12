package gateway

import (
	"github.com/tyk-swe/olp/internal/geminilifecycle"
	"testing"
)

func BenchmarkGeminiLiveServerObservation(b *testing.B) {
	frame := []byte(`{"serverContent":{"modelTurn":{"parts":[{"inlineData":{"mimeType":"audio/pcm;rate=24000","data":"AQIDBA=="}}]},"turnComplete":true},"usageMetadata":{"promptTokenCount":9,"responseTokenCount":5,"cachedContentTokenCount":3}}`)
	b.ReportAllocs()
	for b.Loop() {
		frame, err := geminilifecycle.ObserveLiveServerFrame(frame, 1024)
		if err != nil || frame.Kind != "serverContent" || frame.Usage == nil || !frame.TurnComplete {
			b.Fatal("lost observation", err)
		}
	}
}
