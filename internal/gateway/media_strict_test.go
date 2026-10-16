package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/media"
)

func strictMediaHarness(t *testing.T, op string, policy *contentpolicy.Policy, defaults map[string]json.RawMessage) *harness {
	t.Helper()
	h := newMediaHarness(t)
	snapshot := h.rt.release.Snapshot
	profile, err := connectors.LookupProfile("compatible-chat", "1")
	if err != nil {
		t.Fatal(err)
	}
	for id, provider := range snapshot.Providers {
		provider.ProfileID, provider.ProfileRevision = profile.ID, profile.Revision
		if len(defaults) != 0 {
			provider.OperationDefaults = map[string]connectors.DefaultSet{op: {Dialect: profile.OperationDialect(op), Values: defaults}}
		}
		snapshot.Providers[id] = provider
	}
	route := snapshot.Routes[routeSlug]
	route.Operations = []string{op}
	route.Fidelity = routing.RouteFidelity{Mode: routing.FidelityStrict}
	route.ContentPolicy = policy
	snapshot.Routes[routeSlug] = route
	if err := snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestStrictImageEditRetainsOrderedStagedAssets(t *testing.T) {
	h := strictMediaHarness(t, media.OpImageEdit, nil, nil)
	type received struct {
		names    []string
		contents [][]byte
	}
	observed := make(chan received, 1)
	want := []byte(`{"created":1,"data":[{"url":"https://asset.example/edited"}],"native_integer":9007199254740993}`)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		reader, err := r.MultipartReader()
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var got received
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			body, _ := io.ReadAll(part)
			got.names = append(got.names, part.FormName())
			got.contents = append(got.contents, body)
		}
		observed <- got
		w.Header().Set("Content-Type", "application/json")
		w.Write(want)
	})
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	writer.WriteField("model", routeSlug)
	writer.WriteField("prompt", "edit")
	for _, item := range []struct{ name, filename, content string }{{"image[0]", "one.png", "first-bytes"}, {"image[1]", "two.png", "second-bytes"}, {"mask", "mask.png", "mask-bytes"}} {
		part, err := writer.CreateFormFile(item.name, item.filename)
		if err != nil {
			t.Fatal(err)
		}
		part.Write([]byte(item.content))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	resp := h.do(t.Context(), http.MethodPost, "/v1/images/edits", fullKey, body.Bytes(), map[string]string{"Content-Type": writer.FormDataContentType()})
	defer resp.Body.Close()
	actual, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(actual, want) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, actual)
	}
	got := <-observed
	for i, name := range []string{"model", "prompt", "image[0]", "image[1]", "mask"} {
		if i >= len(got.names) || got.names[i] != name {
			t.Fatalf("part order=%v", got.names)
		}
	}
	for i, value := range []string{"first-bytes", "second-bytes", "mask-bytes"} {
		if string(got.contents[i+2]) != value {
			t.Fatalf("part %d bytes=%q", i, got.contents[i+2])
		}
	}
}

func TestStrictSpeechBinaryAndTranscriptionJSON(t *testing.T) {
	t.Run("speech binary", func(t *testing.T) {
		h := strictMediaHarness(t, media.OpSpeech, nil, nil)
		identifyHarness(h, "header")
		outbound := make(chan []byte, 1)
		payload := []byte{0x52, 0x49, 0x46, 0x46, 0, 0xff, 0x01}
		h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get(endUserHeader) != "" {
				t.Error("media forwarded end-user header")
			}
			body, _ := io.ReadAll(r.Body)
			outbound <- body
			w.Header().Set("Content-Type", "audio/wav")
			w.Write(payload)
		})
		resp := h.do(t.Context(), http.MethodPost, "/v1/audio/speech", fullKey,
			[]byte(`{"model":"team-chat","input":"hello","voice":"alloy","speed":0.500,"future":{"channel_count":9007199254740993}}`), map[string]string{endUserHeader: "media-user"})
		actual, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/wav" || !bytes.Equal(actual, payload) {
			t.Fatalf("speech status=%d content-type=%s body=%x", resp.StatusCode, resp.Header.Get("Content-Type"), actual)
		}
		want := `{"model":"model-a","input":"hello","voice":"alloy","speed":0.500,"future":{"channel_count":9007199254740993}}`
		if got := <-outbound; string(got) != want {
			t.Fatalf("native speech changed: %s", got)
		}
		assertEndUserEnvelope(t, h.sink.last(t), (endUserRuntime{h.rt}).EndUserDigest(nil, "media-user"), "media-user")
	})
	t.Run("transcription JSON", func(t *testing.T) {
		h := strictMediaHarness(t, media.OpTranscription, nil, nil)
		want := []byte(`{"text":"hello","duration":1.0000001,"segments":[{"start":0.10000000001,"end":1.0000001,"text":"hello","native_id":9007199254740993}],"future":{"channel":"left"}}`)
		identifyHarness(h, "header")
		outbound := make(chan []byte, 1)
		h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get(endUserHeader) != "" {
				t.Error("media forwarded end-user header")
			}
			reader, err := r.MultipartReader()
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			for {
				part, err := reader.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				body, _ := io.ReadAll(part)
				if part.FormName() == "file" {
					outbound <- body
				}
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(want)
		})
		body, contentType := mediaFormBody(t, map[string]string{"model": routeSlug, "response_format": "verbose_json"}, "file", "audio.wav", []byte{0, 1, 2, 3, 4})
		resp := h.do(t.Context(), http.MethodPost, "/v1/audio/transcriptions", fullKey, body, map[string]string{"Content-Type": contentType, endUserHeader: "media-user"})
		actual, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !bytes.Equal(actual, want) {
			t.Fatalf("transcription status=%d body=%s", resp.StatusCode, actual)
		}
		if got := <-outbound; !bytes.Equal(got, []byte{0, 1, 2, 3, 4}) {
			t.Fatalf("transcription bytes=%x", got)
		}
		assertEndUserEnvelope(t, h.sink.last(t), (endUserRuntime{h.rt}).EndUserDigest(nil, "media-user"), "media-user")
	})
}

func TestStrictMediaSSEPreservesFramingAndRejectsAmbiguousEvent(t *testing.T) {
	h := strictMediaHarness(t, media.OpImageGeneration, nil, nil)
	h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "id: first\nretry: 2100\nevent: image_generation.partial_image\ndata: {\"type\":\"image_generation.partial_image\",\"native\":{\"rank\":9007199254740993}}\n\n")
		io.WriteString(w, "event: image_generation.completed\ndata: {\"type\":\"image_generation.completed\",\"usage\":{\"input_tokens\":2,\"output_tokens\":3}}\n\n")
	})
	resp := h.do(t.Context(), http.MethodPost, "/v1/images/generations", fullKey, []byte(`{"model":"team-chat","prompt":"photo","stream":true}`), nil)
	actual, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Contains(actual, []byte("id: first\nretry: 2100\n")) || !bytes.Contains(actual, []byte(`9007199254740993`)) || !bytes.Contains(actual, []byte("image_generation.completed")) {
		t.Fatalf("strict SSE status=%d body=%s", resp.StatusCode, actual)
	}
	if fact := h.sink.last(t); fact.Outcome != "success" || len(fact.Attempts) != 1 {
		t.Fatalf("terminal SSE accounting=%+v", fact)
	}

	h2 := strictMediaHarness(t, media.OpImageGeneration, nil, nil)
	h2.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"image_generation.partial_image\"}\n\n")
		w.(http.Flusher).Flush()
		io.WriteString(w, "data: {\"type\":\"image_generation.completed\",\"native\":{\"a\":1,\"a\":2}}\n\n")
	})
	resp = h2.do(t.Context(), http.MethodPost, "/v1/images/generations", fullKey, []byte(`{"model":"team-chat","prompt":"photo","stream":true}`), nil)
	actual, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || strings.Contains(string(actual), "image_generation.completed") || h2.mock.count("b") != 0 {
		t.Fatalf("ambiguous SSE status=%d body=%s", resp.StatusCode, actual)
	}
	if fact := h2.sink.last(t); fact.Outcome != "failure" || !fact.Committed || fact.ErrorClass == "" {
		t.Fatalf("ambiguous SSE was reported complete: %+v", fact)
	}
}
