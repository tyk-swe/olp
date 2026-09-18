package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

type mediaDeadlineWriter struct {
	*httptest.ResponseRecorder
	readDeadline time.Time
}

func (w *mediaDeadlineWriter) SetReadDeadline(d time.Time) error { w.readDeadline = d; return nil }
func (w *mediaDeadlineWriter) SetWriteDeadline(time.Time) error  { return nil }

type checkedMediaBody struct {
	io.ReadCloser
	check func()
	err   error
}

func (b checkedMediaBody) Read(p []byte) (int, error) {
	b.check()
	if b.err != nil {
		return 0, b.err
	}
	return b.ReadCloser.Read(p)
}

func TestMediaJSONBodyDeadlineAndAdmissionCleanup(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "stalled"}[timeout], func(t *testing.T) {
			h := newMediaHarness(t)
			h.mock.set("a", status(200, `{"data":[{"url":"https://example.test/image.png"}]}`))
			w := &mediaDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
			r := httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(`{"model":"team-chat","prompt":"photo"}`))
			r.Header.Set("Authorization", "Bearer "+fullKey)
			r.Header.Set("Content-Type", "application/json")
			checked := false
			body := checkedMediaBody{ReadCloser: r.Body, check: func() {
				checked = true
				remaining := time.Until(w.readDeadline)
				if remaining <= 0 || remaining > requestBodyTimeout {
					t.Error("body read has no bounded deadline")
				}
			}}
			if timeout {
				body.err = os.ErrDeadlineExceeded
			}
			r.Body = body
			h.gateway.imageGenerations(w, r)
			if !checked {
				t.Fatal("body was not read")
			}
			if timeout {
				if w.Code != http.StatusRequestTimeout || w.readDeadline.IsZero() {
					t.Fatalf("timeout status=%d deadline=%v", w.Code, w.readDeadline)
				}
				if h.mock.count("a") != 0 {
					t.Fatal("timed-out upload reached provider")
				}
			} else if w.Code != http.StatusOK || !w.readDeadline.IsZero() {
				t.Fatalf("completed upload status=%d deadline=%v", w.Code, w.readDeadline)
			}
			if h.gateway.admission.Admitted() != 0 {
				t.Fatal("upload retained admission")
			}
		})
	}
}

func TestSpeechDeliveryDeterminesTerminalOutcome(t *testing.T) {
	for _, failure := range []string{"none", "write", "flush"} {
		t.Run(failure, func(t *testing.T) {
			h := newMediaHarness(t)
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "audio/mpeg")
				io.WriteString(w, "audio-content")
			})
			r := httptest.NewRequest("POST", "/v1/audio/speech", strings.NewReader(`{"model":"team-chat","input":"hello","voice":"alloy"}`))
			r.Header.Set("Authorization", "Bearer "+fullKey)
			r.Header.Set("Content-Type", "application/json")
			ended := false
			var env Envelope
			h.gateway.Sink = mediaSinkFunc(func(e Envelope) { ended = true; env = e })
			w := &unaryResponseWriter{ResponseRecorder: httptest.NewRecorder(), beforeDelivery: func() {
				if ended {
					t.Error("terminal event preceded response delivery")
				}
			}}
			if failure == "write" {
				w.writeErr = io.ErrClosedPipe
			}
			if failure == "flush" {
				w.flushErr = io.ErrClosedPipe
			}
			h.gateway.mediaJSONHandler(media.DecodeSpeech)(w, r)
			wantOutcome, wantStatus := "success", 200
			if failure != "none" {
				wantOutcome, wantStatus = "cancelled", 0
			}
			if !ended || env.Outcome != wantOutcome || env.Status != wantStatus || !env.Committed {
				t.Fatalf("delivery outcome: %+v", env)
			}
			if len(env.Attempts) != 1 || env.Attempts[0].Status != 200 || h.mock.count("b") != 0 {
				t.Fatalf("upstream evidence changed after delivery: %+v", env.Attempts)
			}
			if w.deadline.IsZero() || h.gateway.admission.Admitted() != 0 || h.gateway.Media.Jobs.Transport.Spool.UsedBytes() != 0 {
				t.Fatal("unbounded response or leaked admission/artifact")
			}
		})
	}
}

func TestMissingMediaArtifactReportsDeliveryFailure(t *testing.T) {
	h := newMediaHarness(t)
	spool := h.gateway.Media.Jobs.Transport.Spool
	artifact, err := spool.PutBytes(t.Context(), "audio.mp3", "audio/mpeg", []byte("audio"), 5)
	if err != nil {
		t.Fatal(err)
	}
	if err := spool.Remove(artifact.Handle); err != nil {
		t.Fatal(err)
	}
	out := &mediaOutcome{result: &media.Result{Artifact: artifact, ContentType: "audio/mpeg"}, status: 200}
	x := &execution{family: openai.FamilySpeech}
	w := &unaryResponseWriter{ResponseRecorder: httptest.NewRecorder(), beforeDelivery: func() {}}
	h.gateway.streamArtifact(w, x, out)
	h.gateway.finishMedia(x, out, out.status)
	env := h.sink.last(t)
	if w.Code != 502 || env.Status != 502 || env.Outcome != "failure" || env.Committed {
		t.Fatalf("missing artifact reported success: %+v", env)
	}
}
