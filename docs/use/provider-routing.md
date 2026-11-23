# Provider connections and routing

Published route names form OLP's public model API. A vendor identifies an upstream
organization. A provider connection identifies one endpoint, account, region or
deployment and keeps the existing provider ID. Several connections can belong to
the same vendor while using different credentials. A connector defines the wire
protocol and authentication mechanism used to reach that connection.

## Connect and qualify models

1. Choose **Add provider** under Providers and select a vendor. Review the connector,
   endpoint, authentication, and optional seed model. Vendor identity remains
   in `configuration.options.vendor_id` when an endpoint is customized.
2. Test the connection, discover models or enter exact upstream identifiers,
   and select the models to enable. A vendor that publishes no OpenAI-shaped
   model list, such as Cohere, Voyage, Together or an account-scoped platform,
   uses an explicit model for its connection probe.
3. Use **Validate models in bulk**. Keep configured capabilities or choose a
   generation, embedding, or token-count contract. The console reports
   failures per model, can stop after the current model, and can retry only
   failures. Each model's server certification uses at most eight concurrent
   bounded probes.
   The completed-draft probe also checks every enabled model assigned to the
   default credential, including after rotation.
4. Review the completed draft, test it, and activate. Discovery and model
   changes remain drafts until activation. Operator model facts are stored
   separately from discovered identifiers and survive discovery refreshes.
5. In **Compare models and create routes** under Models, select models and review
   route slugs. An explicitly assigned canonical model identity can group
   connections automatically. Other models start with distinct connection
   suffixes; assigning the same slug explicitly creates multiple targets.
   Choose the route fidelity for the generated drafts. Strict is preselected;
   choose transformed when the targets need translation or are Automatic
   providers (providers without a provider profile). Review the generated
   drafts before publishing them individually or in bulk.

![Reviewed models grouped under one published route name](../images/provider-model-comparison.png)

### Reviewed vendor contracts

`GET /api/v1/provider-vendors` lists the vendor catalog. Each vendor is a
reviewed contract in [`internal/domain/vendors`](../../internal/domain/vendors/catalog.go): the
operations its documentation lists, the generation dialects it speaks, and the
request fields it refuses or spells differently. A vendor is admitted only the
operations its contract lists, so a preset never claims media, moderation or
token counting its documentation does not describe. Each model and credential
still needs certification for the requested operation.

A vendor that documents only Chat Completions is sent Chat Completions,
including lossless translation of supported Responses requests, and a profile
in another generation dialect is refused for it. Fields a vendor documents as
unsupported or ignored are refused before selection rather than silently lost:
Cohere's compatibility API, for example, refuses `n`, `dimensions` and its
other documented exceptions. Renamed fields are rewritten: vendors that
document only `max_tokens` receive the OpenAI `max_completion_tokens` under
that name, Snowflake Cortex receives the reverse, Mistral receives `seed` as
`random_seed`, and Voyage maps dimensions to `output_dimension` and float
encoding to its native default. A request that sets both names of a renamed
field is refused. See the [Voyage contract](https://docs.voyageai.com/reference/embeddings-api)
and [Cohere compatibility](https://docs.cohere.com/docs/compatibility-api).

Paid-provider qualification remains an operator activity scoped to the actual
account, model, region, and credential.

## Provider lifecycle

An unused draft can be deleted with `DELETE /api/v1/providers/{provider_id}`,
using its observed ETag and an idempotency key. Published revisions, route
draft references, scoped price history, notification evidence and retained
resource dependencies return `409 provider_in_use`. Deletion cleans the draft's
owned sealed material and advances authority. Use disable for published
providers, preserving their immutable revisions and accounting history.

A provider is created as a draft with its configuration and a credential unless
the authentication mode is `none`, `adc`, `default_chain`, `azure_default`, or a
plugin's `grant`, whose credential versions come from
[grant enrollment](plugins.md#grant-enrollment). The console wizard then:

1. **Probes** the connection (`POST /providers/{id}/probe`), which lists
   upstream models or proves a configured deployment/model when that vendor
   has no model-list API, with bounded time, concurrency, and body size. Probe results
   store only a status, a timestamp, and a sanitized detail; upstream bodies
   never enter persistent diagnostics.
2. **Discovers** models (`POST /providers/{id}/discovery`), either from the
   upstream list or from up to 2000 declared identifiers. Discovered models
   start disabled with no capabilities.
3. **Reviews** capabilities (`PATCH /providers/{id}/models/{model_id}`),
   which records *declared* tuples of operation, surface, and mode.
4. **Certifies** each model (`POST /providers/{id}/models/{model_id}/certify`),
   which proves each operation/surface/mode tuple and marks successful tuples
   as *certified*. An OpenAI-surface generation capability proves both Chat
   and Responses unless the vendor profile explicitly translates Responses
   through Chat. Review at most 64 tuples per model. Each probe has a 15-second
   budget, with at most eight concurrent probes and a one-minute
   model-certification deadline. Only certified tuples of enabled
   models are published to the runtime and are eligible for routes.
5. **Activates** the draft (`POST /providers/{id}/activate`), which validates
   the configuration, requires current validation for each selectable
   credential slot and at least one enabled, fully certified model, writes
   an immutable revision, and publishes a new runtime generation.

Draft edits never change serving traffic: they mark the provider as having a
pending activation, and the runtime keeps using the active revision. Changing
transport or semantic details (kind, authentication mode, endpoint, cloud
addressing, credential headers, parameter defaults, model facts, vendor)
invalidates certification evidence and slot validation, so tuples must be
certified again before activation. Re-reviewing capabilities preserves unchanged
tuples, removes evidence for deleted tuples, and starts new tuples as declared.
Renaming and credential rotation preserve model certification; rotation still
requires fresh slot validation. Revisions can be listed, read, compared, and
restored as a new draft; restoring copies the recorded models and evidence but
never a historical credential.

Disabling a provider (`POST /providers/{id}/disable`) publishes a generation in
which the provider is not selectable; streams that already started keep the
snapshot they were admitted with.

## Private endpoints, headers, defaults, and model facts

OpenAI, OpenAI-compatible, Anthropic, and Gemini connectors accept custom
endpoints. Set `auth_mode` to `none` for an explicitly unauthenticated endpoint
or to `headers` for encrypted custom authentication. Custom authentication
requires an explicit endpoint. Private addresses and HTTP additionally need the
existing [egress allowlists](../operate/configuration.md#provider-egress-policy). DNS
pinning, redirect refusal, and response limits still apply.

For header authentication, put only lowercase header names in
`options.credential_headers`, for example `["authorization","x-account-id"]`.
The write-only `credential` is a JSON **string** containing exactly those names
and their values. Include the complete Authorization value when needed. Reserved
transport and OLP headers cannot be configured. Header values never appear in
management reads or error responses.

`options.parameter_defaults` contains provider-wire JSON defaults. Explicit
request values take precedence, including nested generation configuration.
Response-format defaults apply only to matching chat, image, or speech
endpoints; an explicit response format replaces the complete format default.
Routing, model, messages, input, stream, and other envelope fields cannot be
overridden through defaults. Native HTTP connectors support these defaults;
Vertex and Bedrock retain their native cloud configuration.

`options.models` maps upstream model identifiers to model facts:

```json
{
  "models": {
    "account/model": {
      "canonical_model": "organization/model",
      "context_length": 131072,
      "max_output_tokens": 8192,
      "supported_parameters": ["temperature", "max_output_tokens", "tools"],
      "input_modalities": ["text"],
      "output_modalities": ["text"],
      "region": "eu-west",
      "quantization": "bf16",
      "data_collection": false,
      "zero_data_retention": true,
      "source": "Operator-reviewed account agreement",
      "observed_at": "2026-09-09T00:00:00Z"
    }
  }
}
```

Unspecified facts are unknown. Privacy declarations require a source and an
observation time. `deployment` overrides the wire model for native HTTP
connectors; Azure overrides select an exact deployment path. Changing relevant
connection options invalidates previous certification evidence. A custom
endpoint never inherits official OpenAI media certification merely from its
connector selection.

## Credential pools and limits

Manage versions under `/api/v1/providers/{id}/credentials` and up to 64 slots
(including required default) under `/credential-slots`. Secrets are sealed/
write-only; mutations require ETags/idempotency. Individual slot ETags allow
sibling edits; collection edits retain collection fences. Deleting a nondefault
draft slot removes pending enrollment, not immutable published history.

Slots have enabled/priority/weight and allowed model/route/key restrictions;
empty lists are unrestricted within the connection. Lower priority then weighted
rendezvous chooses among eligible slots. Every serving slot must validate all
allowed enabled certified capabilities (one-minute operation, 15-second probes).
Evidence binds credential/transport/capabilities; concurrent changes invalidate.
Disabled/no-model slots neither serve nor block activation.

Rotation validates then stages a new version; active traffic keeps its old version
until reactivation. Model certification may validate its selected usable slot;
additional/rotated slots need their own proof. Secretless modes still honor default
restrictions/quotas. Published options/contracts/versions and attempt price pins
remain immutable, while new admissions use current published count quotas.

`options.commitment` declares provisioned capacity,
`{"amount": "<decimal>", "period": "hour"|"day"|"month"}`: priced usage on the
connection settles at zero marginal cost and the commitment allocates across
the period's list-cost weight, as [Pricing](../operate/pricing.md#commitments) describes.

Connection `options.limits` and slot RPM/TPM/concurrency both apply. Valkey owns UTC
minutes/leases; configured quotas fail closed. Logical quota identity survives
rotation. Dispatched combined token reservations cannot decrease; settlement releases
concurrency. `priority_shares` sum to 100 across critical/high/normal/low, with
`saturation_percent` 1–100. Below saturation any class borrows idle capacity;
above it atomic shares preserve headroom across replicas:

```json
{"max_concurrency":40,"priority_shares":{"critical":50,"high":25,"normal":20,"low":5},"saturation_percent":60}
```

Providers with separate input and output quotas take `input_tokens_per_minute` and
`output_tokens_per_minute` on connection `options.limits` and on each slot, beside
`tokens_per_minute`. Each configured dimension enforces independently, an unset one is
unbounded, and a combined-only configuration behaves as before. Before dispatch, one
atomic reservation holds the estimated input and a bounded output allowance against the
connection and slot counters shared by every replica, without changing the request. The
output allowance is the reply bound the request or provider defaults name, or 4096 tokens
when neither does, per requested candidate. Subscription code requests and Gemini
interaction continuations hold the same allowance. A request exceeding either dimension is
refused on that target before dispatch and routing tries the next candidate; the refusal
names the exhausted dimension. When usage is reported, the input and output counters
settle to it, with input excluding tokens read from the provider prompt cache, so unused
output allowance returns within the same minute. Missing or uncertain usage keeps the
reservation, any settlement applies at most once, and the combined counter still never
decreases.

OLP accounting differs from a provider's in ways an operator setting split limits should
expect. OLP windows are fixed UTC minutes, while providers commonly replenish
continuously, so capacity returns at different moments. OLP input is an estimate from its
own tokenizer until the upstream reports usage, and its estimate counts the whole prompt
while a provider may exclude cache reads from the start. OLP holds the full output bound
until the attempt ends, while a provider counts output tokens as they are produced and
ignores the bound, so a large `max_tokens` holds output capacity here that costs nothing
upstream; keep it near the expected reply size. A request whose output bound alone exceeds
the output limit never dispatches on that target. During a rolling deploy, split limits
enforce fully only once every replica runs a build that knows them.

A deployment model that several same-project connections serve under one upstream
ceiling joins a shared token quota group through connection
`options.shared_token_quotas`, for example
`[{"model":"gpt-example","group":"upstream-a","tokens_per_minute":250000}]`.
Connections in the same project naming the same group share one tokens per minute
counter, independent of the connection and its secrets, and every member declares
the same ceiling. For ordinary generation an attempt is admitted only when the
group, connection and slot reservations all succeed, and a partial failure charges
none of them. Moving a model between groups changes future reservations only,
once the publishing release installs. Leases already taken keep settling in their
original group exactly once, and the old group keeps its accounting while they
can. The credential pool shows each group's live usage beside the connection
usage, and a request refused by the group names the shared quota rather than the
connection or credential.

Connection/slot day/month supply caps use exact-decimal PostgreSQL facts/Valkey
snapshots and per-attempt holds. Unknown windows skip as
`connection_budget_unavailable`/`slot_budget_unavailable`; exhaustion similarly
uses `_exhausted`. No zero initialization; [reconcile](../operate/operations.md#spend-budget-reconciliation).

Optional `health_probe.interval_seconds` (60–86400, for example 300) sends bounded
paid synthetic work per routed model/fleet interval, thirty-second deadline.
Generation probes use the connector's target dialect: a native text-completion
profile receives a 16-token `/completions` prompt instead of a chat request.
System/probe usage remains accounted; failing health ranks targets last rather
than eliminating availability. Shared circuit/slot cooldown evidence distinguishes
transport/server failures from credentials. Credential-only half-open outcomes
let sibling slots probe. Rotation does not reset logical rate cooldown; validated
credentials may clear it. See [gateway failure rules](../develop/architecture.md#request-path).

Revocation is live authority across old releases/pins. Media jobs keep exact
original credentials and block explicit revocation until durable deletion records.
Workers claim only runnable capacity, revalidate claims immediately before bounded
poll/delete and yield silently on lease takeover.

![Credential pool](../images/provider-credential-pool.png)

## Scoped routing policies

`GET /api/v1/routing-policies/{scope}/{id}` reports `configured` alongside
its policy and observed ETag. Installation policy uses the nil UUID and requires
installation settings authority; API-key policies require key authority, and
route-draft policies require configuration authority and project access.

`DELETE` removes an explicit override under the same permissions, observed
`If-Match`, and `Idempotency-Key` requirements as `PUT`. The next read reports
inherited defaults with a fresh ETag. An observation from before a create/delete
cycle remains stale. Non-draft removal publishes the runtime change; draft removal
becomes effective when that draft is activated. Scoped writes return their exact
parent ETag transition, and replay returns the original transition and outcome.
Successful removals are audited.

## Routes

Automation can create a published route with `POST /api/v1/routes` and replace
its configuration with `PUT /api/v1/routes/{route_id}`. Both validate and publish
an immutable revision in one transaction using the same checks as console draft
activation. Creation refuses an existing slug; replacement requires the observed
published ETag and retains its slug, project, and routing policy. Each successful
write retains a revision source draft and leaves independent console drafts
untouched. Failed validation rolls back the draft, revision and runtime release;
replaying the same Idempotency-Key returns the original result. Published route
reads return their own ETag. Retirement preserves revision and accounting history.

A slug is 1–100 bytes of lowercase ASCII letters, digits, `.`, `_` and `-`,
starting with a letter or digit; uppercase, slashes and other characters refuse.

Route drafts carry a slug, allowed operations (default `generation`), an overall
deadline, an attempt budget, and 1–64 targets with priority, weight, and
timeout. Supported operations also include `token_count`, `embeddings`,
`rerank`, `moderation`, `image_generation`, `image_edit`, `image_variation`,
`speech`, `transcription`, `translation`, the `video_*` operations, `batch`,
`realtime`, and `bedrock_invoke`; see the
[compatibility matrix](compatibility.md).

The overall deadline is 1–3,600,000 milliseconds; target timeouts cannot exceed
it. The attempt budget is 1–32,767 and counts credential attempts, so it can
exceed the target count. Every target must reference a published model with
certified support for each allowed operation; validation and activation reject
unknown, inactive, unpublished, or uncertified targets. Drafts are versioned
with ETags, so stale edits return `412` and the console offers a reload.

Activating a draft writes an immutable route revision, makes it the latest
revision of the route named by the slug, and publishes a runtime generation.
Revisions can be compared and restored as new drafts. The simulation endpoints
(`POST /route-drafts/{id}/simulate` and `POST /routing/simulate`) explain the
deterministic attempt order for a given seed or key without contacting any
provider.

A route revision may also declare fallbacks, selectors, a retry policy, session
affinity and a spend cap, and its targets may carry `tags` and a `shadow`
sample rate. The draft and revision endpoints, revision diffs, restore, and
[configuration promotion](../operate/configuration.md#configuration-promotion-artifacts)
carry them all:

```json
{
  "slug": "assistant",
  "targets": [
    {"provider_model_id": "…", "tags": ["small"]},
    {"provider_model_id": "…", "tags": ["large"]},
    {"provider_model_id": "…", "shadow": {"sample_rate": 0.05}}
  ],
  "fallbacks": [
    {"route": "assistant-long-context", "on": ["context_window"]},
    {"route": "assistant-backup", "on": ["exhausted", "content_filter"]}
  ],
  "selectors": [
    {"id": "short", "when": {"max_input_tokens": 2000, "tools": false}, "tags": ["small"]},
    {"id": "reasoning", "when": {"reasoning_effort": ["high"]}, "route": "assistant-reasoning"}
  ],
  "retry": {"upstream_server": {"max_retries": 2, "base_backoff_ms": 200, "max_backoff_ms": 2000, "respect_retry_after": true}},
  "affinity": {"source": "cache_key"},
  "budget": {"daily_cost_limit": "250"}
}
```

### Fallbacks

`fallbacks` lists up to four other routes, in order, each with the conditions
that start it:

| Condition | The route's attempts ended… |
| --- | --- |
| `exhausted` | without success, every attempt having failed with a retryable class |
| `context_window` | on a context-window refusal, or planning found no target whose window holds the request |
| `content_filter` | on an upstream content-filter refusal |
| `rate_limit` | on an upstream rate limit or a local quota |
| `budget` | on a supply-side spend cap, at planning or before dispatch |

A content-filter refusal is a typed upstream error, such as `content_filter` or
`content_policy_violation`. It is terminal within the route, never counts
against provider health, and reaches the caller as a `400` with code
`content_filter` unless a fallback serves the request.

A fallback route runs with its own targets, policy and fidelity,
and the caller still sees the route it named in the response's `model`. The key
must be allowed to use the fallback route; otherwise the plan records
`fallback_route_forbidden` and moves to the next fallback. A route that is no
longer active is skipped as `fallback_route_unavailable`, and one the request
already visited as `fallback_route_repeated`. The named route's overall deadline
and attempt budget bound the whole request, fallbacks included, and every
attempt records the route leg it ran on. A request never falls back once a
stream has committed, after an ambiguous resource creation, or when it is bound
to its route by a pinned resource, a continuation or provider state.

Publication refuses a route graph, counting fallbacks and selector delegation,
that has a cycle (`route_graph_cycle`), crosses a project boundary
(`route_graph_project_mismatch`), is more than three routes deep
(`route_graph_too_deep`), or lets a strict route serve through a transformed one
(`route_graph_fidelity_mismatch`). Validation and activation refuse a reference
to a route that is not active (`route_reference_unknown`).

### Request selectors

`selectors` are up to 16 rules, with at most eight tags per selector or target,
evaluated in order; the first whose predicate
matches wins. Its action either narrows the route to the targets carrying any
of its `tags`, or delegates the request to another `route` the key may use.
Plan decisions record the selector, and targets it excluded carry the reason
`selector_excluded`. A predicate is the conjunction of what it declares, over
features the gateway computes during admission:

| Predicate | Matches |
| --- | --- |
| `operations` | the planned operation |
| `min_input_tokens`, `max_input_tokens` | the estimated input tokens |
| `min_output_tokens`, `max_output_tokens` | the requested output tokens |
| `streaming`, `tools`, `structured_output` | a streaming request, one that offers tools, one that asks for a JSON schema or object |
| `modalities` | any of `text`, `image`, `audio`, `video` and `file` among the input parts |
| `reasoning_effort` | the request's reasoning effort, such as `high` |
| `classifier` | the label another route returns for the request |
| `plugin` | the verdict of an approved confined plugin |

Attribution labels never take part, so labels stay free of routing effect.

A `classifier` predicate, such as
`{"route": "triage", "labels": ["complex"], "min_score": 0.7, "timeout_ms": 500}`,
sends the text of the request's last user turn, up to 32 KiB, to another OLP
route. A route serving `classification`, such as a TEI classifier, answers with
its top label and score; a generation route answers with its reply text as the
label. The call is an accounted request of the caller's key, with origin
`classifier` and the caller's request as its parent, under its own deadline. A
classifier that fails, times out or that the key may not use falls through to
the next selector, recorded as `classifier_failed` or `classifier_forbidden`.

Classifier timeouts cannot exceed 30 seconds; each classifier route is evaluated
at most once per request.

A `plugin` predicate names a confined plugin by its digest,
`{"digest": "<sha-256>"}`, and asks it to judge the same features through the
`route_predicate` method ([route predicates](../develop/plugin-authoring.md#route-predicates)).
The plugin must be approved when the route is validated
(`route_plugin_unavailable` otherwise); a call that fails or exceeds 250
milliseconds falls through as `plugin_failed`. A plugin predicate can only
narrow the candidate set.

`GET /api/v1/usage/selector-savings` compares each selector's attempts with
what their own usage would have cost on the most expensive eligible target the
selector avoided, priced when each attempt was accounted. Attempts without both
prices are counted but not compared, so the report never invents usage.

### Retry policy

`retry` declares, per retryable class (`connect`, `timeout`, `rate_limit` and
`upstream_server`), how many times an attempt repeats on the same slot before
the route fails over: `max_retries` up to 10, a `base_backoff_ms` and
`max_backoff_ms` up to 60 seconds with full jitter, and whether to honor the
upstream's `Retry-After` instead. Retries consume the attempt budget and the
overall deadline, are recorded as attempts with their retry number, never apply
after commitment or to an ambiguous creation, and stop once the provider's
circuit opens. The slot cools down only after its retries are exhausted.

### Session affinity

`affinity` keeps requests of one session on the same target and slot while it
stays eligible, which raises provider prompt-cache hit rates without any gateway
state. The session key is the value of an attribution label,
`{"source": "label", "label": "session"}`, or the dialect's own cache key,
`{"source": "cache_key"}`, which is OpenAI's `prompt_cache_key`. OLP hashes it
into the rendezvous seed in place of the key's identity; a request without one
keeps the key's seed.

### Shadow targets

A target declared `{"shadow": {"sample_rate": 0.05}}` never serves callers. It
is planned with the request under every hard constraint the request is subject
to, including region, data collection and zero data retention, and a sample of eligible requests, chosen deterministically from
the request's identifier, is mirrored to it after the caller's response
completes. Shadow attempts run in their own admission pool
(`OLP_HTTP_MAX_IN_FLIGHT_SHADOW_REQUESTS`) under the route's deadline and are
dropped rather than queued when it is full, so they never change what the
caller sees. Only stateless generation, embeddings and rerank are mirrored, never
requests with provider state, continuations, pinned resources, files, batches,
realtime or media. Shadow requests are recorded with origin `shadow` and the
caller's request as their parent, and accounted to the route, never to the
caller's key, budget group or end-user budgets. Aggregate installation and project
caps also apply. `GET /api/v1/usage/shadow-experiments` compares
each shadow target with the primary path of the requests it mirrored by status,
latency, time to first byte, token usage and cost, from metadata only. A route
needs at least one serving target.

### Route spend caps

`budget` declares a route's own `daily_cost_limit` and `monthly_cost_limit`,
shared by every target. An exhausted cap removes the whole route from selection
with the reason `route_budget_exhausted` and can start a `budget` fallback; an
attempt refused by any cap at dispatch answers `503 supply_budget_exhausted`
when no fallback serves the request.

### Route templates

A route template turns certified models into ordinary routes. It names a
provider selector (`vendor:<id>` or `provider:<uuid>`), a case-insensitive model
filter where `*` matches any run of characters and `?` one, a slug pattern built
from `{model}`, the model's canonical identity, and optionally `{vendor}`, an
overall deadline, an attempt budget, a fidelity, a routing policy, and whether
to publish automatically:

```json
{"name": "claude", "provider_selector": "vendor:anthropic", "model_filter": "claude-*", "slug_pattern": "{vendor}-{model}", "overall_timeout_ms": 120000, "max_attempts": 2, "auto_publish": true}
```

Manage templates under `/api/v1/route-templates`. When a provider activation
certifies a model that matches, the template creates a route draft, or a
published revision when `auto_publish` is set, in the activation's transaction;
`POST /api/v1/route-templates/{template_id}/apply` places every matching model
now. A second connection serving the same identity joins the same generated
route as another target. A template places each model once, records why it
skipped one (`slug_taken`, `operations_not_certified`, `targets_full` or an
activation refusal), and never creates a route for an uncertified model, so
nothing reaches a caller without certification. Every generated route is a
normal route, with its own revisions, simulation and access control.

### Route fidelity

`fidelity` is `{"mode":"strict"}` or `{"mode":"transformed"}`. Omitted/null/empty
means strict, never inherited; revisions/exports record explicit mode. Invalid
values refuse 422. Every strict target compiles its native operation contract;
Automatic/non-strict profiles, translation or redaction refuse `target_capability`/
policy conflict. Header/auth-only plugin adaptation may be strict; envelopes,
rewrites, forced streams or carried traffic are transformed.

Use transformed routes for translation/redaction/Automatic providers/playground/
Bedrock InvokeModel. Provider-owned state then retains metadata mappings only.
New revisions change fidelity without renaming slugs; restore uses original mode.

Retained state obeys creation fidelity. Strict resources cannot be read/recovered
or start work through transformed ingress (409); transformed resources may still
be read through strict ingress but cannot start work. Lists show owned stored
metadata without polling incompatible resources. Owners can delete/cancel after a
switch. See [compatibility](compatibility.md) for operation contracts.

## Policies and caller preferences

Policies live at installation, route revision, and gateway-key scopes. Use
**Settings**, the route draft editor, and the API-key editor, or
`GET/PUT /api/v1/routing-policies/{scope}/{id}`. Scopes are `installation`,
`route-draft`, and `api-key`; the installation ID is the nil UUID. Installation
and key changes publish immediately. Route policy changes are staged and
published with the route. Policy writes require the corresponding management
permission and current ETag.

Hard constraints intersect across all scopes and the request. They include
`only`, `ignore`, `regions`, `quantizations`, `deny_data_collection`,
`require_zero_data_retention`, `require_parameters`, `max_price`, and
`max_cost`. Selectors are `vendor:<catalog-id>` or `provider:<connection-uuid>`.
Unknown facts cannot satisfy a required constraint. Constraints placed inside
policy defaults also narrow eligibility. They do not grant additional access.

`max_cost` is an exact-decimal ceiling on a request's worst-case attempt cost:
the bound the gateway would reserve (highest input-side rate for the estimate,
the declared or default reply allowance, the per-request fee, the window
multiplier, and the attempt's pinned discount). Every scope that sets it
narrows it, so the effective ceiling is the smallest value. A candidate whose worst
case exceeds the ceiling, or that cannot be bounded at all, is ineligible with
the `cost_limit_exceeded` reason; a committed provider bounds zero and always
passes. When no non-shadow target remains eligible for that reason the request
fails `400` with `cost_limit_exceeded` before anything is dispatched, without
triggering fallback; fallback legs that do run apply the same ceiling.

Ordering and strategy preferences use request, key, route and installation
precedence in that order. A policy can restrict `allowed_strategies`. `order` precedes the
strategy inside an operator priority tier. With `allow_fallbacks: false`, an
explicit order restricts attempts to that list; without an order, only the first
eligible attempt is selected. Requests cannot add published targets, raise
deadlines or attempt limits, or broaden policy constraints.

All native inference surfaces accept one optional `X-OLP-Routing` header:

```http
X-OLP-Routing: {"only":["vendor:deepseek"],"strategy":"price","max_price":{"input_per_million":"0.50","output_per_million":"2.00"},"max_cost":"0.25"}
```

The header also chooses the request's admission `priority`, such as
`{"priority":"high"}`, up to the ceiling its key's policy allows; a higher value
is refused as `priority_increase_forbidden`. Priority decides admission and
capacity shares, never target selection, and is not a routing-policy control.

Malformed JSON, unknown controls, and invalid selectors are rejected before
dispatch. The header is never forwarded upstream. Existing header-size limits
apply. Strict parameter filtering additionally requires affirmative model
support for supplied canonical parameters and semantic extensions. Semantic loss
checks always apply, even when strict parameter filtering is off. Media controls
include `n`, `size`, `mask`, `voice`, `response_format`, `language`, `prompt`,
and `input_reference`; internal cleanup markers are excluded.

| Strategy | Ordering within a priority and preferred-order tier |
| --- | --- |
| `weighted` | Deterministic weighted rendezvous; the default |
| `price` | Exact decimal rates; generation compares input plus output per million, embeddings use input, other units use their unit rate |
| `latency` | Median time to first meaningful output for streams, total attempt latency for unary operations |
| `throughput` | Median generated output tokens per second after first meaningful output; reasoning tokens are excluded |
| `capacity` | Remaining headroom of each credential slot, the smallest of its connection's and its own remaining request, token and concurrency fractions, read in one pipelined Valkey round trip; slots without quotas rank after slots with known headroom |
| `bounded_cost` | Exact-decimal upper estimate of this request on each target (generation only); targets without a bound keep weighted order after the ranked ones |

`bounded_cost` prices the actual request instead of comparing rate sums, so a
long prompt with a short answer can prefer a target `price` would not. It
applies only to generation and only inside the tier every other ordering
already respects (health, priority, locality and preferred order), after all
eligibility checks including `max_cost`. Each candidate's upper estimate is
the same worst case the gateway would reserve, with the attempt's pinned
discount, and a committed provider bounds at zero. A missing bound is never
treated as zero. When at least two candidates in a tier have a bound, they
rank by it, ties fall back to weighted order, and candidates without one
follow in weighted order with `bounded_cost_fallback_reason` set to
`cost_bound_unknown`. When fewer than two have a bound, the whole tier keeps
the weighted order and each candidate reports `insufficient_cost_bounds`. A
route with `caller_cost_exempt` bounds no attempt's cost, so it always keeps
weighted order with `insufficient_cost_bounds`, while route explain still
reports each target's estimated cost for information. A
non-generation request keeps weighted order and reports
`operation_not_generation`. The fallback never restores a target that
eligibility already excluded.

Prices resolve connection scope before vendor scope before connector-kind scope,
then the latest effective revision. Future rates become eligible at their
effective time. Missing required price components fail a price ceiling; unknown
prices rank after known prices. Routing and accounting preserve the selected
revision, including an explicit unpriced decision.

Performance aggregates use five minutes of persisted successful attempts,
partitioned by connection, model, operation, and mode. At least 20 samples are
required; snapshots refresh every ten seconds and expire after 60 seconds.
Unknown measurements rank after known measurements and use weighted order when
all are unknown. `preferred_max_latency_ms` and `preferred_min_throughput` favor
matching observations; they are preferences, not response guarantees.

Targets the fleet marks unhealthy, by a shared circuit or an active probe, move
to the end of the attempt order rather than disappearing, so a fleet-wide false
positive degrades latency instead of availability.

Eligibility filtering precedes ordering and `max_attempts`. Each actual
credential attempt consumes the route's budget, which can exceed target count.
Fallback never restarts a committed stream or an ambiguously created media job,
nor a request an unconfined plugin carried whose outcome is unknown; see
[carrying traffic](plugins.md#carrying-traffic).

## Explain and observe

![Route preview](../images/provider-routing-preview.png)

`POST /api/v1/routing/simulate` and draft/playground preview share the execution
planner. Supply operation/surface/mode/preferences, optional key/seed/native request.
Results include exclusions/order/slots/prices/measurement freshness and target-
prepared input tokens/provenance/family. A generation preview shows the model's
context limit, estimated input, any per-completion output reservation used for
the window check, and remaining headroom. The input breakdown adds up to the
estimate and separates instructions, conversation, tool schemas, tool calls and
results, media, and unclassified input such as message framing. With no output
bound applied, headroom is shown after input alone. An unknown model limit or
unmeasured provider-held context makes headroom unknown. These are planning
estimates, not billed usage. Preview doesn't send inference or store request
content.

Simulations also report each target's
`estimated_cost`: the `minimum` prices the input estimate at the cheapest
input-side rate the selected tier carries, plus the per-request fee; the
`maximum` is the worst case the gateway would reserve. For generation the
estimate also names the `priced_input_tokens` the maximum priced and the
`reserved_output_tokens` it reserved, with the `currency` both are stated in.
Each decision separately reports the effective `max_cost` ceiling that
admitted or excluded it, null when no scope set one, so the estimate that
describes the attempt never stands in for the rule that admits it. Server-side tool fees and
batch multipliers are not bounded. The pair is null when the target's price
cannot bound the request or no token demand is known. An explicit estimated count applies to
all targets without provenance. Admission additionally reserves reply/larger
source-prepared bounds; simulation is not a dispatch guarantee. Decisions have
no bounded maximum, so a target whose key, route, provider or eligible slot caps
need one is marked incompatible with `unbounded_work_budget` (see
[Decisions](compatibility.md#legacy-text-completions-and-decisions)). With a
key, simulated attribution labels resolve through the key and project policies;
labels the key cannot set are rejected, and attribution-scoped budgets apply.

Native source indexing is limited to 65,536 JSON values. Strict inspection shares
8 MiB across targets/legs, including media and recursively decoded tool arguments/
schema keys, charging at least one byte/match. Before overflow, `inspection_limit`
refuses; reduce request/rules/targets, never bypass policy.

Explain traces selectors (optional `classifier_labels` substitutes), delegation/
fallback planned/standby/skipped legs, affinity, capacity headroom, shadow and
unhealthy targets. Selectors never use attribution; configured session affinity may.
Capacity/authority can change between preview and serving.

### Check a draft against saved fixtures

Open a saved route draft and choose **Check saved request fixtures**. Select a
local JSON file, then choose **Compare fixtures**. The check needs an active
published revision for the same slug and project. It uses the revision that is
active at the time of the check, even if you started the draft from an older one.

Keep the file local and use only invented requests, names and classifier labels.
Never copy production prompts, credentials, real identifiers, or personal data
into it. OLP accepts up to 16 uniquely named fixtures in a 256 KiB JSON file.
The file is sent only to the management API and is not stored. The check does
not run inference, tools, classifier routes or selector plugins.

```json
{
  "synthetic": true,
  "fixtures": [
    {
      "name": "Structured output",
      "operation": "generation",
      "surface": "openai",
      "mode": "unary",
      "seed": "regression-suite",
      "expect_eligible": true,
      "request": {
        "model": "replace-with-your-route-slug",
        "messages": [{"role": "user", "content": "Return a JSON object naming a green square."}],
        "response_format": {"type": "json_object"},
        "max_completion_tokens": 128
      }
    }
  ]
}
```

Each pair uses one configuration snapshot and the same request, seed, observed
routing inputs and planner time. It reports changes in eligibility, the first
planned attempt, selector outcomes that affected the request or could not be
evaluated, and reasons. Target details include context window estimates where
available. Optionally set `dialect` and supply `classifier_labels` keyed by
selector ID. When a needed verdict is absent, the trace says
`classifier_not_simulated` and the selector is skipped. When the project
requires attribution labels without defaults, supply synthetic values in each
fixture's `attribution`.

This is a point-in-time planning check. Pricing, performance and credential
eligibility can change between runs. Live capacity, spend and circuit state
are excluded, so a configured spend cap without a reading stays unknown and
can exclude a target. Checks have no caller API key and cannot verify key-specific
policies or gateway tool execution.

For CI, use a management token with read scope and access to the draft's project.
An HTTP 200 response means the comparison ran. Its `passed` field is false if
any `expect_eligible` assertion failed on the draft. Use `true` to require a
serving attempt, `false` to require no attempt, or omit the field to only report
changes. The response includes the baseline revision, draft ETag, planning time
and per-fixture results without any request bodies.

```sh
olp api check_route_draft_fixtures DRAFT_ID --body-file fixtures.json --output fixture-report.json &&
  jq -e '.passed' fixture-report.json
```

Request history records immutable connection/slot/version/revision/policy/prices,
retry/fallback/leg/selector/caps, caller/shadow/classifier/probe origin/parent and
meaningful output timing. No prompts, outputs, secret headers or raw controls.
