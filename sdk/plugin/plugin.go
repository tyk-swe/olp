// Package plugin is the Go SDK for OpenLLMProxy provider plugins.
//
// A plugin is a main package that registers its implementation from init and
// has an empty main function:
//
//	func init() { plugin.Register(myPlugin{}) }
//
//	func main() {}
//
// Build it as a WASI reactor:
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o plugin.wasm .
//
// The SDK implements the ABI in package abi, so a plugin only implements
// Plugin, Signer if a profile declares signing, and GrantEnroller and
// GrantRefresher if its profiles authenticate with a grant, with GrantPoller if
// it enrolls grants by device authorization. OLP runs the module confined: it
// reaches nothing but the capabilities OLP grants, which are a clock,
// randomness, Log and, for grant enrollment steps and grant refresh, Fetch.
//
// A deployment that enables the experimental unconfined tier may run the same
// plugin as an unconfined plugin instead: a native executable in its image,
// with the operating system's privileges, which serves OLP's calls over
// standard input and output. Build it natively, calling Serve from main:
//
//	func main() { plugin.Serve() }
//
// An unconfined plugin may also carry its profiles' upstream traffic itself,
// implementing Carrier.
//
// A WASI reactor never runs main, so the same source builds either way. See
// docs/plugin-authoring.md in the OpenLLMProxy repository.
package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// Manifest declares what a plugin offers and what it may reach.
type Manifest = abi.Manifest

// Profile is a provider profile a plugin supplies around a built-in dialect.
type Profile = abi.Profile

// Hosting is a profile's hosting adaptation, which OLP runs.
type Hosting = abi.Hosting

// Envelope is the upstream's own JSON object around a dialect's bodies.
type Envelope = abi.Envelope

// Rewrite changes one member of a dialect's request body.
type Rewrite = abi.Rewrite

// Rewrite operations.
const (
	RewriteSet     = abi.RewriteSet
	RewriteDefault = abi.RewriteDefault
	RewriteDelete  = abi.RewriteDelete
)

// Discovery is an upstream's model listing, which OLP reads.
type Discovery = abi.Discovery

// Pagination is how an upstream's model listing continues across pages.
type Pagination = abi.Pagination

// FailureRule classifies the upstream failures it matches.
type FailureRule = abi.FailureRule

// Option is a non-secret setting a profile declares, which each provider
// using the profile sets.
type Option = abi.Option

// Provider is the provider a call serves: the profile it uses and its option
// values.
type Provider = abi.Provider

// GrantAuthentication declares that a profile authenticates with a grant.
type GrantAuthentication = abi.GrantAuthentication

// Grant enrollment steps' and grant refresh's parameters and results.
type (
	GrantStart          = abi.GrantStart
	GrantAuthorization  = abi.GrantAuthorization
	DeviceAuthorization = abi.DeviceAuthorization
	GrantExchange       = abi.GrantExchange
	GrantPoll           = abi.GrantPoll
	Grant               = abi.Grant
	GrantRefresh        = abi.GrantRefresh
)

// Error is a failure a plugin reports to OLP with a code of its own.
type Error = abi.Error

// SignRequest is an upstream request for a Signer to sign.
type SignRequest = abi.SignRequest

// SignResult carries the headers a Signer adds to a request.
type SignResult = abi.SignResult

// RouteFeatures are the request features a route selector's plugin predicate
// judges, and RouteVerdict is its answer.
type (
	RouteFeatures = abi.RoutePredicate
	RouteVerdict  = abi.RouteVerdict
)

// Plugin is implemented by every provider plugin.
type Plugin interface {
	// Manifest declares the plugin's profiles and the origins it may reach.
	// OLP reads it once, at install.
	Manifest() Manifest
}

// Signer is implemented by a plugin whose profiles declare signing.
type Signer interface {
	// Sign returns the headers to add to one upstream request of a profile
	// that declares signing, such as a signature of the request made with
	// its credential. OLP calls it once per request, never per stream event,
	// after the profile's hosting adaptation placed the request, and
	// ProviderOf(ctx) returns the provider the request is for, with its
	// option values. A failure fails the request before it is sent.
	Sign(ctx context.Context, request SignRequest) (SignResult, error)
}

// RoutePredicate is implemented by a plugin that route selectors consult. OLP
// calls it only on a confined plugin, while it plans a request, with the
// request's features and never its content. A match can only narrow the
// route's targets or delegate to another route; an error falls through to
// the route's next selector.
type RoutePredicate interface {
	MatchRoute(ctx context.Context, features RouteFeatures) (RouteVerdict, error)
}

// GrantEnroller is implemented by a plugin whose profiles authenticate with a
// grant. OLP runs its steps when an operator enrolls a grant for a provider,
// ProviderOf(ctx) returns that provider, with its option values, and OLP
// grants the steps Fetch.
type GrantEnroller interface {
	// StartGrant builds the authorization request the operator opens to sign
	// in upstream, including its state and PKCE challenge.
	StartGrant(ctx context.Context, start GrantStart) (GrantAuthorization, error)
	// ExchangeGrant exchanges what the operator pasted back for a grant. It
	// reports a value carrying another request's state as an *Error with
	// code abi.CodeStateMismatch.
	ExchangeGrant(ctx context.Context, exchange GrantExchange) (Grant, error)
}

// GrantPoller is implemented by a GrantEnroller whose StartGrant returns a
// device authorization. While the operator approves the device upstream, OLP
// polls it, on behalf of the enrolling provider and with Fetch, waiting the
// device authorization's interval between polls.
type GrantPoller interface {
	// PollGrant returns the grant once the operator approved the device.
	// Until then it reports an *Error with code abi.CodeAuthorizationPending,
	// or abi.CodeSlowDown when the upstream asks OLP to poll less often, and
	// with abi.CodeAccessDenied or abi.CodeExpiredToken when the operator
	// denied the device authorization or it expired.
	PollGrant(ctx context.Context, poll GrantPoll) (Grant, error)
}

// GrantRefresher is implemented by a plugin whose grants carry a refresh
// token. OLP's workers call it to renew a grant's access token ahead of its
// expiry, and early when the upstream refused it; ProviderOf(ctx) returns the
// provider the grant's credential version belongs to, and OLP grants the call
// Fetch.
type GrantRefresher interface {
	// RefreshGrant exchanges the grant's refresh token for a new access
	// token, and returns the refresh token that replaces it when the upstream
	// rotates it. It reports a grant the upstream will no longer refresh,
	// such as one whose refresh token was revoked, as an *Error with code
	// abi.CodeInvalidGrant. OLP retries other failures only when it knows the
	// token was not spent; an ambiguous outcome eventually lapses the grant.
	RefreshGrant(ctx context.Context, refresh GrantRefresh) (Grant, error)
}

// Carrier is implemented by an unconfined plugin whose profiles declare
// CarriesTraffic: it carries their upstream traffic instead of OLP's
// transport.
type Carrier interface {
	// Carry sends one upstream request of a profile that carries traffic,
	// which OLP placed, authenticated and signed, and returns the upstream's
	// response. The SDK streams its Body to OLP as it reads it, so a stream's
	// events reach the caller as the upstream sends them, and closes it.
	// ProviderOf(ctx) returns the provider the request is for, and ctx ends
	// when OLP no longer waits for the response, such as when the caller
	// cancels: send the request with ctx, so the cancellation reaches the
	// upstream.
	//
	// Report a request that never reached the upstream, such as one whose
	// connection failed, as an *Error with code abi.CodeNotSent, and OLP may
	// try the request elsewhere. OLP treats any other failure, including one
	// reading the body, as an unknown upstream outcome, which it never tries
	// elsewhere.
	Carry(ctx context.Context, request HTTPRequest) (CarriedResponse, error)
}

// CarriedResponse is the upstream's response to a request a Carrier carried.
type CarriedResponse struct {
	Status int
	Header map[string][]string
	// Body is the response body, or nil for none. The SDK closes it once it
	// read it, or as soon as OLP cancels the call, even while reading it, as
	// the body of an *http.Response allows.
	Body io.ReadCloser
}

var registered Plugin

// methods serve the methods OLP calls, each given the call's context and
// parameters. A method whose optional interface the plugin lacks reports
// abi.CodeUnknownMethod.
var methods = map[string]func(ctx context.Context, params json.RawMessage) (any, error){
	abi.MethodManifest:       manifest,
	abi.MethodSign:           optional(abi.MethodSign, "A sign request carries the request to sign.", Signer.Sign),
	abi.MethodGrantStart:     optional(abi.MethodGrantStart, invalidParams, GrantEnroller.StartGrant),
	abi.MethodGrantExchange:  optional(abi.MethodGrantExchange, invalidParams, GrantEnroller.ExchangeGrant),
	abi.MethodGrantPoll:      optional(abi.MethodGrantPoll, invalidParams, GrantPoller.PollGrant),
	abi.MethodGrantRefresh:   optional(abi.MethodGrantRefresh, "A grant_refresh call carries the grant to refresh.", GrantRefresher.RefreshGrant),
	abi.MethodCarry:          carry,
	abi.MethodRoutePredicate: optional(abi.MethodRoutePredicate, "A route_predicate call carries the request's features.", RoutePredicate.MatchRoute),
}

type providerKey struct{}

// ProviderOf returns the provider the call ctx serves, for a method OLP calls
// on behalf of a provider: the profile the provider uses and the values its
// operator set for the profile's options. ok is false for any other call.
func ProviderOf(ctx context.Context) (provider Provider, ok bool) {
	provider, ok = ctx.Value(providerKey{}).(Provider)
	return provider, ok
}

// Register makes p the plugin this module serves. Call it once, from init.
func Register(p Plugin) {
	if registered != nil {
		panic("plugin: Register called more than once")
	}
	registered = p
}

// serve answers one request message OLP passes a WASI reactor, which serves
// one call at a time.
func serve(message []byte) []byte {
	var request abi.Request
	response := answer(nil, &abi.Error{Code: abi.CodeInvalidRequest, Message: "The request is not a JSON call."})
	if json.Unmarshal(message, &request) == nil {
		response = handle(context.Background(), request)
	}
	data, _ := json.Marshal(response)
	return data
}

// handle answers one request from OLP in the context of its call. It never
// panics: a panicking plugin reports an internal error instead of stopping.
func handle(ctx context.Context, request abi.Request) (response abi.Response) {
	defer func() {
		if recovered := recover(); recovered != nil {
			Log.ErrorContext(ctx, "plugin panicked", "panic", fmt.Sprint(recovered))
			response = answer(nil, &abi.Error{Code: abi.CodeInternal, Message: "The plugin panicked."})
		}
	}()
	if registered == nil {
		return answer(nil, &abi.Error{Code: abi.CodeInternal, Message: "The module registered no plugin."})
	}
	method, ok := methods[request.Method]
	if !ok {
		return answer(nil, unknownMethod(request.Method))
	}
	if request.Provider != nil {
		ctx = context.WithValue(ctx, providerKey{}, *request.Provider)
	}
	result, err := method(ctx, request.Params)
	return answer(result, reported(err))
}

// manifest answers the manifest call. A profile that declares signing needs a
// Signer, one that authenticates with a grant a GrantEnroller, and one that
// carries traffic a Carrier, so a plugin that declares one without
// implementing it reports no manifest, and OLP refuses to install it.
func manifest(context.Context, json.RawMessage) (any, error) {
	m := registered.Manifest()
	if _, signs := registered.(Signer); !signs && slices.ContainsFunc(m.Profiles, func(p Profile) bool { return p.Signing }) {
		return nil, &abi.Error{Code: abi.CodeInternal, Message: "A profile declares signing, but the plugin does not implement Signer."}
	}
	if _, enrolls := registered.(GrantEnroller); !enrolls && slices.ContainsFunc(m.Profiles, func(p Profile) bool { return p.Grant != nil }) {
		return nil, &abi.Error{Code: abi.CodeInternal, Message: "A profile authenticates with a grant, but the plugin does not implement GrantEnroller."}
	}
	if _, carries := registered.(Carrier); !carries && slices.ContainsFunc(m.Profiles, func(p Profile) bool { return p.CarriesTraffic }) {
		return nil, &abi.Error{Code: abi.CodeInternal, Message: "A profile carries traffic, but the plugin does not implement Carrier."}
	}
	return m, nil
}

// carryChunk bounds the body bytes one part of a carried response holds, so
// that each part, in base64 within a frame, stays well within 1 MiB.
const carryChunk = 64 << 10

// carry answers the carry call with the plugin's Carrier: it streams the
// response as the call's parts, its head first, then its body as it reads it.
func carry(ctx context.Context, params json.RawMessage) (any, error) {
	carrier, ok := registered.(Carrier)
	if !ok {
		return nil, unknownMethod(abi.MethodCarry)
	}
	var request HTTPRequest
	if err := json.Unmarshal(params, &request); err != nil {
		return nil, &abi.Error{Code: abi.CodeInvalidRequest, Message: "A carry request carries the request to send."}
	}
	response, err := carrier.Carry(ctx, request)
	if err != nil {
		return nil, err
	}
	head := HTTPResponse{Status: response.Status, Header: response.Header}
	if response.Body == nil {
		return nil, sendPart(ctx, head)
	}
	defer response.Body.Close()
	// A body that ignores the call's context still stops when OLP cancels.
	defer context.AfterFunc(ctx, func() { response.Body.Close() })()
	if err = sendPart(ctx, head); err != nil {
		return nil, err
	}
	chunk := make([]byte, carryChunk)
	for {
		n, err := response.Body.Read(chunk)
		if n > 0 {
			if sent := sendPart(ctx, HTTPResponse{Body: chunk[:n]}); sent != nil {
				return nil, sent
			}
		}
		switch {
		case errors.Is(err, io.EOF):
			return nil, nil
		case err != nil:
			return nil, fmt.Errorf("reading the upstream's response: %w", err)
		}
	}
}

// invalidParams is what a method reports for parameters that do not match it.
const invalidParams = "The parameters do not match the method."

// optional serves a method with the plugin's implementation of the interface I
// that declares it, which takes the parameters as a P.
func optional[I, P, R any](method, invalid string, call func(I, context.Context, P) (R, error)) func(context.Context, json.RawMessage) (any, error) {
	return func(ctx context.Context, params json.RawMessage) (any, error) {
		implementation, ok := registered.(I)
		if !ok {
			return nil, unknownMethod(method)
		}
		var p P
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &abi.Error{Code: abi.CodeInvalidRequest, Message: invalid}
		}
		return call(implementation, ctx, p)
	}
}

func unknownMethod(method string) *abi.Error {
	return &abi.Error{Code: abi.CodeUnknownMethod, Message: "The plugin does not implement " + method + "."}
}

// reported is err as the plugin reports it across the ABI: an *Error as it
// is, anything else as an internal error.
func reported(err error) *abi.Error {
	if err == nil {
		return nil
	}
	if failure, ok := errors.AsType[*abi.Error](err); ok {
		return failure
	}
	return &abi.Error{Code: abi.CodeInternal, Message: err.Error()}
}

func answer(result any, failure *abi.Error) abi.Response {
	response := abi.Response{Error: failure}
	if failure == nil {
		data, err := json.Marshal(result)
		if err != nil {
			response.Error = &abi.Error{Code: abi.CodeInternal, Message: "The plugin's result is not JSON."}
		}
		response.Result = data
	}
	return response
}

// callHost uses an OLP capability for the call ctx belongs to and returns
// its result.
func callHost(ctx context.Context, capability string, params any) (json.RawMessage, error) {
	data, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	response := hostCall(ctx, abi.Request{Method: capability, Params: data})
	if response.Error != nil {
		return nil, response.Error
	}
	return response.Result, nil
}
