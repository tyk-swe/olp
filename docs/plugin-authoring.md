# Writing a provider plugin

A [provider plugin](plugins.md) supplies a provider profile's authentication and
hosting around one of OLP's built-in dialects. This guide covers writing one with
the Go SDK, `github.com/tyk-swe/olp/sdk/plugin`, and documents the ABI the SDK
implements. The reference plugin in
[`sdk/plugin/reference`](../sdk/plugin/reference/main.go) is a complete,
minimal plugin to start from.

Plugins build with the Go version in OLP's `go.mod`. During 0.x the plugin ABI
carries no compatibility promise
([ADR 0004](adr/0004-no-compatibility-promises-during-0x.md)): build a plugin with
the SDK from the OLP version that will run it, because OLP refuses a module built
for another ABI version at install.

## A minimal plugin

A plugin is a `main` package that registers its implementation from `init` and
has an empty `main`:

```go
package main

import "github.com/tyk-swe/olp/sdk/plugin"

type acme struct{}

func (acme) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Name:    "acme",
		Version: "1.0.0",
		Origins: []string{"https://api.acme.example"},
		Profiles: []plugin.Profile{{
			ID: "acme-chat", Label: "Acme Chat Completions", Dialect: "openai-chat",
			Hosting: plugin.Hosting{
				Address: "https://api.acme.example/v1",
				Headers: map[string]string{"Authorization": "Token {credential}"},
			},
		}},
	}
}

func init() { plugin.Register(acme{}) }

func main() {}
```

Build it as a WASI preview 1 reactor, which OLP initializes once per instance
and then calls:

```sh
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o acme.wasm .
```

A module built without `-buildmode=c-shared` is a WASI command, which OLP
refuses. Install the result from the console's Plugins page.

## The manifest

OLP calls `Manifest` once, at install, and stores the result with the module's
digest. It never changes for that digest, so bump `Version` and install a new
build to change it.

| Field | Rule |
| --- | --- |
| `Name` | 1–64 lowercase letters, digits and hyphens, starting and ending with a letter or digit. Builds sharing a name are digests of the same plugin. |
| `Version` | 1–64 letters, digits, `.`, `-`, `_` and `+`. |
| `Description` | Optional; at most 500 characters, no control characters. |
| `Origins` | At most 16 distinct `http` or `https` origins in canonical form: lowercase, no path, credentials, query or default port, such as `https://api.acme.example` or `http://127.0.0.1:8080`. These are the only origins the plugin may ever reach, once an owner approves them. |
| `Profiles` | 1–16 profiles, each with an `ID` unique in the plugin (same syntax as `Name`), a `Label` of 1–100 characters, the `Dialect` it serves, the `Options` its providers set, its `Hosting` adaptation, optionally `Signing` and, if providers using it authenticate with a grant, its `Grant`. |

A profile serves one of OLP's built-in dialects whose requests and events are
plain HTTP JSON and server-sent events: `openai-chat`, `openai-responses`,
`anthropic-messages` or `gemini-generate-content`. A plugin never defines a
dialect. OLP refuses a manifest that is invalid, names another dialect, or
carries fields it does not know, with the offending field in the problem it
returns.

### Options

A profile's `Options` declare up to 16 non-secret settings that each provider
using the profile sets, such as an account ID, a region or a project. The
console's provider wizard shows them in declared order, and OLP validates every
provider's values against them. Hosting templates place them, and every call
OLP makes to the plugin on behalf of a provider carries them (see
[Calls for a provider](#calls-for-a-provider)).

```go
Options: []plugin.Option{
	{Name: "account", Label: "Account", Description: "The Acme account that serves requests.", Pattern: "^[a-z0-9-]{1,40}$"},
	{Name: "region", Label: "Region", Enum: []string{"us", "eu"}},
},
```

| Field | Rule |
| --- | --- |
| `Name` | 1–64 lowercase letters, digits and underscores, starting with a letter; unique in the profile. Templates reference it as `{options.<name>}`. |
| `Label` | 1–100 characters, as the wizard labels the field. |
| `Description` | Optional; at most 500 characters. |
| `Optional` | A provider may leave an optional option unset, so templates can't reference it; plugin code sees it only when set. Every other option is required. |
| `Enum` | Optional; at most 64 distinct values, the only ones the option takes. |
| `Pattern` | Optional; a regular expression in RE2 syntax, at most 512 characters, that values match. As in JSON Schema it matches anywhere in the value, so anchor it with `^` and `$`. An option declares an enum or a pattern, not both. |

Every value is text of 1–256 characters without control characters. Options
are not secret: they appear in provider configuration, revision diffs and
configuration exports, and OLP does not redact them.

### Hosting adaptation

A profile's `Hosting` declares where and how the dialect's requests reach the
upstream, how the upstream lists its models and how its failures are
classified. OLP runs it for every request of a provider using the profile; the
only plugin code that runs per request is a [signing hook](#signing-hook).
Providers using the profile authenticate with a static credential, or with a
[grant](#grants), which the adaptation places or the signing hook signs with.

| Field | Rule |
| --- | --- |
| `Address` | The upstream's base URL, which the dialect's paths extend: `https://api.acme.example/v1` receives `/chat/completions`. An `http` or `https` URL at one of the manifest's `Origins`, written the same way, without credentials, query or fragment. Required options may appear in its path, such as `https://api.acme.example/accounts/{options.account}/v1`, never in its origin, and each value fills its part of one path segment. It becomes the endpoint of every provider using the profile, with the provider's options in place. A [grant](#grants) profile may instead begin it with the grant fact that holds the upstream's base URL, such as `{grant.api_base}/v1`. |
| `Headers` | At most 16 request headers by name. Hop-by-hop, framing, content negotiation, tracing and `X-OLP-` headers are OLP's, and the dialect's semantic headers, such as `Anthropic-Version` or `OpenAI-Beta`, are the provider's. |
| `Query` | At most 16 query parameters of the address by name: 1–128 letters, digits, `.`, `_`, `~` and `-`, other than the dialect's semantic query settings and addressing, such as Gemini's `alt`. |
| `Envelope` | Optional. The upstream's own JSON object around the dialect's bodies; see [Envelopes and rewrites](#envelopes-and-rewrites). |
| `Rewrites` | Optional. At most 16 declared changes to the dialect's request body; see [Envelopes and rewrites](#envelopes-and-rewrites). |
| `Discovery` | Optional: the upstream's [model listing](#model-discovery). |
| `Classification` | Optional: at most 32 [failure classification](#failure-classification) rules. |
| `ForceStreaming` | Optional. The upstream serves only streaming requests; see [Upstreams that serve only streams](#upstreams-that-serve-only-streams). |

Header and query values are templates of at most 2048 characters without
control characters. `{credential}` stands for the provider's static credential,
such as `Token {credential}`, or a grant's current access token,
`{options.<name>}` for the provider's value of a required option, such as
`{options.region}`, and `{grant.<name>}` for a grant fact the profile declares;
braces appear nowhere else. At least one value must place the credential,
unless the profile signs requests, and the address never does. OLP refuses a
credential it can't place in a header, such as one containing a line break,
before sending anything, and redacts every value that carries the credential
wherever it records upstream text.

A profile whose adaptation changes only authorization, address and declared
headers serves strict routes, whether or not it signs requests. An envelope or
any rewrite changes the dialect's bodies, forced streaming changes how
non-streaming requests reach the upstream, and a plugin that
[carries the traffic](#carrying-traffic) sees and may change all of it, so such
a profile serves only [transformed routes](provider-routing.md#route-fidelity),
and strict activation of a target using it tells the route author to declare
the route transformed.

### Signing hook

An upstream that authenticates each request by a signature or a timestamped
token, rather than by the key itself, needs code per request. Such a profile
sets `Signing: true`, and the plugin implements `plugin.Signer`, as the
reference plugin's `reference-signed-chat` profile does:

```go
func (acme) Sign(ctx context.Context, r plugin.SignRequest) (plugin.SignResult, error) {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(r.Credential))
	mac.Write([]byte(timestamp + "\n"))
	mac.Write(r.Body)
	return plugin.SignResult{Headers: map[string]string{
		"X-Acme-Timestamp": timestamp,
		"X-Acme-Signature": hex.EncodeToString(mac.Sum(nil)),
	}}, nil
}
```

OLP calls `Sign` once per upstream request of a signing profile, never per
stream event, once the hosting adaptation has placed the request and its body
is final. That holds for gateway traffic and for the probes and certification
control sends. The `SignRequest` carries the profile's ID, the method, the
absolute URL with its query, the headers, the body and the static credential,
or a grant's current access token.
The call is on behalf of the provider sending the request, so
[`plugin.ProviderOf(ctx)`](#calls-for-a-provider) returns its option values,
such as the account or region a signature covers.

`Sign` returns the headers to add: at most 16, each one a profile could declare
and the request doesn't already carry. OLP redacts their values wherever it
records upstream text, as it does the credential. If `Sign` fails, exceeds the
plugin limits or returns a header OLP refuses, the request is not sent: the
attempt fails as a credential failure before reaching the upstream, and the
route fails over. A plugin that declares a signing profile without
implementing `Signer` reports no manifest, so OLP refuses to install it.

Signing adds plugin code to every request. It runs in about a millisecond for a
small body, and its cost grows with the body, which crosses the ABI as JSON, so
keep `Sign` to the signature itself.

### Envelopes and rewrites

Some upstreams speak a built-in dialect inside a JSON object of their own, or
need a few request members set or removed. The reference plugin's
`reference-gemini` profile declares both:

```go
Hosting: plugin.Hosting{
	Address: "https://api.example.com/enveloped/v1beta",
	Headers: map[string]string{"Authorization": "Token {credential}"},
	Envelope: &plugin.Envelope{
		Request:  "request",
		Fields:   map[string]string{"model": "{model}"},
		Response: "response",
	},
	Rewrites: []plugin.Rewrite{
		{Op: plugin.RewriteSet, Path: "/generationConfig/candidateCount", Value: json.RawMessage(`1`)},
		{Op: plugin.RewriteDefault, Path: "/systemInstruction", Value: json.RawMessage(`{"parts":[{"text":"You are the reference assistant."}]}`)},
		{Op: plugin.RewriteDelete, Path: "/generationConfig/seed"},
	},
},
```

OLP runs both around its built-in codecs, so codecs, content policy and usage
accounting see plain dialect bodies:

1. OLP prepares the dialect's request, then applies the rewrites in order. The
   route inspector's effective request shows the result.
2. The envelope wraps what OLP sends: a Gemini request travels as
   `{"model": "…", "request": {"contents": […], …}}`.
3. OLP unwraps each successful response and each server-sent event: from
   `{"response": {…}, "traceId": "…"}` it reads the `response` member. A
   response or event without the member, such as an upstream error or
   `[DONE]`, reaches the dialect as it is; error responses are read as they
   are.

| Field | Rule |
| --- | --- |
| `Envelope.Request` | The member of the upstream's request object that carries the dialect's request body. Without it, OLP sends the body as it is. |
| `Envelope.Fields` | Only with `Request`: at most 16 other members of the request object, by name, other than `Request`. Values are templates, as for headers, that become JSON strings; `{model}` stands for the upstream model the request is for, `{options.<name>}` for the provider's value of a required option, and the credential never goes in a body. |
| `Envelope.Response` | The member of each successful response and stream event that carries the dialect's response or event. Without it, OLP reads responses as they are. |
| `Rewrites[].Op` | `set` replaces the member, `default` sets it unless the request has it, even as `null`, and `delete` removes it if present. |
| `Rewrites[].Path` | A JSON pointer to an object member, 1–8 names deep, such as `/generationConfig/seed`. Each member is rewritten at most once, and nothing inside a member another rewrite changes. The model and delivery members OLP binds, `/model`, `/stream` and Chat Completions' `/stream_options`, can't be rewritten. |
| `Rewrites[].Value` | The JSON value to set or default to, which may be `null`. `delete` takes none. |

Member names in envelopes and paths are 1–128 letters, digits, `_`, `-` and
`.`. Setting or defaulting a member creates the objects above it; if one of
them is present but not an object, OLP refuses the request with an
`unsupported_parameter` error on `provider_profile` instead of sending it.

### Model discovery

With `Discovery`, operators discover a provider's models as they do for
built-in kinds; without it, they declare each model, and OLP certifies it.
OLP sends `GET` to the listing with the profile's headers and query parameters
placed as for any request, and reads a JSON object:

| Field | Rule |
| --- | --- |
| `Path` | The listing's path, which extends the address: `/models` under `https://api.acme.example/v1`. At most 512 characters of path segments of letters, digits and URL punctuation, without dot segments, query, fragment or placeholders. |
| `Models` | The listing's top-level field holding the array of model objects, such as `data`. |
| `ID` | The field of each model object holding its ID, such as `id`. OLP removes a `models/` prefix and skips objects without a valid string ID. |
| `Pagination` | Optional. `Parameter` is the query parameter that carries a cursor, such as `page_token`, other than one `Query` places. `Cursor` is the page's field holding the next page's cursor, and `More`, if declared, its boolean field reporting whether another page follows. |

Field names are top-level names of 1–128 characters without control
characters. A page without a cursor is the last; with `More`, so is a page whose
`More` is not `true`, and one whose `More` is `true` must carry a cursor. OLP
lists at most 2000 models and refuses a listing that repeats a cursor.

### Failure classification

`Classification` decides the class of the upstream failures its rules match,
which governs [failover and cooldown](provider-routing.md). A failure is an
unsuccessful response with the error its body states, or an error the upstream
states in-band, such as a stream's error event. The stated error is the
dialect's error object, such as `{"error": {"type": "…", "code": "…"}}`.

A `FailureRule` matches a failure when every value it declares matches exactly,
and it declares at least one: `Status`, an HTTP status from 400 to 599; `Code`
and `Type`, the stated error's code and type, of at most 256 characters. A rule
with a status never matches an in-band error. The first matching rule decides
the `Class`:

| Class | Effect |
| --- | --- |
| `credential` | The upstream refused the credential: the credential version cools down and the request fails over. |
| `rate_limited` | The upstream is limiting the credential, such as an exhausted quota: the slot cools down, for the upstream's `Retry-After` when it sends one, and the request fails over. |
| `retryable` | Another attempt may succeed: the request fails over, unless the upstream may have performed work a strict route must not repeat. |
| `terminal` | The request itself was refused: it does not fail over, and the caller receives the upstream's rejection. |

OLP's built-in rules classify every failure no rule matches: among others, 401
and 403 are credential failures, 429 a rate limit, 5xx retryable and other 4xx
statuses terminal. For example, an upstream that reports an exhausted quota as
a `400` with its own code declares:

```go
Classification: []plugin.FailureRule{{Status: 400, Code: "quota_exhausted", Class: abi.ClassRateLimited}},
```

### Upstreams that serve only streams

Some upstreams, such as subscription backends, accept only streaming requests.
A profile declares one with `ForceStreaming`, as the reference plugin's
`reference-streaming` profile does:

```go
Hosting: plugin.Hosting{
	Address:        "https://api.example.com/streaming/v1",
	Headers:        map[string]string{"Authorization": "Token {credential}"},
	ForceStreaming: true,
},
```

OLP then sends every request of the profile as a streaming one, with
`"stream": true`, which the route inspector's effective request shows. A caller
that streams receives the upstream's events as usual. For a caller that did not
ask to stream, OLP reads the stream to its end and aggregates it into the
dialect's non-streaming result, which the caller, content policy and usage
accounting see as an ordinary response:

- The result is the response the terminal event carries. An upstream may leave
  that response's output empty and deliver each item only in
  `response.output_item.done`; OLP then fills the output with those items, in
  order.
- OLP validates the stream as it does for a streaming caller. A stream that
  fails or ends before its terminal event yields no partial result. The
  upstream had accepted the request, so its outcome is unknown and its cost
  uncertain, unless the stream reported usage, which is accounted as it would
  be for a streaming caller.
- Aggregation is bounded by the gateway's response size limit
  (`OLP_PROVIDER_MAX_RESPONSE_BYTES`). A larger result fails the request with
  `502 upstream_response_too_large`, without failing over, and the caller can
  stream instead.

OLP aggregates the `openai-responses` dialect only, and refuses a manifest that
forces streaming in another dialect with the field
`manifest.profiles[i].hosting.force_streaming`.

## Grants

Some upstreams authorize an account, often with a refresh token that rotates,
rather than issue an API key. A profile that declares `Grant` authenticates
providers with a grant: upstream authorization the plugin obtains when an
operator signs in, which OLP holds beneath a credential version and places
with the profile's hosting adaptation.

```go
plugin.Profile{
	ID: "acme-account", Label: "Acme with sign-in", Dialect: "openai-chat",
	Grant: &plugin.GrantAuthentication{Facts: []string{"account"}},
	Hosting: plugin.Hosting{
		Address: "https://api.acme.example/v1",
		Headers: map[string]string{"Authorization": "Bearer {credential}", "Acme-Account": "{grant.account}"},
	},
}
```

`Facts` names up to 16 grant facts: non-secret values the plugin reports with
every grant, such as the upstream account, that header and query parameter
templates use as `{grant.<name>}`. Each is 1–64 lowercase letters, digits and
underscores, starting with a letter. A template may use only the facts its
profile declares, and OLP refuses to install a plugin whose template names
another. Envelope fields, which belong to the provider rather than to a
credential version, use none.

An upstream that names each account's API in its token response, such as a
regional deployment, puts that base URL in a fact with which the address
begins:

```go
Grant:   &plugin.GrantAuthentication{Facts: []string{"account", "api_base"}},
Hosting: plugin.Hosting{Address: "{grant.api_base}/v1", Headers: headers},
```

Each credential version's requests then go to its own grant's base URL, with
the rest of the address and the dialect's paths after it. The fact's value must
be an `http` or `https` URL without credentials, query or fragment, at one of
the manifest's `Origins`, written the same way, such as
`https://eu.api.acme.example/accounts/7`; normalize what the upstream returns
before reporting it. OLP checks it on every request, under the provider's
network path and the egress policy as always, and sends nothing for a grant
whose base URL is elsewhere: probing or certifying the provider reports why,
and the gateway treats the attempt as a credential failure. Only one fact may
appear in the address, and only at its start.

A plugin with such a profile implements `plugin.GrantEnroller`; one that
declares a grant profile without it reports no manifest, so OLP refuses to
install it. OLP runs its steps in control when an operator enrolls a grant from
the provider wizard, or re-enrolls a slot's grant from the credential pool, on
behalf of the enrolling provider, so
[`plugin.ProviderOf(ctx)`](#calls-for-a-provider) returns its option values,
such as the tenant whose authority the operator signs in to:

```go
func (acme) StartGrant(ctx context.Context, start plugin.GrantStart) (plugin.GrantAuthorization, error)
func (acme) ExchangeGrant(ctx context.Context, exchange plugin.GrantExchange) (plugin.Grant, error)
```

- `StartGrant` builds the authorization request the operator opens: its `URL`,
  at one of the plugin's origins, with a fresh `state` and PKCE challenge, and a
  `Session` of at most 16 KiB holding what `ExchangeGrant` needs, such as the
  state and the PKCE verifier. OLP stores the session encrypted and hands it
  back once.
- `ExchangeGrant` receives the session and what the operator pasted back: the
  whole callback URL the upstream redirected the browser to, typically a
  loopback address where nothing listens, or the code the upstream displayed.
  It checks the state, reporting a mismatch as an `*plugin.Error` with code
  `abi.CodeStateMismatch`, exchanges the code with `plugin.Fetch`, and returns
  the `Grant`: its `AccessToken` (at most 16 KiB, sendable in a header), any
  `RefreshToken`, `ExpiresIn` seconds, the observed `Principal` (the upstream
  account, 1–256 characters) and a value for every declared fact.

The principal is part of the provider's serving identity, so report a stable
identifier of the account, the same at every sign-in: re-enrolling the same
account is then a credential rotation, and another account a serving identity
change ([grant enrollment](plugins.md#re-enrolling-and-the-observed-principal)).

Report a failure the operator should see, such as the upstream refusing the
code, as an `*plugin.Error` with a code of your own; any other error is reported
as `internal`. OLP redacts the session and the pasted value from what the step
logs; keep tokens the plugin receives out of its log.

A plugin whose grants carry a refresh token also implements
`plugin.GrantRefresher`. OLP's workers call it a quarter of the access token's
lifetime before it expires (at most ten minutes before), and at once when the
upstream refuses the access token, on behalf of the provider the grant belongs
to:

```go
func (acme) RefreshGrant(ctx context.Context, refresh plugin.GrantRefresh) (plugin.Grant, error)
```

`RefreshGrant` receives the grant's current `RefreshToken` and the `Facts` its
enrollment reported, exchanges the refresh token with `plugin.Fetch`, and
returns the new `AccessToken` and `ExpiresIn`, with the `RefreshToken` that
replaces the spent one if the upstream rotates it; an empty one keeps the
current token. It may report the `Principal` and `Facts` it observes, and OLP
checks they are the grant's. OLP calls it for one grant at a time, whatever the
number of workers, so a rotating refresh token is spent once.

Report a grant the upstream will no longer refresh, such as one whose refresh
token was revoked or expired, as an `*plugin.Error` with code
`abi.CodeInvalidGrant` (`invalid_grant`): OLP stops refreshing the grant, as it
does when the refresh authorizes another principal or other facts. Any other
failure is transient, and OLP retries it with backoff. OLP redacts the refresh
token from what the refresh logs.

The reference plugin's `reference-grant-chat` profile implements this flow
against a fictional authority with the authorization code flow and PKCE, and
refreshes its grants with rotating refresh tokens. It sends each account's
requests to the API base URL its token response names.

### Device authorization

An upstream that authorizes a device instead, with the OAuth 2.0 device
authorization grant (RFC 8628) or its own variant, has `StartGrant` return a
`Device` rather than a `URL`, and the plugin implements `plugin.GrantPoller`
too:

```go
func (acme) PollGrant(ctx context.Context, poll plugin.GrantPoll) (plugin.Grant, error)
```

- `StartGrant` requests the device authorization upstream and returns its
  `VerificationURL`, at one of the plugin's origins, the `UserCode` the operator
  enters there (1–64 bytes), how many seconds the code lasts in
  `ExpiresIn`, and the polling `Interval` in seconds, at most 300 (0 means 5).
  The `Session`, such as the device code, is handed to every poll. OLP polls
  for as long as the user code lasts, at most 30 minutes.
- While the operator approves the device upstream, OLP calls `PollGrant`,
  waiting the interval between polls. It returns the `Grant` once the operator
  approved, like `ExchangeGrant` does, and until then fails with a
  `*plugin.Error` whose code is RFC 8628's: `abi.CodeAuthorizationPending`, or
  `abi.CodeSlowDown` to have OLP wait 5 seconds longer from then on; and
  `abi.CodeAccessDenied` or `abi.CodeExpiredToken` once the operator denied the
  device or it expired, which ends the enrollment. Any other failure ends it
  too.

OLP never calls `ExchangeGrant` for a device authorization, so a plugin whose
upstream only authorizes devices may have it report an error.

An upstream's own variant maps onto the same steps. For one that answers
polls with an HTTP 403 until the operator approves, and then with an
authorization code to exchange, `PollGrant` reports the 403 as
`abi.CodeAuthorizationPending`, and after the approval exchanges the code with
`plugin.Fetch` within the same poll. The reference plugin's
`reference-device-chat` profile implements RFC 8628 against its fictional
authority, and places and refreshes its grants as `reference-grant-chat` does.

### Fetch

`plugin.Fetch` sends an HTTP request through OLP, the only way a plugin reaches
the network. OLP grants it to grant enrollment steps and grant refresh, so pass
it the call's context: `plugin.Fetch(ctx, request)`. OLP sends a request only
to one of the plugin's approved origins, over the provider's network path and
egress policy. It sets the framing headers itself; a plugin may not set `Host`,
`Content-Length`, hop-by-hop or `Proxy-` headers. It returns a redirect rather
than following it, and any response the upstream sent, whatever its status.
Request and response bodies are at most 512 KiB. A request OLP refuses or can't
complete fails with an `*plugin.Error`: `origin_not_approved`, `http_failed` or
`invalid_request`.

## What a plugin can reach

OLP runs a plugin confined, within 64 MiB of memory and 10 seconds per call. It
grants only:

- a clock: `time.Now` reads the host's wall and monotonic clocks;
- randomness: `crypto/rand` reads the host's cryptographic source;
- logging, through `plugin.Log`;
- to grant enrollment steps and grant refresh, HTTP to the plugin's approved
  origins, through `plugin.Fetch`.

There is no filesystem, other network access, environment or argument list.
Sleeping spends the call's time limit, including the time `Fetch` waits, so
avoid it.

OLP keeps up to four instances of a module and serves each call on an idle one,
so package-level state may survive from one call to the next. Never rely on it:
OLP discards an instance whenever a call on it fails, and starts new ones as it
needs them.

## Logging

`plugin.Log` is a `*slog.Logger` whose records go to OLP's log. Attribute values
travel as text, and groups flatten into dotted keys. Anything written to
standard output or standard error is logged a line at a time as well, including
the Go runtime's panic output.

OLP redacts every secret value it handed the call, and bounds a call's output to
16 KiB, and each message or attribute to 2 KiB, so a plugin can log freely
without leaking what OLP gave it or flooding the log.

## Testing

The SDK builds natively too, so ordinary Go tests can call a plugin's methods
directly; outside OLP, `plugin.Log` discards its records and `plugin.Fetch`
fails as unavailable. Build the module for `wasip1` in your tests to check that
it compiles as a reactor.

## Unconfined plugins

A deployment that enables the experimental
[unconfined tier](plugins.md#unconfined-plugins-experimental) can run a plugin
as a native executable in its image instead, with the operating system
privileges of OLP's processes, once an owner permits it. The same source builds
either way: call `plugin.Serve` from `main`, which a WASI reactor never runs.

```go
func main() { plugin.Serve() }
```

Build it as a static executable for the deployment's image, which has no C
library of its own to link against:

```sh
CGO_ENABLED=0 GOOS=linux go build -o acme .
```

`Serve` serves OLP's calls over standard input and output until OLP closes
standard input. OLP starts the executable with no arguments and an empty
environment, and one process serves all its calls from one OLP process. So:

- calls run concurrently, each on a goroutine of its own, so package-level
  state needs synchronization;
- a method's context is cancelled when OLP stops waiting for its call, such as
  when the request it signs ends. Return promptly: a call left unanswered for
  10 seconds, cancelled or not, makes OLP stop the process, failing every call
  in flight, and start it again for the next call;
- standard output carries the ABI, so `Serve` points `os.Stdout` at standard
  error, which OLP logs a line at a time;
- log with the call's context, such as `plugin.Log.InfoContext(ctx, …)`, so OLP
  attributes the record to its call and redacts the call's secret values. OLP
  redacts a record without one, like standard error, of the secret values of
  every call in flight.

Nothing confines the plugin, so it reaches the network and files itself. OLP's
capabilities remain available to it and behave as they do for a confined
plugin, granted by call, which is why `plugin.Fetch` takes the call's context.

### Carrying traffic

An unconfined plugin may carry its profiles' upstream traffic itself, such as
through an upstream's own client or a transport OLP doesn't speak. A profile
declares it with `CarriesTraffic`, and the plugin implements `plugin.Carrier`:

```go
Profiles: []plugin.Profile{{
	ID: "acme-chat", Label: "Acme Chat", Dialect: "openai-chat", CarriesTraffic: true,
	Hosting: plugin.Hosting{Address: "https://api.acme.example/v1", Headers: map[string]string{"Authorization": "Bearer {credential}"}},
}},

func (acme) Carry(ctx context.Context, r plugin.HTTPRequest) (plugin.CarriedResponse, error) {
	req, err := http.NewRequestWithContext(ctx, r.Method, r.URL, bytes.NewReader(r.Body))
	if err != nil {
		return plugin.CarriedResponse{}, &plugin.Error{Code: abi.CodeNotSent, Message: err.Error()}
	}
	req.Header = r.Header
	resp, err := http.DefaultClient.Do(req)
	if dial, ok := errors.AsType[*net.OpError](err); ok && dial.Op == "dial" {
		return plugin.CarriedResponse{}, &plugin.Error{Code: abi.CodeNotSent, Message: err.Error()}
	}
	if err != nil {
		return plugin.CarriedResponse{}, err
	}
	return plugin.CarriedResponse{Status: resp.StatusCode, Header: resp.Header, Body: resp.Body}, nil
}
```

OLP places, authenticates and signs each request of the profile as usual, then
hands the finished request to `Carry` instead of sending it: the gateway's
requests, and control's probes, model listings and certification alike. The
plugin sees all caller content, so the profile serves only transformed routes.
It carries HTTP requests and their SSE streams only, never WebSocket or realtime
traffic.

- The SDK streams the response's body to OLP as it reads it, so each stream
  event reaches the caller as the upstream sends it, and closes the body at the
  end.
- `ctx` ends when OLP stops waiting, such as when the caller goes away or an
  attempt times out, and the SDK then closes the body. Send the request with
  `ctx`, so the cancellation reaches the upstream, and return promptly.
- Report a request that never reached the upstream, such as one whose
  connection failed, as a `*plugin.Error` with code `abi.CodeNotSent`: OLP may
  then try the request on another target. OLP treats every other failure,
  including one reading the body or a server error the upstream answered with,
  as an unknown outcome, which it never tries elsewhere.

A confined plugin can't carry traffic: OLP refuses its manifest with the field
`manifest.profiles[i].carries_traffic`, and a plugin declaring a profile that
carries traffic without implementing `Carrier` reports no manifest.

## ABI reference

The ABI is defined in package
[`sdk/plugin/abi`](../sdk/plugin/abi/abi.go), which OLP and the SDK share. The
SDK implements all of it; this section is for authors of other SDKs. A confined
plugin exchanges its [messages](#messages) through a WebAssembly
[module](#module)'s exports, and an unconfined plugin through the
[stdio transport](#stdio-transport).

### Module

A plugin is a WebAssembly module targeting WASI preview 1 as a reactor. OLP
runs its `_initialize` export, if present, when it instantiates the module. It
refuses a module that exports `_start`.

The module exports:

| Export | Signature | Purpose |
| --- | --- | --- |
| `memory` | memory | Linear memory, where every message is exchanged. |
| `olp_abi_version` | `() -> i32` | Returns the ABI version the module targets. This document describes version 1. |
| `olp_alloc` | `(size i32) -> i32` | Returns a buffer of `size` bytes for OLP to write a message into. The buffer belongs to the plugin again once OLP passes it to `olp_call` or returns it from `host_call`. |
| `olp_call` | `(ptr i32, len i32) -> i64` | Serves one request and returns its response, packed. The response must stay valid until the next export call. |

It may import:

| Import | Signature | Purpose |
| --- | --- | --- |
| `wasi_snapshot_preview1.*` | WASI | Clock, randomness, and standard output and error, which OLP logs. There are no files, arguments or environment variables. |
| `olp.host_call` | `(ptr i32, len i32) -> i64` | Uses one OLP capability and returns its response, packed, in a buffer OLP obtained from `olp_alloc`. |

A module importing any other `olp` function is refused as built for another ABI
version, and one importing anything else is refused as invalid. Packed results
carry the buffer's pointer in the high 32 bits and its length in the low 32.

### Messages

Every message is one JSON document. What a plugin returns or sends OLP is at
most 1 MiB; OLP's requests are as large as they must be, such as a `sign`
request carrying the body it signs. A request names a method and carries its
parameters:

```json
{"method": "manifest", "params": null}
```

A request OLP makes on behalf of a provider also carries that provider: the
ID of the profile it uses and its option values, without the unset optional
ones. Requests about the plugin itself, such as `manifest`, carry none.

```json
{"method": "…", "params": {}, "provider": {"profile": "acme-chat", "options": {"account": "acme-prod", "region": "eu"}}}
```

A response carries either a result or an error:

```json
{"result": {"name": "acme", "version": "1.0.0", "origins": ["https://api.acme.example"], "profiles": [{"id": "acme-chat", "label": "Acme Chat Completions", "dialect": "openai-chat", "hosting": {"address": "https://api.acme.example/v1", "headers": {"Authorization": "Token {credential}"}}}]}}
{"error": {"code": "unknown_method", "message": "The plugin does not implement sign."}}
```

Error codes `invalid_request`, `unknown_method`, `internal`, `state_mismatch`,
`origin_not_approved`, `http_failed` and `not_sent` are shared, as are RFC
8628's `authorization_pending`, `slow_down`, `access_denied` and
`expired_token` for `grant_poll`; a plugin may report codes of its own.

### Calls for a provider

The SDK hands every method the context of its call. For a call OLP makes on
behalf of a provider, `plugin.ProviderOf(ctx)` returns the profile the provider
uses and its option values:

```go
if provider, ok := plugin.ProviderOf(ctx); ok {
	plugin.Log.InfoContext(ctx, "serving an account", "profile", provider.Profile, "account", provider.Options["account"])
}
```

### Methods

OLP calls, through `olp_call`:

| Method | Parameters | Result |
| --- | --- | --- |
| `manifest` | none | The manifest, as described above. |
| `sign` | `{"profile": "…", "method": "POST", "url": "…", "header": {"Name": ["value"]}, "body": "<base64>", "credential": "…"}` | `{"headers": {"Name": "value"}}` |
| `grant_start` | `{"profile": "…"}` | `{"url": "…", "session": "…"}`, or `{"device": {"verification_url": "…", "user_code": "…", "expires_in": 900, "interval": 5}, "session": "…"}` |
| `grant_exchange` | `{"profile": "…", "session": "…", "input": "…"}` | `{"access_token": "…", "refresh_token": "…", "expires_in": 3600, "principal": "…", "facts": {"name": "value"}}` |
| `grant_poll` | `{"profile": "…", "session": "…"}` | As for `grant_exchange` |
| `grant_refresh` | `{"profile": "…", "refresh_token": "…", "facts": {"name": "value"}}` | `{"access_token": "…", "refresh_token": "…", "expires_in": 3600}`, optionally with `principal` and `facts` |
| `carry` | `{"method": "POST", "url": "…", "header": {"Name": ["value"]}, "body": "<base64>"}` | none, after [streamed parts](#stdio-transport) |

OLP calls `sign` only for profiles that declare `"signing": true`, on behalf of
the provider whose request it signs; the grant steps only for profiles that
declare `"grant": {"facts": ["name"]}`, on behalf of the provider enrolling or
refreshing a grant: `grant_exchange` after a `grant_start` that returned a
`url`, and `grant_poll`, once per interval, after one that returned a `device`,
until it returns a grant or fails with another code than
`authorization_pending` or `slow_down`; and `carry` only on an unconfined
plugin, for profiles that declare `"carries_traffic": true`, on behalf of the
provider whose request it carries. A `carry` call that fails with the error
code `not_sent` reports a request that never reached the upstream.

### Capabilities

The plugin calls, through `host_call`:

| Capability | Parameters | Result |
| --- | --- | --- |
| `log` | `{"level": "debug" \| "info" \| "warn" \| "error", "message": "…", "attrs": {"key": "value"}}` | none |
| `http` | `{"method": "POST", "url": "…", "header": {"Name": ["value"]}, "body": "<base64>"}` | `{"status": 200, "header": {"Name": ["value"]}, "body": "<base64>"}` |

A call may use only the capabilities OLP grants it; any other capability returns
`unknown_method`. OLP grants `http` to `grant_start`, `grant_exchange`,
`grant_poll` and `grant_refresh`.

### Stdio transport

An unconfined plugin exchanges the same messages over its standard input and
output, each wrapped in a frame: one JSON document per line, at most 1 MiB for
a frame the plugin writes. Blank lines are ignored.

The plugin first writes its ABI version, and OLP runs nothing else until it
has, within the call's time limit:

```json
{"abi_version": 1}
```

OLP then writes each call it makes as a request with an ID, numbered from 1,
and the plugin answers it with a response carrying the same ID, in any order:

```json
{"id": 7, "request": {"method": "sign", "params": {}, "provider": {"profile": "acme-chat", "options": {}}}}
{"id": 7, "response": {"result": {"headers": {"X-Acme-Signature": "…"}}}}
```

The plugin numbers its capability requests from 1 in the same way and names,
in `call`, the ID of the call each serves, whose capabilities it may use. OLP
answers with the request's ID. A request that names no call may only log.

```json
{"id": 3, "call": 7, "request": {"method": "log", "params": {"level": "info", "message": "signing"}}}
{"id": 3, "response": {}}
```

When OLP stops waiting for a call, it writes a cancellation, and the plugin
still answers the call, promptly:

```json
{"id": 7, "cancel": true}
```

A `carry` call streams its result: the plugin writes the upstream's response
as parts of the call, each an HTTP response with the call's ID, as the response
arrives. The first part holds its status and headers, and each later part only
the next bytes of its body, at most 1 MiB per frame. The call's response then
ends it, with no result, or with the error that stopped it. OLP waits for a
`carry` call for as long as the request it carries lasts, and gives the plugin
10 seconds to answer it once OLP cancels it.

```json
{"id": 8, "request": {"method": "carry", "params": {"method": "POST", "url": "https://api.acme.example/v1/chat/completions", "header": {}, "body": "…"}, "provider": {"profile": "acme-chat", "options": {}}}}
{"id": 8, "part": {"status": 200, "header": {"Content-Type": ["text/event-stream"]}}}
{"id": 8, "part": {"body": "ZGF0YTogey4uLn0KCg=="}}
{"id": 8, "response": {}}
```

A plugin that writes a line that is not such a frame, or a frame over 1 MiB,
is stopped. When OLP closes standard input, the plugin exits.
