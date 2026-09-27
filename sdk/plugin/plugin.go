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
// Plugin, and GrantEnroller if its profiles authenticate with a grant. OLP
// runs the module confined: it reaches nothing but the capabilities OLP
// grants, which are a clock, randomness, Log and, for grant enrollment steps,
// Fetch. See docs/plugin-authoring.md in the OpenLLMProxy repository.
package plugin

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// Manifest declares what a plugin offers and what it may reach.
type Manifest = abi.Manifest

// Profile is a provider profile a plugin supplies around a built-in dialect.
type Profile = abi.Profile

// Hosting is a profile's hosting adaptation, which OLP runs.
type Hosting = abi.Hosting

// GrantAuthentication declares that a profile authenticates with a grant.
type GrantAuthentication = abi.GrantAuthentication

// Grant enrollment steps' parameters and results.
type (
	GrantStart         = abi.GrantStart
	GrantAuthorization = abi.GrantAuthorization
	GrantExchange      = abi.GrantExchange
	Grant              = abi.Grant
)

// Error is a failure a plugin reports to OLP with a code of its own.
type Error = abi.Error

// Plugin is implemented by every provider plugin.
type Plugin interface {
	// Manifest declares the plugin's profiles and the origins it may reach.
	// OLP reads it once, at install.
	Manifest() Manifest
}

// GrantEnroller is implemented by a plugin whose profiles authenticate with a
// grant. OLP runs its steps when an operator enrolls a grant, and grants them
// Fetch.
type GrantEnroller interface {
	// StartGrant builds the authorization request the operator opens to sign
	// in upstream, including its state and PKCE challenge.
	StartGrant(GrantStart) (GrantAuthorization, error)
	// ExchangeGrant exchanges what the operator pasted back for a grant. It
	// reports a value carrying another request's state as an *Error with
	// code abi.CodeStateMismatch.
	ExchangeGrant(GrantExchange) (Grant, error)
}

var registered Plugin

// Register makes p the plugin this module serves. Call it once, from init.
func Register(p Plugin) {
	if registered != nil {
		panic("plugin: Register called more than once")
	}
	registered = p
}

// serve answers one request from OLP. It never panics: a panicking plugin
// reports an internal error instead of stopping the module.
func serve(message []byte) (response []byte) {
	defer func() {
		if recovered := recover(); recovered != nil {
			Log.Error("plugin panicked", "panic", fmt.Sprint(recovered))
			response = respond(nil, &abi.Error{Code: abi.CodeInternal, Message: "The plugin panicked."})
		}
	}()
	var request abi.Request
	if err := json.Unmarshal(message, &request); err != nil {
		return respond(nil, &abi.Error{Code: abi.CodeInvalidRequest, Message: "The request is not a JSON call."})
	}
	if registered == nil {
		return respond(nil, &abi.Error{Code: abi.CodeInternal, Message: "The module registered no plugin."})
	}
	enroller, enrolls := registered.(GrantEnroller)
	switch {
	case request.Method == abi.MethodManifest:
		return respond(registered.Manifest(), nil)
	case request.Method == abi.MethodGrantStart && enrolls:
		return step(request.Params, enroller.StartGrant)
	case request.Method == abi.MethodGrantExchange && enrolls:
		return step(request.Params, enroller.ExchangeGrant)
	}
	return respond(nil, &abi.Error{Code: abi.CodeUnknownMethod, Message: "The plugin does not implement " + request.Method + "."})
}

// step serves a method implemented by run. An error run returns is reported
// with its own code when it is an *Error, and as an internal error otherwise.
func step[P, R any](params json.RawMessage, run func(P) (R, error)) []byte {
	var p P
	if err := json.Unmarshal(params, &p); err != nil {
		return respond(nil, &abi.Error{Code: abi.CodeInvalidRequest, Message: "The parameters do not match the method."})
	}
	result, err := run(p)
	if err == nil {
		return respond(result, nil)
	}
	if reported, ok := errors.AsType[*Error](err); ok {
		return respond(nil, reported)
	}
	return respond(nil, &abi.Error{Code: abi.CodeInternal, Message: err.Error()})
}

func respond(result any, failure *abi.Error) []byte {
	response := abi.Response{Error: failure}
	if failure == nil {
		data, err := json.Marshal(result)
		if err != nil {
			response.Error = &abi.Error{Code: abi.CodeInternal, Message: "The plugin's result is not JSON."}
		}
		response.Result = data
	}
	data, _ := json.Marshal(response)
	return data
}

// callHost uses an OLP capability and returns its result.
func callHost(capability string, params any) (json.RawMessage, error) {
	data, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	request, err := json.Marshal(abi.Request{Method: capability, Params: data})
	if err != nil {
		return nil, err
	}
	var response abi.Response
	if err = json.Unmarshal(hostCall(request), &response); err != nil {
		return nil, err
	}
	if response.Error != nil {
		return nil, response.Error
	}
	return response.Result, nil
}
