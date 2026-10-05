package media

import (
	"bytes"
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

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/upstream"
)

// FailureClass is the upstream failure class of a media call.
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

// Failure is one classified upstream media failure. Ambiguous marks a
// non-idempotent request whose effect the gateway cannot prove absent.
type Failure struct {
	Class      FailureClass
	Status     int
	Dispatched bool // request bytes reached the upstream; no client response is implied
	Ambiguous  bool
	RetryAfter time.Duration
	Upstream   *openai.UpstreamError
	Detail     string
}

// Target is one resolved upstream destination for a media call.
type Target struct {
	ConnectionScope string
	NetworkSecret   []byte
	Config          connectors.Config
	Model           string // upstream model identifier
	Secret          []byte
}

// Transport executes media calls against provider endpoints.
type Transport struct {
	connectionsOnce sync.Once
	connections     *egress.ConnectionClientCache
	Client          *http.Client
	Auth            *connectors.Auth
	Egress          *egress.Policy
	Spool           *Spool
	// MaxResponseBytes bounds every collected JSON body and every staged
	// binary response.
	MaxResponseBytes int64
	Now              func() time.Time
}

// Result is one decoded upstream media response. Binary payloads are staged
// artifacts; SSE responses keep their body open for the caller to drain.
type Result struct {
	Kind          ResponseKind
	Status        int
	FirstByte     time.Duration
	Images        *ImageResult
	Video         *VideoJobResult
	List          *VideoListResult
	Deleted       *VideoDeleteResult
	Transcription *TranscriptionResult
	Text          []byte        // transcription text/srt/vtt payloads
	Artifact      *Artifact     // staged binary response
	Body          io.ReadCloser // SSE response body; the caller drains it
	ContentType   string
	// BilledCharacters is the characters a speech call bills, as the vendor
	// reports them, and Tokens the tokens it bills, for a vendor that bills
	// speech by token.
	BilledCharacters *int64
	Tokens           *ImageUsage
	Source           oif.Document    // bounded immutable native JSON result for strict media
	BlobSource       *oif.BlobResult // existing spool owns the bytes and lifecycle
}

const errorBodyLimit = 64 * 1024

func (t *Transport) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// Do dispatches one media call and decodes the response per its kind. The
// returned SSE Result leaves Body open; every other kind is fully resolved.
func (t *Transport) Do(ctx context.Context, target Target, call *UpstreamCall, request *Request) (*Result, *Failure) {
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

	// dispatched records whether request bytes may have reached the upstream.
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

	started := t.now()
	client := t.Client
	if target.Config.Network != nil || target.Config.ProfileID != "" {
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
	if resp.StatusCode != http.StatusOK && !(call.Kind == ResponseVideoJob && resp.StatusCode == http.StatusCreated) {
		// A provider can reject headers before reading the upload. Stop the
		// producer and preserve the definitive HTTP rejection in that case.
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
		// A non-idempotent call's work may survive a server failure, never a
		// stated rejection.
		outcome := upstream.Classifier{Declared: target.Config.Classification()}.Classify(upstream.Evidence{Reached: true, Status: resp.StatusCode, Error: failure.Upstream})
		failure.Class, failure.Ambiguous = outcome.Class, call.Ambiguous && outcome.Acceptance.Unresolved()
		if failure.Class == ClassRateLimit {
			failure.RetryAfter = upstream.RetryAfter(resp.Header.Get("Retry-After"), t.now())
		}
		return nil, failure
	}
	if multipartDone != nil {
		// A successful response still requires a complete request body.
		if sendErr := <-multipartDone; sendErr != nil {
			resp.Body.Close()
			return nil, &Failure{Class: ClassConnect, Dispatched: true, Ambiguous: call.Ambiguous,
				Detail: "multipart upload stream failed"}
		}
	}

	result, failure := t.decode(ctx, resp, call, request, firstByte, func(next *http.Request) (*http.Response, error) { return client.Do(next) }, target)
	if failure != nil {
		resp.Body.Close()
		failure.Dispatched = true
		failure.Ambiguous = failure.Ambiguous || call.Ambiguous
		return nil, failure
	}
	if result.Body == nil {
		resp.Body.Close()
	}
	return result, nil
}

func (t *Transport) writeMultipart(pipe *io.PipeWriter, form *multipart.Writer, fields []Field) error {
	failure := func(err error) error {
		pipe.CloseWithError(err)
		return err
	}
	for _, field := range fields {
		if field.File != nil {
			opened, err := t.Spool.Open(field.File.Handle)
			if err != nil {
				return failure(fmt.Errorf("spool open: %w", err))
			}
			name := opened.Filename
			if name == "" {
				name = "media"
			}
			part, err := form.CreatePart(filePartHeader(field.Name, name, opened.Artifact.ContentType))
			if err != nil {
				opened.File.Close()
				return failure(err)
			}
			_, copyErr := io.Copy(part, opened.File)
			opened.File.Close()
			if copyErr != nil {
				return failure(copyErr)
			}
			continue
		}
		if field.Text != nil {
			if err := form.WriteField(field.Name, *field.Text); err != nil {
				return failure(err)
			}
		}
	}
	if err := form.Close(); err != nil {
		return failure(err)
	}
	return pipe.Close()
}

// filePartHeader builds the multipart headers for one staged file part.
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
		if (call.Kind == ResponseVideoJob || call.Kind == ResponseVideoList || call.Kind == ResponseVideoDelete) && len(body) > MaxNativeVideoSourceBytes {
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
		// Without the vendor's count, the call stays billing-uncertain; the
		// audio it billed for is still delivered.
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
		format := "json"
		if request != nil && request.Format != nil {
			format = *request.Format
		}
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
		decode := DecodeTranscriptionJSON
		if call.DecodeTranscription != nil {
			decode = call.DecodeTranscription
		}
		decoded, mErr := decode(body)
		if mErr != nil {
			return nil, decodeFailure(mErr)
		}
		result.Transcription = decoded
		if format == "text" {
			// A vendor that answers in JSON serves a text client its text.
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
		var stagedHandles []Handle
		stage := func(b64 string, index int) (*Artifact, *Error) {
			raw, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				return nil, protocolError("The provider image payload is not valid base64.")
			}
			staged, sErr := t.Spool.Put(ctx, Upload{
				Filename:      "image-" + strconv.Itoa(index) + ".png",
				ContentType:   "image/png",
				MaximumLength: int64(len(raw)),
				Body:          bytes.NewReader(raw),
			})
			if sErr != nil {
				return nil, SpoolError(sErr)
			}
			stagedHandles = append(stagedHandles, staged.Handle)
			return staged, nil
		}
		var decoded *ImageResult
		var mErr *Error
		if call.Next != nil {
			var failure *Failure
			decoded, failure = t.follow(ctx, call, body, send, target, stage)
			if failure != nil {
				for _, handle := range stagedHandles {
					t.Spool.Remove(handle)
				}
				return nil, failure
			}
		} else if call.DecodeImages != nil {
			decoded, mErr = call.DecodeImages(body, stage)
		} else if call.Native != "" {
			expected := int64(1)
			if request != nil && request.Count != nil {
				expected = *request.Count
			}
			decoded, mErr = DecodeNativeImageResponse(call.Native, body, expected, stage)
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
	case ResponseVideoJob, ResponseVideoList, ResponseVideoDelete:
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
		switch call.Kind {
		case ResponseVideoJob:
			result.Video, mErr = DecodeVideoObject(body)
		case ResponseVideoList:
			result.List, mErr = DecodeVideoListResponse(body)
		case ResponseVideoDelete:
			result.Deleted, mErr = DecodeVideoDeleteResponse(body)
		}
		if mErr != nil {
			return nil, &Failure{Class: ClassProtocol, Detail: mErr.Message}
		}
		return result, nil
	}
	return nil, &Failure{Class: ClassProtocol, Detail: "unsupported media response kind"}
}

// readPart reads an uploaded file a JSON body inlines, within the audio
// upload bound.
func (t *Transport) readPart(part *Part) ([]byte, *Error) {
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

// decodeAudio stages the speech a vendor returned inside JSON.
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
	name := "speech"
	if request != nil {
		name = request.Op + "-response"
	}
	artifact, err := t.Spool.Put(ctx, Upload{Filename: name, ContentType: decoded.ContentType, MaximumLength: int64(len(decoded.Audio)), Body: bytes.NewReader(decoded.Audio)})
	if err != nil {
		return nil, stageFailure(SpoolError(err))
	}
	result.Artifact, result.ContentType, result.Tokens = artifact, artifact.ContentType, decoded.Tokens
	return result, nil
}

// decodeFailure maps a result decoder's error onto a transport failure: a
// vendor's verdict on the request, such as its content filter's, is the
// caller's error; anything else violates the vendor's protocol.
func decodeFailure(err *Error) *Failure {
	if err.Status >= 400 && err.Status < 500 {
		return &Failure{Class: ClassUpstreamClient, Status: err.Status, Upstream: &openai.UpstreamError{Type: "invalid_request_error", Code: err.Code, Message: err.Message}, Detail: err.Message}
	}
	return &Failure{Class: ClassProtocol, Detail: err.Message}
}

// stageFailure maps a spool staging error onto a transport failure: an
// oversized body is a protocol violation, anything else is a local failure
// the attempt loop may retry.
func stageFailure(err *Error) *Failure {
	if err.Status == http.StatusRequestEntityTooLarge {
		return &Failure{Class: ClassProtocol, Dispatched: true, Detail: err.Message}
	}
	return &Failure{Class: ClassConnect, Dispatched: true, Detail: err.Message}
}

// stageResponse streams a bounded response body into the spool.
func (t *Transport) stageResponse(ctx context.Context, resp *http.Response, contentType string, request *Request) (*Artifact, *Error) {
	name := "response"
	if request != nil {
		name = request.Op + "-response"
	}
	artifact, err := t.Spool.Put(ctx, Upload{
		Filename:      name,
		ContentType:   contentType,
		MaximumLength: t.MaxResponseBytes,
		Body:          io.LimitReader(resp.Body, t.MaxResponseBytes+1),
	})
	if err != nil {
		return nil, SpoolError(err)
	}
	return artifact, nil
}

// collect reads a bounded JSON/text body into memory.
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

// binaryContentType accepts audio/* (or octet-stream) for speech bodies.
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

// interrupted reports why this side ended a media exchange: the caller's
// cancellation, or an expired deadline including a transport timeout.
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
