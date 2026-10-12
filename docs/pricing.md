# Pricing

A pricing revision is an immutable list of price entries that takes effect at
one instant. Accounting prices every attempt against the newest revision whose
`effective_at` has passed, choosing the most specific entry that matches the
attempt: a provider-scoped entry beats a vendor-wide one, a vendor-scoped entry
beats a bare provider kind. Entries are scoped by `provider_kind`, optional
`vendor_id`, optional `provider_id`, `model` (the upstream model the provider
was asked for) and `operation`, and carry one `currency`.

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

Cached and modality counts are shares of the input and output totals, exactly
like cached tokens today: `audio_input_tokens` is part of `input_tokens`, never
additional. The input side bills

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

## The bill

For a complete attempt the charge is

```
input_charge/1e6 + output_charge/1e6 + media_units × unit_price
  + per_request (successful attempts only)
  + Σ count × tool_fee
```

each side divided by a million once, then scaled by window × batch (and by any
discount). Amounts are stored `numeric(24,12)`, rounded half away from zero at
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
