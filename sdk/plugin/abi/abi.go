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
	// MethodGrantStart takes a GrantStart and returns a GrantAuthorization.
	// It begins grant enrollment for a profile that authenticates with a
	// grant, and may use CapabilityHTTP.
	MethodGrantStart = "grant_start"
	// MethodGrantExchange takes a GrantExchange and returns the Grant that
	// what the operator pasted back is exchanged for. It may use
	// CapabilityHTTP.
	MethodGrantExchange = "grant_exchange"
)

// Capabilities a plugin calls on OLP. OLP grants each call only the
// capabilities it lists; every call may log.
const (
	// CapabilityLog takes a LogRecord and returns no result.
	CapabilityLog = "log"
	// CapabilityHTTP takes an HTTPRequest and returns its HTTPResponse. OLP
	// grants it to grant enrollment steps, and sends the request only to the
	// plugin's approved origins, over the provider's network path.
	CapabilityHTTP = "http"
)

// Error codes shared by plugins and OLP. A plugin may report codes of its own.
const (
	CodeInvalidRequest = "invalid_request"
	CodeUnknownMethod  = "unknown_method"
	CodeInternal       = "internal"
	// CodeStateMismatch: what the operator pasted back belongs to another
	// authorization request than the grant enrollment's own.
	CodeStateMismatch = "state_mismatch"
	// CodeOriginNotApproved: an HTTP request is not to one of the plugin's
	// approved origins.
	CodeOriginNotApproved = "origin_not_approved"
	// CodeHTTPFailed: OLP could not complete an HTTP request, such as one
	// the egress policy refuses or one that timed out.
	CodeHTTPFailed = "http_failed"
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
// Providers using it authenticate with a static credential or, when it
// declares Grant, with a grant; its hosting adaptation places either.
type Profile struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// Dialect names the built-in dialect the profile serves, such as
	// openai-chat. A plugin never defines a dialect.
	Dialect string `json:"dialect"`
	// Hosting places the dialect's requests at the upstream.
	Hosting Hosting `json:"hosting"`
	// Grant, when set, makes providers using the profile authenticate with a
	// grant that the plugin's grant enrollment obtains, instead of a static
	// credential.
	Grant *GrantAuthentication `json:"grant,omitempty"`
}

// GrantAuthentication declares that a profile authenticates with a grant:
// rotating upstream authorization that OLP holds beneath a credential
// version. In the profile's hosting templates, {credential} then stands for
// the grant's current access token.
type GrantAuthentication struct {
	// Facts names the grant facts the plugin reports for every grant it
	// enrolls: non-secret values, such as the upstream account, that the
	// profile's hosting templates use as {grant.<name>}.
	Facts []string `json:"facts,omitempty"`
}

// Hosting is a profile's hosting adaptation: where and how the dialect's
// requests reach the upstream. It is a declaration OLP runs itself, so no
// plugin code runs per request.
//
// Header and query parameter values are templates. The placeholder
// {credential} stands for the provider's static credential, such as
// "Token {credential}", or a grant's current access token; a profile that
// authenticates with a grant may also use {grant.<name>} for its declared
// grant facts. Braces appear nowhere else.
type Hosting struct {
	// Address is the upstream's base URL, which the dialect's paths extend,
	// such as https://api.example.com/v1 for /chat/completions. Its origin is
	// one of the manifest's Origins.
	Address string `json:"address"`
	// Headers are the declared request headers, by name.
	Headers map[string]string `json:"headers,omitempty"`
	// Query holds the declared query parameters of the address, by name.
	Query map[string]string `json:"query,omitempty"`
}

// CredentialPlaceholder is the template placeholder for a provider's static
// credential or its grant's current access token.
const CredentialPlaceholder = "{credential}"

// GrantStart is the parameter of MethodGrantStart.
type GrantStart struct {
	// Profile is the ID of the profile a grant is enrolled for.
	Profile string `json:"profile"`
}

// GrantAuthorization is the result of MethodGrantStart: the authorization
// request the operator opens to sign in upstream.
type GrantAuthorization struct {
	// URL is the authorization request, at one of the plugin's origins, with
	// its state and PKCE challenge. After signing in, the operator pastes
	// back the loopback callback URL the upstream redirects to, or the code
	// it displays.
	URL string `json:"url"`
	// Session is the plugin's state for exchanging what is pasted back, such
	// as the PKCE verifier and the request's state: at most 16 KiB, which
	// OLP stores encrypted and hands to MethodGrantExchange.
	Session string `json:"session"`
}

// GrantExchange is the parameter of MethodGrantExchange.
type GrantExchange struct {
	Profile string `json:"profile"`
	// Session is the GrantAuthorization's session.
	Session string `json:"session"`
	// Input is what the operator pasted back: the whole callback URL, or the
	// code the upstream displayed.
	Input string `json:"input"`
}

// Grant is rotating upstream authorization a plugin obtained, which OLP holds
// beneath a new credential version.
type Grant struct {
	// AccessToken is what OLP's gateways authenticate requests with.
	AccessToken string `json:"access_token"`
	// RefreshToken, if the upstream issued one, renews the access token.
	// Gateways never receive it.
	RefreshToken string `json:"refresh_token,omitempty"`
	// ExpiresIn is how many seconds the access token lasts, or 0 when the
	// upstream did not say.
	ExpiresIn int64 `json:"expires_in,omitempty"`
	// Principal identifies the upstream account the grant authorizes, such as
	// its user ID: the observed principal.
	Principal string `json:"principal"`
	// Facts holds a value for each grant fact the profile declares.
	Facts map[string]string `json:"facts,omitempty"`
}

// HTTPRequest is the parameter of CapabilityHTTP. OLP sets the framing
// headers itself and follows no redirect.
type HTTPRequest struct {
	Method string              `json:"method"`
	URL    string              `json:"url"`
	Header map[string][]string `json:"header,omitempty"`
	// Body travels as base64 in JSON, like every []byte.
	Body []byte `json:"body,omitempty"`
}

// HTTPResponse is the result of CapabilityHTTP.
type HTTPResponse struct {
	Status int                 `json:"status"`
	Header map[string][]string `json:"header,omitempty"`
	Body   []byte              `json:"body,omitempty"`
}

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
