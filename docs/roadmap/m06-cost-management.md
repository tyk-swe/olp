# M6: Cost management and chargeback

| Status | Depends on | Unlocks |
| --- | --- | --- |
| Implemented | [M2](m02-provider-catalog.md), [M4](m04-tenancy-identity.md), [M5](m05-observability.md) | FinOps adoption |

OLP already has exact-decimal pinned prices, fail-closed hierarchical budgets,
completeness evidence and M5 metadata exports. This milestone adds remaining
price structures, chargeback, standalone cost estimates and FinOps/billing formats;
it does not replace existing admission estimates or sink delivery.

The pricing, metering, durable commitment export and OpenAI/Anthropic import
paths have fixture and integration evidence. The commitment implementation now
includes empty periods, exact allocation, retained provenance and the existing
console dataset view. Completed local checks and unrun extended checks are recorded in
[the current evidence record](m06-commitment-evidence.md);
cloud invoice adapters, live billing/metering qualification and candidate-image
performance remain distinct gates.

## Outcome

- The price structures enumerated in M6.1 are representable without floating
  point approximation. Other vendor structures retain explicit
  `unrepresentable` markers in the reference catalog.
- Projects receive rate-carded charges distinct from provider cost, and
  negotiated discounts apply by scope.
- Callers and operators see a cost estimate before dispatch and can cap it.
- Cost exports in the FOCUS format and to metering systems.
- Attributed spend reconciles against imported OpenAI and Anthropic daily cost
  reports, with scope, currency and coverage limits shown to operators.

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
| `per_request` | A fixed fee per successful attempt |
| `tool_calls` | Fees per server-side tool invocation, keyed by tool type (for example web search) |
| `windows` | Time-of-day and day-of-week multipliers in a named IANA time zone (off-peak pricing) |
| `batch_multiplier` | The discount applied to batch results ([M9.2](m09-api-surface.md#m92-files-and-batches-across-providers)) |

Provisioned capacity is a connection-level `commitment`: an amount per period.
Attempts on that connection have zero marginal cost, and usage reports allocate
the commitment across consumers by list-price-weighted share. Open periods are
provisional; periods with no metered usage remain unallocated.

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
  provider cost of one attempt; candidates above it are skipped, and a request
  with none left fails with `400 cost_limit_exceeded` before dispatch. Like other routing
  controls, it can only narrow selection.
- This is an estimate-based candidate filter, not an invoice or total-request
  cap. Retries/fallbacks can produce multiple billable attempts; unreported
  provider dimensions can exceed the local estimate. Use hierarchical budgets
  and the attempt limit alongside it. A commitment has zero marginal attempt
  cost; a noncommitted candidate with no calculable bound is skipped.
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
credentials: the OpenAI organization costs API and the Anthropic usage and cost
API. AWS Cost and Usage Reports, Google Cloud billing export and Azure Cost
Management are deferred adapters, not delivered connections. The console and
API compare attributed spend with invoiced spend by connection and UTC day,
and show the capture rate and the unpriced share.
Admin credentials use a dedicated `billing_credential` seal purpose and never
serve inference. The comparison baseline includes LiteLLM's documented
[capture-rate check](https://docs.litellm.ai/docs/proxy/spend_capture_rate);
its current parity status is owned by the matrix rather than this historical
implementation plan.

## Change map

| Change | Start here |
| --- | --- |
| Price components, discounts, rate cards | `internal/usage/pricing.go`, `internal/usage/pricing_select.go` |
| Estimation | `internal/routing/plan.go`, `internal/routes/simulate.go` |
| FOCUS, CloudEvents, Stripe | `internal/export/` from [M5](m05-observability.md) |
| Reconciliation | `internal/usage/`, `internal/process/workers.go` |
| Console | `console/src/lib/features/usage/` |

## Implementation decisions

The implemented scope uses the following settled contracts.

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

## Settled during implementation

1. **Price lines.** Every fact stores its exact `cost_lines`/`charge_lines`
   decomposition — quantity, unit and list/contracted unit price per component
   and tier — because FOCUS 1.4 requires `ListUnitPrice` and
   `ContractedUnitPrice` per row, and a recomputed average cannot reproduce a
   tier boundary or fallback. When a fact's stored lines do not sum to its
   totals, exports fall back to one `attempt` line and the manifest counts it.
2. **Conformance interpreter.** `internal/export/focusspec` interprets every
   Static rule of the pinned 1.4 requirements model; the official
   focus-validator 2.2.1 crashes on a dependency cycle through the Dynamic
   CommitmentDiscountQuantity rules and mis-evaluates removed and conditional
   rules, so it cannot serve. The interpreter applies the documented errata:
   deprecated ProviderName/PublisherName presence rules skipped and null-safe
   requirement comparisons.
3. **One metering stream.** CloudEvents and Stripe meter sinks both consume
   the `metering` stream, so the priced-attempt payload, consumer resolution
   and retention are shared, and Stripe-only filtering (mapped customer,
   positive value) happens at enqueue.
4. **Stripe idempotency.** A deterministic `olp-<sink>-<attempt>` identifier
   doubles as `Idempotency-Key`; `duplicate_meter_event` counts as delivered.
5. **Caller-paid exclusion.** Caller-credential and budget-exempt attempts are
   excluded from the FOCUS cost basis and from reconciliation attributed cost,
   because the provider does not invoice the installation for them; the charge
   basis and usage reports keep them.
6. **FOCUS restatement.** A dirty day regenerates no sooner than one hour
   after its previous delivery, as the next revision under `revision=NNNN`
   keys; retries rewrite the same revision so revisions never gap, and the
   manifest is always the last object written.
7. **Commitments in FOCUS.** Published schedules incur durable periods even
   without usage. The original bucket-start day carries one Purchase and, for
   unallocated cost, an installation-scoped `Unused` row. Eligible usage receives
   exact 12-decimal allocation before consumer filtering. Effective/pending
   schedules are pinned at attempt start; disabled connections retain obligations.
   See [lifecycle and coverage rules](../finops.md#commitments).
8. **Reconciliation compare.** `capture_rate` divides attributed cost over
   covered days only (`attributed_cost_covered`) by invoiced cost; daily
   `unavailable_reason` distinguishes `not_imported`, `zero_invoice` and
   `currency_mismatch`. Hourly rollup rows only approximate priced/incomplete
   counts and cannot identify caller-paid attempts, so the budget-exempt share
   of their request count is counted as excluded.
9. **No implicit foreign-exchange conversion.** Cost comparisons remain in the
   installation's pricing currency. A mismatched imported currency is disclosed
   and excluded from the ratio; missing days and a zero invoice are distinct
   unavailable reasons, never a fabricated 100% capture rate.
10. **Invoice scope is configured, not inferred.** An empty import scope means
    the provider organization; OpenAI project IDs and Anthropic workspace IDs
    narrow it. Operators must align that scope with the connections being
    compared or the ratio may include spend outside OLP. The initial import
    covers the previous 30 closed UTC days; subsequent runs refresh seven days
    for restatements, with retained rows bounded by usage retention.
11. **Exports preserve their distinct delivery contracts.** FOCUS uses a daily
    ledger, immutable revisioned parts and a manifest commit point. Metering is
    at least once, so CloudEvents receivers deduplicate the event ID. Stripe
    uses deterministic identifiers and its duplicate response contract;
    fixture redelivery does not establish unlimited deduplication at the live
    service. New customer mappings do not backfill already-settled attempts.

## Remaining delivery order

M2's signed v2 catalog/pricing publication, M4's consumer identities and budgets,
and M5's scoped sink and delivery contracts are hard prerequisites. M9 batch
result settlement is a separate dependency: `batch_multiplier` is represented
and tested, but no supported surface currently produces batch usage.

1. **Qualify the implemented tranche.** The cost-accounting maintainer preserves
   Go/PostgreSQL exact-price parity, cost/charge budget tests, invoice fixture
   coverage and late-fact/restatement tests. The release maintainer attaches
   these and the unchanged M1 performance gate to the candidate-image record.
2. **Add external evidence.** Qualify OpenAI and Anthropic imports with scoped
   admin credentials and at least one closed billing day. Record daily totals,
   pagination/restatement behavior, scope, currency and covered-day ratio.
   Qualify a CloudEvents receiver, a Stripe test meter and each claimed object
   transport with readback and redelivery. Test credentials and redacted results
   stay outside the repository; missing credentials leave these gates open.
3. **Qualify commitment lifecycle changes.** Keep regression coverage for
   hourly/daily/monthly boundaries, replaced pending edits, disabled connections,
   empty and excluded-only periods, exact allocation, late facts, retained history,
   backup/restore, scoped API access and console accessibility. Dynamic FOCUS
   checks cover unused cost and period reconciliation; other dynamic rules must
   still be qualified before claiming universal conformance.
4. **Extend cloud reconciliation as separate adapters.** Implement AWS CUR,
   Google billing export and Azure Cost Management one at a time using the same
   daily report contract. Each adapter must specify authoritative account/scope,
   source revision, currency, UTC-day mapping, late corrections and idempotent
   replacement; pass recorded pagination/empty-day/duplicate fixtures and a
   scoped live import before changing its parity claim. These adapters do not
   block shipping the explicitly limited OpenAI/Anthropic tranche.

## Exit criteria

- [x] Every price component above round-trips through revisions, sources,
      snapshots and configuration export, and accounting tests price each one
      exactly.
- [x] The [reference catalog](m02-provider-catalog.md#m24-reference-catalog)
      prices every catalog model with no `unrepresentable` component left for
      structures listed above.
- [x] Usage reports show cost and charge side by side, with commitments
      allocated.
- [x] `max_cost` excludes attempts whose estimated worst case exceeds the
      ceiling, and refuses a request with no eligible candidate before dispatch.
- [x] A FOCUS dataset passes the pinned model's applicable static checks, and
      CloudEvents and Stripe meter deliveries are idempotent under redelivery.
- [x] Reconciliation reports a capture rate for OpenAI and Anthropic
      connections against recorded fixtures.
- [x] Empty commitment periods export their full purchase/unused cost at
      installation scope; allocated plus unallocated effective cost reconciles
      to the obligation, including restatements and retention boundaries.
- [x] The [parity matrix](parity.md) cost rows are `Parity` or better, with the
      FOCUS commitment gap closed and applicable conformance evidence recorded.
- [ ] Candidate-image qualification reruns exact pricing, budget enforcement,
      export/reconciliation integration and the M1 performance gate, with
      versioned results attached to the release record.
- [ ] Live OpenAI/Anthropic invoice imports and external metering/sink readback
      establish the deployment-specific claims above. Fixture-only evidence
      remains labelled as such.

## Delivered and verified

The implemented M6.1–M6.4 paths and M6.5's OpenAI/Anthropic adapters have the
historical branch verification below. [Current commitment verification](m06-commitment-evidence.md)
records the completion changes and their source fingerprint. Historical branch
results do not represent a fresh candidate-image run.

- **Price components.** Pricing revisions, price-list sources, catalog
  snapshots and configuration documents carry modality and cached-modality
  rates, input-token tiers, time-of-day windows, per-request and tool-call
  fees and the batch multiplier. One PostgreSQL function prices every attempt
  and the gateway's Go mirror reproduces it exactly for reservations,
  settlement and `X-OLP-Cost`; a parity suite holds the two together.
- **Price lines.** Every priced fact stores `list_cost`, `contracted_cost`,
  `cost_lines` and `charge_lines` — the exact decomposition FOCUS needs;
  mismatched lines fail closed to a counted fallback.
- **Metering.** OpenAI, Anthropic and Gemini usage populates audio and image
  shares and server-side tool calls; inconsistent provider detail settles the
  attempt incomplete rather than at text rates. A `metering` export stream
  feeds CloudEvents sinks and `stripe_meter` sinks with deterministic,
  idempotent event identifiers; `duplicate_meter_event` counts as delivered
  and redelivery integration tests prove no double reporting.
- **FOCUS 1.4.** Object sinks deliver daily gzip-CSV cost-and-usage datasets
  in cost or charge basis with a last-written manifest, revisioned
  restatement, commitment `Used` allocations, Purchase and `Unused` rows and
  explicit completeness totals. `internal/export/focusspec` interprets the pinned 1.4
  requirements model; generated parts in the integration corpus validate with
  zero applicable static-rule violations (302 rules evaluated, 135 dynamic
  rules not machine-checkable, 48 not applicable). This does not establish the
  all dynamic rules. Current cross-row unused-commitment checks are additional
  evidence, not universal FOCUS certification.
- **Discounts, rate cards and charge.** Usage records carry cost and charge
  with their provenance; reports, exports and request history show both, and
  every consumer budget enforces either.
- **Estimation.** Route simulation states minimum and maximum estimated cost
  per target attempt; `max_cost` excludes candidates above that per-attempt
  bound before dispatch, and the console calculator prices routes and exact usage.
- **Invoice reconciliation.** OpenAI and Anthropic billing imports run on a
  worker task against vendor admin APIs; `/api/v1/usage/reconciliation`
  reports attributed versus invoiced cost with covered-day capture rate,
  unpriced share and provisional marking, and the console exposes the import
  configuration and the panel.
- **Commitments.** Connection commitments bill nothing per attempt and are
  allocated in usage reports by list-price-weighted share; FOCUS reports them
  as commitment-discount `Used`, Purchase and `Unused` rows, with durable
  publication provenance and exact period reconciliation.
- **Reference catalog v2.** Every component the catalog listed as
  unrepresentable in a structure OLP now prices is transcribed, independently
  checked against the vendor figures it came from; what remains carries an
  accurate residual name.

`make check`, `go mod tidy -diff`, `scripts/check-dependencies.sh` and one
complete, unmodified `make integration` pass on the branch; the integration
run includes the race-enabled package phases, the client qualification
suites, the packaged Playwright suite and the hosted journeys. Two gate
failures found during the run were fixed and re-verified green: the worker
task list in `TestProcessModesPrivateProbesAndShutdown` lacked
`billing_import` and `focus_export`, and `TestIntegrationFocusLifecycle`
asserted an exact delivered-days count that a newly eligible empty day could
exceed. `make bench-gate` reports no significant regression above its 10% threshold
(several admission and one resource benchmark show tolerated slowdowns of
5–8%, consistent with the added price-line bookkeeping). Live-provider qualification of
the metering is not part of this evidence; it rests on recorded response
fixtures, including the importer's OpenAI and Anthropic cost fixtures.
