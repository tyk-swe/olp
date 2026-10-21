package media

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
)

func speechRequest(voice, format string, speed *float64) *Request {
	r := &Request{Op: OpSpeech, Route: "route", Input: "Hello there.", Voice: voice, Speed: speed}
	if format != "" {
		r.Format = &format
	}
	return r
}

func TestPollySpeechIsSignedForPolly(t *testing.T) {
	cfg := connectors.Config{Kind: "bedrock", AuthMode: "static", CloudRegion: "eu-west-1", VendorID: "amazon-bedrock"}
	call, failure := encodePollySpeech(speechRequest("Joanna", "opus", nil), cfg, "neural")
	if failure != nil {
		t.Fatal(failure.Message)
	}
	if call.URL != "https://polly.eu-west-1.amazonaws.com/v1/speech" || call.SigningService != "polly" || call.CharacterHeader != "X-Amzn-Requestcharacters" ||
		string(call.JSON) != `{"Engine":"neural","OutputFormat":"ogg_opus","Text":"Hello there.","VoiceId":"Joanna"}` {
		t.Fatalf("call = %+v %s", call, call.JSON)
	}
	for name, test := range map[string]struct {
		r     *Request
		model string
	}{
		"pcm":    {speechRequest("Joanna", "pcm", nil), "neural"},
		"engine": {speechRequest("Joanna", "", nil), "tts-1"},
		"voice":  {speechRequest("", "", nil), "neural"},
		"speed":  {speechRequest("Joanna", "", new(1.5)), "neural"},
	} {
		if _, failure := encodePollySpeech(test.r, cfg, test.model); failure == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	secret := []byte(`{"access_key_id":"ABCDEFGHIJKLMNOP","secret_access_key":"abcdefghijklmnopabcdefghijklmnop"}`)
	signed := cfg
	signed.SigningService = call.SigningService
	req, _ := http.NewRequest(http.MethodPost, call.URL, strings.NewReader(string(call.JSON)))
	if _, err := connectors.NewAuth(&egress.Policy{}).Apply(t.Context(), req, signed, secret, call.JSON); err != nil || !strings.Contains(req.Header.Get("Authorization"), "/eu-west-1/polly/aws4_request") {
		t.Fatalf("Polly request signed as %q: %v", req.Header.Get("Authorization"), err)
	}
}
