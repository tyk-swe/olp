package gateway

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/runtime"
)

func newMediaHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, Config{})
	ops := []string{media.OpImageGeneration, media.OpImageEdit, media.OpImageVariation,
		media.OpSpeech, media.OpTranscription, media.OpVideoCreate, media.OpVideoList,
		media.OpVideoGet, media.OpVideoContent, media.OpVideoDelete}
	snapshot := h.rt.release.Snapshot
	for id, p := range snapshot.Providers {
		model := p.Capabilities[0].Model
		for _, op := range ops {
			for _, mode := range []string{"unary", "streaming", "async"} {
				p.Capabilities = append(p.Capabilities, runtime.Capability{Model: model, Operation: op, Surface: "openai", Mode: mode})
			}
		}
		snapshot.Providers[id] = p
	}
	route := snapshot.Routes[routeSlug]
	route.Operations = append(route.Operations, ops...)
	snapshot.Routes[routeSlug] = route
	spool, err := media.NewSpool(t.TempDir(), media.MinCapacityBytes, h.gateway.log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { spool.Close() })
	h.gateway.Media = &MediaDeps{
		Jobs: &media.Service{Log: h.gateway.log, Transport: &media.Transport{
			Client: h.gateway.client, Auth: h.gateway.auth, Egress: h.gateway.egress,
			Spool: spool, MaxResponseBytes: 1 << 20,
		}},
		Admission: media.NewAdmissionState(media.MinCapacityBytes),
	}
	mux := http.NewServeMux()
	h.gateway.Register(mux)
	h.server = httptest.NewServer(mux)
	t.Cleanup(h.server.Close)
	return h
}

func mediaMultipart(t *testing.T, fields map[string]string, files ...[]byte) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for name, value := range fields {
		if err := w.WriteField(name, value); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range files {
		part, err := w.CreateFormFile("image[]", "image.png")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(file); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes(), w.FormDataContentType()
}

func TestMediaStreamCompletesBeforeAttemptSettlement(t *testing.T) {
	for _, tc := range []struct{ name, path, request, delta, terminal string }{
		{"image", "/v1/images/generations", `{"model":"team-chat","prompt":"photo","stream":true}`,
			"image_generation.partial_image", "image_generation.completed"},
		{"speech", "/v1/audio/speech", `{"model":"team-chat","input":"hello","voice":"alloy","stream_format":"sse"}`,
			"speech.audio.delta", "speech.audio.done"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newMediaHarness(t)
			finish := make(chan struct{})
			defer func() {
				select {
				case <-finish:
				default:
					close(finish)
				}
			}()
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"type\":\""+tc.delta+"\"}\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-finish:
					io.WriteString(w, "data: {\"type\":\""+tc.terminal+"\",\"usage\":{\"input_tokens\":2,\"output_tokens\":3}}\n\n")
				case <-r.Context().Done():
				}
			})
			resp := h.do(t.Context(), "POST", tc.path, fullKey, []byte(tc.request), nil)
			defer resp.Body.Close()
			reader := bufio.NewReader(resp.Body)
			first, err := reader.ReadString('\n')
			if err != nil || !strings.Contains(first, tc.delta) {
				t.Fatalf("first frame = %q, %v", first, err)
			}
			h.sink.mu.Lock()
			finished := len(h.sink.envs)
			h.sink.mu.Unlock()
			if finished != 0 || h.gateway.admission.Admitted() != 1 {
				t.Fatal("stream was finalized before the upstream completed")
			}
			close(finish)
			body, err := io.ReadAll(reader)
			if err != nil || !strings.Contains(string(body), tc.terminal) {
				t.Fatalf("terminal frame missing: %q, %v", body, err)
			}
			env := h.sink.last(t)
			if env.Outcome != "success" || len(env.Attempts) != 1 {
				t.Fatalf("terminal envelope: %+v", env)
			}
			fact := env.Attempts[0]
			if !fact.UsageObserved || !fact.UsageComplete || fact.BillingUncertain || fact.Usage.TotalTokens != 5 {
				t.Fatalf("stream usage evidence: %+v", fact)
			}
			event := accountingEvent(env)
			if event == nil || event.Attempts[0].Usage.InputTokens == nil || *event.Attempts[0].Usage.InputTokens != 2 {
				t.Fatalf("stream usage omitted from accounting: %+v", event)
			}
			if tc.name == "image" && (fact.Usage.MediaUnits == nil || *fact.Usage.MediaUnits != "1") {
				t.Fatalf("completed image quantity: %+v", fact.Usage)
			}
		})
	}
}

func TestMediaStreamRejectsIncompleteAndOversizedResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		limit      int64
	}{
		{"premature EOF", "data: {\"type\":\"image_generation.partial_image\"}\n\n", 4096},
		{"empty stream", "", 4096},
		{"upstream error", "event: error\ndata: {\"message\":\"failed\"}\n\n", 4096},
		{"frame limit", "data: {\"padding\":\"" + strings.Repeat("x", 256) + "\"}\n\n", 128},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newMediaHarness(t)
			h.gateway.cfg.MaxEventBytes = tc.limit
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				io.WriteString(w, tc.body)
			})
			resp := h.do(t.Context(), "POST", "/v1/images/generations", fullKey,
				[]byte(`{"model":"team-chat","prompt":"photo","stream":true}`), nil)
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			env := h.sink.last(t)
			if env.Outcome != "failure" || !env.Attempts[0].BillingUncertain || env.Attempts[0].Usage != nil {
				t.Fatalf("incomplete stream recorded as success or billed as completed images: %+v", env)
			}
			if h.mock.count("b") != 0 {
				t.Fatal("accepted streaming work was retried")
			}
		})
	}
}

func TestMediaSpeechMissingUsageRemainsUncertain(t *testing.T) {
	h := newMediaHarness(t)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		io.WriteString(w, "audio-payload")
	})
	resp := h.do(t.Context(), "POST", "/v1/audio/speech", fullKey,
		[]byte(`{"model":"team-chat","input":"hello","voice":"alloy"}`), nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	env := h.sink.last(t)
	fact := env.Attempts[0]
	if env.Outcome != "success" || fact.UsageObserved || fact.UsageComplete || !fact.BillingUncertain {
		t.Fatalf("missing speech usage: %+v", fact)
	}
	event := accountingEvent(env)
	if event == nil || !event.Attempts[0].Usage.BillingUncertain || event.Attempts[0].Usage.Complete {
		t.Fatalf("missing usage uncertainty lost from accounting: %+v", event)
	}
	if h.gateway.transport().Spool.UsedBytes() != 0 {
		t.Fatal("speech delivery leaked spool bytes")
	}
}

func TestMediaDefinitiveRejectionAllowsFailover(t *testing.T) {
	for _, code := range []int{401, 429} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			h := newMediaHarness(t)
			h.mock.set("a", status(code, `{"error":{"message":"rejected"}}`))
			h.mock.set("b", status(200, `{"data":[{"url":"https://example.com/image.png"}]}`))
			resp := h.do(t.Context(), "POST", "/v1/images/generations", fullKey,
				[]byte(`{"model":"team-chat","prompt":"photo"}`), nil)
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || h.mock.count("a") != 1 || h.mock.count("b") != 1 {
				t.Fatalf("failover: status=%d calls=%d,%d", resp.StatusCode, h.mock.count("a"), h.mock.count("b"))
			}
			fact := h.sink.last(t).Attempts[0]
			if fact.Committed || fact.BillingUncertain || !fact.UsageComplete {
				t.Fatalf("definitive rejection misclassified: %+v", fact)
			}
		})
	}
}

func TestMediaJSONUsesJSONBodyLimit(t *testing.T) {
	for _, name := range []string{"identity", "gzip", "separate multipart cap"} {
		t.Run(name, func(t *testing.T) {
			h := newMediaHarness(t)
			h.gateway.cfg.MaxMediaBodyBytes = 8192
			h.gateway.cfg.MaxBodyBytes = 512
			h.mock.set("a", status(200, `{"data":[{"url":"https://example.com/image.png"}]}`))
			body := []byte(`{"model":"team-chat","prompt":"` + strings.Repeat("x", 1024) + `"}`)
			headers := map[string]string{}
			want := 413
			if name == "gzip" {
				var compressed bytes.Buffer
				writer := gzip.NewWriter(&compressed)
				if _, err := writer.Write(body); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				body = compressed.Bytes()
				headers["Content-Encoding"] = "gzip"
			} else if name == "separate multipart cap" {
				h.gateway.cfg.MaxMediaBodyBytes = 16
				body = []byte(`{"model":"team-chat","prompt":"photo"}`)
				want = 200
			}
			resp := h.do(t.Context(), "POST", "/v1/images/generations", fullKey, body, headers)
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			if resp.StatusCode != want || (want != 200 && h.mock.count("a") != 0) {
				t.Fatalf("status=%d want=%d upstream calls=%d", resp.StatusCode, want, h.mock.count("a"))
			}
		})
	}
}

func TestMediaMultipartBoundsAndAdmission(t *testing.T) {
	for _, name := range []string{"image edit defaults", "busy parser", "declared body", "chunked body", "epilogue"} {
		t.Run(name, func(t *testing.T) {
			h := newMediaHarness(t)
			h.mock.set("a", status(200, `{"data":[{"url":"https://example.com/image.png"}]}`))
			body, contentType := mediaMultipart(t, map[string]string{"model": routeSlug, "prompt": "photo"},
				bytes.Repeat([]byte("x"), 700), bytes.Repeat([]byte("y"), 700))
			want := 200
			switch name {
			case "busy parser":
				lease := h.gateway.Media.Admission.TryAdmit(h.keyID, 1)
				defer lease.Release()
				want = 503
			case "declared body", "chunked body":
				h.gateway.cfg.MaxMediaBodyBytes = 1024
				want = 413
			case "epilogue":
				h.gateway.cfg.MaxMediaBodyBytes = int64(len(body) + 128)
				body = append(body, bytes.Repeat([]byte("z"), 4096)...)
				want = 413
			}
			req := httptest.NewRequest("POST", "/v1/images/edits", bytes.NewReader(body))
			if name == "chunked body" || name == "epilogue" {
				req.ContentLength = -1
			}
			req.Header.Set("Content-Type", contentType)
			req.Header.Set("Authorization", "Bearer "+fullKey)
			recorder := &mediaDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
			h.gateway.mediaFormHandler(media.DefaultImageUploadLimit, 33, media.DecodeImageEdit)(recorder, req)
			if recorder.Code != want {
				t.Fatalf("status=%d want=%d body=%s", recorder.Code, want, recorder.Body)
			}
			if want != 200 && h.mock.count("a") != 0 {
				t.Fatal("rejected body reached upstream")
			}
			if h.gateway.transport().Spool.UsedBytes() != 0 || h.gateway.admission.Admitted() != 0 {
				t.Fatal("multipart completion leaked files or admission")
			}
		})
	}
}

func TestVideoCreateFailsClosedForUnenforceableTargetQuotas(t *testing.T) {
	for _, scope := range []string{"provider", "credential"} {
		t.Run(scope, func(t *testing.T) {
			h := newMediaHarness(t)
			for id, p := range h.rt.release.Snapshot.Providers {
				one := int64(1)
				if scope == "provider" {
					p.Limits = &runtime.Limits{MaxConcurrency: &one}
				} else {
					p.Slots[0].RequestsPerMinute = &one
				}
				h.rt.release.Snapshot.Providers[id] = p
			}
			body, contentType := mediaMultipart(t, map[string]string{"model": routeSlug, "prompt": "video"})
			resp := h.do(t.Context(), "POST", "/v1/videos", fullKey, body, map[string]string{"Content-Type": contentType})
			defer resp.Body.Close()
			var result map[string]any
			json.NewDecoder(resp.Body).Decode(&result)
			if resp.StatusCode != 503 || errorCode(t, result) != "distributed_limits_unavailable" {
				t.Fatalf("quota bypass: status=%d body=%v", resp.StatusCode, result)
			}
			if h.mock.count("a") != 0 || h.mock.count("b") != 0 {
				t.Fatal("video dispatched without enforceable quotas")
			}
		})
	}
}
