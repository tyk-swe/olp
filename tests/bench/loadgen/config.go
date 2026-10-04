//go:build bench

// Package loadgen is an open-loop, constant-arrival-rate load generator for
// LLM gateways. It can drive any base URL, so one generator measures OLP,
// LiteLLM and the mock upstream directly, and the difference between runs is
// the gateway's overhead.
//
// Open loop means the arrival schedule never waits for the system under test.
// Request k is due at start + k/rate, and its latency is measured from that
// scheduled time rather than from when it was actually sent: if the gateway
// stalls, the requests that should have arrived meanwhile record the wait
// they would have suffered, so a stall shows in the percentiles instead of
// being hidden by a generator that politely stops sending (coordinated
// omission). The sends themselves are bounded: at most MaxInFlight requests
// run at once and at most MaxBacklog more wait their turn; beyond that a
// request is dropped and counted, never silently delayed, and a run that
// dropped or sent late is reported invalid.
//
// Latencies are recorded in HDR histograms. Time to first token is the
// arrival of the first server-sent data frame, also measured from the
// scheduled time. A warmup period runs at the same rate. Its requests are left
// out of the latency, time-to-first-token and count statistics, but the
// throughput counts the requests that finish during the measured period,
// whichever period sent them.
package loadgen

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Dialect is the wire format of the endpoint under test.
type Dialect string

const (
	// OpenAI is Chat Completions, POST {url}/v1/chat/completions.
	OpenAI Dialect = "openai"
	// Anthropic is Messages, POST {url}/v1/messages.
	Anthropic Dialect = "anthropic"
	// Gemini is generateContent or streamGenerateContent, POST
	// {url}/v1beta/models/{model}:{operation}.
	Gemini Dialect = "gemini"
)

// Defaults for the fields a caller may leave zero.
const (
	DefaultMaxTokens   = 16
	DefaultMaxInFlight = 4096
	DefaultTimeout     = 30 * time.Second
	DefaultLateAfter   = 5 * time.Millisecond
	// DefaultSlowReadRcvBuf is the receive buffer of a slow reader's
	// connections: small, so unread data stays in the server's hands.
	DefaultSlowReadRcvBuf = 4096
)

// SlowRead makes streaming responses be read slowly. A fast kernel buffer
// would absorb a short stream whole and the server would never see the
// reader's pace, so a slow reader also shrinks its connections' receive
// buffer.
type SlowRead struct {
	// BytesPerSecond is the pace of each stream; 0 reads at full speed.
	BytesPerSecond int
	// RcvBuf is the connections' SO_RCVBUF in bytes; 0 means
	// DefaultSlowReadRcvBuf.
	RcvBuf int
}

// Config is one run.
type Config struct {
	// URL is the base URL. The dialect's path is appended to any path it has.
	URL string
	// Path replaces the dialect's request path (the model replaces {model}),
	// for gateways that serve it elsewhere.
	Path    string
	Dialect Dialect
	// APIKey is sent in the dialect's own header: a bearer token for OpenAI,
	// x-api-key for Anthropic, x-goog-api-key for Gemini.
	APIKey string
	Model  string
	// Headers are added to every request, such as the mock's x-mock-* controls.
	Headers map[string]string

	// Rate is the target arrival rate in requests per second.
	Rate float64
	// Duration is the measured period; Warmup precedes it at the same rate.
	Duration, Warmup time.Duration

	// StreamShare is the fraction of requests that stream, spread evenly.
	StreamShare float64
	// PromptTokens lists the prompt sizes, in tokens, used in equal shares in
	// rotation; 0 is a short fixed prompt. Empty means a short prompt.
	PromptTokens []int
	// MaxTokens is the completion limit each request asks for.
	MaxTokens int

	// MaxInFlight bounds concurrent requests. MaxBacklog bounds how many
	// more may wait for a slot: 0 means as many as MaxInFlight and a negative
	// value means none, so a request that finds no slot is dropped.
	MaxInFlight, MaxBacklog int
	// Timeout bounds each request from its send. Drain is how long to wait
	// for requests still running when the schedule ends, defaulting to
	// Timeout plus a second so that a request sent last times out on its own
	// rather than being canceled at the same instant; those still running
	// after it are canceled and counted.
	Timeout, Drain time.Duration
	// LateAfter is how long after its scheduled time a send counts as late.
	LateAfter time.Duration

	SlowRead SlowRead
	// H2C speaks HTTP/2 without TLS, with prior knowledge.
	H2C bool
	// Client replaces the generated one, which tests use to run in process.
	Client *http.Client
}

// normalize applies defaults and checks the configuration.
func (c Config) normalize() (Config, error) {
	u, err := url.Parse(c.URL)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		return c, fmt.Errorf("url %q is not an http or https base URL", c.URL)
	case u.RawQuery != "" || u.Fragment != "":
		return c, errors.New("url must not carry a query or fragment")
	}
	switch c.Dialect {
	case OpenAI, Anthropic, Gemini:
	case "":
		c.Dialect = OpenAI
	default:
		return c, fmt.Errorf("unknown dialect %q: want openai, anthropic or gemini", c.Dialect)
	}
	if c.Model == "" {
		return c, errors.New("model is required")
	}
	if !(c.Rate > 0) || c.Rate > 1e6 {
		return c, fmt.Errorf("rate %v must be between 0 and 1,000,000 requests per second", c.Rate)
	}
	if c.Duration <= 0 || c.Warmup < 0 {
		return c, errors.New("duration must be positive and warmup not negative")
	}
	if c.measuredRequests() < 1 {
		return c, fmt.Errorf("rate %v for %v schedules no request", c.Rate, c.Duration)
	}
	if c.StreamShare < 0 || c.StreamShare > 1 {
		return c, fmt.Errorf("stream share %v is outside 0 to 1", c.StreamShare)
	}
	for _, n := range c.PromptTokens {
		if n < 0 || n > 10_000_000 {
			return c, fmt.Errorf("prompt size %d is outside 0 to 10,000,000 tokens", n)
		}
	}
	if len(c.PromptTokens) == 0 {
		c.PromptTokens = []int{0}
	}
	if c.MaxTokens == 0 {
		c.MaxTokens = DefaultMaxTokens
	}
	if c.MaxTokens < 1 {
		return c, errors.New("max tokens must be positive")
	}
	if c.MaxInFlight == 0 {
		c.MaxInFlight = DefaultMaxInFlight
	}
	if c.MaxInFlight < 1 {
		return c, errors.New("max in flight must be positive")
	}
	switch {
	case c.MaxBacklog == 0:
		c.MaxBacklog = c.MaxInFlight
	case c.MaxBacklog < 0:
		c.MaxBacklog = 0
	}
	if c.Timeout == 0 {
		c.Timeout = DefaultTimeout
	}
	if c.Drain == 0 {
		c.Drain = c.Timeout + time.Second
	}
	if c.Timeout < 0 || c.Drain < 0 {
		return c, errors.New("timeout and drain must not be negative")
	}
	if c.LateAfter == 0 {
		c.LateAfter = DefaultLateAfter
	}
	if c.SlowRead.BytesPerSecond < 0 || c.SlowRead.RcvBuf < 0 {
		return c, errors.New("slow read settings must not be negative")
	}
	if c.SlowRead.BytesPerSecond > 0 && c.SlowRead.RcvBuf == 0 {
		c.SlowRead.RcvBuf = DefaultSlowReadRcvBuf
	}
	for name := range c.Headers {
		if name == "" || strings.ContainsAny(name, " :\r\n") {
			return c, fmt.Errorf("header name %q is invalid", name)
		}
	}
	return c, nil
}

// warmupRequests and measuredRequests are how many requests the schedule
// holds in each period.
func (c Config) warmupRequests() int64   { return int64(c.Rate*c.Warmup.Seconds() + 0.5) }
func (c Config) measuredRequests() int64 { return int64(c.Rate*c.Duration.Seconds() + 0.5) }

// offset is when request k is due, relative to the start. It is computed from
// k rather than accumulated, so rounding never drifts.
func (c Config) offset(k int64) time.Duration {
	return time.Duration(float64(k)*1e9/c.Rate + 0.5)
}
