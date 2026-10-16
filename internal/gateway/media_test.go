package gateway

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/media"
)

func newMediaHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, Config{})
	ops := []string{media.OpImageGeneration, media.OpImageEdit, media.OpImageVariation,
		media.OpSpeech, media.OpTranscription, media.OpTranslation, media.OpVideoCreate, media.OpVideoList,
		media.OpVideoGet, media.OpVideoContent, media.OpVideoDelete}
	snapshot := h.rt.release.Snapshot
	for id, p := range snapshot.Providers {
		model := p.Capabilities[0].Model
		for _, op := range ops {
			for _, mode := range []string{"unary", "streaming", "async"} {
				p.Capabilities = append(p.Capabilities, routing.Capability{Model: model, Operation: op, Surface: "openai", Mode: mode})
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
					p.Limits = &routing.Limits{MaxConcurrency: &one}
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
