# Negotiated tool continuation client

These small helpers use the public `chat-anthropic-tools-v1` carrier with the
official OpenAI JavaScript **7.4.0** and Python **3.8.0** SDKs. They are for an
explicit strict OpenAI Chat route to a qualified direct Anthropic Messages
profile with native reasoning and tool defaults. Native Anthropic SDK requests
send complete history through the native API and need no OLP helper.

`streamTurn` sends one timestamp/UUID submission identity, reads ordinary Chat
chunks plus ordered `olp.observation` extensions, and returns the standard
assistant message and opaque handle only after a terminal `olp.ready:true`
delivery. A complete assistant plus one ordered result per call is passed to
`nextTurn`; `unaryTurn` submits it with the previous handle and a fresh
submission identity. Keep the returned submission identity unchanged when
retrying the **same** request after a client or network failure. A retry of a
committed result returns the exact recorded delivery and performs no inference.

The route's API key needs `inference`, route access and
`allow_provider_state:true`. The handle references encrypted native reasoning
and complete provider dependencies in the existing resource authority for up
to 24 hours. It is scoped to that key, route, provider revision, serving
identity, parent branch and live credential permission. Recovery requires the
same key and contract header:

```
GET /v1/continuation-submissions/{timestamp.UUID}
GET /v1/continuations/{continuation_<uuid>}
X-OLP-Continuation: chat-anthropic-tools-v1
Authorization: Bearer <api-key>
```

A ready recovery contains the standard assistant message and the original
recorded unary body or streaming chunks; it contains no opaque native
signature. An accepted request that never committed a ready delivery returns
`continuation_outcome_unknown` and **must not** be resent under a new
submission identity as if it were known unused. The provider may have accepted
work before the gateway saw a terminal result. OLP does not promise exactly
once provider work or tool execution. A changed history, missing tool result,
changed control, expired or tampered handle, or revoked key/credential fails
closed. Ordinary text observations may arrive before the encrypted ready
commit; ordinary actionable tool chunks arrive only after it.

The helper rejects unsupported SDK versions before sending a request. A bare
official SDK without the explicit carrier gets a pre-dispatch `state_carrier`
error on this translated workflow. Tested scripts live in
`tests/sdk-smoke/negotiated-continuation.mjs` and
`tests/sdk-smoke-python/negotiated_continuation.py`. They run the real SDK
serializers against the public gateway and compare the provider's second
request with the frozen direct-native two-tool reference.
