# FinOps export and invoice reconciliation

FinOps has three separate content-free workflows. The `metering` stream sends
priced attempt records to CloudEvents consumers and Stripe Billing meters. FOCUS
1.4 datasets publish cost and usage data to object storage. Provider invoice
imports store daily billed totals so operators can reconcile them against OLP's
attributed spend.

The export worker delivers metering records from a durable queue. A separate
FOCUS worker generates and delivers datasets through the FOCUS ledger. The
billing importer polls provider cost reports independently and uses PostgreSQL
and provider egress without the export queue or coordination store. Export
retries preserve event or dataset identity, while a later invoice import replaces
the stored totals for the days it fetched.

## Metering stream

The `metering` stream enqueues one record per priced attempt. A priced attempt
is billable, usage-complete, and has an estimated cost or a charge; a committed
attempt with zero marginal cost still counts. The consumer project resolves as
the API key's project, else the provider's project, else the route's project.

The record payload is one JSON object per attempt:

| Field | Value |
| --- | --- |
| `attempt_id`, `request_id`, `observed_at` | Attempt identity and instant |
| `project_id` | Resolved consumer project |
| `api_key_id`, `route`, `provider_id`, `upstream_model`, `operation`, `surface` | Scope and workload |
| `input_tokens`, `output_tokens`, `cached_input_tokens`, `cache_write_input_tokens`, `cache_write_5m_input_tokens`, `cache_write_1h_input_tokens` | Text and cache usage |
| `audio_input_tokens`, `cached_audio_input_tokens`, `audio_output_tokens`, `image_input_tokens`, `cached_image_input_tokens`, `image_output_tokens`, `media_units` | Modality usage |
| `tool_calls`, `batch` | Tool-call counts and the batch marker |
| `cost`, `charge`, `list_cost`, `currency` | Priced amounts and list price |
| `pricing_revision_id`, `rate_card_revision_id`, `discount_id` | Pricing provenance |
| `commitment_weight` | Commitment allocation weight, when present |
| `attribution`, `end_user_digest` | Attribution labels and end-user identity |

Every sink type carries the stream. A `stripe_meter` sink enqueues only when
the consumer project maps to a Stripe customer and the configured value is
positive; see below. Queue records age out at `retention.requests_days`
(default 30 days); a record a sink never delivered becomes an
`olp_export_gap_total`/`export_gaps` row, not a delivery.

### CloudEvents sinks

An `https` sink with format `cloudevents` carries `metering` only. Each record
becomes one structured-mode CloudEvents 1.0 event POSTed with
`Content-Type: application/cloudevents+json`:

| Attribute | Value |
| --- | --- |
| `id` | The attempt id |
| `source` | `settings.source`, else `/olp/installations/<installation id>` |
| `type` | `dev.openllmproxy.attempt.priced` |
| `subject` | The consumer project id, or `installation` |
| `time` | The attempt's `observed_at`, RFC 3339 |
| `data` | The payload above, `datacontenttype: application/json` |

Every request carries `X-OLP-Event-ID` with the durable record id, the
credential's custom headers when set, and `X-OLP-Signature` when the credential
provides `signing_secret`. A redelivery resends byte-identical payloads, so a
consumer deduplicates on `id`. OpenMeter is the reference consumer.

### Stripe meter sinks

A `stripe_meter` sink with format `stripe` carries `metering` only and posts
`application/x-www-form-urlencoded` to `<destination>/v1/billing/meter_events`
with `Authorization: Bearer <key>`:

| Setting | Meaning |
| --- | --- |
| `event_name` | The configured Stripe meter event name (required) |
| `value` | What the event reports: `charge`, `cost`, `input_tokens`, `output_tokens`, `total_tokens` or `requests` (default `charge`) |
| `customers` | A map of consumer project id to `cus_…` customer id; 1–1000 projects required, each must exist and, on a project sink, be the sink's own |
| `customer_key` | The meter payload key for the customer (default `stripe_customer_id`) |
| `value_key` | The meter payload key for the value (default `value`) |

The credential is `{"api_key": "sk_…|rk_…"}`, sealed as `sink_credential`; the
server validates the key prefix and rejects `signing_secret`, `region`,
`auth_mode` and `secret` in the document.
Each event sends the deterministic identifier `olp-<sink id>-<attempt id>` and
repeats it as the `Idempotency-Key`, so a
redelivery is the same event to Stripe. A `duplicate_meter_event` response
counts as delivered. Stripe enforces identifier uniqueness for at least 24
hours while OLP retries within minutes, and accepts event timestamps up to
35 days old; the default 30-day request retention stays inside that window.
No `Stripe-Version` header is sent.

An attempt with no mapped project, or a null or non-positive value, never
queues for the sink. Adding a project to `customers` does not backfill
attempts that were already priced; the mapping must exist when the attempt
settles.

## FOCUS 1.4 datasets

An object-storage sink (`s3`, `gcs` or `azure_blob`) with format `csv_gzip` and
stream `focus` produces a daily [FOCUS 1.4](https://focus.finops.org/) cost
and usage dataset per UTC charge date. `settings.cost_basis` is `cost` or
`charge` and is immutable after creation; `settings.start_date` bounds how far
back generation reaches. It can reach at most 90 days before the day it is set and never
before the usage-retention cutoff.

### Layout

A day's objects live under one prefix:

```
focus/v1.4/billing_period=<YYYY-MM>/charge_date=<YYYY-MM-DD>/revision=NNNN/part-00000.csv.gz
focus/v1.4/billing_period=<YYYY-MM>/charge_date=<YYYY-MM-DD>/manifest.json
```

Each part is an RFC 4180 CSV, UTF-8 with no BOM and `\n` endings, gzip
compressed and uploaded with `Content-Type: application/gzip`, carrying the
header and at most 50,000 rows. A day with no rows still writes a header-only
part. The manifest (`application/json`) is always the last object written; it
is the commit point, and a retry after a failed manifest rewrites the same
revision's part keys.

### Manifest

```json
{
  "focus_version": "1.4",
  "dataset": "CostAndUsage",
  "cost_basis": "cost",
  "charge_date": "2026-10-11",
  "billing_period": "2026-10",
  "revision": 1,
  "generated_at": "2026-10-12T03:00:00Z",
  "installation_id": "…",
  "sink_id": "…",
  "currency": "USD",
  "applicability_criteria": [ "…" ],
  "correction_handling": "Late facts restate the day as a new revision that replaces the previous one.",
  "parts": [{ "key": "…", "rows": 3, "bytes": 1182, "sha256": "…" }],
  "totals": { "billed_cost": "…", "effective_cost": "…", "list_cost": "…", "contracted_cost": "…" },
  "accounting_totals": { "cost": "…", "charge": null },
  "commitment_totals": { "purchased_cost": "10", "allocated_cost": "7", "unused_cost": "3", "provisional": false, "complete": true },
  "completeness": {
    "included_attempts": 0, "fallback_line_attempts": 0, "unpriced_attempts": 0,
    "incomplete_attempts": 0, "uncharged_attempts": 0,
    "caller_paid_excluded_attempts": 0, "ingestion_gap": false
  },
  "metadata": {
    "DataGenerator": "…", "DatasetInstance": { "…" },
    "Recency": { "…" }, "Schema": { "…" }
  }
}
```

`sha256` covers the stored (gzipped) object bytes. `accounting_totals.cost` is
the exact sum of `estimated_cost` of the facts a cost-basis day included,
`accounting_totals.charge` the exact `charge` sum on a charge basis; either is
null on the other basis. `completeness` counts the day's billable facts in
scope by category; `ingestion_gap` records whether a request-metadata
ingestion gap overlaps the day. `metadata` follows FOCUS 1.4 §5:
`DataGenerator`, `DatasetInstance` (`DatasetInstanceId`,
`DatasetInstanceName`, `FocusDatasetId`), `Recency` (`DatasetInstanceId`,
`RecencyLastUpdateDate`, `DatasetInstanceLastUpdated`,
`DatasetInstanceComplete`) and `Schema` (`SchemaId`, `FocusVersion`,
`DataGeneratorVersion`, `CreationDate`, `DatasetInstanceId`,
`ColumnDefinition`).

### Eligibility, restatement and revisioning

A day becomes eligible two hours after it ends. The worker (`focus_export`,
a 60-second pass on PostgreSQL leases, independent of Valkey) processes at
most four days per sink per pass, oldest first. A late fact marks its delivered
day dirty, as does every day in a committed fact's bucket. A dirty day regenerates
no sooner than one hour after the previous delivery, writes the next
`revision=NNNN` keys and bumps the manifest revision. Restatement applies even
after the day's billing month has closed.
Days older than the usage-retention cutoff are never generated or restated,
and their dirty markers are purged each pass. Failure records a content-free
`last_error_code` (`endpoint`, `credential`, `authentication`, `status`,
`network`, `timeout`, `generation`) and backs off geometrically up to one
hour; the next retry rewrites the same revision keys, so a dataset's
revisions never gap.

### Cost basis vs charge basis

| Basis | Facts included | Amount columns |
| --- | --- | --- |
| `cost` | `estimated_cost IS NOT NULL`, excluding caller-paid and budget-exempt attempts | `BilledCost`/`EffectiveCost`/`ContractedCost`/`ListCost` from estimated cost and price lines |
| `charge` | `charge IS NOT NULL` | All four cost columns equal quantity × charge unit price |

### Row grain and price lines

One usage row per (UTC day, consumer project, provider, upstream model,
operation, route, API key, attribution set, committed flag, price-line
component/tier/unit/list and contracted unit price). Facts store `cost_lines`
and `charge_lines` from the exact decomposition produced by
`olp.price_usage_lines`. Each line has a `quantity` in its unit (`1000000 Tokens`, `Requests`, `Calls` or `Units`),
a `list_unit_price` that already includes the window and batch multipliers,
and a `contracted_unit_price` that also includes the discount. A fact can have no
stored lines when it was written before migration 0067 or when the line-total
guard did not match the fact totals. It then falls back to a single `attempt`
component line priced from the fact totals. The manifest counts such facts in
`fallback_line_attempts`. Line amounts are exact, while accounting rounds each
attempt to 12 decimal places, so `accounting_totals` can differ slightly from
`totals`; the manifest reports both.

### Column mapping

Static columns: `BillingAccountId`/`Name` are the installation id and name;
`BillingCurrency` is the installation pricing currency; `BillingPeriod` is the
UTC month of `ChargePeriodStart`, which spans the charge day for usage rows;
`ChargeCategory` is `Usage` (or `Purchase` for commitment rows);
`ChargeFrequency` is `Usage-Based` (`Recurring` for Purchase);
`ChargeDescription` is `<SkuMeter> for <model> <operation> via <connection>`;
`ServiceCategory` is `AI and Machine Learning`, `ServiceSubcategory`
`Generative AI`, `ServiceName` the vendor contract name (else the connection),
and `ServiceProviderName`/`HostProviderName`/`InvoiceIssuerName` the contract
maintainer, falling back to name and then connection. On the charge basis,
`ServiceProviderName` and `InvoiceIssuerName` instead use the installation name.
`Tags` is the attribution object or null.

| FOCUS column | Cost basis | Charge basis |
| --- | --- | --- |
| `BilledCost` | `ContractedCost`; `0` when committed | quantity × charge unit price |
| `EffectiveCost` | `ContractedCost`; allocated commitment when committed | quantity × charge unit price |
| `ListCost`, `ContractedCost` | quantity × list / contracted unit price | quantity × charge unit price |
| `ListUnitPrice`, `ContractedUnitPrice` | the price line's prices | the charge unit price (both) |
| `PricingCategory` | `Committed` or `Standard` | `Standard` |
| `PricingQuantity`, `PricingUnit` | line quantity and unit | same |
| `ConsumedQuantity`, `ConsumedUnit` | natural units (`Tokens`, `Requests`, `Calls`, `Units`) | same |
| `SkuId` | `<vendor>/<model>/<operation>/<component>` | same |
| `SkuPriceId` | `SkuId@<list unit price>` (`@above=<tier>` when tiered) | `SkuId@<charge unit price>` |
| `SkuMeter` | Component display name, such as `Input Tokens`, `Requests` or `Tool Calls (web_search)`. The `attempt` component displays as `Attempts` | same |
| `SubAccountId`/`SubAccountName` | consumer project | consumer project |
| `CommitmentDiscount*` | set on committed usage and Purchase rows | null |
| `x_ApiKeyId`, `x_ConnectionId`, `x_ConnectionName`, `x_Model`, `x_Operation`, `x_PriceComponent`, `x_PriceTier`, `x_Route`, `x_Attempts` | row context and distinct attempt count | same |

The vendor name resolves `options.vendor_id`, else the kind's default vendor,
else the kind; plugin providers use `plugin` and fall back to the connection
name for service names.

### Commitments

Commitment schedules and period records are independent of request facts.
Publication and lifecycle rules are described in [pricing](pricing.md#commitments).
A period is purchased even when the connection is disabled or has no usage.
Its `CommitmentDiscountId` names the durable schedule; ownership, connection
name, provider revision, amount and currency retain their publication provenance.

The full amount is allocated globally by eligible list-price weight. Cumulative
12-decimal allocation assigns rounding residuals deterministically, including
when a single attempt contains several price lines. Caller-paid and budget-exempt
attempts receive no installation allocation. A positive total weight allocates
the full obligation; a zero weight leaves the full amount unallocated.

Unfiltered installation cost exports emit one `Purchase` per durable period on
its original UTC bucket-start date, and a `Usage` row with
`CommitmentDiscountStatus=Unused` for any unallocated amount. Unused rows carry
commitment and resource identity, zero billed cost and the unallocated effective
cost. Consumer, route, API key and consumed-usage fields are empty. Scoped exports
contain allocated usage only; charge exports contain consumer charges.

`commitment_totals` in the manifest and dataset API contains exact-string
`purchased_cost`, `allocated_cost`, `unused_cost`, plus `provisional` and `complete`.
Installation totals cover entire periods touching the day, so monthly period
totals repeat on each day's manifest and must not be summed across days.
Scoped totals disclose that day's allocated share, with purchased/unused set to
zero. Charge-basis and legacy datasets awaiting regeneration return null.

A purchase outside the export window stays on its original date; it is never
moved into a later day's dataset. `complete=false` discloses partial window
coverage, insufficient historical evidence, ingestion gaps or missing pricing
or metering. `provisional=true` identifies periods still open or within the
two-hour closing delay. Unallocated money is not proof of confirmed idle capacity.
The console dataset table displays all five fields.

The worker materializes at most 512 periods and backfills at most 512 facts and
512 historical hourly summaries per pass. Uniqueness constraints and transaction
locks make retries and concurrent workers idempotent. Retained summaries preserve
monthly allocation after detailed facts expire. Changes dirty affected days;
leases, retry revision identity and manifest-last delivery remain authoritative.
Historical backfill uses published snapshots, never draft configuration, and
retains incomplete coverage when evidence cannot be reconstructed.

### Conformance

`internal/export/focusspec` interprets every Static rule of the FOCUS 1.4
requirements model (upstream asset sha256
`3dd2c0c6fcc2c3d1792060fcbebba4ae074cd4dade8cde72958bd37a9a8b183f`) under the
declared applicability criteria:

```
ACCOUNT_NAMING_SUPPORTED, COMMITMENT_DISCOUNT_SUPPORTED,
MULTIPLE_PRICING_CATEGORIES_SUPPORTED, NEGOTIATED_PRICING_SUPPORTED,
PUBLIC_PRICE_LIST_SUPPORTED, SUB_ACCOUNT_SUPPORTED, TAGGING_SUPPORTED,
UNIT_PRICING_SUPPORTED, USAGE_MEASUREMENT_SUPPORTED
```

The integration suite validates generated parts against the pinned static
model, including unused rows and resource fields. Explicit cross-row checks also
verify exact period reconciliation and `CAU-CostAndUsage-D-082-C`; other dynamic
rules still require qualification. The checker skips the deprecated ProviderName/PublisherName
presence rules the 1.4 model still ships and applies null-safe requirement
comparisons. The official focus-validator 2.2.1 cannot validate these
datasets: it crashes on a dependency cycle through the Dynamic
CommitmentDiscountQuantity rules and mis-evaluates removed and conditional
rules, so the in-repo interpreter is the conformance authority.

## Invoice reconciliation

### Billing imports

An OpenAI, Anthropic, Bedrock, Vertex AI or Azure OpenAI connection gains a billing import at
`PUT /api/v1/providers/{provider_id}/billing-import` with `{enabled, scope,
credential}`. The billing credential is sealed under purpose
`billing_credential` and is separate from the inference credential. It is
required on create, omitting it keeps the stored credential, and `null` is
rejected. `GET` returns the import and its status, including the latest
contiguous covered date range, last error and applied source revision. Older
covered days may remain outside that range. `POST …/run` reschedules it immediately.
`DELETE` removes the import and sealed credential while keeping imported costs and
coverage available for historical reconciliation. Changing the source or scope
clears the current import's coverage status. Previously imported days remain
available for reconciliation until a later import replaces them or retention
purges them.

Amounts retain the source currency; reconciliation does not convert currencies.
Imports from connections with overlapping account, project, workspace,
linked-account or subscription scopes can count the same invoice costs twice.
Choose non-overlapping scopes when combining connections.

For OpenAI and Anthropic, use a vendor admin key. Scope is
`{"project_ids": ["proj_…"]}` for OpenAI or
`{"workspace_ids": ["wrkspc_…", "default"]}` for Anthropic. Each list can
contain up to 100 entries and `{}` imports the whole organization. OpenAI
amounts come from its organization costs endpoint, and Anthropic amounts come
from its organization cost report.

For Bedrock, configure a legacy AWS Cost and Usage Report with hourly time
granularity, CSV format, GZIP or ZIP compression and **create new report
versions** enabled. Include resource IDs and deliver the report to S3. Set
`scope` to `{"payer_account_id":"123456789012", "bucket":"billing-bucket",
"report_prefix":"prefix/report-name", "region":"us-east-1"}`. The prefix
includes the report name and is relative to the bucket. Optionally provide
`"linked_account_ids":["123456789013"]` to limit the import to up to 100 linked
accounts. Without that list all linked accounts in the payer's report are
included. Keep scopes disjoint when multiple providers use the same payer,
since overlapping imports each record the same matching costs.

Use a dedicated billing identity with `s3:ListBucket` on the bucket limited to
the report prefix and `s3:GetObject` on objects under that prefix. Grant any
KMS decrypt permission required by the bucket's encryption key. Supply
`{"auth_mode":"static", "access_key_id":"…", "secret_access_key":"…"}`
as the billing credential, adding `session_token` for temporary credentials,
or `{"auth_mode":"workload"}` to use the workload's AWS credential chain.
The workload chain uses the bounded AWS metadata transport; S3 access uses the
connection's normal network and egress policy. The import reads only Bedrock
service rows, uses effective costs for reserved and Savings Plan usage, and
excludes commitment fees and negation lines. It uses the assembly manifest and
its S3 modification time as the revision and ordering marker. Parquet exports
and reports that overwrite previous versions are not supported.

### Azure Cost Management setup

Create a Cost Management export for cost and usage details (amortized), with CSV
format and daily delivery of month-to-date costs to a Blob Storage container.
Each delivery must start on the first day of its billing month. Partitioned and
gzip exports are supported. Parquet, actual cost, usage-only and FOCUS exports use
different schemas and are not accepted. Management group exports do not support
amortized costs. The export must already exist and historical imports are
limited to deliveries still present in the container. Azure can deliver prior
month corrections during the first five days of the month
([Microsoft export documentation](https://learn.microsoft.com/en-us/azure/cost-management-billing/costs/tutorial-improved-exports)).

Grant the chosen identity Storage Blob Data Reader on the container or a
narrower appropriate scope. The billing connection uses its own storage
account endpoint and the provider connection's network and egress settings.
Use `azure_default` for an explicitly selected workload identity, or supply a
client secret for `azure_client_secret`. Both credentials are write-only. For
example, create the import with the following scope and workload credential,
replacing the values with your own:

```json
{
  "enabled": true,
  "scope": {
    "billing_scope": "/providers/Microsoft.Billing/billingAccounts/1234567",
    "export_name": "costs",
    "storage_account": "billingaccount",
    "container": "exports",
    "prefix": "billing",
    "subscription_ids": ["11111111-1111-4111-8111-111111111111"]
  },
  "credential": {"auth_mode": "azure_default"}
}
```

The `billing_scope` must match the export resource scope. `prefix` is the
export's storage directory and may be omitted. Omit `subscription_ids` to
include all subscriptions in that scope. A client-secret credential is
`{"auth_mode":"azure_client_secret","tenant_id":"...","client_id":"...","client_secret":"..."}`.
Choose disjoint subscription scopes when multiple providers share an export,
or their reported invoice totals overlap.

Azure imports only the documented Cognitive Services meter subcategories
Azure Open AI and Azure Open AI Reservation. Vendor meter names can change, so
review the export if expected charges are missing. Amounts use
`CostInBillingCurrency` from the amortized export. Non-tax credit and adjustment
rows are retained when the export assigns them to those meters; account-wide
credits cannot be allocated to the meters. Tax and other Azure services are
excluded, so these amounts do not necessarily equal the final invoice. The
importer validates the export resource ID, manifest and part counts before
storing closed UTC days. Blob manifests have no documented
completion status, so a delivery that appears complete from its manifest can
still be subject to later correction. If no manifests are found at the configured
location, the import reports a response error and retries. See the
[Cost Management overview](https://learn.microsoft.com/en-us/azure/cost-management-billing/cost-management-billing-overview).

For Vertex AI, first enable the [detailed Cloud Billing export to BigQuery](https://cloud.google.com/billing/docs/how-to/export-data-bigquery-tables/detailed-usage).
Grant the billing principal `bigquery.jobs.create` in the query project and
`bigquery.tables.getData` on the export table. Set the required scope to the
Cloud Billing account ID, BigQuery project ID, dataset ID and detailed export
table ID. `project_ids` optionally limits the imported workloads. Keep scopes
disjoint across providers using the same export because overlapping imports
record the same matching costs more than once:

```json
{
  "billing_account_id": "012345-ABCDEF-012345",
  "bigquery_project_id": "billing-project",
  "dataset_id": "billing_export",
  "table_id": "gcp_billing_export_resource_v1_012345_ABCDEF_012345",
  "project_ids": ["workload-project"]
}
```

The credential is `{"auth_mode":"service_account","service_account":{...}}`
with the complete service account key JSON, or `{"auth_mode":"adc"}` to
explicitly use the worker's application default credentials. The credential is
sealed and is never returned by the API. Configuration promotion passes the
same JSON object serialized as the value of the billing credential's secret
binding. Keep this binding out of the configuration document itself.

The Google query selects the Vertex AI [billing service ID](https://cloud.google.com/billing/docs/how-to/budgets-spend-caps)
`C7E2-9256-1C43` and sums costs by UTC usage day, service, SKU and
currency. Regular charges, adjustments and credit types have separate line
items. Corrections stay on the original usage day even when their invoice month
is later. Google stores detailed export costs and credits as FLOAT. The query
converts each value through its decimal string representation to BIGNUMERIC before
summing and the importer keeps the returned decimal without converting it through
a Go float. This cannot recover precision
already lost in the source FLOAT.

The importer queries retained billing periods and records the export watermark
with each period. It revisits retained historical periods alongside the current
and previous periods, and replaces a covered day only with an equal or newer
watermark. The watermark covers the configured account and projects, starts at
the beginning of the queried period and includes later arrivals. A day is covered
only when the export contains activity for the configured account and projects
on that UTC day, across any service,
and the day closed before the watermark. An evidenced day with no Vertex charges
is covered zero. Days without scope activity remain not imported. Google exports
can begin after historical usage, contain gaps or arrive late. Activity and a
watermark provide evidence of coverage, but do not guarantee that the export
contained every historical cost.

### Importer

The `billing_import` worker scans every 60 seconds for imports due, claims up
to ten, one at a time under a ten-minute lease, and fetches through the connection's
egress-validated client with redirects disabled and a 30-second request timeout.
OpenAI and Anthropic imports cover closed UTC days only. The first covers the 30
days before today, and later imports refresh the last 7 so vendor restatements
land. AWS imports cover closed UTC days within retention on the first pass and
refresh the current and previous billing periods on later passes. Incomplete
historical periods are retried until covered. A delivery covers only days closed
before its manifest was written, and gaps remain unimported. AWS uses unblended
cost for ordinary and other included rows, reservation effective cost for
`DiscountedUsage`, and Savings Plan effective cost for `SavingsPlanCoveredUsage`.
It excludes tax, commitment fees and negations. AWS reads have a
45-second deadline and cumulative compressed and expanded byte, row and
aggregation limits. Azure searches retained billing periods on the first pass,
then refreshes the current and previous periods and retries missing or incomplete
historical periods. A run covers only days closed before it was submitted, and
gaps remain unimported. Newer complete runs replace their period's totals;
older runs cannot overwrite them. Azure keeps service-scoped credits and
adjustments present in the export separate from usage and excludes taxes. Azure
streams up to 200 CSV parts, 128 MiB compressed and 512 MiB expanded per pass,
with two million rows, 100,000 aggregates and a one-minute import budget. Each
day's returned line items are summed per
`(day, line_item, currency)` and replace the stored rows. A returned bucket with
no results records an invoiced zero. On success the import schedules one hour
out. Google schedules the next worker scan after a successful pass while retained
historical periods have not yet been queried; it returns to hourly refresh once
they have all been visited. On failure `last_error_code` records `credential`, `status`, `network`,
`timeout`, `response` or `endpoint`, and the retry backs off from five
minutes to one hour. Rows older than `retention.usage_days` are purged in
bounded batches and their coverage is cleared. If retention later increases,
AWS and Azure imports retry the newly uncovered historical periods. Google revisits
retained billing periods in rotation, including newly retained periods and days
without scope evidence.

Google uses the fixed BigQuery endpoint through the same connection egress
policy, with a 1 GiB maximum bytes billed per query, a 20-second job timeout, a
45-second import budget and a maximum of 200,000 aggregate rows per query. A
pass submits at most three queries, for a maximum of 3 GiB billed, and rotates
their order so a slow period does not starve other periods. A failed query or an
incomplete result does not replace that period's invoice rows. Token exchanges
use the configured connection proxy while validating the Google authentication
destination with a public-only policy. Workload credentials are used only when
ADC is explicitly selected.

`GET /api/v1/providers/{id}/invoiced-costs?start=…&end=…` lists stored line
items (`day`, `line_item`, `currency`, `amount`, `source`, `fetched_at`),
end-exclusive and at most 93 days.

### Reconciliation report

`GET /api/v1/usage/reconciliation?start=…&end=…[&provider_id=]` reports per
(provider, UTC day):

| Field | Meaning |
| --- | --- |
| `attributed_cost` | `estimated_cost` plus allocated commitment for the day's billable attempts |
| `invoiced_cost` | imported line-item total for a covered day, else null; `0` for a covered day with no costs |
| `capture_rate` | `attributed / invoiced` to six decimals, or null with `capture_rate_unavailable_reason` (`not_imported`, `zero_invoice`, `currency_mismatch`) |
| `priced_attempts`, `unpriced_attempts`, `incomplete_attempts` | billable attempt counts; `unpriced_share` is `unpriced/(priced+unpriced)` |
| `excluded_caller_attempts` | billable attempts excluded for caller-paid or budget-exempt status |
| `provisional` | true within two days of today, or when the day's import has not succeeded since 30 hours after the day began |
| `import` | the import's `last_success_at`/`last_error_code`, or null after deletion |

Totals per provider add `attributed_cost` (whole range),
`attributed_cost_covered` (covered days only, the numerator `capture_rate`
uses), `invoiced_cost`, `capture_rate` and `unpriced_share`. Caller-paid and
budget-exempt attempts are excluded from attributed cost; hourly rollup rows
only approximate priced/incomplete counts and cannot distinguish caller-paid
usage, so their `budget_exempt` request count is counted as excluded. That
limitation is documented in the code.

Metrics: `olp_billing_imports_total{provider,outcome}` (process-local) and
`olp_billing_capture_rate{provider}` (last 7 closed covered days).

### Promotion and fixtures

A provider entry in a configuration document carries `billing_import`
`{enabled, scope, credential_ref}` with `credential_ref`
`<provider name>/billing`; a missing `secret_bindings` entry blocks apply,
reusing a matching binding keeps the stored credential, and an entry's absence
deletes the import. The fixtures in `internal/billing/testdata` and the hand-built
AWS CUR fixtures in `tests/integration/billing_aws_test.go` follow documented
response shapes. The AWS fixture follows the documented [report versions](https://docs.aws.amazon.com/cur/latest/userguide/understanding-report-versions.html),
[latest report locations](https://docs.aws.amazon.com/cur/latest/userguide/view-latest-cur.html),
[identity columns](https://docs.aws.amazon.com/cur/latest/userguide/identity-columns.html)
and [line item columns](https://docs.aws.amazon.com/cur/latest/userguide/Lineitem-columns.html),
reviewed October 31, 2026. The fixtures have not been qualified against live billing accounts.
