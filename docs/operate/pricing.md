# Pricing

Pricing begins with immutable pricing revisions. Each revision contains price
entries that take effect at one instant and never change after creation.
Accounting considers every revision in force at an attempt's observed time and
selects the most specific matching entry. Provider scope wins over vendor scope,
and vendor scope wins over provider-kind scope. Among equally specific entries,
the newest `effective_at` wins, followed by the newest revision number.

An entry is scoped by `provider_kind`, optional `vendor_id`, optional
`provider_id`, the upstream `model` and the `operation`. It carries one
`currency`. This guide defines the price components, commitment and discount
rules, consumer rate cards and the difference between provider cost and consumer
charge.

Revisions are created through `POST /api/v1/pricing/revisions`, published from
[pricing sources](operations.md#accounting-delivery-and-shutdown), or applied as
part of a configuration document. A revision never changes after creation, so a
stored cost stays explicable in the terms it was charged under.

## Price components

Every rate, fee and multiplier is a non-negative decimal string with up to 12
integer digits and 12 fractional digits. All components are optional. Every
reported billing dimension needs its rate, and an omitted applicable quantity
remains unknown.

| Member | Charges |
| --- | --- |
| `input_per_million` | The text share of the input tokens, per million. |
| `cached_input_per_million` | Cached text input. Falls back to the input rate. Requires the input rate. |
| `cache_write_input_per_million` | Generic cache writes. Falls back to the input rate. Requires the input rate. |
| `cache_write_5m_input_per_million`, `cache_write_1h_input_per_million` | TTL-classed writes. Each falls back to the generic write rate, then input. Requires the input rate. |
| `output_per_million` | The text share of the output tokens, per million. |
| `audio_input_per_million` | The audio share of input. Falls back to the input rate. |
| `cached_audio_input_per_million` | Cached audio input. Falls back to cached input, then input. |
| `audio_output_per_million` | The audio share of output. Falls back to the output rate. |
| `image_input_per_million` | The image share of input. Falls back to the input rate. |
| `cached_image_input_per_million` | Cached image input. Falls back to cached input, then input. |
| `image_output_per_million` | The image share of output. Falls back to the output rate. |
| `unit_price` | A fixed amount per media unit. |
| `per_request` | A fixed fee charged once per successful attempt. Verified deferred eval invocations use their actual count. |
| `training_per_million` | `training_tokens`, per million training tokens. |
| `storage_per_gib_day` | Exact `storage_byte_seconds` divided by `2^30 * 86400`. |
| `cache_storage_per_million_token_hour` | Exact `cache_token_seconds` divided by `1000000 * 3600`. |
| `container_per_session` | `container_sessions`, keyed by native memory tier such as `1g`, `4g`, `16g`, or `64g`. |
| `batch_multiplier` | Multiplies the whole usage charge of attempts marked batch. |
| `tool_calls` | Fees per metered server-side tool invocation, `{tool: fee}`, from 1 to 16 names matching `^[a-z][a-z0-9_]{0,63}$`. |
| `tiers` | Input-token tiers, described below. |
| `windows` | Time-of-day multipliers in a named IANA zone, described below. |

An entry must name at least one of `input_per_million`, `output_per_million`,
`audio_input_per_million`, `audio_output_per_million`, `image_input_per_million`,
`image_output_per_million`, `unit_price`, `per_request`, `tool_calls`,
`training_per_million`, `storage_per_gib_day`,
`cache_storage_per_million_token_hour`, or `container_per_session`. Only the
four uncached modality rates satisfy this minimum requirement. Setting
`cached_audio_input_per_million` or `cached_image_input_per_million` alone does
not satisfy it, and `cached_input_per_million` or any cache-write rate requires
`input_per_million`.

## Modality shares

Cached and modality counts are shares of the input and output totals, so
`audio_input_tokens` is part of `input_tokens` and never additional. On the
input side, text input equals `input - cached - writes - uncached audio - uncached image`,
cached text equals `cached - cached audio - cached image`, and each modality
share bills at the first rate its fallback chain carries. A price without
modality rates therefore bills every share at the text rates and returns
identical amounts to one that never declared modality rates. The output side
bills the audio and image shares the same way.

## Tiers

`tiers` lists 1 to 8 thresholds `{above_input_tokens, <per-million fields>}` in
strictly increasing order. A tier applies when the attempt's input tokens are
strictly greater than its threshold, and the highest exceeded tier replaces the
entry's whole per-million rate set for that attempt. A tier therefore names
exactly the per-million fields its entry names. Per-request fees, tool fees,
windows and the batch multiplier are entry-level and apply regardless of tier.

Eval runs provide per-model totals rather than per-invocation input sizes.
Their admission rejects input tiers on provider prices and explicit rate-card
prices. Existing aggregate observations with more than one invocation remain
unpriced under tiered tariffs because a sum of invocation inputs cannot choose
a tier.

## Windows

`windows` is `{time_zone, periods}` with 1 to 32 periods, each shaped as
`{days, start, end, multiplier}`. Each period lists unique ISO weekdays from 1
(Monday) to 7 (Sunday), an inclusive local `start` time in `HH:MM`, and a later
exclusive `end` time in `HH:MM` or `"24:00"`. Periods never overlap on a shared
day.

The attempt's start instant is evaluated in the named zone, which must be a
valid IANA name recognized by both the server and the database. The matching
period's multiplier then applies to the whole usage charge. When no period
matches, the multiplier is one.

## Tool fees

`tool_calls` maps a tool name to a fee per invocation. A metered tool with a
positive invocation count and no configured fee makes the attempt unpriced,
while zero-count tools are ignored.

Consumer MCP tool calls price differently from provider-side tool fees. Each
registered server revision pins one operator unit price per dispatched call,
with optional per-tool overrides. That single price is both the settled cost
and the settled consumer charge, because tools carry no separate rate card, so
cost-basis and charge-basis budgets measure the same amount for a tool call.
An explicit zero price is known free, and a missing price stays unpriced and
refuses dispatch under any enforced budget ceiling. See the
[MCP server registration](../use/mcp-servers.md) guide for the full contract.

Usage is marked `batch` by batch-result settlement. Retained training, storage,
cache and container evidence use their explicit units and preserve the original
acceptance pricing, discounts, rate card and tariff-window instant.

## The bill

For a complete attempt, the billed cost follows this formula:

```
input_charge/1e6 + output_charge/1e6 + media_units * unit_price
  + per_request (successful attempts only)
  + sum(count * tool_fee)
  + training_tokens * training_per_million / 1e6
  + storage_byte_seconds * storage_per_gib_day / (2^30 * 86400)
  + cache_token_seconds * cache_storage_per_million_token_hour / (1e6 * 3600)
  + sum(container_sessions[tier] * container_per_session[tier])
```

Retained quantities stay exact until the final monetary rounding. Deferred
facts do not repeat their submission request fee. Eval generation and grader
facts use verified `invocation_count` with each model's generation price.
Amounts are then scaled by window and batch multipliers, stored as
`numeric(24,12)`, and rounded half away from zero at the twelfth decimal.
All pricing math runs in the single Go pricing engine (`internal/usage/rates`)
for gateway admission reservations, settlement and accounting.

## Completeness and unpriced attempts

An attempt is priced only when every dimension its usage reports resolves to
a rate. An input total needs at least one input-side rate (`input_per_million`,
`audio_input_per_million`, or `image_input_per_million`) with every nonzero
share resolvable through its chain, an output total needs an output-side rate
in the same way, media units need a unit price, positive tool counts need their
fees, and batch usage needs a batch multiplier. Anything else is recorded
*unpriced*, meaning it has no estimated cost. It is also *incomplete* when the
usage itself is unsettled. Absent usage is not zero usage.

Open **Unpriced attempts** on the **Usage** page to see the largest groups by
provider, upstream model, operation, and cause. The breakdown counts attempts,
including retries, and uses the selected Usage filters. Incomplete upstream
usage takes precedence over rate issues because the missing usage can hide
which rates apply. When usage is complete, the breakdown distinguishes a missing
model price from a selected price that lacks a required component.

The report reads the cause recorded during accounting. It does not look up
current prices, change pricing revisions, or recalculate past charges. Rows
from before cause tracking show **Cause not recorded**. Retained hourly totals
keep the cause counts for whole hours. Sample requests are only linked when
their detail is still available to you.

## What an attempt records

The fact row keeps every counter it billed (including the modality shares,
tool counts and the batch marker) plus `pricing_components`. That array records
the name of every token rate actually charged with a nonzero quantity after
fallback (for example `input_per_million` for audio billed at the text rate),
`unit_price`, `per_request`, `tool_calls.<name>` per charged tool, the retained
components `training_per_million`, `storage_per_gib_day`,
`cache_storage_per_million_token_hour` and `container_per_session.<tier>`, and
`tiers`, `windows`, `batch_multiplier`, `discount`, `commitment` and
`response_cache` when they applied. The fact row also keeps the money
decomposition in `list_cost` (list-price equivalent before any discount),
`contracted_cost`, `cost_lines` and `charge_lines`. FOCUS and the calculator
read those lines for each component's quantity, unit, list price and contracted
unit price. The attempt also records `caller_credential` for attempts the
caller paid. When the stored line totals do not match the fact's `list_cost`
and `contracted_cost` (or `charge` on the charge side) at 12 decimal places,
accounting clears only `cost_lines` or `charge_lines` to null while keeping
`estimated_cost`, `list_cost`, `contracted_cost` and `charge` intact. The
attempt remains priced, and [FOCUS exports](finops.md#row-grain-and-price-lines)
fall back to a single `attempt` line priced from the fact totals.

## Metering

Usage shares come from the provider's own accounting fields, and the gateway
never invents a share it was not given. When a provider's detail contradicts
its totals (a share larger than the total it is part of, or a cached share
larger than the cached tokens it rides on), the modality and tool detail is
dropped and the attempt settles *incomplete* rather than being billed at text
rates.

| Provider | Mapping |
| --- | --- |
| OpenAI chat and text completions | `prompt_tokens_details.cached_tokens` maps to cached input. `prompt_tokens_details.cache_write_tokens` maps to cache-write input (a count above the prompt tokens not already cached is a protocol error). `prompt_tokens_details.audio_tokens` maps to audio input, and `completion_tokens_details.audio_tokens` maps to audio output. Text completions also require `total_tokens`, when present, to equal prompt plus completion tokens. |
| OpenAI realtime (`response.done`) | `input_token_details.audio_tokens` and `image_tokens` map to audio and image input. `input_token_details.cached_tokens_details.audio_tokens` and `image_tokens` map to cached audio and image input. `output_token_details.audio_tokens` maps to audio output. |
| OpenAI images (generation, edit, variation) | `input_tokens_details.image_tokens` maps to image input. `output_tokens_details.image_tokens` maps to image output, or every output token when no output breakdown is reported. |
| OpenAI speech | Every output token maps to audio output. |
| OpenAI transcription | `input_token_details.audio_tokens` maps to audio input. |
| OpenAI responses | Completed `web_search_call` items map to `web_search`, and completed `file_search_call` items map to `file_search`. |
| Anthropic messages | `usage.server_tool_use.web_search_requests` maps to `web_search`, and `web_fetch_requests` maps to `web_fetch`. Other server-tool counters are ignored. |
| Mistral OCR | `usage_info.pages_processed` maps to media units times `unit_price`. Other usage fields are ignored. |
| Search (Perplexity, Tavily, Exa, Parallel, Brave) | No provider usage is metered, and one successful request maps to `per_request`. Provider-reported credits or `costDollars` are not used for accounting. |
| Gemini generateContent, streaming, Live and interactions | `promptTokensDetails` modality `AUDIO`, `IMAGE`, or `VIDEO` maps to audio or image input, and `cacheTokensDetails` maps in the same way to cached audio or image input. `candidatesTokensDetails` (or `responseTokensDetails` for Live) `AUDIO` and `IMAGE` map to audio and image output, while `TEXT` and `DOCUMENT` stay in the text share. Grounding `webSearchQueries` across candidates map to `google_search_query` per query plus `google_search_prompt` once per grounded prompt. |

A price must carry a fee for every tool name a route's provider can meter. The
fee may be `"0"`, while a missing fee makes the attempt bill as unpriced.
Gemini Interactions usage carries modality shares, while its grounding is not
metered. Only generateContent, streaming and Live grounding counts.

## Commitments

A provider can declare provisioned capacity as a connection option:

```json
"options": { "commitment": { "amount": "300.00", "period": "day" } }
```

`amount` is a positive decimal in the provider's billing currency and `period`
is a UTC calendar `hour`, `day` or `month`. The option is part of the provider
configuration, so it travels with provider revisions and configuration export.

A committed attempt has zero marginal cost. The admission bound reserves
nothing, the settled cost and the `X-OLP-Cost` response header report zero,
and the business cost metric records zero. The attempt still prices against
the list price. Its list cost becomes `commitment_weight`, the share of the
commitment it consumed, and `commitment` joins its `pricing_components`. An
attempt whose usage cannot be priced is unpriced as usual and gets no weight.

Publication establishes a durable installation obligation. First activation
incurs the full current UTC period without proration. Disabling traffic retains
the obligation. Amount changes and removal apply after the current period, and
changing between hours, days and months waits for the next boundary shared by
both periods. The latest publication replaces pending intent, and republishing
unchanged terms creates no additional obligation. Unpublished drafts incur none.
The runtime snapshot carries effective and pending schedules. Each attempt pins
coverage at its start, so a boundary crossed during inference does not change
settlement or require an inference-time database lookup.

Usage reports allocate the full commitment by eligible list-price weight across
the entire period, before project or route filtering. Caller-paid and
budget-exempt attempts are excluded. Cumulative allocation at 12 decimal places
assigns every rounding residual deterministically. With no positive eligible
weight, the full amount remains unallocated installation cost, and
[FOCUS exports](finops.md#commitments) report it as `Unused`. This represents
monetary allocation rather than physical utilization. Unpriced or incomplete
metering keeps coverage incomplete, even when all cost is currently unallocated.
Open periods remain provisional.

Published snapshots and retained accounting evidence support historical FOCUS
regeneration, and missing historical evidence is explicitly marked incomplete.
Full-period allocation context remains available for retained hourly reporting.

## Discounts

A discount is an immutable, effective-dated multiplier on provider cost,
created through `POST /api/v1/pricing/discounts`. Its scope follows the price
rules. `provider_kind` is required, `vendor_id` narrows it to a vendor of that
kind, and `provider_id` narrows it to one provider of that kind (plugin
providers require a provider). `multiplier` obeys the price decimal rule and
`effective_at` defaults to now. Nothing is updated or deleted, so ending a
scope means publishing a new discount at `1`.

At an attempt's observed time, the most specific scope in force wins, with
provider scope over vendor scope and vendor scope over kind-only scope. The next
tie-breakers are the newest effective time, newest creation time and greatest
id. The vendor derives exactly as prices do. A discount scales the whole usage
charge inside the same rounding as window and batch multipliers, so
`cost = round(charge * window * batch * discount, 12)` applies once. `list_cost`
is unaffected, so commitment weights are untouched, and a committed attempt
stays zero whatever the multiplier. `discount` joins `pricing_components` when
a multiplier other than one applied, and the fact keeps `discount_id`. Baseline
(selector savings) pricing selects a discount for the baseline provider the same
way.

The gateway pins the discount on each planned attempt by recording
`discount_pinned` and `discount_id` (which may be null) in the routing policy.
A pinned null prices at one rather than asking accounting to resolve a discount
later. Without a pin, accounting selects at the observed time.

## Rate cards and charge

*Cost* is what the provider bills, and *charge* is what the consumer owes. A
rate card belongs to the installation, one organization or one project, with at
most one card per owner. It publishes immutable revisions, each a whole revision
with `markup` (a multiplier on cost) and optional explicit prices in the
price-entry shape scoped by `provider_kind`, optional `vendor_id`, `model` and
`operation`, never `provider_id`. Every component is allowed, and the currency
is the installation's.

An attempt's consumer is the aggregate-ownership project, resolved from the API
key's project, else the provider's, else the route's. The card resolved is the
most specific card with a revision in force. Resolution checks the project's
card, then its organization card, then the installation card, taking the newest
revision in force at the resolution time. A card whose revisions all start in
the future falls through to the next scope, and revisions never layer.

| Case | Charge |
| --- | --- |
| Card's explicit price matches (vendor-scoped first) | Evaluated directly on the card price at discount 1. Commitments and discounts never apply. |
| No explicit match, card markup | `round(cost * markup, 12)`, or NULL when cost is NULL. |
| No card | Cost itself, with `rate_card_revision_id` NULL. |
| Not billable | NULL. |

A card can therefore charge what the provider could not price (explicit price
with NULL cost), and a markup card over an unpriced cost leaves the charge NULL.
Such billable attempts count toward `uncharged_attempt_count` in usage reports
and CSV exports. A committed attempt has zero cost while still charging through
the card, and explicit rates price its usage normally. Facts store `charge` and
`rate_card_revision_id`, and request history, summaries, breakdowns, series, CSV
and hourly rollups carry them.

The gateway resolves the card for requests with an authority project at
admission and pins `rate_card_pinned` and `rate_card_revision_id`, including a
null revision, on every attempt. Other consumers resolve and pin the card at
each attempt's start when routing inputs are fresh, including a null revision
when no card applies. Accounting preserves those pins even when a newer card
becomes effective before completion. Missing or stale inputs leave the attempt
unpinned, so accounting resolves its card at the event's observed time.

## Budget basis

Every consumer budget chooses a `cost_basis`. The budget owners are API key,
budget group, installation, organization, project, key-route, attribution and
end user. The default `cost` basis enforces provider cost. The `charge` basis
enforces the charge a rate card produces. A billable attempt whose charge is
NULL counts as an uncharged attempt and adds zero, while supply-side caps always
measure provider cost. See [access](../use/access.md#installation-and-project-budgets)
for where each budget is configured.

## Estimates and quotes

Batch results use the price pinned at submission for the line operation
(normally `generation`). Each result's observed usage is priced and multiplied
by the pinned `batch_multiplier`. A missing multiplier leaves the result
unpriced and is never treated as one or zero. Vertex emits one result per
prediction line, while Bedrock's manifest yields one aggregate result. The
settlement event uses origin `batch_settlement` and links to the parent batch
creation request, and errors without usage create no cost fact.

Bedrock batch manifests contain aggregate token counts. Settlement prices these
only with linear token tariffs, because per-request fees, input tiers,
specialized token rates, unit fees and tool fees require per-record evidence.
If either the pinned provider tariff or consumer rate card requires that
evidence, the aggregate remains unpriced and billing-uncertain, while the
observed token totals are retained.

Simulation decisions carry `estimated_cost` per target. The `minimum` figure
prices the input estimate at the cheapest input-side rate of the selected tier
plus the per-request fee, scaled by the window multiplier at the instant and
the pinned discount. The `maximum` value is the bound the gateway would
reserve, using the highest input-side rate for the estimate, the declared or
default reply allowance for generation, the per-request fee, the window
multiplier at the instant, and the attempt's pinned discount. Generation
estimates also carry the priced input tokens and reserved output tokens behind
the maximum, and each decision separately carries the effective `max_cost`
ceiling, null when unset. The `bounded_cost` routing strategy orders generation
candidates by that same maximum within each tier. A committed provider reports
zero for both. The pair is null when the target has no price, the price cannot
bound the request, or no token demand is known. Server-side tool fees and batch
multipliers are excluded from both figures.

`POST /api/v1/pricing/quote` prices one attempt's usage document exactly as
accounting records it. It uses the same price selection, discount, commitment
and rate-card charge in a read-only transaction that writes and audits nothing.
It takes `provider_id`, `model`, `operation`, the usage dimensions, and
optionally `started_at` (default now), `successful` (default true) and
`project_id` naming the consumer whose card prices the charge. When `project_id`
is absent, the installation's card applies.
