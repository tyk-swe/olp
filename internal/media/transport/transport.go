package transport

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/staging"
	"github.com/tyk-swe/olp/internal/upstream"
)

const (
	errorBodyLimit    = 64 * 1024
	connectionTimeout = 5 * time.Minute
)

type Deps struct {
	Client           *http.Client
	Auth             *connectors.Auth
	Egress           *egress.Policy
	Spool            *staging.Spool
	MaxResponseBytes int64
	Log              *slog.Logger
}

type Transport struct {
	deps        Deps
	connections *egress.ConnectionClientCache
}

func New(deps Deps) *Transport {
	return &Transport{deps: deps, connections: egress.NewConnectionClientCache(128)}
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

func (t Target) WithCaller(secret []byte, sensitive *egress.Sensitive) Target {
	return Target{Sensitive: sensitive, CallerCredential: true, ConnectionScope: t.ConnectionScope,
		NetworkSecret: t.NetworkSecret, Config: t.Config, Model: t.Model, Secret: secret}
}

type Options struct {
	Strict  bool
	Headers http.Header
}

type Failure struct {
	Class      upstream.Class
	Status     int
	Dispatched bool
	Ambiguous  bool
	RetryAfter time.Duration
	Upstream   *wire.UpstreamError
	Detail     string
}

type Result struct {
	Status    int
	FirstByte time.Duration
	Source    oif.Document
	Payload   Payload
}

type Payload interface{ payload() }

type Images struct{ *media.ImageResult }

type Audio struct {
	Artifact         *staging.Artifact
	BilledCharacters *int64
	Tokens           *media.ImageUsage
}

type Transcript struct{ *media.TranscriptionResult }

type Text struct {
	Body  []byte
	Usage *media.TranscriptionResult
}

type Events struct{ Body io.ReadCloser }

type Video struct{ *media.VideoJobResult }

type Deleted struct{ *media.VideoDeleteResult }

type Content struct{ Artifact *staging.Artifact }

func (Images) payload()     {}
func (Audio) payload()      {}
func (Transcript) payload() {}
func (Text) payload()       {}
func (Events) payload()     {}
func (Video) payload()      {}
func (Deleted) payload()    {}
func (Content) payload()    {}

func (t *Transport) Do(ctx context.Context, target Target, call *media.UpstreamCall, options Options) (*Result, *Failure) {
	sensitive := sensitiveFor(target)
	result, failure := t.do(ctx, target, sensitive, call, options)
	if failure == nil || sensitive == nil {
		return result, failure
	}
	redacted := *failure
	redacted.Detail = sensitive.Redact(failure.Detail)
	if failure.Upstream != nil {
		redacted.Upstream = failure.Upstream.Redact(sensitive.Redact)
	}
	return nil, &redacted
}

func sensitiveFor(target Target) *egress.Sensitive {
	if target.Sensitive == nil && !target.CallerCredential {
		return nil
	}
	sensitive := cmp.Or(target.Sensitive, &egress.Sensitive{})
	sensitive.Add(string(target.Secret))
	return sensitive
}

func (t *Transport) do(ctx context.Context, target Target, sensitive *egress.Sensitive, call *media.UpstreamCall, options Options) (*Result, *Failure) {
	if target.Config.CredentialSource == "caller" && !target.CallerCredential {
		return nil, &Failure{Class: upstream.Credential, Detail: "A caller credential is required for this connection."}
	}
	if _, err := t.deps.Egress.ValidateEndpoint(target.Config.Endpoint); err != nil {
		return nil, &Failure{Class: upstream.Connect, Detail: "provider endpoint rejected by egress policy"}
	}
	endpoint, config, err := media.Resolve(call.Endpoint, target.Config, target.Model)
	if err != nil {
		return nil, &Failure{Class: upstream.Protocol, Detail: "media endpoint could not be built"}
	}
	if _, absolute := call.Endpoint.(media.Absolute); absolute {
		if _, err := t.deps.Egress.ValidateEndpoint(endpoint); err != nil {
			return nil, &Failure{Class: upstream.Protocol, Detail: "media endpoint could not be built"}
		}
	}
	signed := Target{Sensitive: sensitive, CallerCredential: target.CallerCredential, ConnectionScope: target.ConnectionScope,
		NetworkSecret: target.NetworkSecret, Config: config, Model: target.Model, Secret: target.Secret}
	client, err := t.client(signed)
	if err != nil {
		return nil, &Failure{Class: upstream.Credential, Detail: "provider network connection unavailable"}
	}
	outbound, failure := t.outbound(call.Body)
	if failure != nil {
		return nil, failure
	}
	resp, started, credentials, failure := t.send(ctx, client, signed, call, options, endpoint, outbound)
	if failure != nil {
		return nil, failure
	}
	if !accepted(call.Response, resp.StatusCode) {
		return nil, t.refusal(resp, signed, call, credentials, outbound)
	}
	if err := outbound.finish(nil); err != nil {
		resp.Body.Close()
		return nil, &Failure{Class: upstream.Connect, Dispatched: true, Ambiguous: call.Ambiguous, Detail: "multipart upload stream failed"}
	}
	firstByte := time.Since(started)
	reading := poller{transport: t, ctx: ctx, client: client, target: signed}
	payload, source, failure := t.decode(ctx, resp, call.Response, options.Strict, reading)
	if _, events := payload.(Events); !events {
		resp.Body.Close()
	}
	if failure != nil {
		dispatched := *failure
		dispatched.Dispatched = true
		dispatched.Ambiguous = call.Ambiguous && failure.Class != upstream.ClientError
		return nil, &dispatched
	}
	return &Result{Status: resp.StatusCode, FirstByte: firstByte, Source: source, Payload: payload}, nil
}

func (t *Transport) client(target Target) (*http.Client, error) {
	switch {
	case target.CallerCredential:
		return t.deps.Egress.EphemeralClient(target.Config.Network, target.NetworkSecret, connectionTimeout)
	case target.Config.Network != nil || target.Config.ProfileID != "":
		return t.connections.ClientScoped(target.ConnectionScope, *t.deps.Egress, target.Config.Network, target.NetworkSecret, connectionTimeout)
	}
	return t.deps.Client, nil
}

type outbound struct {
	reader      io.Reader
	contentType string
	length      int64
	signed      []byte
	finish      func(abort error) error
}

func settled(error) error { return nil }

func (t *Transport) outbound(body media.Body) (outbound, *Failure) {
	switch b := body.(type) {
	case nil:
		return outbound{reader: http.NoBody, finish: settled}, nil
	case media.JSONBody:
		return jsonOutbound(b), nil
	case media.NativeJSON:
		return jsonOutbound(b), nil
	case media.InlineBody:
		inlined, failure := b(t.readPart)
		if failure != nil {
			return outbound{}, &Failure{Class: upstream.Protocol, Detail: failure.Message}
		}
		return jsonOutbound(inlined), nil
	case media.UploadBody:
		opened, err := t.deps.Spool.Open(b.Handle)
		if err != nil {
			return outbound{}, &Failure{Class: upstream.Connect, Detail: "staged media upload is unavailable"}
		}
		return outbound{reader: opened.File, contentType: cmp.Or(b.ContentType, "application/octet-stream"),
			length: opened.Artifact.ContentLength, finish: func(error) error {
				_ = opened.File.Close()
				return nil
			}}, nil
	case media.FormBody:
		return t.formOutbound(b), nil
	}
	return outbound{}, &Failure{Class: upstream.Protocol, Detail: "media request body is not supported"}
}

func jsonOutbound(body []byte) outbound {
	return outbound{reader: bytes.NewReader(body), contentType: "application/json", signed: body, finish: settled}
}

func (t *Transport) formOutbound(fields []staging.Field) outbound {
	reader, writer := io.Pipe()
	form := multipart.NewWriter(writer)
	done := make(chan error, 1)
	go func() {
		err := media.WriteForm(form, fields, t.openPart)
		writer.CloseWithError(err)
		done <- err
	}()
	return outbound{reader: reader, contentType: form.FormDataContentType(), finish: func(abort error) error {
		if abort != nil {
			reader.CloseWithError(abort)
		}
		return <-done
	}}
}

var errAbandoned = errors.New("media request abandoned")

func (t *Transport) send(ctx context.Context, client *http.Client, target Target, call *media.UpstreamCall, options Options, endpoint string, body outbound) (*http.Response, time.Time, egress.Sensitive, *Failure) {
	abandon := func(failure *Failure) (*http.Response, time.Time, egress.Sensitive, *Failure) {
		_ = body.finish(errAbandoned)
		return nil, time.Time{}, egress.Sensitive{}, failure
	}
	var dispatched atomic.Bool
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, upstream.Trace(&dispatched)), call.Method, endpoint, body.reader)
	if err != nil {
		return abandon(&Failure{Class: upstream.Connect, Detail: "media request could not be built"})
	}
	if body.contentType != "" {
		req.Header.Set("Content-Type", body.contentType)
	}
	if body.length > 0 {
		req.ContentLength = body.length
	}
	req.Header.Set("User-Agent", "olp/gateway")
	if call.Accept != "" {
		req.Header.Set("Accept", call.Accept)
	}
	if call.Streaming() {
		req.Header.Set("Accept", "text/event-stream")
	}
	maps.Copy(req.Header, options.Headers)
	credentials, err := t.deps.Auth.Apply(ctx, req, target.Config, target.Secret, body.signed)
	if err != nil {
		if ctx.Err() != nil {
			class := upstream.Classifier{}.Classify(upstream.Evidence{Interrupted: interrupted(ctx, err), Err: err}).Class
			return abandon(&Failure{Class: class, Detail: "media request interrupted"})
		}
		return abandon(&Failure{Class: upstream.Credential, Detail: "provider credential could not be applied"})
	}
	if target.Sensitive != nil {
		target.Sensitive.Include(credentials)
	}
	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		sent := dispatched.Load()
		outcome := upstream.Classifier{}.Classify(upstream.Evidence{Reached: sent, Interrupted: interrupted(ctx, err), Err: err})
		return abandon(&Failure{Class: outcome.Class, Dispatched: sent,
			Ambiguous: call.Ambiguous && outcome.Acceptance.Unresolved(), Detail: "upstream transport failed"})
	}
	return resp, started, credentials, nil
}

func accepted(response media.Response, status int) bool {
	switch reply := response.(type) {
	case media.VideoJobReply:
		return status == http.StatusOK || status == http.StatusCreated
	case media.VideoDeletedReply:
		return status == http.StatusOK || reply.NoContent && status == http.StatusNoContent
	}
	return status == http.StatusOK
}

func (t *Transport) refusal(resp *http.Response, target Target, call *media.UpstreamCall, credentials egress.Sensitive, body outbound) *Failure {
	_ = body.finish(errAbandoned)
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
	resp.Body.Close()
	declared := openai.ParseErrorBody(raw).Redact(credentials.Redact)
	outcome := upstream.Classifier{Declared: target.Config.Classification()}.Classify(upstream.Evidence{Reached: true, Status: resp.StatusCode, Error: declared})
	return &Failure{
		Class:      outcome.Class,
		Status:     resp.StatusCode,
		Dispatched: true,
		Ambiguous:  call.Ambiguous && outcome.Acceptance.Unresolved(),
		Upstream:   declared,
		RetryAfter: retryAfter(outcome.Class, resp),
	}
}

func retryAfter(class upstream.Class, resp *http.Response) time.Duration {
	if class != upstream.RateLimit {
		return 0
	}
	return upstream.RetryAfter(resp.Header.Get("Retry-After"), time.Now())
}

func (t *Transport) openPart(part staging.Part) (io.ReadCloser, staging.Part, error) {
	opened, err := t.deps.Spool.Open(part.Handle)
	if err != nil {
		return nil, staging.Part{}, err
	}
	named := part
	named.Filename = cmp.Or(opened.Filename, "media")
	named.ContentType = opened.Artifact.ContentType
	return opened.File, named, nil
}

func (t *Transport) readPart(part staging.Part) ([]byte, *media.Error) {
	opened, err := t.deps.Spool.Open(part.Handle)
	if err != nil {
		return nil, media.Fail(http.StatusInternalServerError, "media_unavailable", "The uploaded file is unavailable.")
	}
	defer opened.File.Close()
	data, err := io.ReadAll(io.LimitReader(opened.File, media.DefaultAudioUploadLimit+1))
	if err != nil || int64(len(data)) > media.DefaultAudioUploadLimit {
		return nil, media.Fail(http.StatusRequestEntityTooLarge, "file_too_large", "The uploaded file exceeds the inline upload bound.")
	}
	return data, nil
}

func interrupted(ctx context.Context, err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if ctx.Err() == context.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if timeout, ok := errors.AsType[interface {
		error
		Timeout() bool
	}](err); ok && timeout.Timeout() {
		return context.DeadlineExceeded
	}
	return nil
}
