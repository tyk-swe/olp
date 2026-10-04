//go:build bench

package mockupstream

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Limits on what a header or a rule may ask for. They keep a typo from
// turning the mock into a memory or time sink.
const (
	maxDelay        = 10 * time.Minute
	maxOutputTokens = 100_000
)

// FailMode selects how a stream that fails after FailAfterTokens ends.
type FailMode uint8

const (
	// FailAbort closes the connection (or resets the HTTP/2 stream) without
	// finishing the response, as a crashed upstream does.
	FailAbort FailMode = iota
	// FailFrame sends the dialect's in-band error event and ends the stream
	// without its terminal event, as an overloaded upstream does.
	FailFrame
)

func (m FailMode) String() string {
	if m == FailFrame {
		return "error"
	}
	return "abort"
}

// Behavior is everything a request can ask of the mock. The zero value is a
// valid but useless behavior: use DefaultBehavior.
type Behavior struct {
	// TTFT is the delay between the request being fully received and the
	// first response frame. A unary response is held for as long as a stream
	// of the same shape takes to complete: TTFT + (OutputTokens-1)*Interval.
	TTFT time.Duration
	// Interval separates consecutive token frames. Zero sends every frame
	// back to back.
	Interval time.Duration
	// OutputTokens is the completion length, between 1 and 100,000.
	OutputTokens int
	// Status is the response status: 0 or 200 succeed, 400 to 599 return the
	// dialect's error body instead.
	Status int
	// ErrorDelay holds an error response, so a rule can simulate a hanging or
	// slowly failing upstream. Errors are immediate by default.
	ErrorDelay time.Duration
	// FailAfterTokens fails a stream after that many tokens (at most the
	// completion length; 0 disables). Unary responses ignore it.
	FailAfterTokens int
	FailMode        FailMode
	// FailFirstN makes the first N requests that resolve to the same model
	// fail with 503. The counter is per model, so a route's first target can
	// fail while its second succeeds; a model that must fail every time sets
	// Status instead, which then also picks the status.
	FailFirstN int
}

// DefaultBehavior answers immediately with sixteen tokens.
func DefaultBehavior() Behavior { return Behavior{OutputTokens: 16} }

// failStatus is the status an injected failure reports: Status when it is an
// error, which fails every request, else the 503 of fail-first-n.
func (b Behavior) failStatus() int {
	if b.Status >= 400 {
		return b.Status
	}
	return 503
}

// Validate reports the first field outside its documented range.
func (b Behavior) Validate() error {
	for _, d := range [...]struct {
		name  string
		value time.Duration
	}{{"ttft", b.TTFT}, {"interval", b.Interval}, {"error delay", b.ErrorDelay}} {
		if d.value < 0 || d.value > maxDelay {
			return fmt.Errorf("%s %v is outside 0 to %v", d.name, d.value, maxDelay)
		}
	}
	switch {
	case b.OutputTokens < 1 || b.OutputTokens > maxOutputTokens:
		return fmt.Errorf("output tokens %d is outside 1 to %d", b.OutputTokens, maxOutputTokens)
	case b.Status != 0 && b.Status != 200 && (b.Status < 400 || b.Status > 599):
		return fmt.Errorf("status %d must be 200 or between 400 and 599", b.Status)
	case b.FailAfterTokens < 0 || b.FailAfterTokens > maxOutputTokens:
		return fmt.Errorf("fail after tokens %d is outside 0 to %d", b.FailAfterTokens, maxOutputTokens)
	case b.FailMode != FailAbort && b.FailMode != FailFrame:
		return fmt.Errorf("unknown fail mode %d", b.FailMode)
	case b.FailFirstN < 0:
		return fmt.Errorf("fail first n %d is negative", b.FailFirstN)
	}
	return nil
}

// Config is the mock's complete behavior: defaults, plus a full behavior for
// each model that deviates from them. Rules are keyed by the model the
// upstream request names, which for OLP is the provider model rather than the
// route slug, so they apply the same way to a direct run and a gateway run.
type Config struct {
	Default Behavior
	Models  map[string]Behavior
}

// Validate checks every behavior in the configuration.
func (c Config) Validate() error {
	if err := c.Default.Validate(); err != nil {
		return fmt.Errorf("default: %w", err)
	}
	for model, b := range c.Models {
		if !safeModel([]byte(model)) {
			return fmt.Errorf("model %q is empty, too long, or has characters a JSON string would need escaped", model)
		}
		if err := b.Validate(); err != nil {
			return fmt.Errorf("model %q: %w", model, err)
		}
	}
	return nil
}

// Rule is a partial Behavior as the configuration endpoint, the configuration
// file and the command line carry it: unset fields inherit.
type Rule struct {
	TTFTMs          *float64 `json:"ttft_ms,omitempty"`
	IntervalMs      *float64 `json:"interval_ms,omitempty"`
	OutputTokens    *int     `json:"output_tokens,omitempty"`
	Status          *int     `json:"status,omitempty"`
	ErrorDelayMs    *float64 `json:"error_delay_ms,omitempty"`
	FailAfterTokens *int     `json:"fail_after_tokens,omitempty"`
	FailMode        *string  `json:"fail_mode,omitempty"`
	FailFirstN      *int     `json:"fail_first_n,omitempty"`
}

// Apply overlays the rule's fields on base.
func (r Rule) Apply(base Behavior) (Behavior, error) {
	b := base
	var err error
	if r.TTFTMs != nil {
		if b.TTFT, err = millis(*r.TTFTMs); err != nil {
			return base, fmt.Errorf("ttft_ms: %w", err)
		}
	}
	if r.IntervalMs != nil {
		if b.Interval, err = millis(*r.IntervalMs); err != nil {
			return base, fmt.Errorf("interval_ms: %w", err)
		}
	}
	if r.ErrorDelayMs != nil {
		if b.ErrorDelay, err = millis(*r.ErrorDelayMs); err != nil {
			return base, fmt.Errorf("error_delay_ms: %w", err)
		}
	}
	if r.OutputTokens != nil {
		b.OutputTokens = *r.OutputTokens
	}
	if r.Status != nil {
		b.Status = *r.Status
	}
	if r.FailAfterTokens != nil {
		b.FailAfterTokens = *r.FailAfterTokens
	}
	if r.FailMode != nil {
		if b.FailMode, err = parseFailMode(*r.FailMode); err != nil {
			return base, fmt.Errorf("fail_mode: %w", err)
		}
	}
	if r.FailFirstN != nil {
		b.FailFirstN = *r.FailFirstN
	}
	if err = b.Validate(); err != nil {
		return base, err
	}
	return b, nil
}

// Overlay returns r with each field it leaves unset taken from base, so a
// later source (a flag) can override an earlier one (a file) field by field.
func (r Rule) Overlay(base Rule) Rule {
	return Rule{
		TTFTMs: pick(r.TTFTMs, base.TTFTMs), IntervalMs: pick(r.IntervalMs, base.IntervalMs), OutputTokens: pick(r.OutputTokens, base.OutputTokens),
		Status: pick(r.Status, base.Status), ErrorDelayMs: pick(r.ErrorDelayMs, base.ErrorDelayMs), FailAfterTokens: pick(r.FailAfterTokens, base.FailAfterTokens),
		FailMode: pick(r.FailMode, base.FailMode), FailFirstN: pick(r.FailFirstN, base.FailFirstN),
	}
}

func pick[T any](a, b *T) *T {
	if a != nil {
		return a
	}
	return b
}

// RuleOf is the full rule that reproduces b, the inverse of Apply.
func RuleOf(b Behavior) Rule {
	ttft, interval, delay := float64(b.TTFT)/float64(time.Millisecond), float64(b.Interval)/float64(time.Millisecond), float64(b.ErrorDelay)/float64(time.Millisecond)
	mode := b.FailMode.String()
	return Rule{TTFTMs: &ttft, IntervalMs: &interval, OutputTokens: &b.OutputTokens, Status: &b.Status, ErrorDelayMs: &delay, FailAfterTokens: &b.FailAfterTokens, FailMode: &mode, FailFirstN: &b.FailFirstN}
}

// ParseRule reads the command line's `key=value,key=value` form, whose keys
// are the JSON field names.
func ParseRule(s string) (Rule, error) {
	var r Rule
	if strings.TrimSpace(s) == "" {
		return r, nil
	}
	for _, pair := range strings.Split(s, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok {
			return Rule{}, fmt.Errorf("%q is not key=value", pair)
		}
		var err error
		switch key {
		case "ttft_ms":
			r.TTFTMs, err = floatField(value)
		case "interval_ms":
			r.IntervalMs, err = floatField(value)
		case "error_delay_ms":
			r.ErrorDelayMs, err = floatField(value)
		case "output_tokens":
			r.OutputTokens, err = intField(value)
		case "status":
			r.Status, err = intField(value)
		case "fail_after_tokens":
			r.FailAfterTokens, err = intField(value)
		case "fail_first_n":
			r.FailFirstN, err = intField(value)
		case "fail_mode":
			r.FailMode = &value
		default:
			return Rule{}, fmt.Errorf("unknown rule key %q", key)
		}
		if err != nil {
			return Rule{}, fmt.Errorf("%s: %w", key, err)
		}
	}
	return r, nil
}

// Spec is the JSON form of Config: partial rules over a default.
type Spec struct {
	Default Rule            `json:"default"`
	Models  map[string]Rule `json:"models,omitempty"`
}

// Config resolves the spec against base: the default rule overlays base.Default
// and each model rule overlays the resulting default. Models in base that the
// spec does not mention are dropped, so a spec describes the whole
// configuration.
func (s Spec) Config(base Config) (Config, error) {
	def, err := s.Default.Apply(base.Default)
	if err != nil {
		return Config{}, fmt.Errorf("default: %w", err)
	}
	cfg := Config{Default: def, Models: make(map[string]Behavior, len(s.Models))}
	for model, rule := range s.Models {
		if cfg.Models[model], err = rule.Apply(def); err != nil {
			return Config{}, fmt.Errorf("model %q: %w", model, err)
		}
	}
	return cfg, cfg.Validate()
}

// SpecOf is the spec that reproduces cfg.
func SpecOf(cfg Config) Spec {
	spec := Spec{Default: RuleOf(cfg.Default), Models: make(map[string]Rule, len(cfg.Models))}
	for model, b := range cfg.Models {
		spec.Models[model] = RuleOf(b)
	}
	return spec
}

func millis(ms float64) (time.Duration, error) {
	if math.IsNaN(ms) || math.IsInf(ms, 0) || ms < 0 || ms > float64(maxDelay/time.Millisecond) {
		return 0, fmt.Errorf("%v ms is outside 0 to %d", ms, maxDelay/time.Millisecond)
	}
	return time.Duration(ms * float64(time.Millisecond)), nil
}

func parseFailMode(s string) (FailMode, error) {
	switch s {
	case "abort":
		return FailAbort, nil
	case "error":
		return FailFrame, nil
	}
	return FailAbort, fmt.Errorf("%q is not abort or error", s)
}

func floatField(s string) (*float64, error) {
	v, err := strconv.ParseFloat(s, 64)
	return &v, err
}

func intField(s string) (*int, error) {
	v, err := strconv.Atoi(s)
	return &v, err
}

// Request headers override the resolved behavior. Go canonicalizes incoming
// header names, so these are looked up in the canonical form without
// allocating.
const (
	headerTTFT       = "X-Mock-Ttft-Ms"
	headerInterval   = "X-Mock-Interval-Ms"
	headerTokens     = "X-Mock-Output-Tokens"
	headerStatus     = "X-Mock-Status"
	headerFailAfter  = "X-Mock-Fail-After-Tokens"
	headerFailMode   = "X-Mock-Fail-Mode"
	headerFailFirstN = "X-Mock-Fail-First-N"
	headerErrorDelay = "X-Mock-Error-Delay-Ms"
)

// controls maps each request header to the behavior field it overrides.
var controls = [...]struct {
	header string
	set    func(*Behavior, string) error
}{
	{headerTTFT, func(b *Behavior, v string) (err error) { b.TTFT, err = parseMillis(v); return }},
	{headerInterval, func(b *Behavior, v string) (err error) { b.Interval, err = parseMillis(v); return }},
	{headerErrorDelay, func(b *Behavior, v string) (err error) { b.ErrorDelay, err = parseMillis(v); return }},
	{headerTokens, func(b *Behavior, v string) (err error) { b.OutputTokens, err = strconv.Atoi(v); return }},
	{headerStatus, func(b *Behavior, v string) (err error) { b.Status, err = strconv.Atoi(v); return }},
	{headerFailAfter, func(b *Behavior, v string) (err error) { b.FailAfterTokens, err = strconv.Atoi(v); return }},
	{headerFailFirstN, func(b *Behavior, v string) (err error) { b.FailFirstN, err = strconv.Atoi(v); return }},
	{headerFailMode, func(b *Behavior, v string) (err error) { b.FailMode, err = parseFailMode(v); return }},
}

// applyHeaders overlays the request's x-mock-* headers on b. A malformed
// header is an error rather than being ignored: a mistyped control that
// silently does nothing would invalidate a measurement.
func applyHeaders(h map[string][]string, b Behavior) (Behavior, error) {
	for i := range controls {
		values := h[controls[i].header]
		if len(values) == 0 {
			continue
		}
		err := controls[i].set(&b, values[0])
		if err == nil {
			err = b.Validate()
		}
		if err != nil {
			return b, fmt.Errorf("%s %q: %w", controls[i].header, values[0], err)
		}
	}
	return b, nil
}

func parseMillis(s string) (time.Duration, error) {
	ms, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	return millis(ms)
}
