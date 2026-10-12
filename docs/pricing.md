# Pricing

A pricing revision is an immutable list of price entries that takes effect at
one instant. Accounting considers every revision in force at an attempt's time
and picks the most specific matching entry: a provider-scoped entry beats a
vendor-scoped one, which beats a provider-kind one; among equally specific
entries the newest `effective_at` wins, then the newest revision number.
Entries are scoped by `provider_kind`, optional `vendor_id`, optional
`provider_id`, `model` (the upstream model the provider was asked for) and
`operation`, and carry one `currency`.

Revisions are created through `POST /api/v1/pricing/revisions`, published from
[pricing sources](operations.md#accounting-delivery-and-shutdown), or applied as
part of a configuration document. A revision never changes after creation, so a
stored cost stays explicable in the terms it was charged under.

## Price components

Every rate, fee and multiplier is a decimal string with no redundant zeros.
All components are optional; an absent component is simply not billed.

| Member | Charges |
| --- | --- |
| `input_per_million` | The text share of the input tokens, per million. |
| `cached_input_per_million` | Cached text input. Falls back to the input rate. Requires the input rate. |
| `cache_write_input_per_million` | Generic cache writes. Falls back to the input rate. Requires the input rate. |
| `cache_write_5m_input_per_million`, `cache_write_1h_input_per_million` | TTL-classed writes; each falls back to the generic write rate, then input. Requires the input rate. |
| `output_per_million` | The text share of the output tokens, per million. |
| `audio_input_per_million` | The audio share of input. Falls back to the input rate. |
| `cached_audio_input_per_million` | Cached audio input. Falls back to cached input, then input. |
| `audio_output_per_million` | The audio share of output. Falls back to the output rate. |
| `image_input_per_million` | The image share of input. Falls back to the input rate. |
| `cached_image_input_per_million` | Cached image input. Falls back to cached input, then input. |
| `image_output_per_million` | The image share of output. Falls back to the output rate. |
| `unit_price` | A fixed amount per media unit. |
| `per_request` | A fixed fee charged once per successful attempt. |
| `batch_multiplier` | Multiplies the whole usage charge of attempts marked batch. |
| `tool_calls` | Fees per metered server-side tool invocation, `{tool: fee}`, 1–16 names matching `^[a-z][a-z0-9_]{0,63}$`. |
| `tiers` | Input-token tiers; see below. |
| `windows` | Time-of-day multipliers in a named IANA zone; see below. |

An entry must name at least one of input, output, any audio or image rate,
unit price, per-request, or tool fees.

## Modality shares

Cached and modality counts are shares of the input and output totals:
`audio_input_tokens` is part of `input_tokens`, never additional. The input side bills

- text = input − cached − writes − uncached audio − uncached image
- cached text = cached − cached audio − cached image
- each modality share at the first rate its fallback chain carries

so a price without modality rates bills every share at the text rates and
returns identical amounts to one that never heard of them. The output side
bills the audio and image shares the same way.

## Tiers

`tiers` lists 1–8 thresholds `{above_input_tokens, <per-million fields>}` in
strictly increasing order. A tier applies when the attempt's input tokens are
strictly greater than its threshold; the highest exceeded tier replaces the
entry's whole per-million rate set for that attempt. A tier therefore names
exactly the per-million fields its entry names. Per-request fees, tool fees,
windows and the batch multiplier are entry-level and apply regardless of tier.

## Windows

`windows` is `{time_zone, periods}` with 1–32 periods, each
`{days, start, end, multiplier}`:

- `days` are unique ISO weekdays, 1 = Monday to 7 = Sunday.
- `start` is `HH:MM` local time, inclusive; `end` is `HH:MM` or `"24:00"`,
  exclusive, and must be later.
- Periods never overlap on a shared day.

The attempt's start instant is evaluated in the named zone — which must be a
valid IANA name the database can resolve — and the matching period's multiplier
applies to the whole usage charge. No matching period means a multiplier of
one.

## Tool fees

`tool_calls` maps a tool name to a fee per invocation. A metered tool with a
positive invocation count and no configured fee makes the attempt unpriced;
zero-count tools are ignored.

Usage is marked `batch` only by batch-result settlement, which no surface
produces yet, so the marker and its multiplier are inert until one does.

## The bill

For a complete attempt the charge is

```
input_charge/1e6 + output_charge/1e6 + media_units × unit_price
  + per_request (successful attempts only)
  + Σ count × tool_fee
```

each side divided by a million once, then scaled by window × batch. Amounts are stored `numeric(24,12)`, rounded half away from zero at
the twelfth decimal — the same figure the gateway's Go mirror computes for
admission reservations and settlement.

## Completeness and unpriced attempts

An attempt is priced only when every dimension its usage reports resolves to
a rate: an input total needs at least one input-side rate (input, audio input
or image input) with every nonzero share resolvable through its chain, an
output total likewise, media units need a unit price, positive tool counts need
their fees, and batch usage needs a batch multiplier. Anything else is
recorded *unpriced* — no estimated cost — and *incomplete* when the usage
itself is unsettled. Absent usage is not zero usage.

## What an attempt records

The fact row keeps every counter it billed (including the modality shares,
tool counts and the batch marker) plus `pricing_components`: the name of every
rate actually charged with a nonzero quantity after fallback (for example
`input_per_million` for audio billed at the text rate), `tool_calls.<name>`
per charged tool, and `tiers`, `windows`, `batch_multiplier` and `per_request`
when they applied.

## Metering

Usage shares come from the provider's own accounting fields; the gateway never
invents a share it was not given. When a provider's detail contradicts its
totals (a share larger than the total it is part of, a cached share larger
than the cached tokens it rides on), the modality and tool detail is dropped
and the attempt settles *incomplete* rather than being billed at text rates.

| Provider | Mapping |
| --- | --- |
| OpenAI chat completions | `prompt_tokens_details.audio_tokens` → audio input; `completion_tokens_details.audio_tokens` → audio output |
| OpenAI realtime (`response.done`) | `input_token_details.audio_tokens`/`image_tokens` → audio/image input; `input_token_details.cached_tokens_details.audio_tokens`/`image_tokens` → cached audio/image input; `output_token_details.audio_tokens` → audio output |
| OpenAI images (generation, edit, variation) | `input_tokens_details.image_tokens` → image input; `output_tokens_details.image_tokens` → image output, or every output token when no output breakdown is reported |
| OpenAI speech | every output token → audio output |
| OpenAI transcription | `input_token_details.audio_tokens` → audio input |
| OpenAI responses | completed `web_search_call` items → `web_search`; completed `file_search_call` items → `file_search` |
| Anthropic messages | `usage.server_tool_use.web_search_requests` → `web_search`; `web_fetch_requests` → `web_fetch`; other server-tool counters are ignored |
| Gemini generateContent / streaming / Live / interactions | `promptTokensDetails` modality `AUDIO`/`IMAGE`/`VIDEO` → audio/image input; `cacheTokensDetails` likewise → cached audio/image input; `candidatesTokensDetails` (Live: `responseTokensDetails`) `AUDIO`/`IMAGE` → audio/image output; `TEXT` and `DOCUMENT` stay in the text share; grounding `webSearchQueries` across candidates → `google_search_query` per query plus `google_search_prompt` once per grounded prompt |

A price must carry a fee for every tool name a route's provider can meter —
possibly `"0"` — or the attempt bills as unpriced. Gemini Interactions usage
carries modality shares but its grounding is not metered; only
generateContent, streaming and Live grounding counts.

## Commitments

A provider can declare provisioned capacity as a connection option:

```json
"options": { "commitment": { "amount": "300.00", "period": "day" } }
```

`amount` is a positive decimal in the provider's billing currency and `period`
is a UTC calendar `hour`, `day` or `month`. The option is part of the provider
configuration, so it travels with provider revisions and configuration export.

A committed attempt's marginal cost is zero: the admission bound reserves
nothing, the settled cost and the `X-OLP-Cost` response header report zero,
and the business cost metric records zero. The attempt still prices against
the list price — its list cost becomes `commitment_weight`, the share of the
commitment it consumed — and `commitment` joins its `pricing_components`. An
attempt whose usage cannot be priced is unpriced as usual and gets no weight.

Usage reports allocate the commitment across the period's committed usage by
list-cost weight: each usage row earns `amount × weight / W`, where `W` is the
total weight of *every* committed fact and retained hourly row in that
provider's `(period, UTC bucket)` — regardless of the report's own filters —
and `amount` is the commitment the bucket's latest row recorded. A report that
fully covers a closed period allocates the whole amount; a period still open
allocates provisionally; a period with no committed usage allocates nothing —
idle capacity is not attributed to anyone.

## Discounts

A discount is an immutable, effective-dated multiplier on provider cost,
created through `POST /api/v1/pricing/discounts`. Its scope follows the price
rules: `provider_kind` is required, `vendor_id` narrows it to a vendor of that
kind and `provider_id` to one provider of it (plugin providers require a
provider). `multiplier` obeys the price decimal rule and `effective_at`
defaults to now. Nothing is updated or deleted: ending a scope means
publishing a new discount at `1`.

At an attempt's observed time, the most specific scope in force wins —
provider over vendor over kind-only — then the newest effective time, newest
creation time and greatest id. The vendor derives exactly as prices do. A
discount scales the whole usage charge inside the same rounding as window and
batch multipliers, so `cost = round(charge × window × batch × discount, 12)`
applies once. `list_cost` is unaffected, so commitment weights are untouched;
a committed attempt stays zero whatever the multiplier. `discount` joins
`pricing_components` when a multiplier other than one applied, and the fact
keeps `discount_id`. Baseline (selector savings) pricing selects a discount
for the baseline provider the same way.

The gateway pins the discount on each planned attempt: the routing policy
records `discount_pinned` and `discount_id`, which may be null — a pinned
null prices at one, not "resolve later". Without a pin, accounting selects at
the observed time.

## Budget basis

Every consumer budget — API-key, budget-group, installation, organization,
project, route, key-route, attribution and end-user — chooses `cost_basis`:
`cost` (the default) enforces provider cost and `charge` enforces the charge a
rate card produces. A billable attempt whose charge is NULL counts as an
uncharged attempt and adds zero; supply-side caps always measure provider
cost. See [access](access.md#installation-and-project-budgets) for where each
budget is configured.

## Rate cards and charge

*Cost* is what the provider bills; *charge* is what the consumer owes. A rate
card belongs to the installation, one organization or one project — at most
one card per owner — and publishes immutable revisions, each a whole revision
with `markup` (a multiplier on cost) and optional explicit prices in the
price-entry shape scoped by `provider_kind`, optional `vendor_id`, `model`
and `operation`, never `provider_id`. Every component is allowed; the
currency is the installation's.

An attempt's consumer is the aggregate-ownership project: the API key's
project, else the provider's, else the route's. The card resolved is the
most specific card with a revision in force — the project's card, then its
organization's, then the installation's — taking the newest revision in force
at the observed time, and a card whose revisions all start in the future
falls through to the next scope; revisions never layer.

| Case | Charge |
| --- | --- |
| Card's explicit price matches (vendor-scoped first) | price_usage on the card price at discount 1 — commitments and discounts never apply |
| No explicit match, card markup | `round(cost × markup, 12)`, NULL when cost is NULL |
| No card | cost itself, `rate_card_revision_id` NULL |
| Not billable | NULL |

So a card can charge what the provider could not price (explicit price, NULL
cost), and a markup card over an unpriced cost leaves the charge NULL — such
billable attempts count toward `uncharged_attempt_count` in usage reports and
CSV. A committed attempt has zero cost but still charges through the card:
explicit rates price its usage normally. Facts store `charge` and
`rate_card_revision_id`; request history, summaries, breakdowns, series, CSV
and hourly rollups carry them.

The gateway resolves the card for requests with an authority project at
admission and pins `rate_card_pinned` and `rate_card_revision_id` — null
included — on every attempt; keyless origins stay unpinned and accounting
resolves the card itself.
