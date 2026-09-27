// Package abi defines the OpenLLMProxy provider plugin ABI: the functions a
// plugin module exports, the host function it may import, and the JSON
// messages that cross between them. OLP and the Go SDK share these
// definitions. docs/plugin-authoring.md describes the ABI in full.
//
// A module is a WASI preview 1 reactor. OLP runs its _initialize export,
// checks olp_abi_version, then serves each call by writing a Request into a
// buffer from olp_alloc and passing it to olp_call, which returns a Response.
// While a call runs, the plugin reaches host capabilities by passing a Request
// to the host_call import. Every message is a JSON document; buffers are
// passed as a pointer and a length, and returned packed into one i64.
//
// The ABI is versioned as a whole and, during 0.x, carries no compatibility
// promise between versions: OLP refuses a module built for another version at
// install.
package abi

import "encoding/json"

// Version is the ABI version this package describes.
const Version = 1

// Functions a plugin module exports.
const (
	// ExportVersion is `() -> i32` and returns the module's ABI version.
	ExportVersion = "olp_abi_version"
	// ExportAlloc is `(size i32) -> i32`. It returns a buffer of size bytes
	// that the host writes a message into; the buffer belongs to the plugin
	// again once the message is passed to olp_call or returned from host_call.
	ExportAlloc = "olp_alloc"
	// ExportCall is `(ptr i32, len i32) -> i64`. It serves one Request and
	// returns its Response, packed, valid until the next export call.
	ExportCall = "olp_call"
)

// HostModule is the import module that provides HostCall.
const HostModule = "olp"

// HostCall is `(ptr i32, len i32) -> i64`, imported from HostModule. It serves
// one capability Request and returns its Response, packed, in a buffer the
// host obtained from olp_alloc.
const HostCall = "host_call"

// Methods OLP calls on a plugin.
const (
	// MethodManifest takes no parameters and returns the plugin's Manifest.
	MethodManifest = "manifest"
)

// Capabilities a plugin calls on OLP. OLP grants each call only the
// capabilities it lists; every call may log.
const (
	// CapabilityLog takes a LogRecord and returns no result.
	CapabilityLog = "log"
)

// Error codes shared by plugins and OLP. A plugin may report codes of its own.
const (
	CodeInvalidRequest = "invalid_request"
	CodeUnknownMethod  = "unknown_method"
	CodeInternal       = "internal"
)

// Request is one call, from OLP to a plugin or from a plugin to OLP.
type Request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response answers a Request with either a result or an error.
type Response struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// Error is a failure reported across the ABI.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Manifest declares what a plugin offers and what it may reach.
type Manifest struct {
	// Name identifies the plugin across its versions. Several digests of the
	// same plugin may be installed side by side.
	Name string `json:"name"`
	// Version is the author's label for this build.
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	// Origins are the only origins the plugin may reach once an owner has
	// approved them, each in canonical scheme://host[:port] form.
	Origins  []string  `json:"origins"`
	Profiles []Profile `json:"profiles"`
}

// Profile is a provider profile the plugin supplies around a built-in dialect.
// Providers using it authenticate with a static credential, which its hosting
// adaptation places.
type Profile struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// Dialect names the built-in dialect the profile serves, such as
	// openai-chat. A plugin never defines a dialect.
	Dialect string `json:"dialect"`
	// Hosting places the dialect's requests at the upstream.
	Hosting Hosting `json:"hosting"`
}

// Hosting is a profile's hosting adaptation: where and how the dialect's
// requests reach the upstream. It is a declaration OLP runs itself, so no
// plugin code runs per request.
//
// Header and query parameter values are templates. The placeholder
// {credential} stands for the provider's static credential, such as
// "Token {credential}"; braces appear nowhere else.
//
// A profile with an envelope or rewrites changes the dialect's bodies, and one
// that forces streaming changes how non-streaming requests reach the upstream,
// so either serves only transformed routes.
type Hosting struct {
	// Address is the upstream's base URL, which the dialect's paths extend,
	// such as https://api.example.com/v1 for /chat/completions. Its origin is
	// one of the manifest's Origins.
	Address string `json:"address"`
	// Headers are the declared request headers, by name.
	Headers map[string]string `json:"headers,omitempty"`
	// Query holds the declared query parameters of the address, by name.
	Query map[string]string `json:"query,omitempty"`
	// Envelope, if the upstream wants one, wraps the dialect's request bodies
	// in the upstream's own JSON object and unwraps its responses and stream
	// events.
	Envelope *Envelope `json:"envelope,omitempty"`
	// Rewrites change the dialect's request body, in order, before the
	// envelope wraps it.
	Rewrites []Rewrite `json:"rewrites,omitempty"`
	// ForceStreaming declares an upstream that serves only streaming
	// requests. OLP sends every request as a streaming one and aggregates the
	// stream into the dialect's non-streaming result for a caller that did
	// not ask to stream. OLP aggregates openai-responses streams.
	ForceStreaming bool `json:"force_streaming,omitempty"`
}

// Envelope is the upstream's own JSON object around the dialect's bodies, such
// as {"model": ..., "request": {...}} around each request and
// {"response": {...}} around each response and stream event. Members are named
// with 1–128 letters, digits, underscores, hyphens and dots.
type Envelope struct {
	// Request names the member of the upstream's request object that carries
	// the dialect's request body, such as "request". Without it, OLP sends the
	// dialect's request body as it is.
	Request string `json:"request,omitempty"`
	// Fields are the request object's other members, by name. Values are
	// templates, which become JSON strings. The placeholder {model} stands for
	// the upstream model the request is for.
	Fields map[string]string `json:"fields,omitempty"`
	// Response names the member of the upstream's successful responses and
	// stream events that carries the dialect's response or event, such as
	// "response". A response or event without the member, such as an upstream
	// error, reaches the dialect as it is. Without it, OLP reads responses as
	// they are.
	Response string `json:"response,omitempty"`
}

// Rewrite changes one member of the dialect's request body.
type Rewrite struct {
	// Op is RewriteSet, RewriteDefault or RewriteDelete.
	Op string `json:"op"`
	// Path is a JSON pointer to an object member, such as /store or
	// /generationConfig/seed. Setting a member creates the objects above it.
	Path string `json:"path"`
	// Value is the JSON value to set or default to. Deleting takes none.
	Value json.RawMessage `json:"value,omitempty"`
}

// Rewrite operations.
const (
	// RewriteSet sets the member to the value, replacing the request's.
	RewriteSet = "set"
	// RewriteDefault sets the member to the value unless the request has it.
	RewriteDefault = "default"
	// RewriteDelete removes the member if the request has it.
	RewriteDelete = "delete"
)

// CredentialPlaceholder is the template placeholder for a provider's static
// credential.
const CredentialPlaceholder = "{credential}"

// ModelPlaceholder is the template placeholder for the upstream model a
// request is for.
const ModelPlaceholder = "{model}"

// LogRecord is the parameter of CapabilityLog.
type LogRecord struct {
	// Level is debug, info, warn or error.
	Level   string            `json:"level"`
	Message string            `json:"message"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

// Pack returns a buffer's pointer and length as the i64 the ABI returns.
func Pack(ptr, size uint32) uint64 { return uint64(ptr)<<32 | uint64(size) }

// Unpack splits an i64 returned across the ABI into a pointer and a length.
func Unpack(packed uint64) (ptr, size uint32) { return uint32(packed >> 32), uint32(packed) }
