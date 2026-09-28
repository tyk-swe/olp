package plugins

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// Bounds on what one plugin call may log.
const (
	maxLogText  = 2 << 10  // bytes of one message or attribute value
	maxLogAttrs = 16       // attributes of one record
	maxCallLog  = 16 << 10 // bytes of records per call
	// recordCost is what a record costs the call's budget beyond its text:
	// the time, level and attribution OLP's log adds to every record.
	recordCost = 64
)

type outputKey struct{}

// callOutput returns the output of the call ctx belongs to.
func callOutput(ctx context.Context) *output { return ctx.Value(outputKey{}).(*output) }

// output carries one call's plugin logging into OLP's log: redacted, with long
// text cut, and silenced once the call has logged its budget. It is safe for
// concurrent use: an unconfined plugin's capability requests for one call are
// served concurrently.
type output struct {
	log      *slog.Logger
	replacer *strings.Replacer
	mu       sync.Mutex
	budget   int
	streams  []*stream
}

func newOutput(log *slog.Logger, secrets []string) *output {
	o := &output{log: log, budget: maxCallLog}
	// Longer values first, so a secret containing another is redacted whole.
	// Match URL components from HTTP errors and JSON strings from nested
	// diagnostics, with either setting of the encoder's HTML escaping.
	var values []string
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		for _, value := range []string{secret, url.QueryEscape(secret), url.PathEscape(secret)} {
			quoted, _ := json.Marshal(value)
			var plain bytes.Buffer
			encoder := json.NewEncoder(&plain)
			encoder.SetEscapeHTML(false)
			_ = encoder.Encode(value)
			// Encoder adds a newline after the closing quote.
			values = append(values, value, string(quoted[1:len(quoted)-1]), plain.String()[1:plain.Len()-2])
		}
	}
	slices.SortFunc(values, func(a, b string) int {
		return cmp.Or(cmp.Compare(len(b), len(a)), strings.Compare(a, b))
	})
	values = slices.Compact(values)
	pairs := make([]string, 0, 2*len(values))
	for _, value := range values {
		pairs = append(pairs, value, "[REDACTED]")
	}
	if len(pairs) > 0 {
		o.replacer = strings.NewReplacer(pairs...)
	}
	return o
}

func (o *output) redact(text string) string {
	if o.replacer == nil {
		return text
	}
	return o.replacer.Replace(text)
}

// record logs one record the plugin wrote.
func (o *output) record(r abi.LogRecord) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.budget <= 0 {
		return
	}
	message := o.clip(r.Message)
	cost := recordCost + len(message)
	keys := slices.Sorted(maps.Keys(r.Attrs))
	attrs := make([]any, 0, min(len(keys), maxLogAttrs))
	for _, key := range keys[:min(len(keys), maxLogAttrs)] {
		key, value := o.clip(key), o.clip(r.Attrs[key])
		cost += len(key) + len(value)
		attrs = append(attrs, slog.String(key, value))
	}
	o.budget -= cost
	if o.budget < 0 {
		o.log.Warn("plugin log output exceeded its budget; the rest of this call's output is dropped")
		return
	}
	o.log.Log(context.Background(), level(r.Level), message, slog.Group("plugin_attrs", attrs...))
}

// clip redacts text and cuts it to maxLogText bytes on a character boundary.
func (o *output) clip(text string) string {
	text = o.redact(text)
	if len(text) <= maxLogText {
		return text
	}
	cut := maxLogText
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
}

func level(name string) slog.Level {
	switch name {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// stream returns the writer of one of the module's output streams. Each line
// it receives becomes a record.
func (o *output) stream(name string) *stream {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, s := range o.streams {
		if s.name == name {
			return s
		}
	}
	s := &stream{output: o, name: name}
	o.streams = append(o.streams, s)
	return s
}

// close logs any unfinished line.
func (o *output) close() {
	o.mu.Lock()
	streams := o.streams
	o.mu.Unlock()
	for _, s := range streams {
		s.flush()
	}
}

type stream struct {
	output   *output
	name     string
	pending  []byte
	dropping bool
}

func (s *stream) Write(p []byte) (int, error) {
	n := len(p)
	for {
		line, rest, complete := bytes.Cut(p, []byte{'\n'})
		if !s.dropping {
			s.pending = append(s.pending, line...)
		}
		if !complete {
			// A line too long to redact whole is dropped entirely: the
			// redactor only knows complete credentials, and a fragment can
			// hold the part of one a split left inside it.
			if len(s.pending) > maxCallLog {
				s.pending = s.pending[:0]
				s.dropping = true
			}
			return n, nil
		}
		s.flush()
		s.dropping = false
		p = rest
	}
}

func (s *stream) flush() {
	if len(s.pending) > 0 {
		s.output.record(abi.LogRecord{Level: "info", Message: string(s.pending), Attrs: map[string]string{"stream": s.name}})
		s.pending = s.pending[:0]
	}
}
