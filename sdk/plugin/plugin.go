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
// Plugin. OLP runs the module confined: it reaches nothing but the
// capabilities OLP grants, which are a clock, randomness and Log. See
// docs/plugin-authoring.md in the OpenLLMProxy repository.
package plugin

import (
	"encoding/json"
	"fmt"

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

// Error is a failure a plugin reports to OLP with a code of its own.
type Error = abi.Error

// Plugin is implemented by every provider plugin.
type Plugin interface {
	// Manifest declares the plugin's profiles and the origins it may reach.
	// OLP reads it once, at install.
	Manifest() Manifest
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
	switch request.Method {
	case abi.MethodManifest:
		return respond(registered.Manifest(), nil)
	}
	return respond(nil, &abi.Error{Code: abi.CodeUnknownMethod, Message: "The plugin does not implement " + request.Method + "."})
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
