package media

import (
	"encoding/json"
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

func TestElevenLabsSpeech(t *testing.T) {
	speed := 1.1
	call, failure := encodeElevenLabsSpeech(speechRequest("21m00Tcm4TlvDq8ikWAM", "pcm", &speed), "eleven_multilingual_v2")
	if failure != nil {
		t.Fatal(failure.Message)
	}
	if call.Path != "text-to-speech/21m00Tcm4TlvDq8ikWAM" || call.Query.Get("output_format") != "pcm_24000" || call.CharacterHeader != "Character-Cost" ||
		string(call.JSON) != `{"model_id":"eleven_multilingual_v2","text":"Hello there.","voice_settings":{"speed":1.1}}` {
		t.Fatalf("call = %+v %s", call, call.JSON)
	}
	fast := 2.0
	instructions := "cheerful"
	for name, invalid := range map[string]*Request{
		"aac":          speechRequest("v", "aac", nil),
		"fast":         speechRequest("v", "", &fast),
		"no voice":     speechRequest("", "", nil),
		"instructions": {Op: OpSpeech, Input: "x", Voice: "v", Instructions: &instructions},
	} {
		if _, failure := encodeElevenLabsSpeech(invalid, "eleven_multilingual_v2"); failure == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

func TestDeepgramSpeechNamesItsVoiceInTheModel(t *testing.T) {
	call, failure := encodeDeepgramSpeech(speechRequest("thalia", "wav", nil), "aura-2-thalia-en")
	if failure != nil {
		t.Fatal(failure.Message)
	}
	if call.Path != "speak" || call.Query.Encode() != "container=wav&encoding=linear16&model=aura-2-thalia-en" || string(call.JSON) != `{"text":"Hello there."}` || call.CharacterHeader != "Dg-Char-Count" {
		t.Fatalf("call = %+v %s", call, call.JSON)
	}
	if _, failure := encodeDeepgramSpeech(speechRequest("alloy", "", nil), "aura-2-thalia-en"); failure == nil {
		t.Fatal("a voice the model does not speak in was accepted")
	}
}

func TestVendorTranscriptsReadAsOpenAIFormats(t *testing.T) {
	for _, test := range []struct {
		name   string
		encode func(*Request, string) (*UpstreamCall, *Error)
		body   string
	}{
		{"elevenlabs", encodeElevenLabsTranscription, `{"language_code":"en","language_probability":0.98,"text":"Hello world.","transcription_id":"t1","audio_duration_secs":2.5,
			"words":[{"text":"Hello","type":"word","start":0.1,"end":0.5},{"text":" ","type":"spacing","start":0.5,"end":0.6},{"text":"world.","type":"word","start":0.6,"end":1.0}]}`},
		{"deepgram", encodeDeepgramTranscription, `{"metadata":{"request_id":"r1","duration":2.5,"channels":1},"results":{"channels":[{"alternatives":[{"transcript":"Hello world.","confidence":0.98,
			"words":[{"word":"hello","start":0.1,"end":0.5,"confidence":0.99,"punctuated_word":"Hello"},{"word":"world","start":0.6,"end":1.0,"confidence":0.99}]}],"detected_language":"en"}]}}`},
	} {
		for _, format := range []string{"json", "verbose_json"} {
			r := mediaProbe(OpTranscription)
			r.Format = &format
			call, failure := test.encode(r, "model")
			if failure != nil {
				t.Fatalf("%s: %s", test.name, failure.Message)
			}
			result, failure := DecodeTranscript(call, format, []byte(test.body))
			if failure != nil || result.DurationSeconds == nil || *result.DurationSeconds != 2.5 {
				t.Fatalf("%s decoded %+v %v", test.name, result, failure)
			}
			body, _ := EncodeTranscriptionJSON(result)
			var rendered map[string]any
			_ = json.Unmarshal(body, &rendered)
			if format == "json" && string(body) != `{"text":"Hello world."}` {
				t.Fatalf("%s json client receives %s", test.name, body)
			}
			if words, _ := rendered["words"].([]any); format == "verbose_json" && (rendered["language"] != "en" || rendered["duration"] != 2.5 || len(words) != 2) {
				t.Fatalf("%s verbose_json client receives %s", test.name, body)
			}
		}
		srt := "srt"
		r := mediaProbe(OpTranscription)
		r.Format = &srt
		if _, failure := test.encode(r, "model"); failure == nil {
			t.Fatalf("%s accepted srt", test.name)
		}
	}
	deepgram, _ := encodeDeepgramTranscription(mediaProbe(OpTranscription), "nova-3")
	if deepgram.Upload == nil || deepgram.Fields != nil || deepgram.Query.Get("model") != "nova-3" {
		t.Fatalf("Deepgram does not send the raw audio: %+v", deepgram)
	}
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
