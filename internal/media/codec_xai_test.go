package media

import (
	"encoding/base64"
	"testing"
)

func TestXAIImagesTakeAspectRatios(t *testing.T) {
	request := imageRequest(2, "1536x1024", "b64_json")
	call, failure := encodeXAIImage(request, "grok-imagine-image-2.0")
	if failure != nil {
		t.Fatal(failure.Message)
	}
	if call.Path != "images/generations" || string(call.JSON) != `{"aspect_ratio":"3:2","model":"grok-imagine-image-2.0","n":2,"prompt":"a small illustration","response_format":"b64_json"}` {
		t.Fatalf("call = %s %s", call.Path, call.JSON)
	}
	quality := "high"
	for name, invalid := range map[string]*Request{
		"odd size":     imageRequest(1, "512x512", ""),
		"eleven":       imageRequest(11, "", ""),
		"quality":      {Op: OpImageGeneration, Prompt: "x", Quality: &quality},
		"streaming":    {Op: OpImageGeneration, Prompt: "x", Stream: true},
		"other format": imageRequest(1, "", "png"),
	} {
		if _, failure := encodeXAIImage(invalid, "grok-imagine-image-2.0"); failure == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	image := base64.StdEncoding.EncodeToString([]byte("png"))
	documented := `{"data":[{"b64_json":"` + image + `","mime_type":"image/png"},{"url":"https://imgen.x.ai/x.png","mime_type":"image/png"}],"usage":{"cost_in_usd_ticks":700000000,"input_tokens":12,"output_tokens":3,"total_tokens":15}}`
	result, failure := call.DecodeImages([]byte(documented), func(string, int) (*Artifact, *Error) { return &Artifact{}, nil })
	if failure != nil || len(result.Images) != 2 || result.Images[1].URL != "https://imgen.x.ai/x.png" || result.CreatedAt == 0 || result.Usage.TotalTokens != 15 {
		t.Fatalf("decoded %+v %v", result, failure)
	}
}

func TestGroqAudioReportsItsDuration(t *testing.T) {
	request := mediaProbe(OpTranscription)
	call, failure := encodeGroqAudio(request, "whisper-large-v3")
	if failure != nil {
		t.Fatal(failure.Message)
	}
	var format string
	for _, field := range call.Fields {
		if field.Name == "response_format" {
			format = *field.Text
		}
	}
	if call.Path != "audio/transcriptions" || format != "verbose_json" || call.DecodeTranscription == nil {
		t.Fatalf("call = %+v", call)
	}
	result, failure := DecodeTranscript(call, "json", []byte(`{"task":"transcribe","language":"english","duration":12.5,"text":"Hello there.","segments":[{"id":0,"start":0,"end":1.2,"text":"Hello there."}],"x_groq":{"id":"req_1"}}`))
	if failure != nil || result.DurationSeconds == nil || *result.DurationSeconds != 12.5 {
		t.Fatalf("decoded %+v %v", result, failure)
	}
	if body, _ := EncodeTranscriptionJSON(result); string(body) != `{"text":"Hello there."}` {
		t.Fatalf("json client receives %s", body)
	}
	translation := mediaProbe(OpTranslation)
	if call, failure := encodeGroqAudio(translation, "whisper-large-v3"); failure != nil || call.Path != "audio/translations" {
		t.Fatalf("translation call = %+v %v", call, failure)
	}
	for _, format := range []string{"srt", "vtt", "diarized_json"} {
		r := mediaProbe(OpTranscription)
		r.Format = &format
		if _, failure := encodeGroqAudio(r, "whisper-large-v3"); failure == nil {
			t.Fatalf("Groq accepted %s", format)
		}
	}
}
