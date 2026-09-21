package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// FailureClass mirrors the gateway attempt classification for media calls.
type FailureClass string

const (
	ClassConnect        FailureClass = "connect"
	ClassTimeout        FailureClass = "timeout"
	ClassRateLimit      FailureClass = "rate_limit"
	ClassUpstreamServer FailureClass = "upstream_server"
	ClassUpstreamClient FailureClass = "upstream_client"
	ClassCredential     FailureClass = "credential"
	ClassProtocol       FailureClass = "protocol"
	ClassCancelled      FailureClass = "cancelled"
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
	Config connectors.Config
	Model  string // upstream model identifier
	Secret []byte
}

// Transport executes media calls against provider endpoints.
type Transport struct {
	Client *http.Client
	Auth   *connectors.Auth
	Egress *egress.Policy
	Spool  *Spool
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
	if err != nil {
		return nil, &Failure{Class: ClassProtocol, Detail: "media endpoint could not be built"}
	}

	var body io.Reader = http.NoBody
	contentType := ""
	var pipe *io.PipeReader
	var multipartDone chan error
	switch {
	case call.JSON != nil:
		body = bytes.NewReader(call.JSON)
		contentType = "application/json"
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

	// dispatched records whether request bytes provably left this gateway.
	var dispatched atomic.Bool
	trace := &httptrace.ClientTrace{
		WroteHeaders: func() { dispatched.Store(true) },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				dispatched.Store(true)
			}
		},
		GotFirstResponseByte: func() { dispatched.Store(true) },
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), call.Method, endpoint, body)
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
	req.Header.Set("User-Agent", "olp-go/gateway")
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
			return nil, &Failure{Class: classifyTransport(ctx, err), Detail: "media request interrupted"}
		}
		return nil, &Failure{Class: ClassCredential, Detail: "provider credential could not be applied"}
	}

	started := t.now()
	resp, err := t.Client.Do(req)
	if err != nil {
		if pipe != nil {
			pipe.Close()
			<-multipartDone
		}
		class := classifyTransport(ctx, err)
		sent := dispatched.Load()
		return nil, &Failure{Class: class, Dispatched: sent,
			Ambiguous: call.Ambiguous && sent, Detail: "upstream transport failed"}
	}
	firstByte := t.now().Sub(started)
	if resp.StatusCode != http.StatusOK {
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
			Upstream:   openai.ParseErrorBody(raw),
		}
		if failure.Upstream != nil {
			failure.Upstream.Message = redactCredentials(failure.Upstream.Message, credentialValues)
		}
		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			failure.Class = ClassCredential
		case resp.StatusCode == http.StatusTooManyRequests:
			failure.Class = ClassRateLimit
			failure.RetryAfter = retryAfterHeader(resp.Header.Get("Retry-After"), t.now())
		case resp.StatusCode >= 500:
			failure.Class = ClassUpstreamServer
			failure.Ambiguous = call.Ambiguous
		default:
			failure.Class = ClassUpstreamClient
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

	result, failure := t.decode(ctx, resp, call, request, firstByte)
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

func (t *Transport) decode(ctx context.Context, resp *http.Response, call *UpstreamCall, request *Request, firstByte time.Duration) (*Result, *Failure) {
	result := &Result{Kind: call.Kind, Status: resp.StatusCode, FirstByte: firstByte}
	switch call.Kind {
	case ResponseSSE:
		if !requireContentType(resp, "text/event-stream") {
			return nil, &Failure{Class: ClassProtocol,
				Detail: "streamed media response is not text/event-stream"}
		}
		result.Body = resp.Body
		return result, nil
	case ResponseBinary:
		contentType := binaryContentType(resp, "audio/")
		artifact, failure := t.stageResponse(ctx, resp, contentType, request)
		if failure != nil {
			return nil, stageFailure(failure)
		}
		result.Artifact = artifact
		result.ContentType = artifact.ContentType
		return result, nil
	case ResponseVideoContent:
		base := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
		if !strings.HasPrefix(base, "video/") && !strings.HasPrefix(base, "image/") {
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
		if TranscriptionFormatIsText(format) {
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
		decoded, mErr := DecodeTranscriptionJSON(body)
		if mErr != nil {
			return nil, &Failure{Class: ClassProtocol, Detail: mErr.Message}
		}
		result.Transcription = decoded
		return result, nil
	case ResponseImages:
		if !requireContentType(resp, "application/json") {
			return nil, &Failure{Class: ClassProtocol, Detail: "image response is not JSON"}
		}
		body, failure := t.collect(resp)
		if failure != nil {
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
		if call.Native != "" {
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
			return nil, &Failure{Class: ClassProtocol, Detail: mErr.Message}
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

// classifyTransport maps a transport error and context state to a class.
func classifyTransport(ctx context.Context, err error) FailureClass {
	if errors.Is(err, context.Canceled) {
		return ClassCancelled
	}
	if ctx.Err() == context.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded) {
		return ClassTimeout
	}
	var nerr interface{ Timeout() bool }
	if errors.As(err, &nerr) && nerr.Timeout() {
		return ClassTimeout
	}
	return ClassConnect
}

func retryAfterHeader(header string, now time.Time) time.Duration {
	if header == "" {
		return 0
	}
	if seconds, err := strconv.ParseFloat(strings.TrimSpace(header), 64); err == nil && seconds > 0 {
		return time.Duration(seconds * float64(time.Second))
	}
	if at, err := http.ParseTime(header); err == nil {
		if d := at.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// redactCredentials removes known credential material from an upstream error
// message before it reaches a client or log.
func redactCredentials(message string, sensitive []string) string {
	for _, value := range sensitive {
		if value == "" {
			continue
		}
		message = strings.ReplaceAll(message, value, "[redacted]")
	}
	return message
}
