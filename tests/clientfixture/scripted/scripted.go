// Package scripted serves deterministic upstream vendors for client
// qualification: OpenAI Chat Completions, Responses and embeddings, Anthropic
// Messages, and Gemini generation, counting and embeddings. Every reply is a
// pure function of the request, so a client under test and the gateway in front
// of it can be held to exactly what the upstream received. The only state is
// the stored Responses, the Anthropic prompt cache and the request recording.
//
// Scripts are directives a test places in the user prompt, for example
// `[[olp:tool get_weather {"city":"Paris"}]]`; see script.go.
package scripted

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"unicode/utf8"
)

// Upstream identities. The gateway rewrites a route slug to these names, so an
// upstream that receives anything else proves the rewrite did not happen.
const (
	OpenAIModel    = "fixture-openai"
	AnthropicModel = "fixture-anthropic"
	GeminiModel    = "fixture-gemini"

	// Each vendor is served under its own prefix, which is also the endpoint a
	// provider is configured with.
	OpenAIPrefix    = "/openai/v1"
	AnthropicPrefix = "/anthropic/v1"
	GeminiPrefix    = "/gemini/v1beta"

	// RecordedPath lists the recorded upstream requests; DELETE resets the
	// fixture. The path is outside every vendor prefix.
	RecordedPath = "/__recorded"

	// ThinkingSignature is the only signature the Anthropic vendor accepts
	// back on a thinking block, as the real API rejects an altered one.
	ThinkingSignature = "fixture-signature"
	// GeminiThoughtSignature signs the first function call of a Gemini step
	// that reasoned.
	GeminiThoughtSignature = "fixture-thought-signature"

	// RetryAfterSeconds is the Retry-After of every upstream 429. The gateway
	// parks the credential slot for it, one second rather than the ten it uses
	// when an upstream names none, which keeps a suite that scripts a rate
	// limit from starving the routes of the suites after it.
	RetryAfterSeconds = "1"

	// The limit exceeds the gateway's, so an oversized request is the
	// gateway's to refuse.
	maxBodyBytes = 32 << 20
)

// Options configures a Fixture.
type Options struct {
	// Credential is the only upstream credential the vendors accept.
	Credential string
	// ClientSecrets are caller credentials that must never reach an upstream;
	// a request carrying one is recorded with LeakedClientCredential.
	ClientSecrets []string
}

// Fixture is the scripted upstream. Its zero value is not usable.
type Fixture struct {
	opts Options
	mux  *http.ServeMux

	mu        sync.Mutex
	seq       int
	records   []*Record
	ids       map[string]int
	responses map[string]*storedResponse
	cache     map[string]bool
}

// New returns a Fixture serving every vendor and the recording endpoint.
func New(opts Options) *Fixture {
	f := &Fixture{opts: opts, mux: http.NewServeMux()}
	f.reset()
	f.registerOpenAI()
	f.registerAnthropic()
	f.registerGemini()
	f.mux.HandleFunc("GET "+RecordedPath, f.listRecorded)
	f.mux.HandleFunc("DELETE "+RecordedPath, f.resetRecorded)
	return f
}

func (f *Fixture) ServeHTTP(w http.ResponseWriter, r *http.Request) { f.mux.ServeHTTP(w, r) }

// reset drops every recording, stored response and cache entry, and restarts
// the identifier counters so a script replays identically.
func (f *Fixture) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq, f.records = 0, nil
	f.ids = map[string]int{}
	f.responses = map[string]*storedResponse{}
	f.cache = map[string]bool{}
}

// nextID returns a deterministic identifier such as msg_fx_3.
func (f *Fixture) nextID(prefix string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids[prefix]++
	return fmt.Sprintf("%s_fx_%d", prefix, f.ids[prefix])
}

// vendor describes how one upstream authenticates and reports errors.
type vendor struct {
	name       string
	authorized func(r *http.Request, credential string) bool
	fail       func(w http.ResponseWriter, status int, kind, message string)
}

// request is one inbound upstream call: the body is read once so the handler
// and the recording see the same bytes.
type request struct {
	f      *Fixture
	v      vendor
	w      http.ResponseWriter
	r      *http.Request
	body   []byte
	record *Record
}

// handle wraps an endpoint: it reads and records the request, authenticates it
// and answers failures in the vendor's own error envelope.
func (f *Fixture) handle(v vendor, dialect string, h func(*request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
		x := &request{f: f, v: v, r: r, body: body}
		x.record = f.begin(dialect, r, body)
		rw := &statusWriter{ResponseWriter: w}
		x.w = rw
		defer func() {
			// A handler that panics is a fixture bug; the recording says so
			// instead of reporting the success net/http would imply.
			if p := recover(); p != nil {
				rw.status = http.StatusInternalServerError
				f.finish(x.record, rw.status)
				panic(p)
			}
			f.finish(x.record, rw.status)
		}()
		switch {
		case err != nil || len(body) > maxBodyBytes:
			x.fail(http.StatusRequestEntityTooLarge, "request_too_large", "The fixture request body is too large.")
		case !v.authorized(r, f.opts.Credential):
			x.update(func(rec *Record) { rec.Authorized = false })
			x.fail(http.StatusUnauthorized, "authentication_error", "The fixture upstream credential is missing or invalid.")
		default:
			x.update(func(rec *Record) { rec.Authorized = true })
			h(x)
		}
	}
}

// update changes the recording under the lock a concurrent listing holds.
func (x *request) update(change func(*Record)) {
	x.f.mu.Lock()
	defer x.f.mu.Unlock()
	change(x.record)
}

func (x *request) fail(status int, kind, message string) {
	x.update(func(rec *Record) {
		if rec.Script == "" {
			rec.Script = "error:" + kind
		}
	})
	if status == http.StatusTooManyRequests {
		// A rate-limited upstream says when to come back, and the gateway
		// parks the credential slot for exactly that long.
		x.w.Header().Set("Retry-After", RetryAfterSeconds)
	}
	x.v.fail(x.w, status, kind, message)
}

// decode parses the JSON request body, answering 400 itself on failure.
func (x *request) decode(into any) bool {
	if err := json.Unmarshal(x.body, into); err != nil {
		x.fail(http.StatusBadRequest, "invalid_request_error", "The request body is not valid JSON: "+err.Error())
		return false
	}
	return true
}

func (x *request) note(script string, stream bool) {
	x.update(func(rec *Record) { rec.Script, rec.Stream = script, stream })
}

// statusWriter captures the status the handler wrote. Unwrap keeps
// http.ResponseController flushing the underlying connection.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func writeJSON(w http.ResponseWriter, status int, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(append(encoded, '\n'))
}

// sse writes server-sent events and flushes after each, so a client observes
// the stream frame by frame.
type sse struct {
	w   http.ResponseWriter
	rc  *http.ResponseController
	end string
}

func newSSE(w http.ResponseWriter, end string) *sse {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	return &sse{w: w, rc: http.NewResponseController(w), end: end}
}

// event writes one frame; an empty name omits the event line.
func (s *sse) event(name string, data any) {
	var frame bytes.Buffer
	if name != "" {
		fmt.Fprintf(&frame, "event: %s\n", name)
	}
	switch v := data.(type) {
	case string:
		fmt.Fprintf(&frame, "data: %s", v)
	default:
		encoded, _ := json.Marshal(v)
		fmt.Fprintf(&frame, "data: %s", encoded)
	}
	frame.WriteString(s.end)
	s.w.Write(frame.Bytes())
	s.rc.Flush()
}

// tokens is the fixture's deterministic token estimate.
func tokens(text string) int { return max(1, (len(text)+3)/4) }

// clip shortens text to at most n bytes without splitting a character.
func clip(text string, n int) string {
	if len(text) <= n {
		return text
	}
	for n > 0 && !utf8.RuneStart(text[n]) {
		n--
	}
	return text[:n]
}

func compact(raw json.RawMessage) string {
	var out bytes.Buffer
	if json.Compact(&out, raw) != nil {
		return strings.TrimSpace(string(raw))
	}
	return out.String()
}
