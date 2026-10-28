package routes

import "testing"

func TestWebRTCSimulationTupleIsDistinct(t *testing.T) {
	if err := validTuple("realtime", "openai", "webrtc"); err != nil {
		t.Fatalf("webrtc simulation tuple refused: %v", err)
	}
	if err := validTuple("realtime", "anthropic", "webrtc"); err == nil {
		t.Fatal("webrtc admitted on a non-openai surface")
	}
	if err := validTuple("generation", "openai", "webrtc"); err == nil {
		t.Fatal("webrtc mode leaked to generation")
	}
}
