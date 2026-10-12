# M6: Cost management and chargeback

| Status | Depends on | Unlocks |
| --- | --- | --- |
| In progress | [M2](m02-provider-catalog.md), [M4](m04-tenancy-identity.md), [M5](m05-observability.md) | FinOps adoption |

OLP already has exact-decimal pinned prices, fail-closed hierarchical budgets,
completeness evidence and M5 metadata exports. This milestone adds remaining
price structures, chargeback, standalone cost estimates and FinOps/billing formats;
it does not replace existing admission estimates or sink delivery.

## Outcome

- Every list price structure the major vendors publish is representable, so
  catalog prices are complete rather than approximated.
- Projects receive rate-carded charges distinct from provider cost, and
  negotiated discounts apply by scope.
- Callers and operators see a cost estimate before dispatch and can cap it.
- Cost exports in the FOCUS format and to metering systems.
- Attributed spend reconciles against provider invoices.

## Baseline

Planning-time comparison; shipped behavior is authoritative in the
[compatibility matrix](../compatibility.md) and [access guide](../access.md).

| | Planning baseline | LiteLLM reference |
| --- | --- | --- |
| Price components | Input, cached input, cache writes (generic, 5-minute, 1-hour), output and a unit price per revision entry ([`pricing.go`](../../internal/usage/pricing.go)) | Model cost map components, [off-peak](https://docs.litellm.ai/docs/proxy/off_peak_pricing) and [provisioned throughput](https://docs.litellm.ai/docs/proxy/ptu_flat_cost) pricing |
| Adjustments | Publish-time overrides of source snapshots | [Discounts](https://docs.litellm.ai/docs/proxy/provider_discounts) and [margins](https://docs.litellm.ai/docs/proxy/provider_margins) |
| Estimation | `max_price` routing constraints on per-million rates | [Pricing calculator](https://docs.litellm.ai/docs/proxy/pricing_calculator) |
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
| `per_request` | A fixed fee per successful request |
| `tool_calls` | Fees per server-side tool invocation, keyed by tool type (for example web search) |
| `windows` | Time-of-day and day-of-week multipliers in a named IANA time zone (off-peak pricing) |
| `batch_multiplier` | The discount applied to batch results ([M9.2](m09-api-surface.md#m92-files-and-batches-across-providers)) |

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
  estimate and the request's output limit.
- `X-OLP-Routing` accepts `max_cost`, a ceiling on the estimated worst-case
  cost of one request; candidates above it are skipped, and a request with none
  left fails with `400 cost_limit_exceeded` before dispatch. Like other routing
  controls, it can only narrow selection.
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

### M6.5 Invoice reconciliation

A worker task imports provider-reported cost for connections that supply admin
credentials: the OpenAI organization costs API, the Anthropic usage and cost
API, AWS Cost and Usage Reports, Google Cloud billing export and Azure Cost
Management. The console and API compare attributed spend with invoiced spend by
connection and UTC day, and show the capture rate and the unpriced share.
Admin credentials use a dedicated `billing_credential` seal purpose and never
serve inference. LiteLLM has announced an OpenAI-only
[capture-rate check](https://docs.litellm.ai/docs/proxy/spend_capture_rate) that
is not yet released.

## Change map

| Change | Start here |
| --- | --- |
| Price components, discounts, rate cards | `internal/usage/pricing.go`, `internal/usage/pricing_select.go` |
| Estimation | `internal/runtime/plan.go`, `internal/routes/simulate.go` |
| FOCUS, CloudEvents, Stripe | `internal/export/` from [M5](m05-observability.md) |
| Reconciliation | `internal/usage/`, `internal/process/workers.go` |
| Console | `console/src/lib/features/usage/` |

## Implementation decisions

M6.1 to M6.3 are implemented first; M6.4 and M6.5 follow with the decisions
below already settled.

1. **Budget basis.** Every consumer budget (key, budget group, end user,
   installation, organization, project, attribution and per-key route caps)
   chooses `cost` or `charge` as its `cost_basis`, defaulting to `cost`, so
   internal consumers can be capped in the amount they are billed. Supply-side
   caps on connections, slots and routes always enforce provider cost.
2. **Commitment allocation.** A commitment is allocated by list-price-weighted
   share: each committed attempt weighs what it would have cost at list price,
   so cheap and expensive models on one commitment allocate fairly.
3. **Reconciliation providers.** M6.5 starts with the OpenAI and Anthropic cost
   APIs; AWS Cost and Usage Reports, Google Cloud billing export and Azure Cost
   Management follow.
4. **FOCUS format.** M6.4 writes FOCUS 1.4 datasets as gzip-compressed CSV,
   which every FOCUS consumer and the FOCUS validator read, so no columnar
   serialization dependency is added.
5. **Cached modality rates.** Vendors price cached audio and image tokens apart
   from cached text, so `cached_audio_input_per_million` and
   `cached_image_input_per_million` join the components listed in M6.1.
6. **Tiers.** A tier applies when a request's input tokens, cache reads and
   writes included, exceed its `above_input_tokens`; the highest such tier
   replaces the entry's whole per-million rate set for every token of the
   request. A rate the tier omits falls back within the tier, as the entry's
   own rates do, never to the entry's rate.
7. **Multipliers.** Window and batch multipliers scale an attempt's whole usage
   charge. Windows are evaluated at the attempt's start in the period's IANA
   time zone. `batch_multiplier` applies to usage marked as batch results,
   which M9.2 produces.
8. **Tool calls.** A fee applies per metered server-side invocation. An attempt
   that used a tool its price names no fee for is unpriced, like any other
   dimension without a rate. Anthropic `server_tool_use` counters, OpenAI
   Responses web and file search calls and Gemini grounding search queries are
   metered.
9. **Per-request fees** apply to successful attempts only.
10. **Commitments** are a connection option published with provider revisions:
    an amount per UTC hour, day or month. Committed attempts cost nothing and
    record their list cost as their allocation weight; an attempt with no list
    price stays unpriced. Usage reports allocate each period's amount across
    the period's committed attempts; a period with none allocates nothing, and
    an open period's allocation is provisional.
11. **Discounts** are immutable, effective-dated multipliers. The most specific
    scope wins (connection, then vendor, then connector kind), then the newest
    effective time. The gateway pins the discount it priced an attempt with, as
    it pins the pricing revision.
12. **Rate cards** belong to the installation, an organization or a project and
    publish immutable revisions: a markup on cost plus optional explicit rates
    in the price format. The project's card applies, else its organization's,
    else the installation's. An explicit rate prices matching usage itself;
    otherwise the charge is the cost times the markup. Without a card the
    charge is the cost.
13. **Catalog format.** Catalogs carrying the new components are
    `openllmproxy.dev/catalog/v2`; strict decoding means an older release
    refuses them as an unsupported version rather than misreading them.
14. **`max_cost`** is checked against the same worst-case bound the gateway
    reserves for an attempt. A candidate with no price cannot be shown to fit
    and is skipped.

## Exit criteria

- [x] Every price component above round-trips through revisions, sources,
      snapshots and configuration export, and accounting tests price each one
      exactly.
- [x] The [reference catalog](m02-provider-catalog.md#m24-reference-catalog)
      prices every catalog model with no `unrepresentable` component left for
      structures listed above.
- [x] Usage reports show cost and charge side by side, with commitments
      allocated.
- [x] `max_cost` refuses requests whose worst case exceeds the ceiling, before
      dispatch, in integration tests.
- [ ] A FOCUS dataset validates against the pinned specification's schema, and
      CloudEvents and Stripe meter deliveries are idempotent under redelivery.
- [ ] Reconciliation reports a capture rate for OpenAI and Anthropic
      connections against recorded fixtures.
- [ ] The [parity matrix](parity.md) cost rows are `Parity` or better.
## Delivered and verified

M6.1 to M6.3 are implemented; M6.4 and M6.5 remain planned, so the milestone
stays in progress.

- **Price components.** Pricing revisions, price-list sources, catalog
  snapshots and configuration documents carry modality and cached-modality
  rates, input-token tiers, time-of-day windows, per-request and tool-call
  fees and the batch multiplier. One PostgreSQL function prices every attempt
  and the gateway's Go mirror reproduces it exactly for reservations,
  settlement and `X-OLP-Cost`; a parity suite holds the two together.
- **Metering.** OpenAI, Anthropic and Gemini usage populates audio and image
  shares and server-side tool calls; inconsistent provider detail settles the
  attempt incomplete rather than at text rates.
- **Commitments.** Connection commitments bill nothing per attempt and are
  allocated in usage reports by list-price-weighted share.
- **Reference catalog v2.** Every component the catalog listed as
  unrepresentable in a structure OLP now prices is transcribed, independently
  checked against the vendor figures it came from; what remains carries an
  accurate residual name.
- **Discounts, rate cards and charge.** Usage records carry cost and charge
  with their provenance; reports, exports and request history show both, and
  every consumer budget enforces either.
- **Estimation.** Route simulation states minimum and maximum cost per target,
  `max_cost` refuses requests whose worst case exceeds it before dispatch, and
  the console calculator prices routes and exact usage.

<!-- H7: fill the verification paragraph from the final gate outputs: which
make targets passed, integration counts, bench-gate result. State plainly any
suite that could not run in this environment. -->
