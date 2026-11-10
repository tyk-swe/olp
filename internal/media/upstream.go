package media

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/staging"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/upstream"
)

type FailureClass = upstream.Class

const (
	ClassConnect        = upstream.Connect
	ClassTimeout        = upstream.Timeout
	ClassRateLimit      = upstream.RateLimit
	ClassUpstreamServer = upstream.ServerError
	ClassUpstreamClient = upstream.ClientError
	ClassCredential     = upstream.Credential
	ClassProtocol       = upstream.Protocol
	ClassCancelled      = upstream.Cancelled
)

type Failure struct {
	Class      FailureClass
	Status     int
	Dispatched bool
	Ambiguous  bool
	RetryAfter time.Duration
	Upstream   *wire.UpstreamError
	Detail     string
}

type Target struct {
	Sensitive        *egress.Sensitive `json:"-"`
	CallerCredential bool
	ConnectionScope  string
	NetworkSecret    []byte
	Config           connectors.Config
	Model            string
	Secret           []byte
}

type Transport struct {
	connectionsOnce  sync.Once
	connections      *egress.ConnectionClientCache
	Client           *http.Client
	Auth             *connectors.Auth
	Egress           *egress.Policy
	Spool            *staging.Spool
	MaxResponseBytes int64
	Now              func() time.Time
}

type Result struct {
	Kind             ResponseKind
	Status           int
	FirstByte        time.Duration
	Images           *ImageResult
	Video            *VideoJobResult
	Deleted          *VideoDeleteResult
	Transcription    *TranscriptionResult
	Text             []byte
	Artifact         *staging.Artifact
	Body             io.ReadCloser
	ContentType      string
	BilledCharacters *int64
	Tokens           *ImageUsage
	Source           oif.Document
	BlobSource       *oif.BlobResult
}

const errorBodyLimit = 64 * 1024

func (t *Transport) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func (t *Transport) Do(ctx context.Context, target Target, call *UpstreamCall, request *Request) (result *Result, failure *Failure) {
	if target.CallerCredential && target.Sensitive == nil {
		target.Sensitive = &egress.Sensitive{}
	}
	if target.Sensitive != nil {
		target.Sensitive.Add(string(target.Secret))
		defer func() {
			if failure != nil {
				failure.Detail = target.Sensitive.Redact(failure.Detail)
				if failure.Upstream != nil {
					failure.Upstream = failure.Upstream.Redact(target.Sensitive.Redact)
				}
			}
		}()
	}
	if target.Config.CredentialSource == "caller" && !target.CallerCredential {
		return nil, &Failure{Class: ClassCredential, Detail: "A caller credential is required for this connection."}
	}
	if _, err := t.Egress.ValidateEndpoint(target.Config.Endpoint); err != nil {
		return nil, &Failure{Class: ClassConnect, Detail: "provider endpoint rejected by egress policy"}
	}
	endpoint, err := target.Config.MediaURL(call.Path, target.Model, call.Query)
	if call.URL != "" {
		endpoint, err = call.URL, nil
		if _, e := t.Egress.ValidateEndpoint(call.URL); e != nil {
			err = e
		}
	}
	if err != nil {
		return nil, &Failure{Class: ClassProtocol, Detail: "media endpoint could not be built"}
	}
	if call.SigningService != "" {
		target.Config.SigningService = call.SigningService
	}

	var body io.Reader = http.NoBody
	contentType := ""
	var pipe *io.PipeReader
	var multipartDone chan error
	var contentLength int64
	if call.JSONFrom != nil {
		inlined, failure := call.JSONFrom(t.readPart)
		if failure != nil {
			return nil, &Failure{Class: ClassProtocol, Detail: failure.Message}
		}
		call.JSON = inlined
	}
	switch {
	case call.JSON != nil:
		body = bytes.NewReader(call.JSON)
		contentType = "application/json"
	case call.Upload != nil:
		opened, err := t.Spool.Open(call.Upload.Handle)
		if err != nil {
			return nil, &Failure{Class: ClassConnect, Detail: "staged media upload is unavailable"}
		}
		defer opened.File.Close()
		body, contentType, contentLength = opened.File, call.Upload.ContentType, opened.Artifact.ContentLength
		if contentType == "" {
			contentType = "application/octet-stream"
		}
	case len(call.Fields) > 0:
		reader, writer := io.Pipe()
		form := multipart.NewWriter(writer)
		contentType = form.FormDataContentType()
		pipe = reader
		multipartDone = make(chan error, 1)
		body = reader
		go func() {
			multipartDone <- t.writeMultipart(writer, form, call.Fields)
		}()
	}

	var dispatched atomic.Bool
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, upstream.Trace(&dispatched)), call.Method, endpoint, body)
	if err != nil {
		if pipe != nil {
			pipe.Close()
			<-multipartDone
		}
		return nil, &Failure{Class: ClassConnect, Detail: "media request could not be built"}
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if contentLength > 0 {
		req.ContentLength = contentLength
	}
	req.Header.Set("User-Agent", "olp/gateway")
	if call.Accept != "" {
		req.Header.Set("Accept", call.Accept)
	}
	if call.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	maps.Copy(req.Header, call.Inject)

	credentialValues, err := t.Auth.Apply(ctx, req, target.Config, target.Secret, call.JSON)
	if err != nil {
		if pipe != nil {
			pipe.Close()
			<-multipartDone
		}
		if ctx.Err() != nil {
			class := upstream.Classifier{}.Classify(upstream.Evidence{Interrupted: interrupted(ctx, err), Err: err}).Class
			return nil, &Failure{Class: class, Detail: "media request interrupted"}
		}
		return nil, &Failure{Class: ClassCredential, Detail: "provider credential could not be applied"}
	}

	if target.Sensitive != nil {
		target.Sensitive.Include(credentialValues)
	}
	started := t.now()
	client := t.Client
	if target.CallerCredential {
		client, err = t.Egress.EphemeralClient(target.Config.Network, target.NetworkSecret, 5*time.Minute)
		if err != nil {
			if pipe != nil {
				pipe.CloseWithError(err)
				<-multipartDone
			}
			return nil, &Failure{Class: ClassCredential, Detail: "provider network connection unavailable"}
		}
	} else if target.Config.Network != nil || target.Config.ProfileID != "" {
		t.connectionsOnce.Do(func() { t.connections = egress.NewConnectionClientCache(128) })
		var err error
		client, err = t.connections.ClientScoped(target.ConnectionScope, *t.Egress, target.Config.Network, target.NetworkSecret, 5*time.Minute)
		if err != nil {
			if pipe != nil {
				pipe.CloseWithError(err)
				<-multipartDone
			}
			return nil, &Failure{Class: ClassCredential, Detail: "provider network connection unavailable"}
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		if pipe != nil {
			pipe.Close()
			<-multipartDone
		}
		sent := dispatched.Load()
		outcome := upstream.Classifier{}.Classify(upstream.Evidence{Reached: sent, Interrupted: interrupted(ctx, err), Err: err})
		return nil, &Failure{Class: outcome.Class, Dispatched: sent,
			Ambiguous: call.Ambiguous && outcome.Acceptance.Unresolved(), Detail: "upstream transport failed"}
	}
	firstByte := t.now().Sub(started)
	if resp.StatusCode != http.StatusOK && !(call.Kind == ResponseVideoJob && resp.StatusCode == http.StatusCreated) && !(call.NoContent && resp.StatusCode == http.StatusNoContent) {
		if pipe != nil {
			pipe.Close()
			<-multipartDone
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		resp.Body.Close()
		failure := &Failure{
			Status:     resp.StatusCode,
			Dispatched: true,
			Upstream:   openai.ParseErrorBody(raw).Redact(credentialValues.Redact),
		}
		outcome := upstream.Classifier{Declared: target.Config.Classification()}.Classify(upstream.Evidence{Reached: true, Status: resp.StatusCode, Error: failure.Upstream})
		failure.Class, failure.Ambiguous = outcome.Class, call.Ambiguous && outcome.Acceptance.Unresolved()
		if failure.Class == ClassRateLimit {
			failure.RetryAfter = upstream.RetryAfter(resp.Header.Get("Retry-After"), t.now())
		}
		return nil, failure
	}
	if multipartDone != nil {
		if sendErr := <-multipartDone; sendErr != nil {
			resp.Body.Close()
			return nil, &Failure{Class: ClassConnect, Dispatched: true, Ambiguous: call.Ambiguous,
				Detail: "multipart upload stream failed"}
		}
	}

	result, failure = t.decode(ctx, resp, call, request, firstByte, func(next *http.Request) (*http.Response, error) { return client.Do(next) }, target)
	if failure != nil {
		resp.Body.Close()
		failure.Dispatched = true
		failure.Ambiguous = call.Ambiguous && failure.Class != ClassUpstreamClient
		return nil, failure
	}
	if result.Body == nil {
		resp.Body.Close()
	}
	return result, nil
}

func (t *Transport) writeMultipart(pipe *io.PipeWriter, form *multipart.Writer, fields []staging.Field) error {
	for _, field := range fields {
		if err := t.writeField(form, field); err != nil {
			pipe.CloseWithError(err)
			return err
		}
	}
	if err := form.Close(); err != nil {
		pipe.CloseWithError(err)
		return err
	}
	return pipe.Close()
}

func (t *Transport) writeField(form *multipart.Writer, field staging.Field) error {
	switch value := field.Value.(type) {
	case staging.Text:
		return form.WriteField(field.Name, string(value))
	case staging.Part:
		opened, err := t.Spool.Open(value.Handle)
		if err != nil {
			return fmt.Errorf("spool open: %w", err)
		}
		defer opened.File.Close()
		name := cmp.Or(opened.Filename, "media")
		part, err := form.CreatePart(filePartHeader(field.Name, name, opened.Artifact.ContentType))
		if err != nil {
			return err
		}
		_, err = io.Copy(part, opened.File)
		return err
	}
	return nil
}

func filePartHeader(field, filename, contentType string) textproto.MIMEHeader {
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name="%s"; filename="%s"`, quoteEscaper(field), quoteEscaper(filename)))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	header.Set("Content-Type", contentType)
	return header
}

var quoteEscaper = strings.NewReplacer("\\", "\\\\", `"`, "\\\"", "\r", "", "\n", "", "\x00", "").Replace

func (t *Transport) decode(ctx context.Context, resp *http.Response, call *UpstreamCall, request *Request, firstByte time.Duration, send func(*http.Request) (*http.Response, error), target Target) (*Result, *Failure) {
	result := &Result{Kind: call.Kind, Status: resp.StatusCode, FirstByte: firstByte}
	retainJSON := func(body []byte) *Failure {
		if !call.Strict {
			return nil
		}
		if (call.Kind == ResponseVideoJob || call.Kind == ResponseVideoDelete) && len(body) > MaxNativeVideoSourceBytes {
			return &Failure{Class: ClassProtocol, Dispatched: true, Ambiguous: call.Ambiguous, Detail: "native video metadata exceeds its durable bound"}
		}
		doc, err := oif.ParseJSON(body, oif.Limits{MaxBytes: int(t.MaxResponseBytes)})
		if err != nil || doc.Root().Kind() != oif.Object {
			return &Failure{Class: ClassProtocol, Dispatched: true, Ambiguous: call.Ambiguous, Detail: "native media result is malformed or ambiguous"}
		}
		result.Source = doc
		return nil
	}
	switch call.Kind {
	case ResponseSSE:
		if !requireContentType(resp, "text/event-stream") {
			return nil, &Failure{Class: ClassProtocol,
				Detail: "streamed media response is not text/event-stream"}
		}
		result.Body = resp.Body
		return result, nil
	case ResponseBinary:
		if call.DecodeAudio != nil {
			return t.decodeAudio(ctx, resp, call, request, result)
		}
		contentType := binaryContentType(resp, "audio/")
		if call.Strict {
			original := resp.Header.Get("Content-Type")
			mediaType, _, err := mime.ParseMediaType(original)
			if err != nil || !strings.HasPrefix(strings.ToLower(mediaType), "audio/") {
				return nil, &Failure{Class: ClassProtocol, Dispatched: true, Detail: "native audio codec metadata is invalid"}
			}
			contentType = original
		}
		if billed, err := strconv.ParseInt(resp.Header.Get(call.CharacterHeader), 10, 64); call.CharacterHeader != "" && err == nil && billed >= 0 {
			result.BilledCharacters = &billed
		}
		artifact, failure := t.stageResponse(ctx, resp, contentType, request)
		if failure != nil {
			return nil, stageFailure(failure)
		}
		result.Artifact = artifact
		result.ContentType = artifact.ContentType
		return result, nil
	case ResponseVideoContent:
		if call.Next != nil {
			if !requireContentType(resp, "application/json") {
				return nil, &Failure{Class: ClassProtocol, Detail: "video job response is not JSON"}
			}
			body, failure := t.collect(resp)
			if failure != nil {
				return nil, failure
			}
			asset, _, failure := t.follow(ctx, call, body, send, target)
			if failure != nil {
				return nil, failure
			}
			if asset == nil {
				return nil, &Failure{Class: ClassProtocol, Dispatched: true, Detail: "the vendor named no video content"}
			}
			contentType := strings.ToLower(strings.TrimSpace(strings.Split(asset.contentType, ";")[0]))
			artifact, err := t.Spool.Put(ctx, staging.Upload{Filename: "video-content", ContentType: contentType, MaximumLength: int64(len(asset.body)), Body: bytes.NewReader(asset.body)})
			if err != nil {
				return nil, stageFailure(spoolError(err))
			}
			result.Artifact, result.ContentType = artifact, artifact.ContentType
			return result, nil
		}
		base := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
		if call.Strict {
			original := resp.Header.Get("Content-Type")
			mediaType, _, err := mime.ParseMediaType(original)
			if err != nil || !strings.HasPrefix(strings.ToLower(mediaType), "video/") && !strings.HasPrefix(strings.ToLower(mediaType), "image/") {
				return nil, &Failure{Class: ClassProtocol, Dispatched: true, Detail: "native video codec metadata is invalid"}
			}
			base = original
		}
		if !strings.HasPrefix(strings.ToLower(base), "video/") && !strings.HasPrefix(strings.ToLower(base), "image/") {
			base = "application/octet-stream"
		}
		artifact, failure := t.stageResponse(ctx, resp, base, request)
		if failure != nil {
			return nil, stageFailure(failure)
		}
		result.Artifact = artifact
		result.ContentType = artifact.ContentType
		return result, nil
	case ResponseTranscription:
		format := request.TranscriptFormat()
		if TranscriptionFormatIsText(format) && call.DecodeTranscription == nil {
			if !transcriptionTextContentType(resp, format) {
				return nil, &Failure{Class: ClassProtocol,
					Detail: "transcription response content type does not match the requested format"}
			}
			body, failure := t.collect(resp)
			if failure != nil {
				return nil, failure
			}
			result.Text = body
			return result, nil
		}
		if !requireContentType(resp, "application/json") {
			return nil, &Failure{Class: ClassProtocol,
				Detail: "transcription response is not JSON"}
		}
		body, failure := t.collect(resp)
		if failure != nil {
			return nil, failure
		}
		if failure := retainJSON(body); failure != nil {
			return nil, failure
		}
		if call.Next != nil {
			var failure *Failure
			if _, body, failure = t.follow(ctx, call, body, send, target); failure != nil {
				return nil, failure
			}
		}
		decoded, mErr := DecodeTranscript(call, format, body)
		if mErr != nil {
			return nil, decodeFailure(mErr)
		}
		result.Transcription = decoded
		if format == "text" {
			result.Text = []byte(decoded.Text)
		}
		return result, nil
	case ResponseImages:
		if !requireContentType(resp, "application/json") {
			return nil, &Failure{Class: ClassProtocol, Detail: "image response is not JSON"}
		}
		body, failure := t.collect(resp)
		if failure != nil {
			return nil, failure
		}
		if failure := retainJSON(body); failure != nil {
			return nil, failure
		}
		var stagedHandles []staging.Handle
		stageRaw := func(raw []byte, index int) (*staging.Artifact, *Error) {
			staged, sErr := t.Spool.Put(ctx, staging.Upload{
				Filename:      "image-" + strconv.Itoa(index) + ".png",
				ContentType:   "image/png",
				MaximumLength: int64(len(raw)),
				Body:          bytes.NewReader(raw),
			})
			if sErr != nil {
				return nil, spoolError(sErr)
			}
			stagedHandles = append(stagedHandles, staged.Handle)
			return staged, nil
		}
		stage := func(b64 string, index int) (*staging.Artifact, *Error) {
			raw, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				return nil, protocolError("The provider image payload is not valid base64.")
			}
			return stageRaw(raw, index)
		}
		var decoded *ImageResult
		var mErr *Error
		if call.Next != nil {
			asset, _, failure := t.follow(ctx, call, body, send, target)
			if failure != nil {
				return nil, failure
			}
			if asset == nil {
				return nil, &Failure{Class: ClassProtocol, Dispatched: true, Detail: "the vendor's work ended without an image"}
			}
			staged, sErr := stageRaw(asset.body, 0)
			if sErr != nil {
				return nil, stageFailure(sErr)
			}
			handle := staged.Handle
			decoded = &ImageResult{CreatedAt: t.now().Unix(), Images: []ImageArtifact{{Handle: &handle}}}
		} else if call.DecodeImages != nil {
			decoded, mErr = call.DecodeImages(body, stage)
		} else if call.Native != "" {
			decoded, mErr = DecodeNativeImageResponse(call.Native, body, request.expectedImages(), stage)
		} else {
			decoded, mErr = DecodeImageResponse(body, stage)
		}
		if mErr != nil {
			for _, handle := range stagedHandles {
				t.Spool.Remove(handle)
			}
			return nil, decodeFailure(mErr)
		}
		result.Images = decoded
		return result, nil
	case ResponseVideoJob, ResponseVideoDelete:
		if call.NoContent && resp.StatusCode == http.StatusNoContent {
			result.Deleted = &VideoDeleteResult{Deleted: true}
			return result, nil
		}
		if !requireContentType(resp, "application/json") {
			return nil, &Failure{Class: ClassProtocol, Detail: "video response is not JSON"}
		}
		body, failure := t.collect(resp)
		if failure != nil {
			return nil, failure
		}
		if failure := retainJSON(body); failure != nil {
			return nil, failure
		}
		var mErr *Error
		switch {
		case call.DecodeVideo != nil:
			result.Video, mErr = call.DecodeVideo(body)
		case call.Kind == ResponseVideoJob:
			result.Video, mErr = DecodeVideoObject(body)
		case call.Kind == ResponseVideoDelete:
			result.Deleted, mErr = DecodeVideoDeleteResponse(body)
		}
		if mErr != nil {
			return nil, &Failure{Class: ClassProtocol, Detail: mErr.Message}
		}
		return result, nil
	}
	return nil, &Failure{Class: ClassProtocol, Detail: "unsupported media response kind"}
}

func (t *Transport) readPart(part *staging.Part) ([]byte, *Error) {
	opened, err := t.Spool.Open(part.Handle)
	if err != nil {
		return nil, Fail(http.StatusInternalServerError, "media_unavailable", "The uploaded file is unavailable.")
	}
	defer opened.File.Close()
	data, err := io.ReadAll(io.LimitReader(opened.File, DefaultAudioUploadLimit+1))
	if err != nil || int64(len(data)) > DefaultAudioUploadLimit {
		return nil, Fail(http.StatusRequestEntityTooLarge, "file_too_large", "The uploaded file exceeds the inline upload bound.")
	}
	return data, nil
}

func (t *Transport) decodeAudio(ctx context.Context, resp *http.Response, call *UpstreamCall, request *Request, result *Result) (*Result, *Failure) {
	if !requireContentType(resp, "application/json") {
		return nil, &Failure{Class: ClassProtocol, Detail: "speech response is not JSON"}
	}
	body, failure := t.collect(resp)
	if failure != nil {
		return nil, failure
	}
	decoded, mErr := call.DecodeAudio(body)
	if mErr != nil {
		return nil, decodeFailure(mErr)
	}
	artifact, err := t.Spool.Put(ctx, staging.Upload{Filename: request.spoolName("speech"), ContentType: decoded.ContentType, MaximumLength: int64(len(decoded.Audio)), Body: bytes.NewReader(decoded.Audio)})
	if err != nil {
		return nil, stageFailure(spoolError(err))
	}
	result.Artifact, result.ContentType, result.Tokens = artifact, artifact.ContentType, decoded.Tokens
	return result, nil
}

func DecodeTranscript(call *UpstreamCall, format string, body []byte) (*TranscriptionResult, *Error) {
	if call.DecodeTranscription == nil {
		return DecodeTranscriptionJSON(body)
	}
	result, failure := call.DecodeTranscription(body)
	if failure != nil {
		return nil, failure
	}
	result.TextOnly = format == "json"
	return result, nil
}

func decodeFailure(err *Error) *Failure {
	if err.Status >= 400 && err.Status < 500 {
		return &Failure{Class: ClassUpstreamClient, Status: err.Status, Upstream: &wire.UpstreamError{Type: "invalid_request_error", Code: err.Code, Message: err.Message}, Detail: err.Message}
	}
	return &Failure{Class: ClassProtocol, Detail: err.Message}
}

func stageFailure(err *Error) *Failure {
	if err.Status == http.StatusRequestEntityTooLarge {
		return &Failure{Class: ClassProtocol, Dispatched: true, Detail: err.Message}
	}
	return &Failure{Class: ClassConnect, Dispatched: true, Detail: err.Message}
}

func (t *Transport) stageResponse(ctx context.Context, resp *http.Response, contentType string, request *Request) (*staging.Artifact, *Error) {
	name := request.spoolName("response")
	artifact, err := t.Spool.Put(ctx, staging.Upload{
		Filename:      name,
		ContentType:   contentType,
		MaximumLength: t.MaxResponseBytes,
		Body:          io.LimitReader(resp.Body, t.MaxResponseBytes+1),
	})
	if err != nil {
		return nil, spoolError(err)
	}
	return artifact, nil
}

func (t *Transport) collect(resp *http.Response) ([]byte, *Failure) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, t.MaxResponseBytes+1))
	if err != nil {
		return nil, &Failure{Class: ClassConnect, Dispatched: true, Detail: "upstream response read failed"}
	}
	if int64(len(body)) > t.MaxResponseBytes {
		return nil, &Failure{Class: ClassProtocol, Dispatched: true, Detail: "upstream response exceeded the response bound"}
	}
	return body, nil
}

func requireContentType(resp *http.Response, want string) bool {
	value := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	return value == want || strings.HasPrefix(value, want+"+") ||
		(want == "application/json" && strings.HasSuffix(value, "+json"))
}

func binaryContentType(resp *http.Response, prefix string) string {
	value := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	if strings.HasPrefix(value, prefix) {
		return value
	}
	return "application/octet-stream"
}

func transcriptionTextContentType(resp *http.Response, format string) bool {
	value := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	switch format {
	case "srt":
		return value == "application/x-subrip" || strings.HasPrefix(value, "text/")
	case "vtt":
		return value == "text/vtt" || strings.HasPrefix(value, "text/")
	default:
		return strings.HasPrefix(value, "text/") || value == "application/json"
	}
}

func interrupted(ctx context.Context, err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if ctx.Err() == context.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	var nerr interface{ Timeout() bool }
	if errors.As(err, &nerr) && nerr.Timeout() {
		return context.DeadlineExceeded
	}
	return nil
}
