# Release execution receipt — 2026-09-24

All local executions below used committed code `3a3a3644ab06220478e07080e864f19e9e540c05`. The working tree was
clean before validation; generated contracts and release inventory produced no
tracked differences. Providers were independently scripted local fixtures;
no paid inference or empirical model-quality study ran. This receipt is an
execution record, separate from the historical `66a3ccb3` receipt.

## Executed checks

| Check | Observed result |
| --- | --- |
| `make check` | Passed Go formatting/vet/unit/protocol tests, console formatting/types/lint, **652 console tests in 78 files**, and **61 script tests**. |
| `make test-race` | Passed uncached Go tests with race detection; no race diagnostic. |
| `make build` | Passed native binary and static console build; manifest verified **437 assets**. |
| Contract/version/inventory/Helm | Passed `scripts/check-contracts.sh`, `scripts/check-release-version.mjs`, `scripts/release-inventory.mjs`, generated-file diff checks and `scripts/check-helm.sh`. Inventory reconciled 100 management operations, 90 inference tuples, 133 explicit suite mappings, 18 unchanged reference fixtures and 229 modules. |
| Public and SDK race selection below | **50/50 named tests passed**, zero top-level or nested skips; Go reported 151.066s, wall time including compilation 203.149s. |
| `OLP_SDK_SMOKE_SURFACES=openai,anthropic,gemini ./tests/sdk-smoke/run.sh` | Passed official JavaScript success/error contracts; native Anthropic next turn retained 19 events, 2 tools and 2 verified dispatches. |
| `OLP_SDK_SMOKE_SURFACES=openai,anthropic,gemini ./tests/sdk-smoke-python/run.sh` | Passed official Python success/error contracts and the same native Anthropic continuation counts. |
| Bounded strict 64× performance smoke | Passed all 22 paths in 23.725s wall time (Go benchmark 20.234s): 1,280 successful timed dispatches and 128 intentional zero-dispatch rejections. |

Full disposable-service, recovery, mixed-version, browser, dependency and
native-image validation is also required through the [PR's CI checks](https://github.com/tyk-swe/olp/pull/219/checks).
Those checks publish their own exact head and result; this local receipt does
not substitute a selected test run for the full CI workflow.

The public selection used PostgreSQL/Valkey and the pinned workspace SDKs under
the [service prerequisites](../../../tests/README.md). It includes every Go
symbol referenced by the previous matrix, the new audio/Cohere/Responses
workflows, and explicit identity/privacy/revocation/accepted-work negatives:

```sh
go test -mod=readonly -race -tags=integration,pythonsdk \
  -run '^(TestAcceptedStrictBatchCancellationSurvivesClientDisconnect|TestAudioTranslationPublicStrictRouteAndPinnedSDK|TestContinuationConnectionLossDoesNotInventAcceptedWork|TestGeminiAcceptedResourceCommitsOutliveClientDisconnect|TestGeminiBackgroundStreamResumesSameOwnedWorkAfterReaderLoss|TestGeminiInteractionRefusesEscapedNativeResourceIDs|TestGeminiInteractionsPublicOwnedTwoTurnAndResourceLifecycle|TestGeminiLifecyclePinnedOfficialSDKsThroughTrustedTLS|TestGeminiLifecycleRefusesUnauthorizedStateAndInvalidSetupBeforeProviderWork|TestGeminiLivePublicNativeAudioAndSetupAffinity|TestNativeMediaSourceRejectsNestedAmbiguityBeforeDispatch|TestNegotiatedContinuationOfficialJavaScriptSDK|TestNegotiatedContinuationOfficialPythonSDK|TestPublicContinuationFaultsNeverReplayUnknownInference|TestStrictBackgroundFailedUnaryResourceRedactsEscapedCredential|TestStrictBackgroundPostStreamFailedTerminalSettlesBeforeDelivery|TestStrictBackgroundResourceHTTPErrorRedactsEscapedCredential|TestStrictBackgroundResponseFailedTerminalIsVisibleAndSettled|TestStrictBackgroundResponsePendingCancelAndExpiry|TestStrictBackgroundResponsePinnedJavaScriptSDKRecovery|TestStrictBackgroundResponseStreamRecoversAfterReaderLoss|TestStrictBackgroundStreamErrorKeepsAcceptedWorkRecoverable|TestStrictBackgroundStreamRejectsResponseIDDrift|TestStrictBatchPinnedOpenAISDKs|TestStrictBatchSourcePartialFilesAndLifecycle|TestStrictClassificationAndNativeCountPublic|TestStrictCohereNativeEmbedV2PreservesTypedStorageAndBilling|TestStrictCohereNativeEmbedV2RetainsMultimodalInputAndRejectsCorruption|TestStrictCohereNativeRejectsForeignControlsAndUninspectablePolicyFields|TestStrictCohereNativeRerankPreservesResultsAndBillsOnce|TestStrictCohereNativeV2CatalogueKeepsCompatibilityPresetSeparate|TestStrictFileEarlyProviderAcceptanceNeverLooksComplete|TestStrictMediaPublicNativeSourcesAndAssets|TestStrictNativeHostedEmbeddingShapesPublic|TestStrictNativeRerankScoresAndIdentityPublic|TestStrictNativeSparseAndMultivectorPublic|TestStrictNativeVectorStoragePublic|TestStrictOperationsPinnedSDKStorage|TestStrictPublicAnthropicDocumentAssetIdentity|TestStrictPublicQualifiedTextAndPreciseRefusals|TestStrictPublishedProviderProfilesPreserveCloudInvocation|TestStrictQualifiedUnaryOperationsPublic|TestStrictRealtimeCurrentNetworkCredentialRevocation|TestStrictRealtimeNativeIdentityAndRefusals|TestStrictRealtimeNormalCloseContracts|TestStrictResponseParentIdentitySurvivesParentExpiry|TestStrictUnaryBackgroundResponseRetainsOneAcceptedWork|TestStrictVideoNativeSourceSurvivesKeyRotation|TestStrictVideoPinnedOpenAISDKs|TestStrictVideoPublicOriginalAssetsAndDurableIdentity)$' \
  -count=1 -v -timeout=15m ./tests/integration
```

The batch, video, Gemini and translated-continuation tests execute their
pinned JavaScript/Python client scripts; the two standalone SDK commands
above additionally exercise the native OpenAI/Anthropic/Gemini smoke clients.
All observations are local scripted protocol/effect evidence, not provider-wide
serving or intelligence guarantees.

## Bounded performance observations

The exact command, strict route/profile overlays, all 22 measured paths and
available metrics are in the [small smoke record](../../evidence/fidelity-performance/release-smoke-2026-09-24.json).
It ran the existing canonical `BenchmarkFidelity` with 64 reported samples per
path, concurrency 1 and 8, `GOMAXPROCS=4`, and a two-minute timeout. Setup and
connection warmup are outside the reported timed sample counts. Complete
request/result/SSE and effect oracles passed, including rejection with no
provider dispatch.

The following are descriptive whole-fixture gateway-path observations, in
microseconds and bytes. They include the local client/provider and oracle;
they are not isolated gateway allocations or stable tail estimates. The JSON
record also includes relay observations, CPU, event delays and sampled heap.

| Workload | Latency p50 µs | Latency p95 µs | Bytes allocated/request | Allocations/request |
| --- | ---: | ---: | ---: | ---: |
| `native_unary/c1/gateway` | 859.2 | 1,496.0 | 77,466 | 1,008 |
| `native_unary/c8/gateway` | 1,843.0 | 4,101.0 | 81,283 | 1,021 |
| `native_stream_256/c1/gateway` | 18,725.0 | 21,486.0 | 3,174,576 | 40,924 |
| `native_stream_256/c8/gateway` | 48,110.0 | 78,313.0 | 3,172,599 | 40,930 |
| `native_slow_stream_64/c1/gateway` | 32,654.0 | 44,297.0 | 24,873,971 | 12,400 |
| `native_slow_stream_64/c8/gateway` | 135,712.0 | 167,469.0 | 24,760,946 | 12,345 |
| `native_asset_png/c1/gateway` | 120,187.0 | 140,007.0 | 89,861,648 | 1,727 |
| `native_asset_png/c8/gateway` | 250,762.0 | 324,049.0 | 89,826,661 | 1,643 |
| `translated_unary/c1/gateway` | 997.1 | 1,159.0 | 95,921 | 1,323 |
| `translated_unary/c8/gateway` | 2,167.0 | 3,980.0 | 100,458 | 1,338 |
| `rejected_extension/c1/gateway` | 330.2 | 430.2 | 31,175 | 391 |
| `rejected_extension/c8/gateway` | 654.5 | 1,648.0 | 34,480 | 399 |

The 1 MiB asset case remains allocation-heavy: 89,861,648 B/op at c1 in this
whole-fixture sample, compared with 89,869,125 B/op in the earlier bounded
cleanup smoke. That comparison is descriptive and does not establish
statistical noninferiority. No workload was omitted or retried to improve a
number. Resource limits, overflow and cancellation are checked separately by
functional/race/service tests. Historical failed and invalid studies keep
their original outcomes in the [archive](../../evidence/fidelity-performance/archive.md);
the amended T09/G6 release criteria do not relabel those studies as passing.

## Review and evidence boundaries

Independent Standards and Spec reviews are clear at this code revision. They
closed the original three correctness findings, the audio translation,
retained Responses streaming and native Cohere feature gaps, and follow-up
failed-event privacy/state and nested-policy issues. The Spec audit traces
all 66 stories and D01–D23; no feature requirement was removed. The product
changes are covered by public boundary regressions, including canceled-client
commits delayed by real database locks, escaped identities/secrets, current
credential revocation, native terminal events, cursor recovery and zero
redispatch.

The compatibility ledger reports scoped combinations and evidence states;
unknown provider/model conjunctions and empirical quality remain unknown.
A fixture's existence, a connectivity probe, a passing negative control or a
short timing sample is not counted as empirical parity.

## Local log integrity

These small hashes identify the local command logs retained under
`/tmp/olp-spec-context/release-final/`; raw logs are not added to the repository.
The commands and checked-in test sources provide the reproduction path, while
PR CI publishes its own validation logs.

| Log | SHA-256 |
| --- | --- |
| `make-check.log` | `0151718c5a7daea6c0228d1a79a22642de940acfb2640ed6742f35d9acb3e999` |
| `make-build.log` | `f38df04b2c34af6889f64aaebabb117529f80f84af0cbadb9ff6955620f852b8` |
| `make-race.log` | `6634d01996ab08e7120f5a8d2c9051697952c1586316a2b1fd69483d9ae50ba5` |
| `contracts.log` | `65ef96d378c82046556659421ad059afe9a56906ed2bc08d27f5c310bb4bc1e9` |
| `helm.log` | `bee8b11def487a885b62192ae0053dd23563fc3004319213ae97f93fce2ba783` |
| `public-sdk-race.log` | `1b1f3356590c5e952bea1bd1fa9255d392be23cf9d63498cdb55f4c66838747f` |
| `native-js-sdk.log` | `4d2f32f3043d03759a448549bd797e6ccb28c0fd3459a6620ef6b2b41943b558` |
| `native-python-sdk.log` | `6eb0bbcf96f9f877fe9e9affbed0c2f60a0e21026fe9b00acff85c419aa8ac4c` |
| `strict-performance-smoke.log` | `7753d3d10654cfbd75cfe673aeaa917a39008b37472822c30d86cf46490bdbed` |
