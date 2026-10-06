# Reference catalog

The reference catalog is a signed document of model facts and list prices by
vendor, transcribed from each vendor's own documentation. Every release embeds
it in the `olp` binary and publishes it beside the release. It is advisory:

- **Pricing** reads it only through a [catalog pricing source](operations.md#accounting-delivery-and-shutdown),
  whose snapshots an operator reviews and publishes. Accounting prices only
  against published revisions.
- **Discovery** offers its facts beside discovered models. Accepting them stores
  operator model facts tagged `catalog@<sha256>`; certification is still
  required.
- **Route editing** warns about targets whose model the vendor has deprecated or
  is retiring.

Nothing in the catalog certifies a model or prices an attempt on its own.

## Format

The document is `openllmproxy.dev/catalog/v1`, defined by
[`internal/catalog/schema.json`](../internal/catalog/schema.json):

| Member | Contents |
| --- | --- |
| `published_at` | Orders catalogs; a refresh never accepts an older one. |
| `currency` | The currency of every price. |
| `estimation` | Token-estimation factors for model families without a public tokenizer, each measured against the vendor's own counts. |
| `vendors[].id` | An OLP vendor identifier, as `GET /api/v1/provider-vendors` lists. |
| `vendors[].models[]` | The model identifier the vendor's API is sent, its aliases, an `organization/model` canonical identity, context and output limits, input and output modalities, documented capability hints, and documented deprecation and retirement dates. |
| `prices[]` | One price per operation in every component OLP prices: input, cached input, cache writes, output, and a unit price per image, second of audio or video, character of speech input, or search unit. |
| `unrepresentable[]` | Every component the vendor charges that OLP cannot yet price, such as long-context tiers, batch discounts, audio tokens or regional uplifts, in the vendor's own terms. |
| `provenance` | On every model, price, lifecycle and factor: the HTTPS page it was transcribed from and when it was observed. |

Prices are decimal strings with no redundant zeros. A catalog validates only if
every model has a price, every cache rate has an input rate, identifiers and
aliases are unique within a vendor, and nothing was observed after publication.

## Canonical form and signature

A signature covers the exact bytes of the document, so anyone can verify it
without reproducing an encoding. The canonical form is two-space indented JSON
with a trailing newline, members in schema order, every list sorted, and times
in UTC to the second. `make catalog` rewrites the catalog canonically;
CI refuses one that is not.

The signature file beside the document, `catalog.json.sig`, holds one Ed25519
signature per key:

```json
{
  "signatures": [
    { "key_id": "dev-2026a", "signature": "<base64>" }
  ]
}
```

Verification ignores keys the binary does not trust, requires at least one
trusted key, and requires every trusted key's signature to verify. Every
process mode, and `olp doctor`, refuses to start when the embedded catalog
does not verify.

## Signing keys

Two kinds of key verify catalogs:

- **The development key** (`dev-2026a`). Its seed is committed in
  [`internal/signing/devkey`](../internal/signing/devkey) and protects nothing:
  it lets contributors and CI sign the catalog after an edit with
  `make catalog-sign`. Only builds without the `release` build tag trust it.
- **Release keys** (`release-2026a`), listed in [`internal/signing/keys.go`](../internal/signing/keys.go).
  Release builds (`-tags release`) trust only these. The release workflow signs
  the catalog with the seed only CI holds, as the `OLP_SIGNING_KEY` secret,
  before it builds the release image, and publishes `catalog.json`,
  `catalog.json.sig` and `catalog.schema.json` with the release.

To create the first release key, a maintainer runs
`go run ./internal/signing/cmd/olpsign keygen` offline, commits the public key
to `releaseKeys`, and stores the seed as the `OLP_SIGNING_KEY` secret and the
key identifier as the `OLP_SIGNING_KEY_ID` Actions variable of the repository.

To rotate a key:

1. Add the incoming key to `releaseKeys` beside the outgoing one and release.
2. Sign with both keys; `olpsign sign` adds a signature and keeps the others, so
   installations trusting either key verify the same document.
3. After two minor releases, sign with the incoming key alone.
4. Remove the outgoing key in a later release.

If a key is compromised, remove it from `releaseKeys`, sign with a new key and
release. Installations on older binaries keep trusting the compromised key until
they upgrade; the anti-rollback check and the human publish step for prices
bound what a forged catalog can do meanwhile.

## Coverage

The catalog lists every model with a published US-dollar list price that OLP
serves through a priced operation, for these vendors: OpenAI, Anthropic, the
Gemini API, Vertex AI, Amazon Bedrock (including Amazon Polly's engines), IBM
watsonx.ai, Mistral, DeepSeek, Groq, xAI, Cohere, Voyage, Cerebras, Moonshot,
Jina AI, ElevenLabs, Deepgram, AssemblyAI, Stability AI, Recraft, Black Forest
Labs, Runway, SambaNova, Nebius, Novita, Baseten, Alibaba Cloud Model Studio
(international), Z.ai, MiniMax, BytePlus ModelArk and Cloudflare Workers AI.
Each price is in the unit OLP meters for its operation: tokens, images,
seconds of audio or video, characters of speech input, or search units.

Other vendors are left to operator prices:

| Vendors | Reason |
| --- | --- |
| Azure OpenAI | Prices match by model, but an Azure provider calls a deployment the operator names. |
| OpenRouter, Together AI, Fireworks AI, DeepInfra, Hugging Face, Vercel AI Gateway | Aggregators whose large catalogs change daily; their own model listings carry prices. |
| vLLM, Ollama, LM Studio, llama.cpp, Docker Model Runner, Infinity and custom endpoints | Self-hosted, with no list price. |
| Amazon SageMaker AI, Databricks, Snowflake Cortex | Billed by instance, DBU or credit, not by token. |
| Moonshot China, DashScope China, Zhipu, MiniMax China, Volcengine Ark, Scaleway, OVHcloud | Priced in yuan or euros; the catalog has one currency, US dollars. |
| NVIDIA NIM, Featherless, Nscale | No public per-token list price: NVIDIA's hosted endpoints are a free trial, Featherless sells subscriptions, and Nscale shows prices only in its console. |
| Deepgram Aura voices | Each voice is its own model, `aura-2-{voice}-{language}`; Aura-2 costs $0.030 per 1,000 characters. |

## Maintenance

Prices and facts come only from each vendor's own pricing, model and
deprecation pages, never from aggregators. A weekly workflow compares the
catalog with each vendor's model listing and opens or updates a `catalog-drift`
issue for human review; corrections land as reviewed pull requests that
re-sign the catalog. See [`internal/catalog/drift`](../internal/catalog/drift).
