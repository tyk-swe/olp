package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestStrictAudioTranslationPreservesNativeFormatsAndAccounting(t *testing.T) {
	for _, test := range []struct{ format, contentType, response string }{
		{"json", "application/json", `{"text":"hello","native":9007199254740993}`},
		{"verbose_json", "application/json", `{"text":"hello","language":"english","duration":1.25,"segments":[{"start":-0,"end":1.25,"text":"hello","native":9007199254740993}]}`},
		{"text", "text/plain", "hello\n"},
		{"srt", "application/x-subrip", "1\n00:00:00,000 --> 00:00:01,250\nhello\n"},
		{"vtt", "text/vtt", "WEBVTT\n\n00:00.000 --> 00:01.250\nhello\n"},
	} {
		t.Run(test.format, func(t *testing.T) {
			h := strictMediaHarness(t, media.OpTranslation, nil, map[string]json.RawMessage{"temperature": json.RawMessage(`0.7`)})
			var captured []string
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/audio/translations") {
					t.Errorf("wrong native path: %s", r.URL.Path)
				}
				reader, err := r.MultipartReader()
				if err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				for {
					part, err := reader.NextPart()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Error(err)
						return
					}
					data, _ := io.ReadAll(part)
					captured = append(captured, part.FormName()+"="+string(data))
					if part.FormName() == "file" && part.FileName() != "original.wav" {
						t.Error("filename changed")
					}
				}
				w.Header().Set("Content-Type", test.contentType)
				_, _ = io.WriteString(w, test.response)
			})
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			_ = writer.WriteField("prompt", " English hint ")
			part, _ := writer.CreateFormFile("file", "original.wav")
			_, _ = part.Write([]byte{0, 1, 0xff, 3})
			_ = writer.WriteField("model", routeSlug)
			_ = writer.WriteField("temperature", "0.000")
			_ = writer.WriteField("response_format", test.format)
			_ = writer.Close()
			resp := h.do(t.Context(), http.MethodPost, "/v1/audio/translations", fullKey, body.Bytes(), map[string]string{"Content-Type": writer.FormDataContentType()})
			actual, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || string(actual) != test.response || !strings.HasPrefix(resp.Header.Get("Content-Type"), test.contentType) {
				t.Fatalf("translation status=%d type=%q body=%s", resp.StatusCode, resp.Header.Get("Content-Type"), actual)
			}
			want := []string{"prompt= English hint ", "file=" + string([]byte{0, 1, 0xff, 3}), "model=model-a", "temperature=0.000", "response_format=" + test.format}
			if strings.Join(captured, "|") != strings.Join(want, "|") {
				t.Fatalf("native multipart changed: %q", captured)
			}
			env := h.sink.last(t)
			if env.Operation != "translation" {
				t.Fatalf("wrong operation: %s", env.Operation)
			}
			event := accountingEvent(env)
			encoded, err := usage.Encode(event)
			if err != nil {
				t.Fatal(err)
			}
			decoded, _, err := usage.Decode(encoded)
			if err != nil || decoded.Operation != "translation" {
				t.Fatalf("translation usage roundtrip: %v", err)
			}
			if len(env.Attempts) != 1 || env.Attempts[0].Interaction == nil || env.Attempts[0].Interaction.PlanClass != "native_identity" {
				t.Fatalf("translation lost strict accounting: %+v", env)
			}
		})
	}
}

func TestAudioTranslationRejectsUnsupportedControlsBeforeDispatch(t *testing.T) {
	for _, fields := range []map[string]string{
		{"stream": "false"}, {"language": "fr"}, {"timestamp_granularities[]": "segment"},
		{"response_format": "diarized_json"}, {"temperature": "NaN"}, {"temperature": "1.1"},
	} {
		h := strictMediaHarness(t, media.OpTranslation, nil, nil)
		fields["model"] = routeSlug
		body, contentType := mediaFormBody(t, fields, "file", "audio.wav", []byte{0, 1})
		resp := h.do(t.Context(), http.MethodPost, "/v1/audio/translations", fullKey, body, map[string]string{"Content-Type": contentType})
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 400 || h.mock.count("a") != 0 || h.mock.count("b") != 0 {
			t.Fatalf("invalid translation dispatched: fields=%v status=%d", fields, resp.StatusCode)
		}
	}
}
