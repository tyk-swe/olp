package transport

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/panics"
	"github.com/tyk-swe/olp/internal/platform/staging"
	"github.com/tyk-swe/olp/internal/upstream"
)

func loopbackPolicy() *egress.Policy {
	return &egress.Policy{
		AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		PlainHTTPHosts:  []string{"127.0.0.1"},
	}
}

func newTestSpool(t *testing.T) *staging.Spool {
	t.Helper()
	spool, err := staging.NewSpool(t.TempDir(), staging.MinCapacityBytes, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("new spool: %v", err)
	}
	t.Cleanup(func() { _ = spool.Close() })
	return spool
}

func stageBytes(t *testing.T, spool *staging.Spool, name, contentType string, data []byte) *staging.Artifact {
	t.Helper()
	artifact, err := spool.Put(t.Context(), staging.Upload{
		Filename:      name,
		ContentType:   contentType,
		MaximumLength: int64(len(data)),
		Body:          bytes.NewReader(data),
	})
	if err != nil {
		t.Fatalf("stage %s: %v", name, err)
	}
	return artifact
}

func readArtifact(t *testing.T, spool *staging.Spool, handle staging.Handle) []byte {
	t.Helper()
	opened, err := spool.Open(handle)
	if err != nil {
		t.Fatalf("open artifact: %v", err)
	}
	defer opened.File.Close()
	data, err := io.ReadAll(opened.File)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	return data
}

func newTestTransport(t *testing.T, server *httptest.Server, spool *staging.Spool, now func() time.Time) (*Transport, Target) {
	t.Helper()
	policy := loopbackPolicy()
	tr := New(Deps{
		Client:           server.Client(),
		Auth:             connectors.NewAuth(policy, func(string) string { return "" }),
		Egress:           policy,
		Spool:            spool,
		MaxResponseBytes: 1 << 20,
		Log:              slog.New(slog.DiscardHandler),
		Now:              now,
	})
	target := Target{
		Config: connectors.Config{
			Endpoint: server.URL,
			AuthMode: connectors.AuthAPIKey,
		},
		Model:  "test-model",
		Secret: []byte("sk-test-secret-12345"),
	}
	return tr, target
}

func TestNewDefaultsAndNilGuards(t *testing.T) {
	t.Parallel()

	t.Run("nil egress rejects plain http", func(t *testing.T) {
		t.Parallel()
		tr := New(Deps{})
		call := &media.UpstreamCall{
			Method:   http.MethodGet,
			Endpoint: media.Path{Path: "v1/videos/vid_1"},
			Response: media.VideoJobReply{},
		}
		_, fail := tr.Do(t.Context(), Target{Config: connectors.Config{Endpoint: "http://127.0.0.1:8080"}}, call, Options{})
		if fail == nil || fail.Class != upstream.Connect {
			t.Fatalf("failure = %+v, want Connect", fail)
		}
	})

	t.Run("nil client returns clean failure", func(t *testing.T) {
		t.Parallel()
		tr := New(Deps{Egress: loopbackPolicy()})
		call := &media.UpstreamCall{
			Method:   http.MethodGet,
			Endpoint: media.Path{Path: "v1/videos/vid_1"},
			Response: media.VideoJobReply{},
		}
		_, fail := tr.Do(t.Context(), Target{Config: connectors.Config{Endpoint: "http://127.0.0.1:8080", AuthMode: connectors.AuthNone}}, call, Options{})
		if fail == nil || fail.Class != upstream.Credential {
			t.Fatalf("failure = %+v, want Credential", fail)
		}
	})

	t.Run("nil spool handled cleanly for upload and audio staging", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write([]byte("audio-bytes"))
		}))
		defer srv.Close()

		tr, target := newTestTransport(t, srv, nil, nil)
		_, fail := tr.Do(t.Context(), target, &media.UpstreamCall{
			Method:   http.MethodPost,
			Endpoint: media.Path{Path: "v1/upload"},
			Body:     media.UploadBody{Handle: "deadbeef"},
			Response: media.VideoJobReply{},
		}, Options{})
		if fail == nil || fail.Class != upstream.Connect {
			t.Fatalf("upload without spool = %+v, want Connect", fail)
		}

		_, fail = tr.Do(t.Context(), target, &media.UpstreamCall{
			Method:   http.MethodPost,
			Endpoint: media.Path{Path: "v1/audio/speech"},
			Body:     media.JSONBody(`{"input":"hi"}`),
			Response: media.AudioReply{},
		}, Options{})
		if fail == nil || fail.Class != upstream.Connect || !fail.Dispatched {
			t.Fatalf("audio without spool = %+v, want dispatched Connect", fail)
		}
	})
}

func TestDoBodies(t *testing.T) {
	t.Parallel()
	spool := newTestSpool(t)

	t.Run("JSONBody and NativeJSON preserve headers without mutating caller options", func(t *testing.T) {
		t.Parallel()
		var gotContentType, gotAccept, gotCustom, gotBody string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotContentType = r.Header.Get("Content-Type")
			gotAccept = r.Header.Get("Accept")
			gotCustom = r.Header.Get("X-Custom")
			raw, _ := io.ReadAll(r.Body)
			gotBody = string(raw)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"vid_1","object":"video","status":"queued"}`)
		}))
		defer srv.Close()

		tr, target := newTestTransport(t, srv, spool, nil)
		callerHeaders := http.Header{"X-Custom": []string{"v1"}}
		for _, body := range []media.Body{media.JSONBody(`{"a":1}`), media.NativeJSON(`{"b":2}`)} {
			res, fail := tr.Do(t.Context(), target, &media.UpstreamCall{
				Method:   http.MethodPost,
				Endpoint: media.Path{Path: "v1/videos"},
				Accept:   "application/json",
				Body:     body,
				Response: media.VideoJobReply{},
			}, Options{Headers: callerHeaders})
			if fail != nil {
				t.Fatalf("Do(%T) failed: %+v", body, fail)
			}
			if res.Status != http.StatusOK || gotContentType != "application/json" || gotAccept != "application/json" || gotCustom != "v1" {
				t.Fatalf("unexpected headers: ct=%q accept=%q custom=%q", gotContentType, gotAccept, gotCustom)
			}
		}
		if gotBody != `{"b":2}` {
			t.Fatalf("last body = %q, want native json", gotBody)
		}
		if len(callerHeaders["X-Custom"]) != 1 || callerHeaders["X-Custom"][0] != "v1" {
			t.Fatalf("caller headers mutated: %v", callerHeaders)
		}
	})

	t.Run("nil body sends empty GET", func(t *testing.T) {
		t.Parallel()
		var gotMethod string
		var gotLen int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			raw, _ := io.ReadAll(r.Body)
			gotLen = len(raw)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"vid_1","object":"video","status":"completed"}`)
		}))
		defer srv.Close()

		tr, target := newTestTransport(t, srv, spool, nil)
		res, fail := tr.Do(t.Context(), target, &media.UpstreamCall{
			Method:   http.MethodGet,
			Endpoint: media.Path{Path: "v1/videos/vid_1"},
			Response: media.VideoJobReply{},
		}, Options{})
		if fail != nil {
			t.Fatalf("Do failed: %+v", fail)
		}
		if gotMethod != http.MethodGet || gotLen != 0 || res.Status != http.StatusOK {
			t.Fatalf("unexpected request: method=%s len=%d status=%d", gotMethod, gotLen, res.Status)
		}
	})

	t.Run("UploadBody streams staged artifact with length and type", func(t *testing.T) {
		t.Parallel()
		artifact := stageBytes(t, spool, "clip.wav", "audio/wav", []byte("RIFF-WAV-DATA"))
		var gotType string
		var gotLength int64
		var gotBody []byte
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotType = r.Header.Get("Content-Type")
			gotLength = r.ContentLength
			gotBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"text":"hello"}`)
		}))
		defer srv.Close()

		tr, target := newTestTransport(t, srv, spool, nil)
		res, fail := tr.Do(t.Context(), target, &media.UpstreamCall{
			Method:   http.MethodPost,
			Endpoint: media.Path{Path: "v1/audio/transcriptions"},
			Body:     media.UploadBody{Handle: artifact.Handle, ContentType: "audio/wav"},
			Response: media.TranscriptReply{Format: "json"},
		}, Options{})
		if fail != nil {
			t.Fatalf("Do failed: %+v", fail)
		}
		if gotType != "audio/wav" || gotLength != int64(len("RIFF-WAV-DATA")) || string(gotBody) != "RIFF-WAV-DATA" {
			t.Fatalf("upload mismatch: type=%q len=%d body=%q", gotType, gotLength, gotBody)
		}
		trPayload, ok := res.Payload.(Transcript)
		if !ok || trPayload.Text != "hello" {
			t.Fatalf("payload = %#v, want Transcript hello", res.Payload)
		}
	})

	t.Run("InlineBody reads staged file and surfaces media error", func(t *testing.T) {
		t.Parallel()
		artifact := stageBytes(t, spool, "voice.mp3", "audio/mpeg", []byte("mp3-bytes"))
		var gotBody string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			gotBody = string(raw)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"text":"ok"}`)
		}))
		defer srv.Close()

		tr, target := newTestTransport(t, srv, spool, nil)
		inline := media.InlineBody(func(read func(staging.Part) ([]byte, *media.Error)) ([]byte, *media.Error) {
			raw, err := read(staging.Part{Handle: artifact.Handle})
			if err != nil {
				return nil, err
			}
			encoded, marshalErr := json.Marshal(map[string]string{"audio": base64.StdEncoding.EncodeToString(raw)})
			if marshalErr != nil {
				return nil, media.Fail(http.StatusInternalServerError, "marshal_error", marshalErr.Error())
			}
			return encoded, nil
		})
		if _, fail := tr.Do(t.Context(), target, &media.UpstreamCall{
			Method:   http.MethodPost,
			Endpoint: media.Path{Path: "v1/inline"},
			Body:     inline,
			Response: media.TranscriptReply{Format: "json"},
		}, Options{}); fail != nil {
			t.Fatalf("inline Do failed: %+v", fail)
		}
		if !strings.Contains(gotBody, base64.StdEncoding.EncodeToString([]byte("mp3-bytes"))) {
			t.Fatalf("inline body = %q", gotBody)
		}

		badInline := media.InlineBody(func(read func(staging.Part) ([]byte, *media.Error)) ([]byte, *media.Error) {
			_, err := read(staging.Part{Handle: "00000000000000000000000000000000"})
			return nil, err
		})
		_, fail := tr.Do(t.Context(), target, &media.UpstreamCall{
			Method:   http.MethodPost,
			Endpoint: media.Path{Path: "v1/inline"},
			Body:     badInline,
			Response: media.TranscriptReply{Format: "json"},
		}, Options{})
		if fail == nil || fail.Class != upstream.Protocol {
			t.Fatalf("missing inline handle = %+v, want Protocol", fail)
		}
	})

	t.Run("FormBody streams multipart fields and handles early refusal abort", func(t *testing.T) {
		t.Parallel()
		artifact := stageBytes(t, spool, "pic.png", "image/png", []byte("PNGDATA"))
		var gotPrompt, gotFilename, gotFileType, gotFileData string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/reject" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"bad form"}}`)
				return
			}
			_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil {
				t.Errorf("parse content-type: %v", err)
				return
			}
			raw, _ := io.ReadAll(r.Body)
			mr := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
			for {
				part, err := mr.NextPart()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Errorf("next part: %v", err)
					return
				}
				data, _ := io.ReadAll(part)
				switch part.FormName() {
				case "prompt":
					gotPrompt = string(data)
				case "image":
					gotFilename = part.FileName()
					gotFileType = part.Header.Get("Content-Type")
					gotFileData = string(data)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"vid_form","object":"video","status":"queued"}`)
		}))
		defer srv.Close()

		tr, target := newTestTransport(t, srv, spool, nil)
		fields := media.FormBody{
			{Name: "prompt", Value: staging.Text("draw a cat")},
			{Name: "image", Value: staging.Part{Handle: artifact.Handle}},
		}
		res, fail := tr.Do(t.Context(), target, &media.UpstreamCall{
			Method:   http.MethodPost,
			Endpoint: media.Path{Path: "ok"},
			Body:     fields,
			Response: media.VideoJobReply{},
		}, Options{})
		if fail != nil {
			t.Fatalf("form Do failed: %+v", fail)
		}
		if res.Status != http.StatusOK || gotPrompt != "draw a cat" || gotFilename != "pic.png" || gotFileType != "image/png" || gotFileData != "PNGDATA" {
			t.Fatalf("multipart mismatch: prompt=%q file=%q type=%q data=%q", gotPrompt, gotFilename, gotFileType, gotFileData)
		}

		large := stageBytes(t, spool, "big.bin", "application/octet-stream", bytes.Repeat([]byte("x"), 256*1024))
		_, fail = tr.Do(t.Context(), target, &media.UpstreamCall{
			Method:   http.MethodPost,
			Endpoint: media.Path{Path: "reject"},
			Body:     media.FormBody{{Name: "file", Value: staging.Part{Handle: large.Handle}}},
			Response: media.VideoJobReply{},
		}, Options{})
		if fail == nil || fail.Status != http.StatusBadRequest || fail.Upstream == nil || fail.Upstream.Message != "bad form" {
			t.Fatalf("early refusal = %+v, want 400 bad form", fail)
		}
	})
}

func TestFormOutboundPanicRecovery(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	var logMu sync.Mutex
	logger := slog.New(slog.NewTextHandler(&lockedWriter{mu: &logMu, w: &logBuf}, nil))

	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		_, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		return httpResponse(http.StatusOK, "application/json", `{}`), nil
	})}

	policy := loopbackPolicy()
	tr := New(Deps{
		Client:           client,
		Auth:             connectors.NewAuth(policy, func(string) string { return "" }),
		Egress:           policy,
		MaxResponseBytes: 1 << 20,
		Log:              logger,
	})
	panickingOpen := func(staging.Part) (io.ReadCloser, staging.Part, error) {
		panic("simulated multipart panic")
	}
	fields := []staging.Field{{Name: "file", Value: staging.Part{Handle: "deadbeef"}}}

	ob := tr.formOutbound(fields, panickingOpen)
	_, readErr := io.ReadAll(ob.reader)
	var panicErr *panics.Error
	if !errors.As(readErr, &panicErr) {
		t.Fatalf("pipe read error = %v, want *panics.Error", readErr)
	}
	if finishErr := ob.finish(errAbandoned); !errors.As(finishErr, &panicErr) {
		t.Fatalf("finish error = %v, want *panics.Error", finishErr)
	}

	ob2 := tr.formOutbound(fields, panickingOpen)
	target := Target{Config: connectors.Config{Endpoint: "http://127.0.0.1:8080", AuthMode: connectors.AuthNone}}
	call := &media.UpstreamCall{Method: http.MethodPost, Endpoint: media.Path{Path: "upload"}, Response: media.VideoJobReply{}}
	_, _, _, fail := tr.send(t.Context(), client, target, call, Options{}, "http://127.0.0.1:8080/upload", ob2)
	if fail == nil || fail.Class != upstream.Connect {
		t.Fatalf("send failure on panic = %+v, want Connect", fail)
	}

	logMu.Lock()
	logged := logBuf.String()
	logMu.Unlock()
	if !strings.Contains(logged, "panic recovered") || !strings.Contains(logged, "simulated multipart panic") {
		t.Fatalf("expected panic recovery log, got %q", logged)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestDoResponses(t *testing.T) {
	t.Parallel()
	spool := newTestSpool(t)
	fixedNow := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	t.Run("ImageReply base64 staging and rollback on partial failure", func(t *testing.T) {
		t.Parallel()
		localSpool := newTestSpool(t)
		pngBytes := []byte("fake-png-image")
		encoded := base64.StdEncoding.EncodeToString(pngBytes)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/partial-bad" {
				_, _ = io.WriteString(w, `{"created":1700000000,"data":[{"b64_json":"`+encoded+`"},{"b64_json":"!!!not-base64!!!"}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"created":1700000000,"data":[{"b64_json":"`+encoded+`"}]}`)
		}))
		defer srv.Close()

		tr, target := newTestTransport(t, srv, localSpool, func() time.Time { return fixedNow })
		res, fail := tr.Do(t.Context(), target, &media.UpstreamCall{
			Method:   http.MethodPost,
			Endpoint: media.Path{Path: "ok"},
			Body:     media.JSONBody(`{"prompt":"cat"}`),
			Response: media.ImageReply{},
		}, Options{Strict: true})
		if fail != nil {
			t.Fatalf("ImageReply failed: %+v", fail)
		}
		imgs, ok := res.Payload.(Images)
		if !ok || len(imgs.Images) != 1 || imgs.Images[0].Handle == nil {
			t.Fatalf("unexpected image payload: %#v", res.Payload)
		}
		if got := readArtifact(t, localSpool, *imgs.Images[0].Handle); !bytes.Equal(got, pngBytes) {
			t.Fatalf("staged image = %q, want %q", got, pngBytes)
		}
		if res.Source.Root().Kind() == 0 {
			t.Fatalf("expected parsed strict oif.Document")
		}

		usedBefore := localSpool.UsedBytes()
		_, fail = tr.Do(t.Context(), target, &media.UpstreamCall{
			Method:   http.MethodPost,
			Endpoint: media.Path{Path: "partial-bad"},
			Body:     media.JSONBody(`{"prompt":"cat"}`),
			Response: media.ImageReply{},
		}, Options{})
		if fail == nil || fail.Class != upstream.Protocol {
			t.Fatalf("partial bad image = %+v, want Protocol", fail)
		}
		if localSpool.UsedBytes() != usedBefore {
			t.Fatalf("spool leaked bytes after rollback: before=%d after=%d", usedBefore, localSpool.UsedBytes())
		}
	})

	t.Run("ImageReply polled uses injected clock", func(t *testing.T) {
		t.Parallel()
		var srv *httptest.Server
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/start":
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"asset":"`+srv.URL+`/img.png"}`)
			case "/img.png":
				w.Header().Set("Content-Type", "image/png")
				_, _ = w.Write([]byte("polled-png"))
			}
		}))
		defer srv.Close()

		tr, target := newTestTransport(t, srv, spool, func() time.Time { return fixedNow })
		poll := &media.Poll{
			Next: func(body []byte) (media.Step, *media.Error) {
				var doc struct {
					Asset string `json:"asset"`
				}
				_ = json.Unmarshal(body, &doc)
				return media.Fetch{URL: doc.Asset, Prefix: "image/"}, nil
			},
		}
		res, fail := tr.Do(t.Context(), target, &media.UpstreamCall{
			Method:   http.MethodPost,
			Endpoint: media.Path{Path: "start"},
			Body:     media.JSONBody(`{}`),
			Response: media.ImageReply{Poll: poll},
		}, Options{})
		if fail != nil {
			t.Fatalf("polled ImageReply failed: %+v", fail)
		}
		imgs := res.Payload.(Images)
		if imgs.CreatedAt != fixedNow.Unix() {
			t.Fatalf("CreatedAt = %d, want %d", imgs.CreatedAt, fixedNow.Unix())
		}
		if got := string(readArtifact(t, spool, *imgs.Images[0].Handle)); got != "polled-png" {
			t.Fatalf("polled image bytes = %q", got)
		}
	})

	t.Run("AudioReply and InlineAudioReply", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/audio":
				w.Header().Set("Content-Type", "audio/mpeg")
				w.Header().Set("X-Characters", "42")
				_, _ = w.Write([]byte("mp3-stream"))
			case "/audio-bad-ct":
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte("not-audio"))
			case "/inline-audio":
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"audio":"inline-pcm"}`)
			case "/inline-filter":
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"blocked":true}`)
			}
		}))
		defer srv.Close()

		tr, target := newTestTransport(t, srv, spool, nil)
		res, fail := tr.Do(t.Context(), target, &media.UpstreamCall{
			Method:   http.MethodPost,
			Endpoint: media.Path{Path: "audio"},
			Body:     media.JSONBody(`{}`),
			Response: media.AudioReply{CharacterHeader: "X-Characters"},
		}, Options{Strict: true})
		if fail != nil {
			t.Fatalf("AudioReply failed: %+v", fail)
		}
		audio := res.Payload.(Audio)
		if audio.BilledCharacters == nil || *audio.BilledCharacters != 42 {
			t.Fatalf("billed characters = %v, want 42", audio.BilledCharacters)
		}
		if got := string(readArtifact(t, spool, audio.Artifact.Handle)); got != "mp3-stream" {
			t.Fatalf("audio bytes = %q", got)
		}

		_, fail = tr.Do(t.Context(), target, &media.UpstreamCall{
			Method:   http.MethodPost,
			Endpoint: media.Path{Path: "audio-bad-ct"},
			Response: media.AudioReply{},
		}, Options{Strict: true})
		if fail == nil || fail.Class != upstream.Protocol {
			t.Fatalf("strict audio bad content-type = %+v, want Protocol", fail)
		}

		res, fail = tr.Do(t.Context(), target, &media.UpstreamCall{
			Method:   http.MethodPost,
			Endpoint: media.Path{Path: "inline-audio"},
			Response: media.InlineAudioReply{Decode: func([]byte) (*media.AudioResult, *media.Error) {
				return &media.AudioResult{
					Audio:       []byte("pcm-bytes"),
					ContentType: "audio/wav",
					Tokens:      &media.ImageUsage{InputTokens: 10, OutputTokens: 20, TotalTokens: 30},
				}, nil
			}},
		}, Options{})
		if fail != nil {
			t.Fatalf("InlineAudioReply failed: %+v", fail)
		}
		inlineAudio := res.Payload.(Audio)
		if inlineAudio.Tokens == nil || inlineAudio.Tokens.TotalTokens != 30 {
			t.Fatalf("inline tokens = %+v", inlineAudio.Tokens)
		}
		if got := string(readArtifact(t, spool, inlineAudio.Artifact.Handle)); got != "pcm-bytes" {
			t.Fatalf("inline audio bytes = %q", got)
		}

		_, fail = tr.Do(t.Context(), target, &media.UpstreamCall{
			Method:    http.MethodPost,
			Endpoint:  media.Path{Path: "inline-filter"},
			Ambiguous: true,
			Response: media.InlineAudioReply{Decode: func([]byte) (*media.AudioResult, *media.Error) {
				return nil, media.Fail(http.StatusBadRequest, string(upstream.ContentFilter), "blocked by safety filter")
			}},
		}, Options{})
		if fail == nil || fail.Class != upstream.ContentFilter || fail.Ambiguous {
			t.Fatalf("content filter = %+v, want non-ambiguous ContentFilter", fail)
		}
	})

	t.Run("TranscriptReply raw text, JSON, and custom text decode", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/srt":
				w.Header().Set("Content-Type", "application/x-subrip")
				_, _ = io.WriteString(w, "1\n00:00:00,000 --> 00:00:01,000\nHi\n")
			case "/vtt":
				w.Header().Set("Content-Type", "text/vtt")
				_, _ = io.WriteString(w, "WEBVTT\n\nHi\n")
			case "/bad-srt":
				w.Header().Set("Content-Type", "application/octet-stream")
				_, _ = io.WriteString(w, "nope")
			case "/json":
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"text":"spoken words"}`)
			}
		}))
		defer srv.Close()

		tr, target := newTestTransport(t, srv, spool, nil)
		for _, tc := range []struct {
			path, format, want string
		}{
			{"srt", "srt", "Hi"},
			{"vtt", "vtt", "WEBVTT"},
		} {
			res, fail := tr.Do(t.Context(), target, &media.UpstreamCall{
				Method:   http.MethodPost,
				Endpoint: media.Path{Path: tc.path},
				Response: media.TranscriptReply{Format: tc.format},
			}, Options{})
			if fail != nil {
				t.Fatalf("raw transcript %s failed: %+v", tc.format, fail)
			}
			txt, ok := res.Payload.(Text)
			if !ok || !strings.Contains(string(txt.Body), tc.want) {
				t.Fatalf("payload = %#v, want Text containing %q", res.Payload, tc.want)
			}
		}

		_, fail := tr.Do(t.Context(), target, &media.UpstreamCall{
			Method:   http.MethodPost,
			Endpoint: media.Path{Path: "bad-srt"},
			Response: media.TranscriptReply{Format: "srt"},
		}, Options{})
		if fail == nil || fail.Class != upstream.Protocol {
			t.Fatalf("bad srt content-type = %+v, want Protocol", fail)
		}

		res, fail := tr.Do(t.Context(), target, &media.UpstreamCall{
			Method:   http.MethodPost,
			Endpoint: media.Path{Path: "json"},
			Response: media.TranscriptReply{
				Format: "text",
				Decode: func(body []byte) (*media.TranscriptionResult, *media.Error) {
					return media.DecodeTranscriptionJSON(body)
				},
			},
		}, Options{})
		if fail != nil {
			t.Fatalf("decoded text transcript failed: %+v", fail)
		}
		txt, ok := res.Payload.(Text)
		if !ok || string(txt.Body) != "spoken words" || txt.Usage == nil {
			t.Fatalf("payload = %#v, want Text with Usage", res.Payload)
		}
	})

	t.Run("EventReply returns open stream and rejects non-SSE", func(t *testing.T) {
		t.Parallel()
		var gotAccept string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAccept = r.Header.Get("Accept")
			if r.URL.Path == "/bad" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			_, _ = io.WriteString(w, "data: hello\n\n")
		}))
		defer srv.Close()

		tr, target := newTestTransport(t, srv, spool, nil)
		res, fail := tr.Do(t.Context(), target, &media.UpstreamCall{
			Method:   http.MethodPost,
			Endpoint: media.Path{Path: "events"},
			Response: media.EventReply{},
		}, Options{})
		if fail != nil {
			t.Fatalf("EventReply failed: %+v", fail)
		}
		if gotAccept != "text/event-stream" {
			t.Fatalf("Accept = %q, want text/event-stream", gotAccept)
		}
		events := res.Payload.(Events)
		defer events.Body.Close()
		streamed, err := io.ReadAll(events.Body)
		if err != nil || string(streamed) != "data: hello\n\n" {
			t.Fatalf("stream = %q err=%v", streamed, err)
		}

		_, fail = tr.Do(t.Context(), target, &media.UpstreamCall{
			Method:   http.MethodPost,
			Endpoint: media.Path{Path: "bad"},
			Response: media.EventReply{},
		}, Options{})
		if fail == nil || fail.Class != upstream.Protocol {
			t.Fatalf("non-SSE event reply = %+v, want Protocol", fail)
		}
	})

	t.Run("VideoJobReply, VideoDeletedReply, and VideoContentReply", func(t *testing.T) {
		t.Parallel()
		var srv *httptest.Server
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/create":
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, `{"id":"vid_created","object":"video","status":"queued"}`)
			case "/delete-json":
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"vid_1","deleted":true,"object":"video.deleted"}`)
			case "/delete-204":
				w.WriteHeader(http.StatusNoContent)
			case "/content-direct":
				w.Header().Set("Content-Type", "video/mp4")
				_, _ = w.Write([]byte("mp4-bytes"))
			case "/content-poll":
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"video":"`+srv.URL+`/content-direct"}`)
			}
		}))
		defer srv.Close()

		tr, target := newTestTransport(t, srv, spool, nil)
		res, fail := tr.Do(t.Context(), target, &media.UpstreamCall{
			Method:   http.MethodPost,
			Endpoint: media.Path{Path: "create"},
			Response: media.VideoJobReply{},
		}, Options{Strict: true})
		if fail != nil {
			t.Fatalf("VideoJobReply 201 failed: %+v", fail)
		}
		if res.Status != http.StatusCreated || res.Payload.(Video).ID != "vid_created" {
			t.Fatalf("unexpected VideoJobReply: status=%d payload=%#v", res.Status, res.Payload)
		}

		res, fail = tr.Do(t.Context(), target, &media.UpstreamCall{
			Method:   http.MethodDelete,
			Endpoint: media.Path{Path: "delete-json"},
			Response: media.VideoDeletedReply{},
		}, Options{})
		if fail != nil || !res.Payload.(Deleted).Deleted || res.Payload.(Deleted).ID != "vid_1" {
			t.Fatalf("VideoDeletedReply JSON = %+v / %+v", res, fail)
		}

		res, fail = tr.Do(t.Context(), target, &media.UpstreamCall{
			Method:   http.MethodDelete,
			Endpoint: media.Path{Path: "delete-204"},
			Response: media.VideoDeletedReply{NoContent: true},
		}, Options{})
		if fail != nil || res.Status != http.StatusNoContent || !res.Payload.(Deleted).Deleted {
			t.Fatalf("VideoDeletedReply 204 = %+v / %+v", res, fail)
		}

		_, fail = tr.Do(t.Context(), target, &media.UpstreamCall{
			Method:   http.MethodDelete,
			Endpoint: media.Path{Path: "delete-204"},
			Response: media.VideoDeletedReply{NoContent: false},
		}, Options{})
		if fail == nil || fail.Status != http.StatusNoContent {
			t.Fatalf("disallowed 204 = %+v, want refusal", fail)
		}

		res, fail = tr.Do(t.Context(), target, &media.UpstreamCall{
			Method:   http.MethodGet,
			Endpoint: media.Path{Path: "content-direct"},
			Response: media.VideoContentReply{},
		}, Options{Strict: true})
		if fail != nil {
			t.Fatalf("direct VideoContentReply failed: %+v", fail)
		}
		if got := string(readArtifact(t, spool, res.Payload.(Content).Artifact.Handle)); got != "mp4-bytes" {
			t.Fatalf("direct video bytes = %q", got)
		}

		poll := &media.Poll{
			Next: func(body []byte) (media.Step, *media.Error) {
				var doc struct {
					Video string `json:"video"`
				}
				_ = json.Unmarshal(body, &doc)
				return media.Fetch{URL: doc.Video, Prefix: "video/"}, nil
			},
		}
		res, fail = tr.Do(t.Context(), target, &media.UpstreamCall{
			Method:   http.MethodGet,
			Endpoint: media.Path{Path: "content-poll"},
			Response: media.VideoContentReply{Poll: poll},
		}, Options{})
		if fail != nil {
			t.Fatalf("polled VideoContentReply failed: %+v", fail)
		}
		if got := string(readArtifact(t, spool, res.Payload.(Content).Artifact.Handle)); got != "mp4-bytes" {
			t.Fatalf("polled video bytes = %q", got)
		}
	})
}

func TestCallerCredentialRedactionAndEgress(t *testing.T) {
	t.Parallel()
	const callerSecret = "sk-caller-top-secret-999"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"auth_error","message":"invalid key `+callerSecret+`"}}`)
	}))
	defer srv.Close()

	tr, baseTarget := newTestTransport(t, srv, nil, nil)
	baseTarget.Config.CredentialSource = "caller"
	call := &media.UpstreamCall{
		Method:   http.MethodPost,
		Endpoint: media.Path{Path: "v1/images/generations"},
		Body:     media.JSONBody(`{}`),
		Response: media.ImageReply{},
	}

	_, fail := tr.Do(t.Context(), baseTarget, call, Options{})
	if fail == nil || fail.Class != upstream.Credential || fail.Dispatched {
		t.Fatalf("missing caller credential = %+v, want undispatched Credential", fail)
	}

	sensitive := &egress.Sensitive{}
	callerTarget := baseTarget.WithCaller([]byte(callerSecret), sensitive)
	_, fail = tr.Do(t.Context(), callerTarget, call, Options{})
	if fail == nil || fail.Status != http.StatusUnauthorized || fail.Upstream == nil {
		t.Fatalf("caller refusal = %+v, want 401 with upstream error", fail)
	}
	if strings.Contains(fail.Upstream.Message, callerSecret) || !strings.Contains(fail.Upstream.Message, "[REDACTED]") {
		t.Fatalf("secret leaked in upstream message: %q", fail.Upstream.Message)
	}

	_, fail = tr.Do(t.Context(), baseTarget.WithCaller([]byte(callerSecret), nil), &media.UpstreamCall{
		Method:   http.MethodGet,
		Endpoint: media.Absolute{URL: "http://169.254.169.254/latest/meta-data"},
		Response: media.VideoJobReply{},
	}, Options{})
	if fail == nil || fail.Class != upstream.Protocol || fail.Dispatched {
		t.Fatalf("disallowed absolute endpoint = %+v, want undispatched Protocol", fail)
	}
}

func TestRetryAfterAndFirstByteWithInjectedClock(t *testing.T) {
	t.Parallel()
	baseTime := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)

	t.Run("Retry-After delta seconds and HTTP date", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name   string
			status int
			header string
			want   time.Duration
		}{
			{"delta seconds", http.StatusTooManyRequests, "42", 42 * time.Second},
			{"future http date", http.StatusTooManyRequests, baseTime.Add(15 * time.Second).Format(http.TimeFormat), 15 * time.Second},
			{"past http date clamps to zero", http.StatusTooManyRequests, baseTime.Add(-10 * time.Second).Format(http.TimeFormat), 0},
			{"ignored on non rate limit", http.StatusInternalServerError, "42", 0},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Retry-After", tc.header)
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, `{"error":{"message":"slow down"}}`)
				}))
				defer srv.Close()

				tr, target := newTestTransport(t, srv, nil, func() time.Time { return baseTime })
				_, fail := tr.Do(t.Context(), target, &media.UpstreamCall{
					Method:   http.MethodPost,
					Endpoint: media.Path{Path: "v1/images/generations"},
					Body:     media.JSONBody(`{}`),
					Response: media.ImageReply{},
				}, Options{})
				if fail == nil || fail.RetryAfter != tc.want {
					t.Fatalf("RetryAfter = %v (fail=%+v), want %v", fail.RetryAfter, fail, tc.want)
				}
			})
		}
	})

	t.Run("FirstByte derives from injected clock", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"vid_t","object":"video","status":"queued"}`)
		}))
		defer srv.Close()

		ticks := []time.Time{baseTime, baseTime.Add(250 * time.Millisecond)}
		idx := 0
		clock := func() time.Time {
			if idx >= len(ticks) {
				return ticks[len(ticks)-1]
			}
			now := ticks[idx]
			idx++
			return now
		}

		tr, target := newTestTransport(t, srv, nil, clock)
		res, fail := tr.Do(t.Context(), target, &media.UpstreamCall{
			Method:   http.MethodGet,
			Endpoint: media.Path{Path: "v1/videos/vid_t"},
			Response: media.VideoJobReply{},
		}, Options{})
		if fail != nil {
			t.Fatalf("Do failed: %+v", fail)
		}
		if res.FirstByte != 250*time.Millisecond {
			t.Fatalf("FirstByte = %v, want 250ms", res.FirstByte)
		}
	})
}

func TestCollectBoundAndContextCancellation(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"`+strings.Repeat("a", 128)+`"}`)
	}))
	defer srv.Close()

	policy := loopbackPolicy()
	tr := New(Deps{
		Client:           srv.Client(),
		Auth:             connectors.NewAuth(policy, func(string) string { return "" }),
		Egress:           policy,
		MaxResponseBytes: 32,
	})
	target := Target{Config: connectors.Config{Endpoint: srv.URL, AuthMode: connectors.AuthNone}}
	_, fail := tr.Do(t.Context(), target, &media.UpstreamCall{
		Method:   http.MethodGet,
		Endpoint: media.Path{Path: "big"},
		Response: media.TranscriptReply{Format: "json"},
	}, Options{})
	if fail == nil || fail.Class != upstream.Protocol || !fail.Dispatched {
		t.Fatalf("oversized response = %+v, want dispatched Protocol", fail)
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, fail = tr.Do(canceled, target, &media.UpstreamCall{
		Method:   http.MethodGet,
		Endpoint: media.Path{Path: "big"},
		Response: media.TranscriptReply{Format: "json"},
	}, Options{})
	if fail == nil || fail.Class != upstream.Cancelled {
		t.Fatalf("canceled request = %+v, want Cancelled", fail)
	}

	if !sameOrigin("https://api.example.com:443/a", "https://API.EXAMPLE.COM:443/b") {
		t.Fatalf("sameOrigin should match case-insensitively")
	}
	if sameOrigin("https://user@api.example.com/a", "https://api.example.com/b") {
		t.Fatalf("sameOrigin must reject embedded userinfo")
	}
}
