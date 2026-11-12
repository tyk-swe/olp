package gateway

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

	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/media/transport"
	"github.com/tyk-swe/olp/internal/mediacontract"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/sse"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/staging"
	"github.com/tyk-swe/olp/internal/usage"
)

func mediaUsage(request *media.Request, result *transport.Result) *wire.Usage {
	switch payload := result.Payload.(type) {
	case transport.Images:
		units := strconv.Itoa(len(payload.Images))
		if payload.Usage == nil {
			return &wire.Usage{MediaUnits: &units}
		}
		usage := tokenUsage(payload.Usage)
		usage.MediaUnits = &units
		return usage
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

func strictBlob(descriptor oif.Descriptor, artifact *staging.Artifact) error {
	blob, err := artifact.BlobReference()
	if err != nil {
		return err
	}
	_, err = oif.NewBlobResult(descriptor, blob, oif.Complete)
	return err
}

func (s *Server) removeImages(images *media.ImageResult) {
	for _, image := range images.Images {
		if image.Handle == nil {
			continue
		}
		if err := s.deps.Staging.Remove(*image.Handle); err != nil {
			s.log.Warn("staged media image cleanup failed", "error", err)
		}
	}
}

func tokenUsage(tokens *media.ImageUsage) *wire.Usage {
	return &wire.Usage{InputTokens: tokens.InputTokens, OutputTokens: tokens.OutputTokens, TotalTokens: tokens.TotalTokens,
		AudioInputTokens: tokens.AudioInputTokens, AudioOutputTokens: tokens.AudioOutputTokens,
		ImageInputTokens: tokens.ImageInputTokens, ImageOutputTokens: tokens.ImageOutputTokens}
}

func (s *Server) writeMediaResult(w http.ResponseWriter, x *call, out *mediaOutcome) {
	result := out.result
	switch payload := result.Payload.(type) {
	case transport.Images:
		if x.strict() && result.Source.Valid() {
			defer s.removeImages(payload.ImageResult)
			s.deliverMediaJSON(w, x, out, result.Source.Bytes())
			return
		}
		s.writeImageResult(w, x, out, payload.ImageResult)
	case transport.Audio:
		s.streamArtifact(w, x, out, payload.Artifact)
	case transport.Content:
		s.streamArtifact(w, x, out, payload.Artifact)
	case transport.Text:
		out.committed = true
		format := "text/plain; charset=utf-8"
		switch x.media.TranscriptFormat() {
		case "srt":
			format = "application/x-subrip; charset=utf-8"
		case "vtt":
			format = "text/vtt; charset=utf-8"
		}
		s.deliverMedia(w, x, out, format, func() error {
			_, err := w.Write(payload.Body)
			return err
		})
	case transport.Transcript:
		out.committed = true
		if x.strict() && result.Source.Valid() {
			s.deliverMediaJSON(w, x, out, result.Source.Bytes())
			return
		}
		body, failure := media.EncodeTranscriptionJSON(payload.TranscriptionResult)
		if failure != nil {
			out.err = fault.ServerError(http.StatusBadGateway, failure.Code, failure.Message)
			out.status, out.committed = out.err.Status, false
			fault.WriteSurfaceError(w, out.err, "openai")
			return
		}
		s.deliverMediaJSON(w, x, out, body)
	case transport.Video:
		var body []byte
		var failure *media.Error
		if x.strict() {
			body, failure = media.EncodeStrictVideoObject(result.Source, payload.ID, outJobID(out), x.media.Route())
		} else {
			body, failure = media.EncodeVideoObject(payload.VideoJobResult, outJobID(out), x.media.Route())
		}
		if failure != nil {
			out.err = fault.ServerError(http.StatusBadGateway, failure.Code, failure.Message)
			out.status, out.committed = out.err.Status, false
			fault.WriteSurfaceError(w, out.err, "openai")
			return
		}
		s.deliverMediaJSON(w, x, out, body)
	}
}

func (s *Server) deliverMedia(w http.ResponseWriter, x *call, out *mediaOutcome, contentType string, write func() error) {
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout)); err != nil {
		out.err = fault.ServerError(http.StatusInternalServerError, "internal_error", "The response could not be written.")
		out.status, out.committed = out.err.Status, false
		w.Header().Del("Content-Length")
		fault.WriteSurfaceError(w, out.err, "openai")
		return
	}
	w.Header().Set("Content-Type", contentType)
	x.responseHeaders(w.Header(), false)
	w.WriteHeader(out.status)
	out.committed = true
	err := write()
	if err == nil {
		err = rc.Flush()
	}
	if err != nil {
		out.err = (&attemptFailure{class: classCancelled}).toError()
		out.cancelled, out.status = true, 0
		if n := len(x.facts); n > 0 && x.facts[n-1].Interaction != nil {
			x.facts[n-1].Interaction.ClientState = usage.ClientPartial
		}
		return
	}
	x.delivered(s.now())
	if n := len(x.facts); n > 0 && x.facts[n-1].Interaction != nil {
		x.facts[n-1].Interaction.ClientState = usage.ClientTerminal
	}
}

func (s *Server) deliverMediaJSON(w http.ResponseWriter, x *call, out *mediaOutcome, body []byte) {
	s.deliverMedia(w, x, out, "application/json", func() error {
		_, err := w.Write(body)
		return err
	})
}

func outJobID(out *mediaOutcome) string {
	if out.localJob != "" {
		return out.localJob
	}
	if video := mediaVideo(out.result); video != nil {
		return video.ID
	}
	return ""
}

func (s *Server) writeImageResult(w http.ResponseWriter, x *call, out *mediaOutcome, images *media.ImageResult) {
	defer s.removeImages(images)
	body, markers, failure := media.EncodeImageResponse(images)
	if failure != nil {
		out.err = fault.ServerError(http.StatusBadGateway, failure.Code, failure.Message)
		out.status, out.committed = out.err.Status, false
		fault.WriteSurfaceError(w, out.err, "openai")
		return
	}
	handles := map[string]staging.Handle{}
	index := 0
	for _, image := range images.Images {
		if image.Handle != nil && index < len(markers) {
			handles[markers[index]] = *image.Handle
			index++
		}
	}
	s.deliverMedia(w, x, out, "application/json", func() error {
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
		if _, err := w.Write(rest); err != nil {
			return err
		}
		return nil
	})
}

func (s *Server) streamBase64(w io.Writer, handle staging.Handle) error {
	opened, err := s.deps.Staging.Open(handle)
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

func (s *Server) streamArtifact(w http.ResponseWriter, x *call, out *mediaOutcome, artifact *staging.Artifact) {
	opened, err := s.deps.Staging.Open(artifact.Handle)
	if err != nil {
		s.deps.Staging.Remove(artifact.Handle)
		out.err = fault.ServerError(http.StatusBadGateway, "media_content_unavailable", "The staged media could not be read.")
		out.status, out.committed = out.err.Status, false
		fault.WriteSurfaceError(w, out.err, "openai")
		return
	}
	defer s.deps.Staging.Remove(artifact.Handle)
	defer opened.File.Close()
	if artifact.ContentLength > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(artifact.ContentLength, 10))
	}
	s.deliverMedia(w, x, out, artifact.ContentType, func() error {
		_, err := io.Copy(w, opened.File)
		return err
	})
}

func (s *Server) streamMediaEvents(ctx context.Context, w http.ResponseWriter, x *call, events io.ReadCloser, contract *mediacontract.Template, bound mediacontract.Bound) (*wire.Usage, bool, *attemptFailure) {
	defer events.Close()
	sw := &fault.StreamWriter{W: w, Family: x.family, Headers: streamedHeaders{x}}
	var usage *wire.Usage
	var sequence uint64
	terminal := errors.New("media stream completed")
	decodeErr := sse.Decode(events, int(s.cfg.MaxEventBytes), func(frame sse.Frame) error {
		var payload struct {
			Type  string          `json:"type"`
			Error json.RawMessage `json:"error"`
		}
		event := "done"
		if frame.Data != "[DONE]" {
			if contract != nil {
				frameName := ""
				if frame.Event != nil {
					frameName = *frame.Event
				}
				if _, err := contract.Event(bound.Descriptor, frame.Data, frameName, sequence); err != nil {
					return err
				}
				sequence++
			}
			if err := json.Unmarshal([]byte(frame.Data), &payload); err != nil {
				return err
			}
			event = payload.Type
			if frame.Event != nil && *frame.Event != "" {
				event = *frame.Event
			}
		}
		if event == "error" || (len(payload.Error) > 0 && string(payload.Error) != "null") {
			return errors.New("provider media stream failed")
		}
		var buf bytes.Buffer
		if contract != nil && frame.ID != nil {
			buf.WriteString("id: " + *frame.ID + "\n")
		}
		if contract != nil && frame.RetryMS != nil {
			buf.WriteString("retry: " + strconv.FormatUint(*frame.RetryMS, 10) + "\n")
		}
		if frame.Event != nil && *frame.Event != "" {
			buf.WriteString("event: " + *frame.Event + "\n")
		}
		for line := range strings.SplitSeq(frame.Data, "\n") {
			buf.WriteString("data: " + line + "\n")
		}
		buf.WriteByte('\n')
		if observed := media.ObserveMediaUsage(frame.Data, x.media.Op()); observed != nil {
			if usage == nil {
				usage = &wire.Usage{}
			}
			if observed.Input != nil {
				usage.InputTokens = *observed.Input
			}
			if observed.Output != nil {
				usage.OutputTokens = *observed.Output
			}
			if observed.Input != nil && observed.Output != nil {
				usage.TotalTokens = *observed.Input + *observed.Output
			}
			if observed.Cached != nil {
				usage.CachedInputTokens = observed.Cached
			}
			usage.AudioInputTokens = observed.AudioInput
			usage.AudioOutputTokens = observed.AudioOutput
			usage.ImageInputTokens = observed.ImageInput
			usage.ImageOutputTokens = observed.ImageOutput
		}
		completed := media.IsMediaTerminal(event)
		if completed {
			if units := x.media.SeedMediaUnits(); units != nil {
				if usage == nil {
					usage = &wire.Usage{}
				}
				usage.MediaUnits = units
			}
		}
		if err := sw.Emit(buf.Bytes()); err != nil {
			return err
		}
		x.delivered(s.now())
		if completed {
			return terminal
		}
		return nil
	})
	if errors.Is(decodeErr, terminal) {
		return usage, sw.Committed, nil
	}
	class := classProtocol
	switch {
	case errors.Is(decodeErr, fault.ErrClientWrite), errors.Is(ctx.Err(), context.Canceled):
		class = classCancelled
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		class = classTimeout
	}
	return usage, sw.Committed, &attemptFailure{class: class, committed: sw.Committed, dispatched: true}
}
