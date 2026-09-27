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
// requests reach the upstream, how the upstream lists its models and how its
// failures are classified. It is a declaration OLP runs itself, so no plugin
// code runs per request.
//
// Header and query parameter values are templates. The placeholder
// {credential} stands for the provider's static credential, such as
// "Token {credential}"; braces appear nowhere else.
type Hosting struct {
	// Address is the upstream's base URL, which the dialect's paths extend,
	// such as https://api.example.com/v1 for /chat/completions. Its origin is
	// one of the manifest's Origins.
	Address string `json:"address"`
	// Headers are the declared request headers, by name.
	Headers map[string]string `json:"headers,omitempty"`
	// Query holds the declared query parameters of the address, by name.
	Query map[string]string `json:"query,omitempty"`
	// Discovery declares how the upstream lists its models. Without it,
	// operators declare the models of a provider using the profile.
	Discovery *Discovery `json:"discovery,omitempty"`
	// Classification declares the failure class of upstream failures, in
	// order: the first rule that matches a failure decides its class. OLP's
	// built-in rules classify every failure no rule matches.
	Classification []FailureRule `json:"classification,omitempty"`
}

// CredentialPlaceholder is the template placeholder for a provider's static
// credential.
const CredentialPlaceholder = "{credential}"

// Discovery is an upstream's model listing: OLP GETs Path, placed like every
// request, and reads a JSON object whose Models field holds an array of model
// objects, each with its ID in its ID field. Field names are top-level names,
// such as data and id.
type Discovery struct {
	// Path is the listing's path, which extends the address, such as /models.
	Path string `json:"path"`
	// Models names the listing's field that holds the array of models.
	Models string `json:"models"`
	// ID names the field of a model that holds its ID.
	ID string `json:"id"`
	// Pagination, when declared, follows a listing across pages.
	Pagination *Pagination `json:"pagination,omitempty"`
}

// Pagination is how a listing continues: a page's Cursor field holds the
// cursor of the next page, which OLP sends back in the Parameter query
// parameter. A page without a cursor is the last.
type Pagination struct {
	// Parameter is the query parameter that carries the cursor, such as
	// page_token.
	Parameter string `json:"parameter"`
	// Cursor names the page's field that holds the next page's cursor, such
	// as next_page_token.
	Cursor string `json:"cursor"`
	// More, when declared, names the page's boolean field that reports
	// whether another page follows, such as has_more. A page whose More is
	// not true is then the last, whatever its cursor.
	More string `json:"more,omitempty"`
}

// A FailureRule classifies the upstream failures it matches. A failure is an
// unsuccessful response, with the error its body states, or an error stated
// in-band, such as a stream's error event. A rule matches a failure when every
// value it declares matches exactly, and it declares at least one.
type FailureRule struct {
	// Status matches an unsuccessful response's HTTP status, from 400 to 599.
	// A rule with a status never matches an in-band error.
	Status int `json:"status,omitempty"`
	// Code matches the code of the error the failure states.
	Code string `json:"code,omitempty"`
	// Type matches the type of the error the failure states.
	Type string `json:"type,omitempty"`
	// Class is the failure class of the failures the rule matches: one of
	// the Class constants.
	Class string `json:"class"`
}

// Failure classes a FailureRule declares. They govern whether a request fails
// over to another attempt and what cools down.
const (
	// ClassCredential: the upstream refused the credential. The credential
	// version cools down and the request fails over.
	ClassCredential = "credential"
	// ClassRateLimited: the upstream is limiting the credential, such as an
	// exhausted quota. Its slot cools down, for the upstream's Retry-After
	// when it sends one, and the request fails over.
	ClassRateLimited = "rate_limited"
	// ClassRetryable: another attempt may succeed. The request fails over
	// unless the upstream may have performed work that must not be repeated.
	ClassRetryable = "retryable"
	// ClassTerminal: the request itself was refused. It does not fail over,
	// and the caller receives the upstream's rejection.
	ClassTerminal = "terminal"
)

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
