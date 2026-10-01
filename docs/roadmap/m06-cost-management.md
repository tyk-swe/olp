# M6: Cost management and chargeback

| Status | Depends on | Integrates with | Unlocks |
| --- | --- | --- | --- |
| Planned | [M2](m02-provider-catalog.md), [M4](m04-tenancy-identity.md), [M5](m05-observability.md) | [M1](m01-measured-advantage.md) (token estimates), [M9](m09-api-surface.md) (batch pricing) | None |

OLP's accounting is already more rigorous than most gateways: exact decimals,
pricing revisions pinned per attempt, fail-closed budgets and explicit
completeness evidence. Its pricing model, however, cannot express several real
price structures, and its spend data stays inside OLP. This milestone completes
the pricing model, adds internal chargeback, estimates cost before dispatch, and
exports cost to FinOps and billing systems.

## Outcome

- Every list price structure the major vendors publish is representable, so
  catalog prices are complete rather than approximated.
- Projects receive rate-carded charges distinct from provider cost, and
  negotiated discounts apply by scope.
- Callers and operators see a cost estimate before dispatch and can cap it.
- Cost exports in the FOCUS format and to metering systems.
- Attributed spend reconciles against provider invoices.

## Baseline

| | OLP today | LiteLLM reference |
| --- | --- | --- |
| Price components | Input, cached input, cache writes (generic, 5-minute, 1-hour) and output per million tokens, and a `unit_price` multiplied by the units an operation reports ([`pricing.go`](../../internal/usage/pricing.go)) | Model cost map components, [off-peak](https://docs.litellm.ai/docs/proxy/off_peak_pricing) and [provisioned throughput](https://docs.litellm.ai/docs/proxy/ptu_flat_cost) pricing |
| Adjustments | Publish-time overrides of source snapshots | [Discounts](https://docs.litellm.ai/docs/proxy/provider_discounts) and [margins](https://docs.litellm.ai/docs/proxy/provider_margins) |
| Estimation | `max_price` routing ceilings on per-million rates and unit price. Cost budgets refuse on accrued spend, not on an estimate | [Pricing calculator](https://docs.litellm.ai/docs/proxy/pricing_calculator) |
| Export | Usage API | [FOCUS](https://docs.litellm.ai/docs/observability/focus) (experimental), [Lago](https://docs.litellm.ai/docs/observability/lago), [OpenMeter](https://docs.litellm.ai/docs/observability/openmeter) |

## Scope

### M6.1 Pricing dimensions

Pricing revision entries gain these components. Each stays an exact decimal
string multiplied in PostgreSQL, and each is optional; an absent component keeps
today's meaning.

| Component | Meaning |
| --- | --- |
| `tiers` | Ordered thresholds on input tokens per request, each replacing any per-million rate above it (long-context pricing) |
| `audio_input_per_million`, `audio_output_per_million` | Audio token rates, separate from text |
| `image_input_per_million`, `image_output_per_million` | Image token rates for multimodal generation |
| `per_request` | A fixed fee per successful request, added to its token cost. Today's `unit_price` stays the price of operations billed by reported units, such as images, seconds or searches |
| `tool_calls` | Fees per server-side tool invocation, keyed by tool type (for example web search) |
| `windows` | Time-of-day and day-of-week multipliers in a named IANA time zone (off-peak pricing) |
| `batch_multiplier` | The discount applied to batch results. [M9.2](m09-api-surface.md#m92-files-and-batches-across-providers) introduces it with batch settlement if M9 ships first |

Provisioned capacity is a connection-level `commitment`: an amount per period.
Attempts on that connection have zero marginal cost, and usage reports allocate
the commitment across consumers by token share, so chargeback stays complete.

Attempts record which components applied. Usage that a revision cannot price
remains unpriced rather than estimated.

### M6.2 Rate cards and discounts

- **Discounts.** A discount is a decimal multiplier on provider cost scoped to a
  connection, vendor or connector kind, effective from a time, so negotiated
  rates no longer require editing every price.
- **Rate cards.** A rate card belongs to the installation, an organization or a
  project, and defines the charge for usage as a markup on provider cost or as
  explicit rates. Usage facts carry both `cost` (what the provider bills) and
  `charge` (what the consumer is billed), each with its revision provenance.
- Budgets choose which amount they enforce, defaulting to `cost`.

### M6.3 Cost estimation

- Route simulation reports the minimum and maximum cost of each candidate for a
  sample request, from the [M1.2](m01-measured-advantage.md#m12-accurate-admission-token-estimates)
  estimate and the request's output limit. Until M1.2 ships, the estimate is
  today's heuristic and simulation labels it so.
- `X-OLP-Routing` accepts `max_cost`, a ceiling on the estimated worst-case
  cost of one request; candidates above it are skipped, and a request with none
  left fails with `400 cost_limit_exceeded` before dispatch. Like other routing
  controls, it can only narrow selection. It is the first control that prices
  the estimate: budgets keep refusing on accrued spend.
- The console offers a calculator over routes and pricing revisions for
  planning.

### M6.4 FinOps and billing export

- **FOCUS.** A sink stream, `focus`, writes daily cost and usage datasets in the
  FinOps Open Cost and Usage Specification, at a version pinned in the
  configuration reference, to the object-store sinks of
  [M5.1](m05-observability.md#m51-export-sinks).
- **Metering.** An HTTPS sink format, `cloudevents`, emits one CloudEvent per
  priced attempt with consumer, route, usage and cost, for OpenMeter and other
  CloudEvents consumers. A `stripe_meter` sink type reports usage to Stripe
  Billing meters, with idempotent event identifiers.
- Lago and other meters with their own event shape receive the JSON HTTPS
  stream; the billing row closes under the
  [breadth rule](parity.md#how-to-read-the-matrix).

### M6.5 Invoice reconciliation

A worker task imports provider-reported cost for connections that supply admin
credentials: the OpenAI organization costs API, the Anthropic usage and cost
API, AWS Cost and Usage Reports, Google Cloud billing export and Azure Cost
Management. The console and API compare attributed spend with invoiced spend by
connection and UTC day, and show the capture rate and the unpriced share.
Admin credentials use a dedicated `billing_credential` seal purpose and never
serve inference.

This workstream has no parity row. LiteLLM documents an OpenAI-only
[capture-rate check](https://docs.litellm.ai/docs/proxy/spend_capture_rate) as
upcoming, and unreleased work is outside the reference.

## Non-goals

- Invoicing or collecting payment. OLP exports metered usage to billing
  systems.
- Estimating a price for usage no revision can price.
- More than one installation currency.

## Data and secrets

| Data | Where | Retention | Purpose |
| --- | --- | --- | --- |
| New price components, discounts and rate cards | Immutable pricing revisions in PostgreSQL | As pricing revisions today | None |
| `cost` and `charge` per attempt, with applied components | PostgreSQL usage facts | Usage retention | None; metadata only |
| Commitments and their allocation | PostgreSQL | Usage retention | None |
| Provider-reported cost by connection and day | PostgreSQL | Usage retention | None |
| Billing admin credentials | PostgreSQL, sealed | Until rotated | New seal purpose `billing_credential` |
| Stripe meter keys | PostgreSQL, sealed | Until rotated | `sink_credential` from M5.1 |

## Change map

| Change | Start here |
| --- | --- |
| Price components, discounts, rate cards | `internal/usage/pricing.go`, `internal/usage/pricing_select.go`, `internal/usage/pricing_sources.go` |
| Prices in routing and export | `internal/usage/routing_inputs.go`, `internal/configuration/artifact.go` |
| Estimation and `max_cost` | `internal/runtime/plan.go`, `internal/runtime/policy.go`, `internal/routes/simulate.go` |
| FOCUS, CloudEvents, Stripe | `internal/export/` from [M5](m05-observability.md) |
| Reconciliation | `internal/usage/`, `internal/process/workers.go` |
| Console | `console/src/lib/features/usage/` |

## Decisions to settle

1. Whether budgets may enforce `charge` (recommended: yes, chosen per budget, so
   internal consumers can be capped in the currency they are billed in).
2. Commitment allocation: by tokens or by cost-weighted tokens (recommended: by
   the list-price-weighted share, so cheap and expensive models on one
   commitment allocate fairly).
3. The minimum provider set for invoice reconciliation (recommended: OpenAI and
   Anthropic first, then the cloud billing exports).
4. Whether a cost budget may reserve the estimated worst-case cost at
   admission, which would bound today's overshoot (recommended: offer it per
   budget, off by default, because worst-case reservation refuses work that
   would have fit).

## Exit criteria

- [ ] **M6.1** Every price component above round-trips through revisions,
      sources, snapshots and configuration export, and accounting tests price
      each one exactly.
- [ ] **M6.1** The [reference catalog](m02-provider-catalog.md#m24-reference-catalog)
      prices every catalog model with no `unrepresentable` component left for
      structures listed above.
- [ ] **M6.1, M6.2** Usage reports show cost and charge side by side, with
      commitments allocated and each discount applied only from its effective
      time and within its scope.
- [ ] **M6.3** `max_cost` refuses requests whose worst case exceeds the
      ceiling, before dispatch, in integration tests, and the calculator and
      route simulation return the same estimate for the same request.
- [ ] **M6.4** A FOCUS dataset validates against the pinned specification's
      schema, and CloudEvents and Stripe meter deliveries are idempotent under
      redelivery.
- [ ] **M6.5** Reconciliation reports a capture rate for OpenAI and Anthropic
      connections against recorded fixtures.
- [ ] The [parity matrix](parity.md) cost rows are `Parity` or better.
