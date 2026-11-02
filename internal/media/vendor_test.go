package media

import (
	"slices"
	"testing"
)

func TestWiresListsEveryVendorCodec(t *testing.T) {
	wires := Wires()
	want := []string{
		"assemblyai", "bfl", "deepgram", "elevenlabs", "gemini", "groq-audio",
		"polly", "recraft", "runway", "stability", "xai-images",
	}
	got := make([]string, 0, len(wires))
	for wire, operations := range wires {
		got = append(got, wire)
		if len(operations) == 0 {
			t.Fatalf("wire %q serves no operations", wire)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("wires = %v, want %v", got, want)
	}
	if operations := wires["groq-audio"]; !slices.Equal(operations, []string{OpTranscription, OpTranslation}) {
		t.Fatalf("groq-audio operations = %v, want transcription and translation", operations)
	}
	if operations := wires["runway"]; !slices.Equal(operations, []string{OpVideoContent, OpVideoCreate, OpVideoDelete, OpVideoGet}) {
		t.Fatalf("runway operations = %v, want the four video operations", operations)
	}
}
