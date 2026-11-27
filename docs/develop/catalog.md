# Reference catalog

The reference catalog is a signed document containing model facts and list
prices transcribed from vendor documentation. Every release embeds it in the
`olp` binary and publishes it beside the release. The catalog serves as advisory
input to three workflows. Pricing reads it only through a
[catalog pricing source](../operate/operations.md#accounting-delivery-and-shutdown),
whose snapshots an operator reviews and publishes because accounting prices only
against published revisions. Discovery offers its facts beside discovered
models, and accepting them stores operator model facts tagged `catalog@<sha256>`
while still requiring certification. Route editing warns about targets whose
model the vendor has deprecated or is retiring. Nothing in the catalog certifies
a model or prices an attempt on its own.

## Format

The document is `openllmproxy.dev/catalog/v2`, defined by
[`internal/catalog/schema.json`](../../internal/catalog/schema.json). A v2
catalog is refused by older releases as an unsupported version.

| Member | Contents |
| --- | --- |
| `published_at` | Orders catalogs so a refresh never accepts an older one. |
| `currency` | The currency of every price. |
| `estimation` | Token-estimation factors for model families without a public tokenizer, each measured against the vendor's own counts. |
| `vendors[].id` | An OLP vendor identifier, as `GET /api/v1/provider-vendors` lists. |
| `vendors[].models[]` | The model identifier the vendor's API is sent, its aliases, an `organization/model` canonical identity, context and output limits, input and output modalities, documented capability hints, and documented deprecation and retirement dates. |
| `prices[]` | One price per operation in every component OLP prices, matching a [pricing revision](../operate/pricing.md#price-components), including the twelve per-million token rates (input, cached input, the three cache writes, output, and audio and image input and output with their cached shares), a `unit_price` per image, second of audio or video, character of speech input or search unit, `per_request`, `tool_calls` fees, input-token `tiers`, time-of-day `windows` and `batch_multiplier`. |
| `unrepresentable[]` | Every vendor charge component that OLP cannot yet price, named in the vendor's own terms. The vocabulary is closed and never names a component that a price can carry. |
| `provenance` | On every model, price, lifecycle and factor, records the HTTPS page it was transcribed from and when it was observed. |

Each `unrepresentable[].component` value names one of the structures below.

| Component | The vendor structure OLP cannot price |
| --- | --- |
| `ambiguous` | The vendor's figures contradict each other or the model's other rates. |
| `batch_rates` | Batch prices a batch multiplier does not express or does not demonstrably cover. |
| `cache_storage` | Charges for holding cached context over time. |
| `conditions` | Rates or fees that apply only under conditions usage does not record. |
| `free_allowance` | Free quotas, trials or credits. |
| `holiday_calendar` | Rates that change on public holidays. |
| `media_units` | Media pricing by resolution, duration or quality beyond one unit price. |
| `minimum_charge` | Minimum billed quantities. |
| `plan_pricing` | Rates that depend on a subscription plan. |
| `prepaid_credits` | Prepaid packs or credit currencies. |
| `promotion` | Time-limited prices and their scheduled change. |
| `regional_uplift` | Rates that differ by region, endpoint or data residency. |
| `service_tier` | Rates of other service tiers such as priority, flex or reserved. |
| `thinking_mode` | Separate rates for reasoning or thinking output. |
| `tool_usage` | Server-side tool charges not billed per metered invocation. |
| `unpublished_rates` | Rates a vendor names but does not publish. |
| `video_input` | Video input rates distinct from image input, because video tokens are metered as image shares. |

Prices are decimal strings with no redundant zeros. A catalog validates only if
every model has a price, every cache rate has an input rate, every tier names
exactly the per-million rates its entry does at a strictly increasing positive
threshold, window periods do not overlap on any shared day, identifiers and
aliases are unique within a vendor, and nothing was observed after publication.

Vendor documents write thresholds inconsistently. A `K` a vendor writes is read
as 1,024 for Alibaba Cloud (DashScope), BytePlus and MiniMax, whose exact
figures and reseller figures use binary multiples. It is read as 1,000
elsewhere. "Reaches" or "at least" thresholds transcribe as one less than the
stated count because a tier applies above its threshold.

## Canonical form and signature

A signature covers the exact bytes of the document, so anyone can verify it
without reproducing an encoding. The canonical form is two-space indented JSON
with a trailing newline, members in schema order, every list sorted and times in
UTC to the second. `make catalog` rewrites the catalog canonically, and CI
refuses one that is not.

The signature file beside the document, `catalog.json.sig`, holds one Ed25519
signature per key.

```json
{
  "signatures": [
    { "key_id": "dev-2026a", "signature": "<base64>" }
  ]
}
```

Verification ignores keys the binary does not trust, requires at least one
trusted key, and requires every trusted key's signature to verify. Every process
mode and `olp doctor` refuse to start when the embedded catalog does not verify.

## Signing keys

Two kinds of key verify catalogs. The development key (`dev-2026a`) has its seed
committed in [`internal/signing/devkey`](../../internal/signing/devkey) and
protects nothing. It lets contributors and CI sign the catalog after an edit
with `make catalog-sign`, and only builds without the `release` build tag trust
it. Release keys live in `releaseKeys()` inside
[`internal/signing/keys.go`](../../internal/signing/keys.go), which already
commits `release-2026a`. Release builds (`-tags release`) trust only those keys.
The release workflow signs the catalog with the private seed held only in CI as
the `OLP_SIGNING_KEY` secret before building the release image, and publishes
`catalog.json`, `catalog.json.sig` and `catalog.schema.json` with the release.

To generate a new release key, a maintainer runs
`go run ./cmd/olpsign keygen` offline, commits the public key to `releaseKeys()`
in `internal/signing/keys.go`, and stores the seed in the `OLP_SIGNING_KEY`
secret and the key identifier in the `OLP_SIGNING_KEY_ID` Actions variable of
the repository.

To rotate a release key, follow these steps.

1. Add the incoming key to `releaseKeys()` beside the outgoing one and release.
2. Sign with both keys. `olpsign sign` adds a signature and keeps the others, so
   installations trusting either key verify the same document.
3. After two minor releases, sign with the incoming key alone.
4. Remove the outgoing key in a later release.

If a key is compromised, remove it from `releaseKeys()`, sign with a new key and
release. Installations on older binaries keep trusting the compromised key until
they upgrade, while the anti-rollback check and the human publish step for
prices bound what a forged catalog can do meanwhile.

## Coverage

The catalog lists every model with a published US-dollar list price that OLP
serves through a priced operation across 32 vendor entries. Those entries cover
OpenAI, Anthropic, the Gemini API, Vertex AI, Amazon Bedrock (including Amazon
Polly's engines), IBM watsonx.ai, Mistral, DeepSeek, Groq, xAI, Cohere (as the
two distinct vendor entries `cohere` and `cohere-native-v2`), Voyage, Cerebras,
Moonshot, Jina AI, ElevenLabs, Deepgram, AssemblyAI, Stability AI, Recraft,
Black Forest Labs, Runway, SambaNova, Nebius, Novita, Baseten, Alibaba Cloud
Model Studio (international), Z.ai, MiniMax, BytePlus ModelArk and Cloudflare
Workers AI. Each price is in the unit OLP meters for its operation, namely
tokens, images, seconds of audio or video, characters of speech input or search
units.

Other vendors are left to operator prices for the reasons below.

| Vendors | Reason |
| --- | --- |
| Azure OpenAI | Prices match by model, but an Azure provider calls a deployment the operator names. |
| OpenRouter, Together AI, Fireworks AI, DeepInfra, Hugging Face, Vercel AI Gateway | Aggregators whose large catalogs change daily and whose own model listings carry prices. |
| vLLM, Ollama, LM Studio, llama.cpp, Docker Model Runner, Infinity and custom endpoints | Self-hosted endpoints with no list price. |
| Amazon SageMaker AI, Databricks, Snowflake Cortex | Billed by instance, DBU or credit rather than by token. |
| Moonshot China, DashScope China, Zhipu, MiniMax China, Volcengine Ark, Scaleway, OVHcloud | Priced in yuan or euros while the catalog uses one currency, US dollars. |
| NVIDIA NIM, Featherless, Nscale | No public per-token list price because NVIDIA's hosted endpoints are a free trial, Featherless sells subscriptions, and Nscale shows prices only in its console. |
| Deepgram Aura voices | Each voice is its own model, `aura-2-{voice}-{language}`, billed per character of speech input, so operators configure unit pricing for the specific voices they enable instead of enumerating every voice in the catalog. |

## Maintenance

Prices and facts come only from each vendor's own pricing, model and deprecation
pages, never from aggregators. The weekly drift workflow
(`.github/workflows/catalog-drift.yml`, running `cmd/catalogdrift`) compares the
catalog against live model listings for 9 vendors (`openai`, `anthropic`,
`google`, `mistral`, `groq`, `xai`, `deepseek`, `cerebras` and `moonshot`) while
still checking all 32 catalog vendors for past retirements and observations
older than 90 days. The remaining 23 catalog vendors appear in the report as
unchecked because no listing credentials are wired for them. The workflow opens
or updates a `catalog-drift` issue for human review, and corrections land as
reviewed pull requests that re-sign the catalog. See
[`internal/catalog/drift`](../../internal/catalog/drift).

## Calibration

No measured Anthropic or Gemini factor ships. Unmeasured families retain factor
`1` and heuristic provenance. Run `tests/fixtures/tokens/calibrate.py` against
the checked-in corpus and vendor count endpoints, then review ratios and publish
the upward-rounded factor through the signed catalog workflow. Record model,
endpoint, observation time, totals and source provenance. A measured factor is
neither per-request exactness nor live certification of other models. See
[Qualification](qualification.md#providers-and-token-estimates).
