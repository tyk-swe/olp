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
// A WASI reactor never runs main, so the same source builds either way. See
// docs/plugin-authoring.md in the OpenLLMProxy repository.
package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// abi.CodeInvalidGrant; OLP retries any other failure.
	RefreshGrant(ctx context.Context, refresh GrantRefresh) (Grant, error)
}

var registered Plugin

// methods serve the methods OLP calls, each given the call's context and
// parameters. A method whose optional interface the plugin lacks reports
// abi.CodeUnknownMethod.
var methods = map[string]func(ctx context.Context, params json.RawMessage) (any, error){
	abi.MethodManifest:      manifest,
	abi.MethodSign:          sign,
	abi.MethodGrantStart:    enrollment(abi.MethodGrantStart, GrantEnroller.StartGrant),
	abi.MethodGrantExchange: enrollment(abi.MethodGrantExchange, GrantEnroller.ExchangeGrant),
	abi.MethodGrantPoll:     enrollment(abi.MethodGrantPoll, GrantPoller.PollGrant),
	abi.MethodGrantRefresh:  refreshGrant,
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
// Signer, and one that authenticates with a grant a GrantEnroller, so a plugin
// that declares one without implementing it reports no manifest, and OLP
// refuses to install it.
func manifest(context.Context, json.RawMessage) (any, error) {
	m := registered.Manifest()
	if _, signs := registered.(Signer); !signs && slices.ContainsFunc(m.Profiles, func(p Profile) bool { return p.Signing }) {
		return nil, &abi.Error{Code: abi.CodeInternal, Message: "A profile declares signing, but the plugin does not implement Signer."}
	}
	if _, enrolls := registered.(GrantEnroller); !enrolls && slices.ContainsFunc(m.Profiles, func(p Profile) bool { return p.Grant != nil }) {
		return nil, &abi.Error{Code: abi.CodeInternal, Message: "A profile authenticates with a grant, but the plugin does not implement GrantEnroller."}
	}
	return m, nil
}

// sign answers the sign call with the plugin's Signer.
func sign(ctx context.Context, params json.RawMessage) (any, error) {
	signer, ok := registered.(Signer)
	if !ok {
		return nil, unknownMethod(abi.MethodSign)
	}
	var request SignRequest
	if err := json.Unmarshal(params, &request); err != nil {
		return nil, &abi.Error{Code: abi.CodeInvalidRequest, Message: "A sign request carries the request to sign."}
	}
	return signer.Sign(ctx, request)
}

// enrollment serves a grant enrollment step with the plugin's implementation
// of the step's interface E, GrantEnroller or GrantPoller.
func enrollment[E, P, R any](method string, step func(E, context.Context, P) (R, error)) func(context.Context, json.RawMessage) (any, error) {
	return func(ctx context.Context, params json.RawMessage) (any, error) {
		enroller, ok := registered.(E)
		if !ok {
			return nil, unknownMethod(method)
		}
		var p P
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &abi.Error{Code: abi.CodeInvalidRequest, Message: "The parameters do not match the method."}
		}
		return step(enroller, ctx, p)
	}
}

// refreshGrant answers the grant_refresh call with the plugin's
// GrantRefresher.
func refreshGrant(ctx context.Context, params json.RawMessage) (any, error) {
	refresher, ok := registered.(GrantRefresher)
	if !ok {
		return nil, unknownMethod(abi.MethodGrantRefresh)
	}
	var refresh GrantRefresh
	if err := json.Unmarshal(params, &refresh); err != nil {
		return nil, &abi.Error{Code: abi.CodeInvalidRequest, Message: "A grant_refresh call carries the grant to refresh."}
	}
	return refresher.RefreshGrant(ctx, refresh)
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
