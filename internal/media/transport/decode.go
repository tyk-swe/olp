package transport

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/staging"
	"github.com/tyk-swe/olp/internal/upstream"
)

func protocol(detail string) *Failure {
	return &Failure{Class: upstream.Protocol, Detail: detail}
}

func decodeFailure(err *media.Error) *Failure {
	if err.Status >= 400 && err.Status < 500 {
		return &Failure{Class: upstream.ClientError, Status: err.Status,
			Upstream: &wire.UpstreamError{Type: "invalid_request_error", Code: err.Code, Message: err.Message}, Detail: err.Message}
	}
	return protocol(err.Message)
}

func stageFailure(err error) *Failure {
	if _, tooLarge := errors.AsType[*staging.TooLargeError](err); tooLarge {
		return &Failure{Class: upstream.Protocol, Dispatched: true, Detail: "The media body exceeds the configured limit."}
	}
	return &Failure{Class: upstream.Connect, Dispatched: true, Detail: "The media staging area is unavailable."}
}

func (t *Transport) decode(ctx context.Context, resp *http.Response, response media.Response, strict bool, poll poller) (Payload, oif.Document, *Failure) {
	switch reply := response.(type) {
	case media.EventReply:
		if !hasType(resp, "text/event-stream") {
			return nil, oif.Document{}, protocol("streamed media response is not text/event-stream")
		}
		return Events{Body: resp.Body}, oif.Document{}, nil
	case media.AudioReply:
		payload, failure := t.audio(ctx, resp, reply, strict)
		return payload, oif.Document{}, failure
	case media.InlineAudioReply:
		payload, failure := t.inlineAudio(ctx, resp, reply)
		return payload, oif.Document{}, failure
	case media.VideoContentReply:
		payload, failure := t.content(ctx, resp, reply, strict, poll)
		return payload, oif.Document{}, failure
	case media.TranscriptReply:
		return t.transcript(resp, reply, strict, poll)
	case media.ImageReply:
		return t.images(ctx, resp, reply, strict, poll)
	case media.VideoJobReply:
		body, source, failure := t.document(resp, "video response is not JSON", strict, media.MaxNativeVideoSourceBytes)
		if failure != nil {
			return nil, oif.Document{}, failure
		}
		video, err := reply.Video(body)
		if err != nil {
			return nil, oif.Document{}, protocol(err.Message)
		}
		return Video{video}, source, nil
	case media.VideoDeletedReply:
		if reply.NoContent && resp.StatusCode == http.StatusNoContent {
			return Deleted{&media.VideoDeleteResult{Deleted: true}}, oif.Document{}, nil
		}
		body, source, failure := t.document(resp, "video response is not JSON", strict, media.MaxNativeVideoSourceBytes)
		if failure != nil {
			return nil, oif.Document{}, failure
		}
		deleted, err := media.DecodeVideoDeleteResponse(body)
		if err != nil {
			return nil, oif.Document{}, protocol(err.Message)
		}
		return Deleted{deleted}, source, nil
	}
	return nil, oif.Document{}, protocol("unsupported media response kind")
}

func (t *Transport) document(resp *http.Response, notJSON string, strict bool, bound int) ([]byte, oif.Document, *Failure) {
	if !hasType(resp, "application/json") {
		return nil, oif.Document{}, protocol(notJSON)
	}
	body, failure := t.collect(resp.Body)
	if failure != nil || !strict {
		return body, oif.Document{}, failure
	}
	if len(body) > bound {
		return nil, oif.Document{}, protocol("native video metadata exceeds its durable bound")
	}
	source, err := oif.ParseJSON(body, oif.Limits{MaxBytes: int(t.deps.MaxResponseBytes)})
	if err != nil || source.Root().Kind() != oif.Object {
		return nil, oif.Document{}, protocol("native media result is malformed or ambiguous")
	}
	return body, source, nil
}

const unbounded = int(^uint(0) >> 1)

func (t *Transport) audio(ctx context.Context, resp *http.Response, reply media.AudioReply, strict bool) (Payload, *Failure) {
	contentType, failure := stagedType(resp, strict, "native audio codec metadata is invalid", "audio/")
	if failure != nil {
		return nil, failure
	}
	artifact, err := t.stage(ctx, "speech", contentType, resp.Body)
	if err != nil {
		return nil, stageFailure(err)
	}
	return Audio{Artifact: artifact, BilledCharacters: billedCharacters(resp, reply.CharacterHeader)}, nil
}

func billedCharacters(resp *http.Response, header string) *int64 {
	if header == "" {
		return nil
	}
	billed, err := strconv.ParseInt(resp.Header.Get(header), 10, 64)
	if err != nil || billed < 0 {
		return nil
	}
	return &billed
}

func (t *Transport) inlineAudio(ctx context.Context, resp *http.Response, reply media.InlineAudioReply) (Payload, *Failure) {
	body, _, failure := t.document(resp, "speech response is not JSON", false, unbounded)
	if failure != nil {
		return nil, failure
	}
	decoded, err := reply.Decode(body)
	if err != nil {
		return nil, decodeFailure(err)
	}
	artifact, stageErr := t.put(ctx, "speech", decoded.ContentType, decoded.Audio)
	if stageErr != nil {
		return nil, stageFailure(stageErr)
	}
	return Audio{Artifact: artifact, Tokens: decoded.Tokens}, nil
}

func (t *Transport) content(ctx context.Context, resp *http.Response, reply media.VideoContentReply, strict bool, poll poller) (Payload, *Failure) {
	if reply.Poll != nil {
		body, _, failure := t.document(resp, "video job response is not JSON", false, unbounded)
		if failure != nil {
			return nil, failure
		}
		asset, failure := poll.fetch(*reply.Poll, body, "the vendor named no video content")
		if failure != nil {
			return nil, failure
		}
		artifact, err := t.put(ctx, "video-content", baseType(asset.contentType), asset.body)
		if err != nil {
			return nil, stageFailure(err)
		}
		return Content{Artifact: artifact}, nil
	}
	contentType, failure := stagedType(resp, strict, "native video codec metadata is invalid", "video/", "image/")
	if failure != nil {
		return nil, failure
	}
	artifact, err := t.stage(ctx, "video-content", contentType, resp.Body)
	if err != nil {
		return nil, stageFailure(err)
	}
	return Content{Artifact: artifact}, nil
}

func (t *Transport) transcript(resp *http.Response, reply media.TranscriptReply, strict bool, poll poller) (Payload, oif.Document, *Failure) {
	if reply.Raw() {
		if !textTranscriptType(baseType(resp.Header.Get("Content-Type")), reply.Format) {
			return nil, oif.Document{}, protocol("transcription response content type does not match the requested format")
		}
		body, failure := t.collect(resp.Body)
		if failure != nil {
			return nil, oif.Document{}, failure
		}
		return Text{Body: body}, oif.Document{}, nil
	}
	body, source, failure := t.document(resp, "transcription response is not JSON", strict, unbounded)
	if failure != nil {
		return nil, oif.Document{}, failure
	}
	final, failure := poll.finish(reply.Poll, body)
	if failure != nil {
		return nil, oif.Document{}, failure
	}
	decoded, err := reply.Transcript(final)
	if err != nil {
		return nil, oif.Document{}, decodeFailure(err)
	}
	if reply.Format == "text" {
		return Text{Body: []byte(decoded.Text), Usage: decoded}, source, nil
	}
	return Transcript{decoded}, source, nil
}

func (t *Transport) images(ctx context.Context, resp *http.Response, reply media.ImageReply, strict bool, poll poller) (Payload, oif.Document, *Failure) {
	body, source, failure := t.document(resp, "image response is not JSON", strict, unbounded)
	if failure != nil {
		return nil, oif.Document{}, failure
	}
	if reply.Poll != nil {
		asset, failure := poll.fetch(*reply.Poll, body, "the vendor's work ended without an image")
		if failure != nil {
			return nil, oif.Document{}, failure
		}
		artifact, err := t.put(ctx, "image-0.png", "image/png", asset.body)
		if err != nil {
			return nil, oif.Document{}, stageFailure(err)
		}
		return Images{&media.ImageResult{CreatedAt: time.Now().Unix(), Images: []media.ImageArtifact{{Handle: &artifact.Handle}}}}, source, nil
	}
	staged := &stagedImages{ctx: ctx, transport: t}
	decoded, err := reply.Images(body, staged.put)
	if err != nil {
		staged.discard()
		return nil, oif.Document{}, decodeFailure(err)
	}
	return Images{decoded}, source, nil
}

type stagedImages struct {
	ctx       context.Context
	transport *Transport
	handles   []staging.Handle
}

func (s *stagedImages) put(b64 string, index int) (*staging.Artifact, *media.Error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, media.Fail(http.StatusBadGateway, "provider_protocol_error", "The provider image payload is not valid base64.")
	}
	artifact, err := s.transport.put(s.ctx, "image-"+strconv.Itoa(index)+".png", "image/png", raw)
	if err != nil {
		return nil, media.Fail(http.StatusBadGateway, "provider_protocol_error", "The provider image could not be staged.")
	}
	s.handles = append(s.handles, artifact.Handle)
	return artifact, nil
}

func (s *stagedImages) discard() {
	for _, handle := range s.handles {
		if err := s.transport.deps.Spool.Remove(handle); err != nil {
			s.transport.deps.Log.Warn("staged image was not removed", "error", err)
		}
	}
}

func (t *Transport) stage(ctx context.Context, name, contentType string, body io.Reader) (*staging.Artifact, error) {
	return t.deps.Spool.Put(ctx, staging.Upload{Filename: name, ContentType: contentType,
		MaximumLength: t.deps.MaxResponseBytes, Body: io.LimitReader(body, t.deps.MaxResponseBytes+1)})
}

func (t *Transport) put(ctx context.Context, name, contentType string, body []byte) (*staging.Artifact, error) {
	return t.deps.Spool.Put(ctx, staging.Upload{Filename: name, ContentType: contentType,
		MaximumLength: int64(len(body)), Body: bytes.NewReader(body)})
}

func (t *Transport) collect(body io.Reader) ([]byte, *Failure) {
	data, err := io.ReadAll(io.LimitReader(body, t.deps.MaxResponseBytes+1))
	if err != nil {
		return nil, &Failure{Class: upstream.Connect, Dispatched: true, Detail: "upstream response read failed"}
	}
	if int64(len(data)) > t.deps.MaxResponseBytes {
		return nil, &Failure{Class: upstream.Protocol, Dispatched: true, Detail: "upstream response exceeded the response bound"}
	}
	return data, nil
}

func baseType(header string) string {
	return strings.ToLower(strings.TrimSpace(strings.Split(header, ";")[0]))
}

func hasType(resp *http.Response, want string) bool {
	value := baseType(resp.Header.Get("Content-Type"))
	return value == want || strings.HasPrefix(value, want+"+") || want == "application/json" && strings.HasSuffix(value, "+json")
}

func stagedType(resp *http.Response, strict bool, invalid string, prefixes ...string) (string, *Failure) {
	original := resp.Header.Get("Content-Type")
	if strict {
		parsed, _, err := mime.ParseMediaType(original)
		if err != nil || !hasPrefix(strings.ToLower(parsed), prefixes) {
			return "", protocol(invalid)
		}
		return original, nil
	}
	if base := baseType(original); hasPrefix(base, prefixes) {
		return base, nil
	}
	return "application/octet-stream", nil
}

func hasPrefix(value string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func textTranscriptType(value, format string) bool {
	switch format {
	case "srt":
		return value == "application/x-subrip" || strings.HasPrefix(value, "text/")
	case "vtt":
		return value == "text/vtt" || strings.HasPrefix(value, "text/")
	}
	return strings.HasPrefix(value, "text/") || value == "application/json"
}
