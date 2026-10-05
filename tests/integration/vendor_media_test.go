//go:build integration

package integration_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync/atomic"
	"testing"
)

// vendorMediaFixture is a vendor's API: its account probe, which proves a
// credential, and the media call each test answers.
type vendorMediaFixture struct {
	*httptest.Server
	probes atomic.Int32
}

func newVendorMediaFixture(t *testing.T, probe string, authorized func(*http.Request) bool, media http.HandlerFunc) *vendorMediaFixture {
	t.Helper()
	fixture := &vendorMediaFixture{}
	fixture.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			http.Error(w, `{"error":{"message":"unauthenticated"}}`, http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/"+probe) {
			fixture.probes.Add(1)
			writeJSON(w, map[string]any{"data": []any{}})
			return
		}
		media(w, r)
	}))
	t.Cleanup(fixture.Close)
	return fixture
}

func bearer(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer "+vendorSecret }

func TestReviewedVendorMedia(t *testing.T) {
	pixel := base64.StdEncoding.EncodeToString([]byte{0x89, 0x50, 0x4e, 0x47})
	t.Run("xai images", func(t *testing.T) {
		var body map[string]any
		fixture := newVendorMediaFixture(t, "api-key", bearer, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				writeJSON(w, map[string]any{"data": []any{map[string]string{"id": "grok-imagine-image-2.0"}}})
				return
			}
			if r.URL.Path != "/v1/images/generations" {
				http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
				return
			}
			body = decodeBody(t, r)
			writeJSON(w, map[string]any{"data": []any{map[string]any{"b64_json": pixel, "mime_type": "image/png"}}, "usage": map[string]any{"cost_in_usd_ticks": 700000000}})
		})
		h := newAccessHarness(t)
		slug, secret := provisionRoute(t, h, map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": fixture.URL + "/v1", "options": map[string]any{"vendor_id": "xai"}},
			vendorSecret, "grok-imagine-image-2.0", []any{map[string]any{"operation": "image_generation", "surface": "openai", "mode": "unary"}}, []string{"image_generation"})
		if fixture.probes.Load() == 0 {
			t.Fatal("certification did not prove the credential with the account probe")
		}
		status, reply, _ := h.gateway("POST", "/v1/images/generations", secret, map[string]any{"model": slug, "prompt": "a small illustration", "size": "1536x1024", "response_format": "b64_json"})
		data, _ := reply["data"].([]any)
		if status != http.StatusOK || len(data) != 1 || data[0].(map[string]any)["b64_json"] != pixel {
			t.Fatalf("xAI image: %d %v", status, reply)
		}
		if body["aspect_ratio"] != "3:2" || body["model"] != "grok-imagine-image-2.0" || body["size"] != nil {
			t.Fatalf("xAI request: %v", body)
		}
	})
	t.Run("groq transcription", func(t *testing.T) {
		var fields map[string]string
		fixture := newVendorMediaFixture(t, "models", bearer, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				writeJSON(w, map[string]any{"data": []any{map[string]string{"id": "whisper-large-v3"}}})
				return
			}
			if r.URL.Path != "/openai/v1/audio/transcriptions" || r.ParseMultipartForm(1<<20) != nil {
				http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
				return
			}
			fields = map[string]string{}
			for name, values := range r.MultipartForm.Value {
				fields[name] = values[0]
			}
			writeJSON(w, map[string]any{"task": "transcribe", "duration": 12.5, "text": "Hello there.", "segments": []any{}, "x_groq": map[string]string{"id": "req_1"}})
		})
		h := newAccessHarness(t)
		slug, secret := provisionRoute(t, h, map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": fixture.URL + "/openai/v1", "options": map[string]any{"vendor_id": "groq"}},
			vendorSecret, "whisper-large-v3", []any{map[string]any{"operation": "transcription", "surface": "openai", "mode": "unary"}}, []string{"transcription"})
		var upload bytes.Buffer
		form := multipart.NewWriter(&upload)
		form.WriteField("model", slug)
		part, _ := form.CreateFormFile("file", "hello.wav")
		part.Write([]byte("RIFF\x24\x00\x00\x00WAVEfmt "))
		form.Close()
		status, reply, _ := h.gatewayRaw("POST", "/v1/audio/transcriptions", secret, &upload, map[string]string{"Content-Type": form.FormDataContentType()})
		// A json client is served from verbose_json, whose duration Groq bills by.
		if status != http.StatusOK || string(reply) != `{"text":"Hello there."}` || fields["model"] != "whisper-large-v3" || fields["response_format"] != "verbose_json" {
			t.Fatalf("Groq transcription: %d %s %v", status, reply, fields)
		}
	})
	t.Run("elevenlabs speech", func(t *testing.T) {
		var path, format string
		var body map[string]any
		fixture := newVendorMediaFixture(t, "user", func(r *http.Request) bool {
			return r.Header.Get("Xi-Api-Key") == vendorSecret && r.Header.Get("Authorization") == ""
		},
			func(w http.ResponseWriter, r *http.Request) {
				path, format, body = r.URL.Path, r.URL.Query().Get("output_format"), decodeBody(t, r)
				w.Header().Set("Content-Type", "audio/mpeg")
				w.Header().Set("Character-Cost", "12")
				w.Write([]byte("ID3audio"))
			})
		h := newAccessHarness(t)
		slug, secret := provisionRoute(t, h, map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": fixture.URL + "/v1", "options": map[string]any{"vendor_id": "elevenlabs"}},
			vendorSecret, "eleven_multilingual_v2", []any{map[string]any{"operation": "speech", "surface": "openai", "mode": "unary"}}, []string{"speech"})
		encoded, _ := json.Marshal(map[string]any{"model": slug, "input": "Hello there.", "voice": "21m00Tcm4TlvDq8ikWAM"})
		status, audio, headers := h.gatewayRaw("POST", "/v1/audio/speech", secret, bytes.NewReader(encoded), map[string]string{"Content-Type": "application/json"})
		if status != http.StatusOK || string(audio) != "ID3audio" || !strings.HasPrefix(headers.Get("Content-Type"), "audio/mpeg") {
			t.Fatalf("ElevenLabs speech: %d %q %v", status, audio, headers)
		}
		if path != "/v1/text-to-speech/21m00Tcm4TlvDq8ikWAM" || format != "mp3_44100_128" || body["model_id"] != "eleven_multilingual_v2" || body["text"] != "Hello there." {
			t.Fatalf("ElevenLabs request: %s %s %v", path, format, body)
		}
	})
	t.Run("deepgram transcription", func(t *testing.T) {
		var audio []byte
		var contentType string
		fixture := newVendorMediaFixture(t, "projects", func(r *http.Request) bool { return r.Header.Get("Authorization") == "Token "+vendorSecret },
			func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/listen" || r.URL.Query().Get("model") != "nova-3" {
					http.Error(w, "unexpected "+r.URL.String(), http.StatusNotFound)
					return
				}
				audio, _ = io.ReadAll(r.Body)
				contentType = r.Header.Get("Content-Type")
				writeJSON(w, map[string]any{"metadata": map[string]any{"duration": 2.5}, "results": map[string]any{"channels": []any{map[string]any{"alternatives": []any{map[string]any{"transcript": "Hello world.", "words": []any{}}}}}}})
			})
		h := newAccessHarness(t)
		slug, secret := provisionRoute(t, h, map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": fixture.URL + "/v1", "options": map[string]any{"vendor_id": "deepgram"}},
			vendorSecret, "nova-3", []any{map[string]any{"operation": "transcription", "surface": "openai", "mode": "unary"}}, []string{"transcription"})
		var upload bytes.Buffer
		form := multipart.NewWriter(&upload)
		form.WriteField("model", slug)
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", `form-data; name="file"; filename="hello.wav"`)
		header.Set("Content-Type", "audio/wav")
		part, _ := form.CreatePart(header)
		part.Write([]byte("RIFF\x24\x00\x00\x00WAVEfmt "))
		form.Close()
		status, reply, _ := h.gatewayRaw("POST", "/v1/audio/transcriptions", secret, &upload, map[string]string{"Content-Type": form.FormDataContentType()})
		if status != http.StatusOK || string(reply) != `{"text":"Hello world."}` {
			t.Fatalf("Deepgram transcription: %d %s", status, reply)
		}
		if string(audio) != "RIFF\x24\x00\x00\x00WAVEfmt " || contentType != "audio/wav" {
			t.Fatalf("Deepgram received %q as %s", audio, contentType)
		}
	})
	t.Run("stability images", func(t *testing.T) {
		var fields map[string]string
		var accept string
		fixture := newVendorMediaFixture(t, "user/balance", bearer, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v2beta/stable-image/generate/core" || r.ParseMultipartForm(1<<20) != nil {
				http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
				return
			}
			fields, accept = map[string]string{}, r.Header.Get("Accept")
			for name, values := range r.MultipartForm.Value {
				fields[name] = values[0]
			}
			if fields["prompt"] == "forbidden" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(`{"id":"a1","name":"content_moderation","errors":["Your request was flagged."]}`))
				return
			}
			w.Header().Set("Content-Type", "application/json; type=image/png")
			w.Write([]byte(`{"image":"` + pixel + `","finish_reason":"SUCCESS","seed":42}`))
		})
		h := newAccessHarness(t)
		slug, secret := provisionRoute(t, h, map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": fixture.URL, "options": map[string]any{"vendor_id": "stability"}},
			vendorSecret, "stable-image-core", []any{map[string]any{"operation": "image_generation", "surface": "openai", "mode": "unary"}}, []string{"image_generation"})
		status, reply, _ := h.gateway("POST", "/v1/images/generations", secret, map[string]any{"model": slug, "prompt": "a small illustration", "size": "1024x1536", "response_format": "b64_json"})
		data, _ := reply["data"].([]any)
		if status != http.StatusOK || len(data) != 1 || data[0].(map[string]any)["b64_json"] != pixel || accept != "application/json" || fields["aspect_ratio"] != "2:3" {
			t.Fatalf("Stability image: %d %v %s %v", status, reply, accept, fields)
		}
		// Moderation is the caller's error, which leaves the credential usable.
		status, reply, _ = h.gateway("POST", "/v1/images/generations", secret, map[string]any{"model": slug, "prompt": "forbidden", "response_format": "b64_json"})
		if failure, _ := reply["error"].(map[string]any); status == http.StatusOK || failure["code"] != "upstream_rejected" {
			t.Fatalf("moderated image: %d %v", status, reply)
		}
		status, _, _ = h.gateway("POST", "/v1/images/generations", secret, map[string]any{"model": slug, "prompt": "a second illustration", "response_format": "b64_json"})
		if status != http.StatusOK {
			t.Fatalf("the credential was cooled down after moderation: %d", status)
		}
	})
	t.Run("gemini images and transcripts", func(t *testing.T) {
		var lastPath string
		var lastBody map[string]any
		fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Goog-Api-Key") != vendorSecret {
				http.Error(w, "unauthenticated", http.StatusUnauthorized)
				return
			}
			if r.Method == http.MethodGet {
				writeJSON(w, map[string]any{"models": []any{map[string]string{"name": "models/gemini-3.1-flash-image"}, map[string]string{"name": "models/gemini-3.5-flash"}}})
				return
			}
			lastPath, lastBody = r.URL.Path, decodeBody(t, r)
			part := map[string]any{"text": "Hello world."}
			if strings.Contains(r.URL.Path, "image") {
				part = map[string]any{"inlineData": map[string]string{"mimeType": "image/png", "data": pixel}}
			}
			writeJSON(w, map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{part}}, "finishReason": "STOP"}},
				"usageMetadata": map[string]int{"promptTokenCount": 9, "candidatesTokenCount": 1290, "totalTokenCount": 1299}})
		}))
		t.Cleanup(fixture.Close)
		h := newAccessHarness(t)
		images, secret := provisionRoute(t, h, map[string]any{"kind": "gemini", "auth_mode": "api_key", "endpoint": fixture.URL + "/v1beta"},
			vendorSecret, "gemini-3.1-flash-image", []any{map[string]any{"operation": "image_generation", "surface": "openai", "mode": "unary"}}, []string{"image_generation"})
		status, reply, _ := h.gateway("POST", "/v1/images/generations", secret, map[string]any{"model": images, "prompt": "a small illustration", "size": "1024x1024", "response_format": "b64_json"})
		data, _ := reply["data"].([]any)
		if status != http.StatusOK || len(data) != 1 || data[0].(map[string]any)["b64_json"] != pixel || lastPath != "/v1beta/models/gemini-3.1-flash-image:generateContent" {
			t.Fatalf("Gemini image: %d %v at %s", status, reply, lastPath)
		}
		h = newAccessHarness(t)
		transcripts, secret := provisionRoute(t, h, map[string]any{"kind": "gemini", "auth_mode": "api_key", "endpoint": fixture.URL + "/v1beta"},
			vendorSecret, "gemini-3.5-flash", []any{map[string]any{"operation": "transcription", "surface": "openai", "mode": "unary"}}, []string{"transcription"})
		var upload bytes.Buffer
		form := multipart.NewWriter(&upload)
		form.WriteField("model", transcripts)
		form.WriteField("response_format", "text")
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", `form-data; name="file"; filename="hello.wav"`)
		header.Set("Content-Type", "audio/wav")
		part, _ := form.CreatePart(header)
		part.Write([]byte("RIFF-audio"))
		form.Close()
		status, text, _ := h.gatewayRaw("POST", "/v1/audio/transcriptions", secret, &upload, map[string]string{"Content-Type": form.FormDataContentType()})
		if status != http.StatusOK || string(text) != "Hello world." {
			t.Fatalf("Gemini transcription: %d %q", status, text)
		}
		contents, _ := lastBody["contents"].([]any)
		inline, _ := contents[0].(map[string]any)["parts"].([]any)[0].(map[string]any)["inlineData"].(map[string]any)
		if inline["mimeType"] != "audio/wav" || inline["data"] != base64.StdEncoding.EncodeToString([]byte("RIFF-audio")) {
			t.Fatalf("Gemini received %v", lastBody)
		}
	})
}
