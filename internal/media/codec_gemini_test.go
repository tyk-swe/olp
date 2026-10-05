package media

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestGeminiImagesAnswerFromGenerateContent(t *testing.T) {
	call, failure := encodeGeminiImage(imageRequest(1, "1536x1024", "b64_json"), "gemini-3.1-flash-image")
	if failure != nil {
		t.Fatal(failure.Message)
	}
	if call.Path != "models/gemini-3.1-flash-image:generateContent" ||
		string(call.JSON) != `{"contents":[{"parts":[{"text":"a small illustration"}],"role":"user"}],"generationConfig":{"imageConfig":{"aspectRatio":"3:2"},"responseModalities":["IMAGE"]}}` {
		t.Fatalf("call = %s %s", call.Path, call.JSON)
	}
	if imagen, failure := encodeGeminiImage(imageRequest(1, "", ""), "imagen-4.0-generate-001"); failure != nil || !strings.HasSuffix(imagen.Path, ":predict") {
		t.Fatalf("Imagen kept its predict API: %+v %v", imagen, failure)
	}
	stage := func(string, int) (*Artifact, *Error) { return &Artifact{Handle: "staged"}, nil }
	image := base64.StdEncoding.EncodeToString([]byte("png"))
	result, failure := call.DecodeImages([]byte(`{"candidates":[{"content":{"parts":[{"text":"Here is the image."},{"inlineData":{"mimeType":"image/png","data":"`+image+`"}}],"role":"model"},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":1290,"totalTokenCount":1297}}`), stage)
	if failure != nil || len(result.Images) != 1 || result.Usage.OutputTokens != 1290 {
		t.Fatalf("decoded %+v %v", result, failure)
	}
	for _, withheld := range []string{
		`{"candidates":[{"finishReason":"IMAGE_SAFETY"}]}`,
		`{"promptFeedback":{"blockReason":"SAFETY"}}`,
		`{"candidates":[{"content":{"parts":[{"text":"I cannot draw that."}]},"finishReason":"STOP"}]}`,
	} {
		if _, failure := call.DecodeImages([]byte(withheld), stage); failure == nil || failure.Status != 400 {
			t.Fatalf("%s: %+v", withheld, failure)
		}
	}
}

func TestGeminiSpeechBecomesTheRequestedFormat(t *testing.T) {
	pcm := []byte{1, 0, 2, 0, 3, 0}
	answer := func(mimeType string, audio []byte) []byte {
		return []byte(`{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"` + mimeType + `","data":"` + base64.StdEncoding.EncodeToString(audio) + `"}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":25,"totalTokenCount":29}}`)
	}
	call, failure := encodeGeminiSpeech(speechRequest("Kore", "wav", nil), "gemini-3.8-flash-tts")
	if failure != nil || !strings.Contains(string(call.JSON), `"voiceName":"Kore"`) || !strings.Contains(string(call.JSON), `"responseModalities":["AUDIO"]`) {
		t.Fatalf("call = %+v %v", call, failure)
	}
	wav, failure := call.DecodeAudio(answer("audio/L16;codec=pcm;rate=24000", pcm))
	if failure != nil || wav.ContentType != "audio/wav" || string(wav.Audio[:4]) != "RIFF" || string(wav.Audio[44:]) != string(pcm) || wav.Tokens.OutputTokens != 25 {
		t.Fatalf("PCM as WAV = %+v %v", wav, failure)
	}
	call, _ = encodeGeminiSpeech(speechRequest("Kore", "pcm", nil), "gemini-3.8-flash-tts")
	raw, failure := call.DecodeAudio(answer("audio/wav", wavFromPCM(pcm, 24000)))
	if failure != nil || raw.ContentType != "audio/pcm" || string(raw.Audio) != string(pcm) {
		t.Fatalf("WAV as PCM = %+v %v", raw, failure)
	}
	if _, failure := encodeGeminiSpeech(speechRequest("Kore", "mp3", nil), "gemini-3.8-flash-tts"); failure == nil {
		t.Fatal("Gemini speech accepted mp3")
	}
}

func TestGeminiTranscriptionInlinesTheAudio(t *testing.T) {
	r := mediaProbe(OpTranscription)
	r.File = &Part{Handle: "upload", ContentType: "audio/wav"}
	call, failure := encodeGeminiTranscription(r, "gemini-3.5-flash")
	if failure != nil || call.JSONFrom == nil {
		t.Fatalf("call = %+v %v", call, failure)
	}
	body, failure := call.JSONFrom(func(part *Part) ([]byte, *Error) {
		if part.Handle != "upload" {
			t.Errorf("read %s", part.Handle)
		}
		return []byte("RIFFaudio"), nil
	})
	var sent map[string]any
	if failure != nil || json.Unmarshal(body, &sent) != nil || !strings.Contains(string(body), base64.StdEncoding.EncodeToString([]byte("RIFFaudio"))) || !strings.Contains(string(body), `"mimeType":"audio/wav"`) {
		t.Fatalf("body = %s %v", body, failure)
	}
	result, failure := call.DecodeTranscription([]byte(`{"candidates":[{"content":{"parts":[{"text":"Hello world.\n"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":80,"candidatesTokenCount":4,"totalTokenCount":84}}`))
	if failure != nil || result.Text != "Hello world." || result.Tokens.InputTokens != 80 {
		t.Fatalf("decoded %+v %v", result, failure)
	}
	if rendered, _ := EncodeTranscriptionJSON(result); string(rendered) != `{"text":"Hello world."}` {
		t.Fatalf("rendered %s", rendered)
	}
	r.Language = new("en")
	if _, failure := encodeGeminiTranscription(r, "gemini-3.5-flash"); failure == nil {
		t.Fatal("Gemini transcription accepted a language hint it cannot pass")
	}
}
