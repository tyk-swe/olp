// Package upstream classifies how an exchange with a provider ended: the
// failure class that governs failover and cooldown, and the upstream acceptance
// that records what is known about the provider's work. Every upstream call
// path reports its evidence explicitly, so a transport other than net/http is
// classified the same way.
package upstream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// Class is the failure class of an attempt, shared with the routing retry
// taxonomy.
type Class string

const (
	Connect     Class = "connect"
	Timeout     Class = "timeout"
	RateLimit   Class = "rate_limit"
	ServerError Class = "upstream_server"
	ClientError Class = "upstream_client"
	Credential  Class = "credential"
	Protocol    Class = "protocol"
	Cancelled   Class = "cancelled"
	// ContextWindow is a typed rejection of a request too large for the
	// target's context window, which a target with a larger one may serve.
	ContextWindow Class = "context_window"
	// Ambiguous marks a failure whose work the upstream may have performed on
	// a call that must not repeat it; it never fails over.
	Ambiguous Class = "ambiguous"
)

// Acceptance is what is known about the provider's work. Absence of
// client-visible bytes does not establish absence of work.
type Acceptance string

const (
	NotSent  Acceptance = "not-sent"
	Unknown  Acceptance = "outcome-unknown"
	Accepted Acceptance = "accepted"
	Terminal Acceptance = "terminal"
)

// Unresolved reports whether the provider may hold work whose outcome it has
// not stated.
func (a Acceptance) Unresolved() bool { return a == Unknown || a == Accepted }

// Evidence is what one exchange with the upstream established.
type Evidence struct {
	// Reached reports that request bytes may have reached the upstream. A
	// transport reports it once it began writing the request or a response
	// began; Trace reports it for net/http.
	Reached bool
	// Status is an unsuccessful HTTP status the upstream answered with.
	Status int
	// Accepted reports that the upstream answered with success and began its
	// result.
	Accepted bool
	// Settled reports that the upstream's complete result arrived.
	Settled bool
	// Error is the failure the upstream stated: the error body of Status, or
	// an in-band error inside an accepted response.
	Error *openai.UpstreamError
	// Committed reports that response bytes had been delivered to the client.
	Committed bool
	// Interrupted is why this side ended the exchange: context.Canceled when
	// the caller went away, context.DeadlineExceeded when a deadline expired.
	Interrupted error
	// Err is the error that otherwise ended the exchange.
	Err error
}

// Acceptance derives what is known about the provider's work.
func (e Evidence) Acceptance() Acceptance {
	switch {
	case e.Settled:
		return Terminal
	case e.Status >= 500:
		// A server failure does not show that the work is absent.
		return Unknown
	case e.Status != 0:
		return Terminal
	case e.Accepted:
		return Accepted
	case e.Reached:
		return Unknown
	}
	return NotSent
}

// Classifier classifies the failed exchanges of one upstream call.
type Classifier struct {
	// ContextWindow reports that the request is bounded by a context window
	// another target may exceed, so a typed context rejection is ContextWindow
	// rather than ClientError.
	ContextWindow bool
	// AtMostOnce reports that the call must not risk performing its work
	// twice: a failure that would fail over while the upstream's acceptance
	// is unresolved is Ambiguous.
	AtMostOnce bool
	// Declared is the provider profile's declared classification. The first
	// rule that matches a failure the upstream stated decides its class ahead
	// of the built-in rules; AtMostOnce still applies to it.
	Declared []Rule
}

// A Rule is a declared classification: the class of the failures the upstream
// stated that it matches. It matches an unsuccessful response or an in-band
// error when each value it sets equals the failure's: Status its status, Code
// and Type its stated error's code and type. A rule with a status never
// matches an in-band error.
type Rule struct {
	Status     int
	Code, Type string
	Class      Class
}

func (r Rule) matches(e Evidence) bool {
	switch {
	case r.Status != 0 && r.Status != e.Status:
		return false
	case r.Code != "" && (e.Error == nil || e.Error.Code != r.Code):
		return false
	case r.Type != "" && (e.Error == nil || e.Error.Type != r.Type):
		return false
	}
	return true
}

// Outcome is a classified failure.
type Outcome struct {
	Class      Class
	Acceptance Acceptance
}

// Classify classifies an exchange that ended without the result its call
// needed.
func (c Classifier) Classify(e Evidence) Outcome {
	outcome := Outcome{Class: c.class(e), Acceptance: e.Acceptance()}
	if c.AtMostOnce && outcome.Acceptance.Unresolved() {
		switch outcome.Class {
		case Connect, Timeout, ServerError:
			outcome.Class = Ambiguous
		}
	}
	return outcome
}

func (c Classifier) class(e Evidence) Class {
	switch {
	case errors.Is(e.Interrupted, context.DeadlineExceeded):
		return Timeout
	case e.Interrupted != nil:
		return Cancelled
	}
	if class, ok := c.declared(e); ok {
		return class
	}
	switch {
	case e.Status != 0:
		return c.rejection(e.Status, e.Error)
	case e.Error != nil:
		return c.inBand(e.Error, e.Committed)
	case e.Committed:
		return Protocol
	}
	var malformed *openai.ProtocolError
	if errors.As(e.Err, &malformed) || errors.Is(e.Err, openai.ErrEventTooLarge) {
		return Protocol
	}
	return Connect
}

// declared classifies a failure the upstream stated, an unsuccessful status or
// an in-band error, by the first declared rule that matches it.
func (c Classifier) declared(e Evidence) (Class, bool) {
	if e.Status == 0 && e.Error == nil {
		return "", false
	}
	for _, rule := range c.Declared {
		if rule.matches(e) {
			return rule.Class, true
		}
	}
	return "", false
}

// rejection classifies an unsuccessful status by its code and error body.
func (c Classifier) rejection(status int, stated *openai.UpstreamError) Class {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return Credential
	case status == http.StatusTooManyRequests:
		return RateLimit
	case status >= 500:
		return ServerError
	case contextRejected(stated):
		return c.contextClass()
	}
	return ClientError
}

// inBand classifies the typed envelope a streaming provider reports inside a
// successful response, at the same attempt boundary as an HTTP status.
func (c Classifier) inBand(stated *openai.UpstreamError, committed bool) Class {
	kind := codeFold.Replace(strings.ToLower(stated.Type + " " + stated.Code))
	has := func(names ...string) bool {
		for _, name := range names {
			if strings.Contains(kind, name) {
				return true
			}
		}
		return false
	}
	switch {
	case contextRejected(stated):
		return c.contextClass()
	case has("authentication", "unauthenticated", "invalidapikey", "accessdenied", "permissiondenied") || stated.Code == "401" || stated.Code == "403":
		return Credential
	case has("ratelimit", "resourceexhausted", "throttl") || stated.Code == "429":
		return RateLimit
	case has("timeout", "deadlineexceeded") || stated.Code == "504":
		return Timeout
	case has("invalidrequest", "invalidargument", "validationexception", "notfound") || stated.Code == "400" || stated.Code == "404":
		return ClientError
	}
	if committed {
		return Protocol
	}
	return ServerError
}

func (c Classifier) contextClass() Class {
	if c.ContextWindow {
		return ContextWindow
	}
	return ClientError
}

var contextWindowCodes = map[string]bool{
	"contextlengthexceeded":    true,
	"contextwindowexceeded":    true,
	"maxcontextlengthexceeded": true,
	"prompttoolong":            true,
}

var codeFold = strings.NewReplacer("_", "", "-", "", " ", "")

// contextRejected matches typed context-window codes and types exactly;
// message prose never classifies.
func contextRejected(e *openai.UpstreamError) bool {
	if e == nil {
		return false
	}
	return contextWindowCodes[codeFold.Replace(strings.ToLower(e.Code))] ||
		contextWindowCodes[codeFold.Replace(strings.ToLower(e.Type))]
}

// Trace reports through reached when a net/http request may have reached the
// upstream: once its headers are written, its request is written or its
// response begins. A failed body write can still leave work at the upstream,
// so only a failure before writing leaves reached unset.
func Trace(reached *atomic.Bool) *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		WroteHeaders: func() { reached.Store(true) },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				reached.Store(true)
			}
		},
		GotFirstResponseByte: func() { reached.Store(true) },
	}
}
