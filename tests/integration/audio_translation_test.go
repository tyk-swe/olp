//go:build integration

package integration_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"mime"
	"mime/multipart"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestAudioTranslationPublicStrictRouteAndPinnedSDK(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "tests/sdk-smoke/node_modules/openai")); err != nil {
		t.Fatal("pinned OpenAI SDK missing; run pnpm install --frozen-lockfile")
	}
	audio, err := os.ReadFile(filepath.Join(root, "tests/fixtures/media/tiny-pcm.wav"))
	if err != nil || len(audio) != 48 || !bytes.Equal(audio[:4], []byte("RIFF")) ||
		binary.LittleEndian.Uint32(audio[4:8]) != uint32(len(audio)-8) || !bytes.Equal(audio[8:12], []byte("WAVE")) ||
		!bytes.Equal(audio[12:16], []byte("fmt ")) || binary.LittleEndian.Uint16(audio[20:22]) != 1 ||
		binary.LittleEndian.Uint16(audio[22:24]) != 1 || binary.LittleEndian.Uint32(audio[24:28]) != 8000 ||
		binary.LittleEndian.Uint16(audio[34:36]) != 16 || !bytes.Equal(audio[36:40], []byte("data")) ||
		binary.LittleEndian.Uint32(audio[40:44]) != 4 {
		t.Fatalf("translation fixture is not a tiny mono PCM WAV: %v", err)
	}
	h := newAccessHarness(t)
	owner := h.owner()
	sink := &captureSink{}
	h.Gateway.Sink = sink
	price := map[string]any{"provider_kind": "openai_compatible", "model": vendorModel, "operation": "translation", "currency": "USD", "unit_price": "0.000100000000"}
	created := h.want(owner, "POST", "/api/v3/pricing/revisions", map[string]any{"effective_at": time.Now().UTC().Format(time.RFC3339Nano), "prices": []any{price}}, idem("audio-translation-price"), 201)
	if created["prices"].([]any)[0].(map[string]any)["operation"] != "translation" {
		t.Fatalf("translation pricing did not persist: %v", created)
	}
	listed := h.want(owner, "GET", "/api/v3/pricing/revisions", nil, nil, 200)
	if len(listed["items"].([]any)) != 1 {
		t.Fatalf("translation pricing missing: %v", listed)
	}
	for _, test := range []struct{ format, contentType, response string }{
		{"json", "application/json", `{"text":"hello","native_integer":9007199254740993}`},
		{"verbose_json", "application/json", `{"text":"hello","language":"english","duration":1.25,"segments":[{"start":0,"end":1.25,"text":"hello"}]}`},
		{"text", "text/plain", "hello\n"},
		{"srt", "application/x-subrip", "1\n00:00:00,000 --> 00:00:01,250\nhello\n"},
		{"vtt", "text/vtt", "WEBVTT\n\n00:00.000 --> 00:01.250\nhello\n"},
	} {
		t.Run(test.format, func(t *testing.T) {
			fixture := newOperationFixture(t, "/v1/audio/translations", test.response)
			fixture.resultType(test.contentType)
			slug, key := publishStrictMediaFixture(t, h, owner, fixture, "translation", "unary", map[string]any{"prompt": "English hint", "temperature": 0.8})
			cmd := exec.CommandContext(t.Context(), "node", "tests/sdk-smoke/audio-translation.mjs")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "OLP_TRANSLATION_BASE="+h.HTTP.URL, "OLP_TRANSLATION_KEY="+key, "OLP_TRANSLATION_ROUTE="+slug, "OLP_TRANSLATION_FORMAT="+test.format, "OLP_TRANSLATION_EXPECT="+test.response)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("pinned translation SDK: %v\n%s", err, output)
			}
			calls := fixture.snapshot()
			if len(calls) != 1 {
				t.Fatalf("translation dispatches=%d", len(calls))
			}
			_, params, err := mime.ParseMediaType(calls[0].headers.Get("Content-Type"))
			if err != nil {
				t.Fatal(err)
			}
			reader := multipart.NewReader(bytes.NewReader(calls[0].body), params["boundary"])
			fields := map[string]string{}
			for {
				part, err := reader.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(part)
				if err != nil {
					t.Fatal(err)
				}
				if _, duplicate := fields[part.FormName()]; duplicate {
					t.Fatal("duplicated native field")
				}
				fields[part.FormName()] = string(body)
				if part.FormName() == "file" && part.FileName() != "original.wav" {
					t.Fatal("filename changed")
				}
			}
			if len(fields) != 5 || fields["model"] != vendorModel || fields["file"] != string(audio) || fields["temperature"] != "0" || fields["prompt"] != "English hint" || fields["response_format"] != test.format {
				t.Fatalf("translation native fields changed: %v", fields)
			}
			event := sink.last()
			if event.Operation != "translation" || event.Outcome != "success" || len(event.Attempts) != 1 || event.Attempts[0].Interaction == nil || event.Attempts[0].Interaction.PlanClass != "native_identity" {
				t.Fatalf("translation accounting changed: %+v", event)
			}
		})
	}
}
