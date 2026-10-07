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
// An unconfined plugin is a native executable that exchanges the same
// messages over its standard input and output instead, wrapped in Frames, one
// JSON document per line. It first writes a Frame holding its ABI version.
// OLP then writes its calls, and cancellations of them, to the plugin's
// standard input; the plugin writes their responses, and its capability
// requests, to its standard output. Calls run concurrently, so every Frame
// carries the ID of the request it belongs to. A call whose result streams,
// such as MethodCarry, is answered with its parts before its response.
//
// The ABI is versioned as a whole and, during 0.x, carries no compatibility
// promise between versions: OLP refuses a plugin built for another version.
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
	// MethodSign takes a SignRequest and returns a SignResult. OLP calls it
	// once per upstream request of a profile that declares Signing, on behalf
	// of the request's provider (Request.Provider).
	MethodSign = "sign"
	// MethodGrantStart takes a GrantStart and returns a GrantAuthorization.
	// It begins grant enrollment for a provider whose profile authenticates
	// with a grant, on behalf of that provider (Request.Provider), and may use
	// CapabilityHTTP.
	MethodGrantStart = "grant_start"
	// MethodGrantExchange takes a GrantExchange and returns the Grant that
	// what the operator pasted back is exchanged for, on behalf of the
	// enrolling provider (Request.Provider). It may use CapabilityHTTP.
	MethodGrantExchange = "grant_exchange"
	// MethodGrantPoll takes a GrantPoll and returns the Grant a device
	// authorization obtained once the operator approved it, on behalf of the
	// enrolling provider (Request.Provider). Until then it fails with
	// CodeAuthorizationPending or CodeSlowDown, and with CodeAccessDenied or
	// CodeExpiredToken once the device authorization can't be approved. It
	// may use CapabilityHTTP.
	MethodGrantPoll = "grant_poll"
	// MethodGrantRefresh takes a GrantRefresh and returns the Grant it
	// refreshes to. OLP's workers call it ahead of the grant's access token's
	// expiry, and early when the upstream refused the access token, on behalf
	// of the provider the grant's credential version belongs to
	// (Request.Provider). It may use CapabilityHTTP.
	MethodGrantRefresh = "grant_refresh"
	// MethodCarry takes an HTTPRequest: one finished upstream request of a
	// profile that declares CarriesTraffic, which the plugin sends upstream
	// itself, on behalf of the request's provider (Request.Provider). Only an
	// unconfined plugin serves it, over stdio: it streams the upstream's
	// response as HTTPResponse parts of the call, the first holding its
	// Status and Header and each part its Body's next bytes, as they arrive.
	// The call's Response then ends the response, with no result, or reports
	// why the plugin could not carry it. CodeNotSent reports a request that
	// never reached the upstream.
	MethodCarry = "carry"
	// MethodRoutePredicate takes a RoutePredicate and returns a RouteVerdict.
	// OLP calls it while it plans a request on a route whose selector names
	// the plugin, and only on a confined plugin. The call carries the
	// request's features, never its content, and grants no capability but
	// log; a selector the plugin matches can only narrow the route's targets
	// or delegate to a route the key may use.
	MethodRoutePredicate = "route_predicate"
)

// Capabilities a plugin calls on OLP. OLP grants each call only the
// capabilities it lists; every call may log.
const (
	// CapabilityLog takes a LogRecord and returns no result.
	CapabilityLog = "log"
	// CapabilityHTTP takes an HTTPRequest and returns its HTTPResponse. OLP
	// grants it to grant enrollment steps and grant refresh, and sends the
	// request only to the plugin's approved origins, over the provider's
	// network path.
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
	// CodeInvalidGrant: the grant can no longer be refreshed, such as when
	// the upstream revoked its refresh token or let it expire. OLP stops
	// refreshing the grant. Other failures are retried only when OLP knows
	// the refresh token was not spent; an ambiguous outcome stays fenced and
	// lapses the grant after its attempt deadline.
	CodeInvalidGrant = "invalid_grant"
	// CodeNotSent: a plugin carrying a request did not send it upstream, such
	// as when it could not connect, so OLP may try the request elsewhere.
	// OLP treats any other failure to carry a request as an unknown upstream
	// outcome, which it never tries elsewhere.
	CodeNotSent = "not_sent"
)

// Codes a device authorization's poll reports while it obtains no grant: the
// token endpoint's error codes in the OAuth 2.0 device authorization grant
// (RFC 8628, section 3.5), which a plugin reports for its upstream's own
// variant too.
const (
	// CodeAuthorizationPending: the operator has not approved the device yet.
	CodeAuthorizationPending = "authorization_pending"
	// CodeSlowDown: the operator has not approved the device yet, and OLP
	// waits 5 seconds longer between this and every later poll.
	CodeSlowDown = "slow_down"
	// CodeAccessDenied: the operator denied the device authorization.
	CodeAccessDenied = "access_denied"
	// CodeExpiredToken: the device authorization expired before the operator
	// approved it.
	CodeExpiredToken = "expired_token"
)

// RoutePredicate is the request a route selector's plugin predicate judges:
// the features OLP computed during admission.
type RoutePredicate struct {
	Route        string `json:"route"`
	Selector     string `json:"selector"`
	Operation    string `json:"operation"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens,omitempty"`
	Streaming    bool   `json:"streaming"`
	Tools        bool   `json:"tools"`
	// Modalities are the kinds of input parts, from text, image, audio, video
	// and file.
	Modalities       []string `json:"modalities,omitempty"`
	StructuredOutput bool     `json:"structured_output"`
	ReasoningEffort  string   `json:"reasoning_effort,omitempty"`
}

// RouteVerdict answers a RoutePredicate.
type RouteVerdict struct {
	Match bool `json:"match"`
}

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
// Providers using it authenticate with a static credential or, when it
// declares Grant, with a grant, which its hosting adaptation places or its
// signing hook signs with.
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
	// Signing declares the profile's signing hook: OLP calls MethodSign once
	// per upstream request, after hosting placed it, and adds the headers the
	// plugin returns.
	Signing bool `json:"signing,omitempty"`
	// Grant, when set, makes providers using the profile authenticate with a
	// grant that the plugin's grant enrollment obtains, instead of a static
	// credential.
	Grant *GrantAuthentication `json:"grant,omitempty"`
	// CarriesTraffic declares that an unconfined plugin carries the profile's
	// upstream traffic instead of OLP's transport: OLP hands the plugin each
	// finished request, placed, authenticated and signed, with MethodCarry,
	// and reads the response and its stream back. The plugin then sees all
	// caller content, so the profile serves only transformed routes.
	CarriesTraffic bool `json:"carries_traffic,omitempty"`
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

// GrantAuthentication declares that a profile authenticates with a grant:
// rotating upstream authorization that OLP holds beneath a credential
// version. In the profile's hosting templates, {credential} then stands for
// the grant's current access token.
type GrantAuthentication struct {
	// Facts names the grant facts the plugin reports for every grant it
	// enrolls: non-secret values, such as the upstream account, that the
	// profile's header and query parameter templates use as {grant.<name>},
	// or the upstream's base URL, with which the address may begin.
	Facts []string `json:"facts,omitempty"`
	// Input, when GrantInputSecret, declares that the operator continues
	// enrollment with a secret the upstream issued, such as an API key, rather
	// than a callback URL or code. OLP masks the value as it is pasted. The
	// profile's StartGrant returns the URL of the page that issues the secret,
	// never a device authorization. Empty means a callback URL or code.
	Input string `json:"input,omitempty"`
}

// GrantInputSecret is the GrantAuthentication input of a profile whose
// enrollment continues with a pasted upstream secret.
const GrantInputSecret = "secret"

// Hosting is a profile's hosting adaptation: where and how the dialect's
// requests reach the upstream, how the upstream lists its models and how its
// failures are classified. It is a declaration OLP runs itself; the only
// plugin code that runs per request is a signing hook or, in an unconfined
// plugin that carries the traffic, its carrier.
//
// The address and the header and query parameter values are templates.
// Placeholders stand for a provider's values: {credential} for its static
// credential, such as "Token {credential}", or its grant's current access
// token, and {options.<name>} for one of the profile's required options. A
// profile that authenticates with a grant may also use {grant.<name>} for one
// of its declared grant facts in header and query parameter values, and to
// begin the address. Braces appear nowhere else.
//
// A profile with an envelope or rewrites changes the dialect's bodies, and one
// that forces streaming changes how non-streaming requests reach the upstream,
// so either serves only transformed routes.
type Hosting struct {
	// Address is the upstream's base URL, which the dialect's paths extend,
	// such as https://api.example.com/v1 for /chat/completions. Its origin is
	// one of the manifest's Origins. Options may appear in its path, such as
	// https://api.example.com/accounts/{options.account}/v1; the credential
	// never does. A profile that authenticates with a grant may instead begin
	// it with the grant fact that holds the upstream's base URL, such as
	// {grant.api_base}/v1: each grant's value is then an http or https URL
	// without credentials, query or fragment, at one of the manifest's
	// Origins, written the same way, or its credential version can't serve.
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
	// Discovery declares how the upstream lists its models. Without it,
	// operators declare the models of a provider using the profile.
	Discovery *Discovery `json:"discovery,omitempty"`
	// Classification declares the failure class of upstream failures, in
	// order: the first rule that matches a failure decides its class. OLP's
	// built-in rules classify every failure no rule matches.
	Classification []FailureRule `json:"classification,omitempty"`
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
	// the upstream model the request is for, and {options.<name>} for the
	// provider's value of one of the profile's required options. The
	// credential never goes in a body.
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
// credential or its grant's current access token.
const CredentialPlaceholder = "{credential}"

// ModelPlaceholder is the template placeholder for the upstream model a
// request is for.
const ModelPlaceholder = "{model}"

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
	// ClassContentFilter: the upstream's safety system refused the request's
	// content. It is terminal within the route, does not count against the
	// provider's health, and may start a route's content_filter fallback.
	ClassContentFilter = "content_filter"
)

// SignRequest is the parameter of MethodSign: one upstream request, placed by
// the profile's hosting adaptation, whose body is final.
type SignRequest struct {
	// Profile is the ID of the profile whose request this is.
	Profile string `json:"profile"`
	Method  string `json:"method"`
	// URL is the request's absolute URL, including its query.
	URL string `json:"url"`
	// Header holds the request's headers by canonical name.
	Header map[string][]string `json:"header"`
	// Body is the request body, base64-encoded in JSON, or empty.
	Body []byte `json:"body,omitempty"`
	// Credential is the provider's static credential, or its grant's current
	// access token.
	Credential string `json:"credential"`
}

// SignResult is the result of MethodSign.
type SignResult struct {
	// Headers are the headers to add to the request, by name. Each must be
	// one the request lacks and a profile could declare. OLP redacts their
	// values wherever it records upstream text.
	Headers map[string]string `json:"headers"`
}

// GrantStart is the parameter of MethodGrantStart.
type GrantStart struct {
	// Profile is the ID of the profile a grant is enrolled for.
	Profile string `json:"profile"`
}

// GrantAuthorization is the result of MethodGrantStart: how the operator
// authorizes the upstream account, with either an authorization request to
// open or a device authorization to approve.
type GrantAuthorization struct {
	// URL is the authorization request, at one of the plugin's origins, with
	// its state and PKCE challenge. After signing in, the operator pastes
	// back the loopback callback URL the upstream redirects to, or the code
	// it displays, which MethodGrantExchange exchanges.
	URL string `json:"url,omitempty"`
	// Device, instead of URL, is a device authorization the operator
	// approves upstream while OLP polls it with MethodGrantPoll.
	Device *DeviceAuthorization `json:"device,omitempty"`
	// Session is the plugin's state for the next step, such as the PKCE
	// verifier and the request's state, or the device code: at most 16 KiB,
	// which OLP stores encrypted and hands to MethodGrantExchange once, or to
	// each MethodGrantPoll.
	Session string `json:"session"`
}

// DeviceAuthorization is a device authorization the operator approves
// upstream, as in the OAuth 2.0 device authorization grant (RFC 8628) or an
// upstream's own variant: the operator opens the verification URL, on any
// device, enters the user code and approves.
type DeviceAuthorization struct {
	// VerificationURL is where the operator enters the user code, at one of
	// the plugin's origins.
	VerificationURL string `json:"verification_url"`
	// UserCode is the code the operator enters: 1–64 bytes of text without
	// control characters.
	UserCode string `json:"user_code"`
	// ExpiresIn is how many seconds the user code lasts. OLP polls for at
	// most 30 minutes.
	ExpiresIn int64 `json:"expires_in"`
	// Interval is how many seconds OLP waits between polls, at most 300,
	// or 5 when 0.
	Interval int64 `json:"interval,omitempty"`
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

// GrantPoll is the parameter of MethodGrantPoll.
type GrantPoll struct {
	Profile string `json:"profile"`
	// Session is the GrantAuthorization's session.
	Session string `json:"session"`
}

// Grant is rotating upstream authorization a plugin obtained, which OLP holds
// beneath a new credential version, or what a refresh renewed it to.
type Grant struct {
	// AccessToken is what OLP's gateways authenticate requests with.
	AccessToken string `json:"access_token"`
	// RefreshToken, if the upstream issued one, renews the access token.
	// Gateways never receive it. A refresh leaves it empty to keep the
	// grant's current refresh token.
	RefreshToken string `json:"refresh_token,omitempty"`
	// ExpiresIn is how many seconds the access token lasts, or 0 when the
	// upstream did not say.
	ExpiresIn int64 `json:"expires_in,omitempty"`
	// Principal identifies the upstream account the grant authorizes, such as
	// its user ID: the observed principal. A refresh that does not observe
	// the principal leaves it empty.
	Principal string `json:"principal"`
	// Facts holds a value for each grant fact the profile declares. A
	// refresh may leave them out: the grant keeps the facts its enrollment
	// reported.
	Facts map[string]string `json:"facts,omitempty"`
}

// GrantRefresh is the parameter of MethodGrantRefresh.
type GrantRefresh struct {
	// Profile is the ID of the profile the grant was enrolled for.
	Profile string `json:"profile"`
	// RefreshToken is the grant's current refresh token.
	RefreshToken string `json:"refresh_token"`
	// Facts are the grant facts the grant's enrollment reported.
	Facts map[string]string `json:"facts,omitempty"`
}

// HTTPRequest is the parameter of CapabilityHTTP, for which OLP sets the
// framing headers itself and follows no redirect, and of MethodCarry.
type HTTPRequest struct {
	Method string              `json:"method"`
	URL    string              `json:"url"`
	Header map[string][]string `json:"header,omitempty"`
	// Body travels as base64 in JSON, like every []byte.
	Body []byte `json:"body,omitempty"`
}

// HTTPResponse is the result of CapabilityHTTP, and each part of the result of
// MethodCarry, of which only the first has a Status and Header.
type HTTPResponse struct {
	Status int                 `json:"status,omitempty"`
	Header map[string][]string `json:"header,omitempty"`
	Body   []byte              `json:"body,omitempty"`
}

// Frame is one message of an unconfined plugin's standard input or output,
// written as one line of JSON. A Frame the plugin writes is at most 1 MiB.
type Frame struct {
	// Version is set in the first Frame the plugin writes, and only there:
	// the ABI version the plugin was built for.
	Version int `json:"abi_version,omitempty"`
	// ID identifies a request: OLP numbers its calls, and the plugin its
	// capability requests, each from 1. A response carries the ID of the
	// request it answers, and a cancellation the ID of the call it cancels.
	ID uint64 `json:"id,omitempty"`
	// Call, on a capability request, is the ID of the call the request
	// serves, which grants its capabilities. A request that serves no call
	// may only log.
	Call     uint64    `json:"call,omitempty"`
	Request  *Request  `json:"request,omitempty"`
	Response *Response `json:"response,omitempty"`
	// Part is one part of the result of the call ID, for a method whose
	// result streams, such as MethodCarry. The plugin writes a call's parts
	// in order, then the call's Response.
	Part json.RawMessage `json:"part,omitempty"`
	// Cancel tells the plugin that OLP no longer waits for the response to
	// call ID, such as when the request the call serves ended. The plugin
	// still answers the call, promptly.
	Cancel bool `json:"cancel,omitempty"`
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
