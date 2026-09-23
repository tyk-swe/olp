# Scoped compatibility execution receipt, version 1

This selected public/SDK execution ran at product revision
`66a3ccb3fc737010a345d309ee280a541d4176f5`. The compatibility matrix now
assesses the later locked product `bc325da50c575803c771531dc4e3aab1ac203a53`;
all 38 pinned test-source hashes are unchanged at that revision, and the
original run commit remains its ancestor. That source identity and ancestry
do not substitute for an exact-head rerun. The frozen v1 inventory and
benchmark budgets were not changed. The selected command below ran public
published-route gateway instances,
independently scripted local providers and isolated PostgreSQL databases.
Required PostgreSQL and Valkey service variables were checked before execution,
as described in [tests/README.md](../../../tests/README.md). No paid provider
was called. At the locked product commit, the matrix pins the original
receipt bytes and 66a test-source hashes through Git. This later clarification
does not rewrite that execution evidence. Final exact-head
integration/browser/CI and review checks remain separate.

```sh
GOMAXPROCS=4 go test -mod=readonly -race -tags=integration,pythonsdk \
  -run '^(TestStrictPublishedProviderProfilesPreserveCloudInvocation|TestStrictNativeVectorStoragePublic|TestStrictNativeSparseAndMultivectorPublic|TestStrictNativeHostedEmbeddingShapesPublic|TestStrictNativeRerankScoresAndIdentityPublic|TestStrictClassificationAndNativeCountPublic|TestStrictOperationsPinnedSDKStorage|TestStrictQualifiedUnaryOperationsPublic|TestNativeMediaSourceRejectsNestedAmbiguityBeforeDispatch|TestStrictPublicQualifiedTextAndPreciseRefusals|TestStrictPublicAnthropicDocumentAssetIdentity|TestStrictMediaPublicNativeSourcesAndAssets|TestStrictBatchSourcePartialFilesAndLifecycle|TestStrictBatchPinnedOpenAISDKs|TestStrictUnaryBackgroundResponseRetainsOneAcceptedWork|TestStrictVideoPublicOriginalAssetsAndDurableIdentity|TestStrictVideoPinnedOpenAISDKs|TestStrictVideoNativeSourceSurvivesKeyRotation|TestGeminiInteractionsPublicOwnedTwoTurnAndResourceLifecycle|TestGeminiLivePublicNativeAudioAndSetupAffinity|TestGeminiLifecycleRefusesUnauthorizedStateAndInvalidSetupBeforeProviderWork|TestGeminiLifecyclePinnedOfficialSDKsThroughTrustedTLS|TestGeminiBackgroundStreamResumesSameOwnedWorkAfterReaderLoss|TestNegotiatedContinuationOfficialJavaScriptSDK|TestNegotiatedContinuationOfficialPythonSDK|TestStrictRealtimeNativeIdentityAndRefusals|TestStrictRealtimeNormalCloseContracts|TestStrictFileEarlyProviderAcceptanceNeverLooksComplete|TestContinuationConnectionLossDoesNotInventAcceptedWork|TestPublicContinuationFaultsNeverReplayUnknownInference|TestAcceptedStrictBatchCancellationSurvivesClientDisconnect)$' \
  -count=1 -v -timeout=15m ./tests/integration
```

**Result: 31 top-level test symbols passed under race detection in 102.970 s;
zero selected tests failed or skipped.** The full local output is retained at
`/tmp/olp-spec-context/final-matrix-66a3-combined-race.log`. Each positive or
pre-dispatch-incompatible matrix row names the narrower test source and pins
its SHA-256 and this receipt's SHA-256. The selected run includes the
`integration && pythonsdk` official Python next-turn test that the ordinary
integration tag alone would omit. `scripts/integration.sh` now includes
`pythonsdk` in its required service/race suite, and the CI setup installs the
pinned uv/Python toolchain.

The fresh isolated worktree initially lacked pinned JavaScript packages, so
one setup-only selection failed the JavaScript continuation, Gemini and batch
SDK symbols before `pnpm install --frozen-lockfile` completed. The full
selection above was rerun after installation and passed without skipped
symbols or product assertions. Separately, `./tests/sdk-smoke/run.sh` and
`./tests/sdk-smoke-python/run.sh` passed at this source. Each exercised its
native OpenAI/Anthropic/Gemini SDK surface; the Anthropic native two-tool
workflow observed all 19 events and exactly two verified dispatches. Their
local outputs are `/tmp/olp-spec-context/final-matrix-66a3-native-js.log` and
`/tmp/olp-spec-context/final-matrix-66a3-native-python.log`. An explicitly
selected strict batch test with `OLP_TEST_DATABASE_URL` removed failed
immediately with `OLP_TEST_DATABASE_URL is required; run make integration`;
it did not silently skip. That negative check is recorded at
`/tmp/olp-spec-context/final-matrix-66a3-missing-service.log`.

| Checked class | Public/SDK observation |
| --- | --- |
| 14 strict cloud profiles and registered unary contracts | Published target, exact request/result, typed vector/rerank/count representations and qualified subset/refusal through scripted providers. |
| Direct Anthropic document and compatible media | Inline PDF base64/title/citation order; strict image generation/edit/variation, speech, transcription, event grammar, original multipart assets and binary bytes. |
| Compatible video and Azure durable resources | Original video reference and native metadata, encrypted job source, exact video/thumbnail content, key rotation; batch partial output/error files, cancellation and accepted-disconnect behavior. Official pinned OpenAI JavaScript and Python clients complete video and batch journeys. |
| Direct Gemini Interactions and Live | Public two-turn/cursor and raw audio/video/tool/VAD frame order, official JavaScript/Python TLS clients, same accepted background resource across reader loss and fresh gateway cursor reads. Unowned nonempty Live resumption handle refuses before provider dial. |
| Negotiated tool continuation | Official OpenAI JavaScript and Python **Chat** clients complete streamed two-tool next turns through Anthropic with encrypted recovery; connection/terminal/signature/event faults do not invent a second accepted provider work item. |
| Direct OpenAI and Azure v1 realtime | Exact native VAD/session, interruption, function-call-output and audio-timing WebSocket frames for both profiles; unsupported query and semantic headers refuse with zero provider dials. The terminal regression distinguishes early graceful client cancellation, provider normal close before `response.done`, and clean close after `response.done` with exact Attempt/observation/usage states. |

The strict media/video positives use a certified `compatible-chat` local
fixture. Azure batch/files and unary Responses background use exact versioned
Azure profiles. Those tests do not promote the frozen **direct OpenAI** media,
video, batch or file rows. The direct OpenAI realtime subtest does match the
frozen realtime row's VAD/interruption/tool-timing scope; Azure v1 has its own
additive row. Audio translation has no exposed endpoint, strict Responses
background streaming remains refused, and native Gemini Live resumption still
requires an unimplemented owned mapping. These limits stay visible in the
machine-readable matrix.

The accepted-file-early-response and lost-before-ready continuation cases
are ambiguous negative controls, counted zero positive completions and never
replayed as fresh inference. Wrong accepted batch input identity and four
late continuation terminal/signature/event corruptions are runtime violation
controls, not successful workload samples. Reader loss during an accepted
Gemini background stream does not prove provider work stopped; the separate
resume test proves the same encrypted owned resource and one provider POST.
No script fixture proves live model quality or provider-side exactly-once work.
The later locked product has scoped passing v2 lifecycle, stress and barrier
comparisons, but source paired r6 is invalid and frozen v1 failures remain.
Those outcomes are reported in
[qualification-status-v1.md](qualification-status-v1.md).
