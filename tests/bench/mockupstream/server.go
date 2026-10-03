//go:build bench

// Package mockupstream is a deterministic LLM upstream for the gateway
// benchmark. It speaks four dialects, chosen by path suffix: OpenAI Chat
// Completions (.../chat/completions), OpenAI Responses (.../responses),
// Anthropic Messages (.../messages) and Gemini (.../models/{model}:generateContent
// and :streamGenerateContent), each as a unary response or a server-sent event
// stream, over HTTP/1.1 and cleartext HTTP/2.
//
// It is built never to be the bottleneck: request bodies are drained through a
// pooled buffer, never parsed or retained, and responses are rendered by
// appending to pooled buffers, so a 100K-token prompt at 3,000 requests per
// second costs it little more than the read. The prompt token count it reports
// is derived from the body size (one token per four bytes), so accounting is
// exercised end to end.
//
// What a request does is resolved in three layers: the configured default, the
// rule for the model the request names, and its x-mock-* headers. Headers
// only reach the mock directly; OLP does not forward client headers upstream,
// so a gateway run is shaped by per-model rules, which a test can change at
// runtime with PUT /_mock/config or Server.SetConfig. Control endpoints:
//
//	GET  /_mock/stats    request, stream, failure and in-flight counters
//	POST /_mock/reset    zero the counters and the fail-first-n counters
//	GET  /_mock/config   the effective configuration
//	PUT  /_mock/config   replace the configuration (a Spec)
//
// Model listings (GET .../models) are served in each dialect's shape so a
// provider can be probed and discovered against the mock.
package mockupstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// defaultModel is echoed when a request does not name a model.
const defaultModel = "mock-model"

// maxTrackedModels bounds the per-model counters; further names share one.
const maxTrackedModels = 256

// flushSize is how much of a zero-interval stream is written at once.
const flushSize = 32 << 10

var (
	headerJSON        = []string{"application/json"}
	headerEventStream = []string{"text/event-stream"}
	headerNoCache     = []string{"no-cache"}
)

// work is the per-request state, pooled so a request allocates nothing of its
// own: the body scan, the reply being rendered, the pacer and its timer, and
// the buffer responses are rendered into.
type work struct {
	scan bodyScan
	rep  reply
	p    pacer
	out  []byte
}

var workPool = sync.Pool{New: func() any { return &work{out: make([]byte, 0, 4<<10)} }}

func getWork(ctx context.Context) *work {
	wk := workPool.Get().(*work)
	wk.scan = bodyScan{}
	wk.p.ctx = ctx
	return wk
}

func (wk *work) release() {
	wk.p.stop()
	// A timer is not carried to the next request: it would outlive the
	// request that created it, and a test's fake clock cannot share one.
	wk.p = pacer{}
	wk.rep = reply{}
	if cap(wk.out) > 1<<20 {
		wk.out = nil
	}
	workPool.Put(wk)
}

// modelState counts what happened to one model. failSeen backs FailFirstN.
type modelState struct {
	requests, errors, failSeen atomic.Int64
}

// Server is the mock upstream. Use New.
type Server struct {
	cfg atomic.Pointer[Config]

	mu     sync.RWMutex
	states map[string]*modelState
	other  modelState

	requests, unary, streams, injected atomic.Int64
	aborted, errorFrames, clientGone   atomic.Int64
	requestBytes                       atomic.Int64
	inFlight, maxInFlight              atomic.Int64
	dialects                           [4]atomic.Int64
}

var dialectIndex = map[*dialect]int{openAI: 0, anthropic: 1, gemini: 2, responses: 3}

// New returns a mock with cfg, which is validated.
func New(cfg Config) (*Server, error) {
	s := &Server{states: map[string]*modelState{}}
	if err := s.SetConfig(cfg); err != nil {
		return nil, err
	}
	return s, nil
}

// SetConfig replaces the configuration. It takes effect for requests that
// resolve their behavior afterwards and does not reset any counter.
func (s *Server) SetConfig(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	cfg.Models = maps.Clone(cfg.Models)
	s.cfg.Store(&cfg)
	return nil
}

// Config returns a copy of the current configuration.
func (s *Server) Config() Config {
	cfg := *s.cfg.Load()
	cfg.Models = maps.Clone(cfg.Models)
	return cfg
}

// ModelStats is the per-model part of Stats.
type ModelStats struct {
	// Requests counts inference requests that resolved to the model.
	Requests int64 `json:"requests"`
	// Errors counts the error responses the mock sent for them.
	Errors int64 `json:"errors"`
}

// Stats is a snapshot of the mock's counters.
type Stats struct {
	Requests int64 `json:"requests"`
	Unary    int64 `json:"unary"`
	Streams  int64 `json:"streams"`
	// InjectedErrors counts error responses sent for a status rule or
	// fail-first-n, not malformed requests.
	InjectedErrors int64 `json:"injected_errors"`
	// AbortedStreams and ErrorFrames count mid-stream failures by mode.
	AbortedStreams int64 `json:"aborted_streams"`
	ErrorFrames    int64 `json:"error_frames"`
	// ClientGone counts requests abandoned because the client went away.
	ClientGone   int64 `json:"client_gone"`
	RequestBytes int64 `json:"request_bytes"`
	InFlight     int64 `json:"in_flight"`
	MaxInFlight  int64 `json:"max_in_flight"`
	// Dialects counts inference requests by dialect.
	Dialects map[string]int64 `json:"dialects"`
	// Models counts them by the model named. Names past the tracking bound
	// share the entry "*".
	Models map[string]ModelStats `json:"models"`
}

// Stats snapshots the counters.
func (s *Server) Stats() Stats {
	st := Stats{
		Requests: s.requests.Load(), Unary: s.unary.Load(), Streams: s.streams.Load(), InjectedErrors: s.injected.Load(),
		AbortedStreams: s.aborted.Load(), ErrorFrames: s.errorFrames.Load(), ClientGone: s.clientGone.Load(),
		RequestBytes: s.requestBytes.Load(), InFlight: s.inFlight.Load(), MaxInFlight: s.maxInFlight.Load(),
		Dialects: map[string]int64{}, Models: map[string]ModelStats{},
	}
	for d, i := range dialectIndex {
		st.Dialects[d.name] = s.dialects[i].Load()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for name, m := range s.states {
		st.Models[name] = ModelStats{Requests: m.requests.Load(), Errors: m.errors.Load()}
	}
	if n := s.other.requests.Load(); n > 0 {
		st.Models["*"] = ModelStats{Requests: n, Errors: s.other.errors.Load()}
	}
	return st
}

// Reset zeroes the counters, including the fail-first-n counters, which makes
// the next requests count from the start again.
func (s *Server) Reset() {
	for _, c := range []*atomic.Int64{&s.requests, &s.unary, &s.streams, &s.injected, &s.aborted, &s.errorFrames, &s.clientGone, &s.requestBytes} {
		c.Store(0)
	}
	for i := range s.dialects {
		s.dialects[i].Store(0)
	}
	s.maxInFlight.Store(s.inFlight.Load())
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.states)
	s.other.requests.Store(0)
	s.other.errors.Store(0)
	s.other.failSeen.Store(0)
}

// state returns the counters for model, creating them on first sight.
func (s *Server) state(model []byte) *modelState {
	s.mu.RLock()
	m, ok := s.states[string(model)]
	s.mu.RUnlock()
	if ok {
		return m
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok = s.states[string(model)]; ok {
		return m
	}
	if len(s.states) >= maxTrackedModels {
		return &s.other
	}
	m = new(modelState)
	s.states[string(model)] = m
	return m
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, "/_mock/"):
		s.admin(w, r)
	case strings.HasSuffix(path, "/chat/completions"):
		s.inference(w, r, openAI)
	case strings.HasSuffix(path, "/messages"):
		s.inference(w, r, anthropic)
	case strings.HasSuffix(path, "/responses"):
		s.inference(w, r, responses)
	case strings.HasSuffix(path, ":generateContent"), strings.HasSuffix(path, ":streamGenerateContent"):
		s.inference(w, r, gemini)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/models"):
		s.listModels(w, r)
	default:
		writeError(w, openAI, nil, http.StatusNotFound, "no such mock endpoint: "+path)
	}
}

// writeError renders an error response in the dialect's shape, using buf as
// scratch space.
func writeError(w http.ResponseWriter, d *dialect, buf []byte, status int, message string) {
	out := d.errorBody(buf[:0], status, message)
	h := w.Header()
	h["Content-Type"] = headerJSON
	h["Content-Length"] = []string{strconv.Itoa(len(out))}
	w.WriteHeader(status)
	w.Write(out)
}

// inference serves one generation request in dialect d.
func (s *Server) inference(w http.ResponseWriter, r *http.Request, d *dialect) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, d, nil, http.StatusMethodNotAllowed, "inference endpoints take POST")
		return
	}
	if cur := s.inFlight.Add(1); cur > s.maxInFlight.Load() {
		for old := s.maxInFlight.Load(); cur > old && !s.maxInFlight.CompareAndSwap(old, cur); old = s.maxInFlight.Load() {
		}
	}
	defer s.inFlight.Add(-1)
	wk := getWork(r.Context())
	defer wk.release()

	// The body is read in full before anything is written: it is what the
	// token count comes from, an HTTP/1 server cannot read a request after it
	// has begun replying, and an upstream starts generating once it has the
	// whole prompt, which is when the configured delays start.
	var stream bool
	if !d.bodyModel {
		var ok bool
		if stream, ok = geminiTarget(r.URL.Path, &wk.scan); !ok {
			writeError(w, d, wk.out, http.StatusNotFound, "expected /models/{model}:generateContent or :streamGenerateContent")
			return
		}
	}
	n, err := wk.scan.drain(r.Body, d.bodyModel)
	if err != nil {
		s.clientGone.Add(1)
		return
	}
	received := time.Now()
	if d.bodyModel {
		stream = wk.scan.stream
	}
	model := wk.scan.Model()
	s.requests.Add(1)
	s.requestBytes.Add(n)
	s.dialects[dialectIndex[d]].Add(1)
	if stream {
		s.streams.Add(1)
	} else {
		s.unary.Add(1)
	}

	cfg := s.cfg.Load()
	b, ok := cfg.Models[string(model)]
	if !ok {
		b = cfg.Default
	}
	state := s.state(model)
	state.requests.Add(1)
	b, err = applyHeaders(r.Header, b)
	if err != nil {
		writeError(w, d, wk.out, http.StatusBadRequest, err.Error())
		return
	}

	failing := b.Status >= 400
	if b.FailFirstN > 0 && state.failSeen.Add(1) <= int64(b.FailFirstN) {
		failing = true
	}
	if failing {
		state.errors.Add(1)
		s.injected.Add(1)
		if !wk.p.until(received.Add(b.ErrorDelay)) {
			s.clientGone.Add(1)
			return
		}
		status := b.failStatus()
		writeError(w, d, wk.out, status, failureMessage(status))
		return
	}

	wk.rep = reply{model: model, prompt: promptTokens(n), output: b.OutputTokens}
	if len(model) == 0 {
		wk.rep.model = []byte(defaultModel)
	}
	if stream {
		s.stream(w, d, wk, &b, received)
		return
	}
	// A unary response is held for as long as the same generation would take
	// to stream.
	if !wk.p.until(received.Add(b.TTFT + time.Duration(b.OutputTokens-1)*b.Interval)) {
		s.clientGone.Add(1)
		return
	}
	out := d.unary(wk.out[:0], &wk.rep)
	wk.out = out
	h := w.Header()
	h["Content-Type"] = headerJSON
	h["Content-Length"] = []string{strconv.Itoa(len(out))}
	w.WriteHeader(http.StatusOK)
	if _, err = w.Write(out); err != nil {
		s.clientGone.Add(1)
	}
}

// stream writes the server-sent events. Frames are paced against absolute
// deadlines (the first at TTFT, token i at TTFT plus i intervals), so timer
// latency does not accumulate into the stream's length.
func (s *Server) stream(w http.ResponseWriter, d *dialect, wk *work, b *Behavior, received time.Time) {
	flusher, _ := w.(http.Flusher)
	flush := func(out []byte) bool {
		if _, err := w.Write(out); err != nil {
			s.clientGone.Add(1)
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	first := received.Add(b.TTFT)
	if !wk.p.until(first) {
		s.clientGone.Add(1)
		return
	}
	h := w.Header()
	h["Content-Type"] = headerEventStream
	h["Cache-Control"] = headerNoCache
	w.WriteHeader(http.StatusOK)

	rep := &wk.rep
	failAt := min(b.FailAfterTokens, rep.output)
	out := d.start(wk.out[:0], rep)
	for i := 0; i < rep.output; i++ {
		if i > 0 && b.Interval > 0 && !wk.p.until(first.Add(time.Duration(i)*b.Interval)) {
			s.clientGone.Add(1)
			return
		}
		out = d.token(out, rep, i)
		// Without an interval, frames after the first go out in batches: the
		// stream is as fast as the upstream can be, and a write per frame
		// would measure the mock.
		if i == 0 || b.Interval > 0 || len(out) >= flushSize || i+1 == failAt {
			if !flush(out) {
				return
			}
			wk.out = out[:0]
			out = wk.out
		}
		if i+1 == failAt {
			if b.FailMode == FailAbort {
				s.aborted.Add(1)
				// The server ends the response without its terminator, which
				// closes an HTTP/1 connection and resets an HTTP/2 stream.
				panic(http.ErrAbortHandler)
			}
			s.errorFrames.Add(1)
			flush(d.streamError(out))
			return
		}
	}
	out = d.end(out, rep)
	flush(out)
	wk.out = out[:0]
}

// pacer sleeps until deadlines, giving up when the client goes away.
type pacer struct {
	ctx   context.Context
	timer *time.Timer
}

// until waits for the deadline and reports whether the client is still there.
func (p *pacer) until(deadline time.Time) bool {
	wait := time.Until(deadline)
	if wait <= 0 {
		return p.ctx.Err() == nil
	}
	if p.timer == nil {
		p.timer = time.NewTimer(wait)
	} else {
		p.timer.Reset(wait)
	}
	select {
	case <-p.timer.C:
		return true
	case <-p.ctx.Done():
		return false
	}
}

func (p *pacer) stop() {
	if p.timer != nil {
		p.timer.Stop()
	}
}

// geminiTarget reads the model and mode from a Gemini path, recording the
// model in scan.
func geminiTarget(path string, scan *bodyScan) (stream, ok bool) {
	i := strings.LastIndex(path, "/models/")
	if i < 0 {
		return false, false
	}
	name, operation, found := strings.Cut(path[i+len("/models/"):], ":")
	if !found || (operation != "generateContent" && operation != "streamGenerateContent") {
		return false, false
	}
	if len(name) <= maxModelLen {
		if n := copy(scan.model[:], name); safeModel(scan.model[:n]) {
			scan.modelLen, scan.haveModel = n, true
		}
	}
	return operation == "streamGenerateContent", true
}

func (s *Server) admin(w http.ResponseWriter, r *http.Request) {
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	fail := func(status int, err error) { reply(status, map[string]string{"error": err.Error()}) }
	switch r.Method + " " + r.URL.Path {
	case "GET /_mock/stats":
		reply(http.StatusOK, s.Stats())
	case "POST /_mock/reset":
		s.Reset()
		reply(http.StatusOK, s.Stats())
	case "GET /_mock/config":
		reply(http.StatusOK, SpecOf(s.Config()))
	case "PUT /_mock/config":
		var spec Spec
		dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&spec); err != nil {
			fail(http.StatusBadRequest, err)
			return
		}
		cfg, err := spec.Config(Config{Default: DefaultBehavior()})
		if err == nil {
			err = s.SetConfig(cfg)
		}
		if err != nil {
			fail(http.StatusBadRequest, err)
			return
		}
		reply(http.StatusOK, SpecOf(s.Config()))
	default:
		fail(http.StatusNotFound, fmt.Errorf("no such control endpoint: %s %s", r.Method, r.URL.Path))
	}
}

// listModels serves the configured models in the shape of the caller's
// dialect, which it tells from the credential header the connector sent.
func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Load()
	names := slices.Sorted(maps.Keys(cfg.Models))
	if _, ok := cfg.Models[defaultModel]; !ok {
		names = append(names, defaultModel)
		slices.Sort(names)
	}
	type object map[string]any
	var body object
	items := make([]object, 0, len(names))
	switch {
	case len(r.Header["X-Goog-Api-Key"]) > 0 || strings.HasSuffix(r.URL.Path, "/v1beta/models"):
		for _, name := range names {
			items = append(items, object{"name": "models/" + name, "displayName": name, "supportedGenerationMethods": []string{"generateContent", "streamGenerateContent"}})
		}
		body = object{"models": items}
	case len(r.Header["X-Api-Key"]) > 0 || len(r.Header["Anthropic-Version"]) > 0:
		for _, name := range names {
			items = append(items, object{"type": "model", "id": name, "display_name": name, "created_at": "2024-01-01T00:00:00Z"})
		}
		body = object{"data": items, "has_more": false, "first_id": names[0], "last_id": names[len(names)-1]}
	default:
		for _, name := range names {
			items = append(items, object{"id": name, "object": "model", "created": 1700000000, "owned_by": "mock"})
		}
		body = object{"object": "list", "data": items}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// NewHTTPServer returns an http.Server for h that speaks HTTP/1.1 and
// cleartext HTTP/2 with prior knowledge on the same port. It sets no read or
// write timeout, since a stream may legitimately last minutes.
func NewHTTPServer(h http.Handler) *http.Server {
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	return &http.Server{
		Handler:           h,
		Protocols:         &protocols,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		HTTP2:             &http.HTTP2Config{MaxConcurrentStreams: 4096, MaxReadFrameSize: 1 << 20},
	}
}

// Serve serves h, normally a *Server, on ln until ctx ends, then closes every
// connection.
func Serve(ctx context.Context, ln net.Listener, h http.Handler) error {
	srv := NewHTTPServer(h)
	go func() { <-ctx.Done(); srv.Close() }()
	if err := srv.Serve(ln); err != http.ErrServerClosed {
		return err
	}
	return nil
}
