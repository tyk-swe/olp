# Scoped compatibility execution receipt, version 1

The assessed product revision is
`bf3058bf2b34221df961df0b3e2e6e78d15f5d23`. The matrix's test branch
at `236073201c7f50056d09adbf895ee661ad682dac` adds official video SDK,
Live-refusal, variation, and same-resource Gemini background fixtures plus the
required Python continuation build tag. It changes no provider model or
frozen v1 inventory/benchmark reference. The selected test command below ran
against one published-route gateway and independently scripted local providers
with isolated PostgreSQL databases. Required service variables are described
in [tests/README.md](../../../tests/README.md); no paid provider was called.

```sh
GOMAXPROCS=4 go test -mod=readonly -race -tags=integration,pythonsdk \
  -run '^(TestStrictPublishedProviderProfilesPreserveCloudInvocation|TestStrictNativeVectorStoragePublic|TestStrictNativeSparseAndMultivectorPublic|TestStrictNativeHostedEmbeddingShapesPublic|TestStrictNativeRerankScoresAndIdentityPublic|TestStrictClassificationAndNativeCountPublic|TestStrictOperationsPinnedSDKStorage|TestStrictQualifiedUnaryOperationsPublic|TestNativeMediaSourceRejectsNestedAmbiguityBeforeDispatch|TestStrictPublicQualifiedTextAndPreciseRefusals|TestStrictPublicAnthropicDocumentAssetIdentity|TestStrictMediaPublicNativeSourcesAndAssets|TestStrictBatchSourcePartialFilesAndLifecycle|TestStrictBatchPinnedOpenAISDKs|TestStrictUnaryBackgroundResponseRetainsOneAcceptedWork|TestStrictVideoPublicOriginalAssetsAndDurableIdentity|TestStrictVideoPinnedOpenAISDKs|TestStrictVideoNativeSourceSurvivesKeyRotation|TestGeminiInteractionsPublicOwnedTwoTurnAndResourceLifecycle|TestGeminiLivePublicNativeAudioAndSetupAffinity|TestGeminiLifecycleRefusesUnauthorizedStateAndInvalidSetupBeforeProviderWork|TestGeminiLifecyclePinnedOfficialSDKsThroughTrustedTLS|TestGeminiBackgroundStreamResumesSameOwnedWorkAfterReaderLoss|TestNegotiatedContinuationOfficialJavaScriptSDK|TestNegotiatedContinuationOfficialPythonSDK|TestStrictRealtimeNativeIdentityAndRefusals|TestStrictFileEarlyProviderAcceptanceNeverLooksComplete|TestContinuationConnectionLossDoesNotInventAcceptedWork|TestPublicContinuationFaultsNeverReplayUnknownInference|TestAcceptedStrictBatchCancellationSurvivesClientDisconnect)$' \
  -count=1 -v -timeout=15m ./tests/integration
```

**Result: 30 top-level test symbols passed under race detection in 105.113 s;
zero selected tests failed or skipped.** The full local output is retained at
`/tmp/olp-spec-context/final-matrix-combined-row-race.log`. Each positive or
pre-dispatch-incompatible matrix row names the narrower test source and pins
its SHA-256 and this receipt's SHA-256. The selected run includes the
`integration && pythonsdk` official Python next-turn test that the ordinary
integration tag alone would omit. `scripts/integration.sh` now includes
`pythonsdk` in its required service/race suite, and the CI setup installs the
pinned uv/Python toolchain.

| Checked class | Public/SDK observation |
| --- | --- |
| 14 strict cloud profiles and registered unary contracts | Published target, exact request/result, typed vector/rerank/count representations and qualified subset/refusal through scripted providers. |
| Direct Anthropic document and compatible media | Inline PDF base64/title/citation order; strict image generation/edit/variation, speech, transcription, event grammar, original multipart assets and binary bytes. |
| Compatible video and Azure durable resources | Original video reference and native metadata, encrypted job source, exact video/thumbnail content, key rotation; batch partial output/error files, cancellation and accepted-disconnect behavior. Official pinned OpenAI JavaScript and Python clients complete video and batch journeys. |
| Direct Gemini Interactions and Live | Public two-turn/cursor and raw audio/video/tool/VAD frame order, official JavaScript/Python TLS clients, same accepted background resource across reader loss and fresh gateway cursor reads. Unowned nonempty Live resumption handle refuses before provider dial. |
| Negotiated tool continuation | Official OpenAI JavaScript and Python **Chat** clients complete streamed two-tool next turns through Anthropic with encrypted recovery; connection/terminal/signature/event faults do not invent a second accepted provider work item. |
| Direct OpenAI and Azure v1 realtime | Exact native VAD/session, interruption, function-call-output and audio-timing WebSocket frames for both profiles; unsupported query and semantic headers refuse with zero provider dials, while abrupt disconnect records one cancelled native Attempt. |

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
No script fixture proves live model quality, provider-side exactly-once work,
or a passing frozen performance comparison. Those outcomes are reported in
[qualification-status-v1.md](qualification-status-v1.md).
