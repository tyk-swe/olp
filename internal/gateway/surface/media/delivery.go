package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	mediacontract "github.com/tyk-swe/olp/internal/contracts/media"
	"github.com/tyk-swe/olp/internal/domain/protocols/sse"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/media/transport"
	"github.com/tyk-swe/olp/internal/platform/staging"
	"github.com/tyk-swe/olp/internal/usage"
)

type delivery struct {
	outcome kernel.Outcome
	status  int
}

func mediaError(failure *media.Error) *fault.Error {
	if failure == nil {
		return nil
	}
	typeName := "invalid_request_error"
	if failure.Status >= 500 {
		typeName = "server_error"
	}
	return &fault.Error{Status: failure.Status, Type: typeName, Code: failure.Code, Message: failure.Message}
}

func mediaUsage(request *media.Request, result *transport.Result) *wire.Usage {
	switch payload := result.Payload.(type) {
	case transport.Images:
		units := strconv.Itoa(len(payload.Images))
		if payload.Usage == nil {
			return &wire.Usage{MediaUnits: &units}
		}
		u := tokenUsage(payload.Usage)
		u.MediaUnits = &units
		return u
	case transport.Audio:
		if payload.Tokens != nil {
			return tokenUsage(payload.Tokens)
		}
		if payload.BilledCharacters == nil {
			return nil
		}
		units := strconv.FormatInt(*payload.BilledCharacters, 10)
		return &wire.Usage{MediaUnits: &units}
	case transport.Transcript:
		return transcriptUsage(payload.TranscriptionResult)
	case transport.Text:
		return transcriptUsage(payload.Usage)
	case transport.Video:
		if request == nil || request.Op() != media.OpVideoCreate || payload.Seconds == nil {
			return nil
		}
		units := *payload.Seconds
		return &wire.Usage{MediaUnits: &units}
	}
	return nil
}

func transcriptUsage(transcript *media.TranscriptionResult) *wire.Usage {
	if transcript != nil && transcript.Tokens != nil {
		return tokenUsage(transcript.Tokens)
	}
	if transcript == nil || transcript.DurationSeconds == nil {
		return nil
	}
	duration := *transcript.DurationSeconds
	if duration < 0 || duration >= 1e18 || math.IsNaN(duration) || math.IsInf(duration, 0) {
		return nil
	}
	units := strconv.FormatFloat(duration, 'f', 6, 64)
	return &wire.Usage{MediaUnits: &units}
}

func mediaArtifact(payload transport.Payload) *staging.Artifact {
	switch payload := payload.(type) {
	case transport.Audio:
		return payload.Artifact
	case transport.Content:
		return payload.Artifact
	}
	return nil
}

func mediaVideo(result *transport.Result) *media.VideoJobResult {
	if result == nil {
		return nil
	}
	video, _ := result.Payload.(transport.Video)
	return video.VideoJobResult
}

func mediaDeleted(result *transport.Result) *media.VideoDeleteResult {
	if result == nil {
		return nil
	}
	deleted, _ := result.Payload.(transport.Deleted)
	return deleted.VideoDeleteResult
}

func tokenUsage(tokens *media.ImageUsage) *wire.Usage {
	return &wire.Usage{
		InputTokens: tokens.InputTokens, OutputTokens: tokens.OutputTokens, TotalTokens: tokens.TotalTokens,
		AudioInputTokens: tokens.AudioInputTokens, AudioOutputTokens: tokens.AudioOutputTokens,
		ImageInputTokens: tokens.ImageInputTokens, ImageOutputTokens: tokens.ImageOutputTokens,
	}
}

func (s surface) removeImages(images *media.ImageResult) {
	for _, image := range images.Images {
		if image.Handle == nil {
			continue
		}
		if err := s.spool.Remove(*image.Handle); err != nil {
			s.kernel.Log.Warn("staged media image cleanup failed", "error", err)
		}
	}
}

func (s surface) failMediaOutcome(x *kernel.Call, w http.ResponseWriter, failure *kernel.AttemptFailure) {
	e := failure.Fault()
	s.kernel.Finish(x, &kernel.Outcome{Err: e, Committed: failure.Committed()}, e.Status)
	fault.WriteSurfaceError(w, e, "openai")
}

func (s surface) writeMediaResult(w http.ResponseWriter, x *kernel.Call, request *media.Request, result *transport.Result, status int, localJob string) delivery {
	switch payload := result.Payload.(type) {
	case transport.Images:
		if x.Strict() && result.Source.Valid() {
			defer s.removeImages(payload.ImageResult)
			return s.deliverMediaJSON(w, x, status, true, result.Source.Bytes())
		}
		return s.writeImageResult(w, x, status, payload.ImageResult)
	case transport.Audio:
		return s.streamMediaArtifact(w, x, status, true, payload.Artifact)
	case transport.Content:
		return s.streamMediaArtifact(w, x, status, true, payload.Artifact)
	case transport.Text:
		format := "text/plain; charset=utf-8"
		switch request.TranscriptFormat() {
		case "srt":
			format = "application/x-subrip; charset=utf-8"
		case "vtt":
			format = "text/vtt; charset=utf-8"
		}
		return s.deliverMedia(w, x, status, format, true, func() error {
			_, err := w.Write(payload.Body)
			return err
		})
	case transport.Transcript:
		if x.Strict() && result.Source.Valid() {
			return s.deliverMediaJSON(w, x, status, true, result.Source.Bytes())
		}
		body, failure := media.EncodeTranscriptionJSON(payload.TranscriptionResult)
		if failure != nil {
			return writeEncodingFailure(w, failure)
		}
		return s.deliverMediaJSON(w, x, status, true, body)
	case transport.Video:
		jobID := localJob
		if jobID == "" {
			jobID = payload.ID
		}
		var body []byte
		var failure *media.Error
		if x.Strict() {
			body, failure = media.EncodeStrictVideoObject(result.Source, payload.ID, jobID, request.Route())
		} else {
			body, failure = media.EncodeVideoObject(payload.VideoJobResult, jobID, request.Route())
		}
		if failure != nil {
			return writeEncodingFailure(w, failure)
		}
		return s.deliverMediaJSON(w, x, status, true, body)
	}
	return delivery{outcome: kernel.Outcome{Committed: true}, status: status}
}

func writeEncodingFailure(w http.ResponseWriter, failure *media.Error) delivery {
	e := fault.ServerError(http.StatusBadGateway, failure.Code, failure.Message)
	fault.WriteSurfaceError(w, e, "openai")
	return delivery{outcome: kernel.Outcome{Err: e}, status: e.Status}
}

func (s surface) deliverMediaJSON(w http.ResponseWriter, x *kernel.Call, status int, hasUpstreamResult bool, body []byte) delivery {
	return s.deliverMedia(w, x, status, "application/json", hasUpstreamResult, func() error {
		_, err := w.Write(body)
		return err
	})
}

func (s surface) deliverMedia(w http.ResponseWriter, x *kernel.Call, status int, contentType string, hasUpstreamResult bool, write func() error) delivery {
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout)); err != nil {
		e := fault.ServerError(http.StatusInternalServerError, "internal_error", "The response could not be written.")
		w.Header().Del("Content-Length")
		fault.WriteSurfaceError(w, e, "openai")
		return delivery{outcome: kernel.Outcome{Err: e}, status: e.Status}
	}
	w.Header().Set("Content-Type", contentType)
	x.ResponseHeaders(w.Header(), false)
	w.WriteHeader(status)
	var attempt *events.AttemptFact
	if hasUpstreamResult {
		if last := x.LastAttemptFact(); last != nil {
			attempt = last
			attempt.Committed = true
		}
	}
	err := write()
	if err == nil {
		err = rc.Flush()
	}
	if err != nil {
		if attempt != nil && attempt.Interaction != nil {
			attempt.Interaction.ClientState = usage.ClientPartial
		}
		return delivery{
			outcome: kernel.Outcome{
				Err:       kernel.ClassFailure(kernel.ClassCancelled),
				Committed: true,
				Cancelled: true,
			},
			status: 0,
		}
	}
	x.Delivered(s.kernel.Now())
	if attempt != nil && attempt.Interaction != nil {
		attempt.Interaction.ClientState = usage.ClientTerminal
	}
	return delivery{outcome: kernel.Outcome{Committed: true}, status: status}
}

func (s surface) writeImageResult(w http.ResponseWriter, x *kernel.Call, status int, images *media.ImageResult) delivery {
	defer s.removeImages(images)
	body, markers, failure := media.EncodeImageResponse(images)
	if failure != nil {
		return writeEncodingFailure(w, failure)
	}
	handles := map[string]staging.Handle{}
	index := 0
	for _, image := range images.Images {
		if image.Handle != nil && index < len(markers) {
			handles[markers[index]] = *image.Handle
			index++
		}
	}
	return s.deliverMedia(w, x, status, "application/json", true, func() error {
		rest := body
		for _, marker := range markers {
			cut := bytes.Index(rest, []byte(marker))
			if cut < 0 {
				return errors.New("staged media marker missing")
			}
			if _, err := w.Write(rest[:cut]); err != nil {
				return err
			}
			if err := s.streamBase64(w, handles[marker]); err != nil {
				return err
			}
			rest = rest[cut+len(marker):]
		}
		_, err := w.Write(rest)
		return err
	})
}

func (s surface) streamBase64(w io.Writer, handle staging.Handle) error {
	opened, err := s.spool.Open(handle)
	if err != nil {
		return err
	}
	defer opened.File.Close()
	encoder := base64.NewEncoder(base64.StdEncoding, w)
	_, err = io.Copy(encoder, opened.File)
	if cerr := encoder.Close(); err == nil {
		err = cerr
	}
	return err
}

func (s surface) streamMediaArtifact(w http.ResponseWriter, x *kernel.Call, status int, hasUpstreamResult bool, artifact *staging.Artifact) delivery {
	opened, err := s.spool.Open(artifact.Handle)
	if err != nil {
		_ = s.spool.Remove(artifact.Handle)
		e := fault.ServerError(http.StatusBadGateway, "media_content_unavailable", "The staged media could not be read.")
		fault.WriteSurfaceError(w, e, "openai")
		return delivery{outcome: kernel.Outcome{Err: e}, status: e.Status}
	}
	defer func() { _ = s.spool.Remove(artifact.Handle) }()
	defer opened.File.Close()
	if artifact.ContentLength > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(artifact.ContentLength, 10))
	}
	return s.deliverMedia(w, x, status, artifact.ContentType, hasUpstreamResult, func() error {
		_, err := io.Copy(w, opened.File)
		return err
	})
}

func (s surface) streamMediaEvents(ctx context.Context, w http.ResponseWriter, x *kernel.Call, request *media.Request, stream io.ReadCloser, contract *mediacontract.Template, bound mediacontract.Bound) (*wire.Usage, bool, *kernel.AttemptFailure) {
	defer stream.Close()
	sw := x.StreamWriter(w)
	var u *wire.Usage
	var sequence uint64
	terminal := errors.New("media stream completed")
	decodeErr := sse.Decode(stream, int(s.kernel.Config.MaxEventBytes), func(frame sse.Frame) error {
		event, nextSequence, err := parseMediaEvent(frame, contract, bound, sequence)
		if err != nil {
			return err
		}
		sequence = nextSequence
		u = applyObservedUsage(u, media.ObserveMediaUsage(frame.Data, request.Op()))
		completed := media.IsMediaTerminal(event)
		if completed {
			u = applyTerminalUnits(u, request.SeedMediaUnits())
		}
		if err := sw.Emit(formatSSEFrame(frame, contract != nil)); err != nil {
			return err
		}
		x.Delivered(s.kernel.Now())
		if completed {
			return terminal
		}
		return nil
	})
	if errors.Is(decodeErr, terminal) {
		return u, sw.Committed, nil
	}
	class := kernel.ClassProtocol
	switch {
	case errors.Is(decodeErr, fault.ErrClientWrite), errors.Is(ctx.Err(), context.Canceled):
		class = kernel.ClassCancelled
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		class = kernel.ClassTimeout
	}
	return u, sw.Committed, kernel.NewAttemptFailure(kernel.AttemptFailureSpec{Class: class, Committed: sw.Committed, Dispatched: true})
}

func parseMediaEvent(frame sse.Frame, contract *mediacontract.Template, bound mediacontract.Bound, sequence uint64) (string, uint64, error) {
	if frame.Data == "[DONE]" {
		return "done", sequence, nil
	}
	nextSequence := sequence
	if contract != nil {
		frameName := ""
		if frame.Event != nil {
			frameName = *frame.Event
		}
		if _, err := contract.Event(bound.Descriptor, frame.Data, frameName, sequence); err != nil {
			return "", sequence, err
		}
		nextSequence = sequence + 1
	}
	var payload struct {
		Type  string          `json:"type"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(frame.Data), &payload); err != nil {
		return "", nextSequence, err
	}
	event := payload.Type
	if frame.Event != nil && *frame.Event != "" {
		event = *frame.Event
	}
	if event == "error" || (len(payload.Error) > 0 && string(payload.Error) != "null") {
		return "", nextSequence, errors.New("provider media stream failed")
	}
	return event, nextSequence, nil
}

func formatSSEFrame(frame sse.Frame, strict bool) []byte {
	var buf bytes.Buffer
	if strict && frame.ID != nil {
		buf.WriteString("id: " + *frame.ID + "\n")
	}
	if strict && frame.RetryMS != nil {
		buf.WriteString("retry: " + strconv.FormatUint(*frame.RetryMS, 10) + "\n")
	}
	if frame.Event != nil && *frame.Event != "" {
		buf.WriteString("event: " + *frame.Event + "\n")
	}
	for line := range strings.SplitSeq(frame.Data, "\n") {
		buf.WriteString("data: " + line + "\n")
	}
	buf.WriteByte('\n')
	return buf.Bytes()
}

func applyObservedUsage(current *wire.Usage, observed *media.MediaUsage) *wire.Usage {
	if observed == nil {
		return current
	}
	u := wire.Usage{}
	if current != nil {
		u = *current
	}
	if observed.Input != nil {
		u.InputTokens = *observed.Input
	}
	if observed.Output != nil {
		u.OutputTokens = *observed.Output
	}
	if observed.Input != nil && observed.Output != nil {
		u.TotalTokens = *observed.Input + *observed.Output
	}
	if observed.Cached != nil {
		u.CachedInputTokens = observed.Cached
	}
	u.AudioInputTokens = observed.AudioInput
	u.AudioOutputTokens = observed.AudioOutput
	u.ImageInputTokens = observed.ImageInput
	u.ImageOutputTokens = observed.ImageOutput
	return &u
}

func applyTerminalUnits(current *wire.Usage, units *string) *wire.Usage {
	if units == nil {
		return current
	}
	u := wire.Usage{}
	if current != nil {
		u = *current
	}
	u.MediaUnits = units
	return &u
}
