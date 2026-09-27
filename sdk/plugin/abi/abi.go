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
	// Provider is the provider a call from OLP serves, for a method OLP calls
	// on behalf of one. Calls about the plugin itself, such as manifest, and
	// capability requests carry none.
	Provider *Provider `json:"provider,omitempty"`
}

// Provider is the provider a call serves: the profile it uses and the values
// its operator set for the profile's options.
type Provider struct {
	// Profile is the ID of the profile the provider uses.
	Profile string `json:"profile"`
	// Options holds the provider's option values by option name. An optional
	// option the operator left unset is absent.
	Options map[string]string `json:"options"`
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
	// Options are the non-secret settings each provider using the profile
	// sets, such as an account ID, a region or a project, in the order the
	// provider wizard shows them.
	Options []Option `json:"options,omitempty"`
	// Hosting places the dialect's requests at the upstream.
	Hosting Hosting `json:"hosting"`
}

// Option is a non-secret setting a profile declares, whose value the operator
// sets on each provider using the profile. Hosting templates reference a
// required option as {options.<name>}, and every call OLP makes on behalf of
// a provider carries its values (Request.Provider). Values are text of 1–256
// characters without control characters.
type Option struct {
	// Name identifies the option: 1–64 lowercase letters, digits and
	// underscores, starting with a letter.
	Name  string `json:"name"`
	Label string `json:"label"`
	// Description helps the operator choose a value.
	Description string `json:"description,omitempty"`
	// Optional options may be left unset. Templates can't reference them.
	Optional bool `json:"optional,omitempty"`
	// Enum lists the only values the option takes.
	Enum []string `json:"enum,omitempty"`
	// Pattern is a regular expression in RE2 syntax that values match, as
	// a JSON Schema pattern: it matches anywhere unless anchored with ^ and
	// $. An option declares an enum or a pattern, not both.
	Pattern string `json:"pattern,omitempty"`
}

// Hosting is a profile's hosting adaptation: where and how the dialect's
// requests reach the upstream. It is a declaration OLP runs itself, so no
// plugin code runs per request.
//
// The address and the header and query parameter values are templates.
// Placeholders stand for a provider's values: {credential} for its static
// credential, such as "Token {credential}", and {options.<name>} for one of
// the profile's required options. Braces appear nowhere else.
type Hosting struct {
	// Address is the upstream's base URL, which the dialect's paths extend,
	// such as https://api.example.com/v1 for /chat/completions. Its origin is
	// one of the manifest's Origins. Options may appear in its path, such as
	// https://api.example.com/accounts/{options.account}/v1; the credential
	// never does.
	Address string `json:"address"`
	// Headers are the declared request headers, by name.
	Headers map[string]string `json:"headers,omitempty"`
	// Query holds the declared query parameters of the address, by name.
	Query map[string]string `json:"query,omitempty"`
}

// CredentialPlaceholder is the template placeholder for a provider's static
// credential.
const CredentialPlaceholder = "{credential}"

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
