# Native audio translation

`POST /v1/audio/translations` exposes the distinct unary `translation`
operation on OpenAI-family profiles. It uses the existing bounded multipart
upload and media dispatch path. Publish a route with `translation` and a
certified compatible provider/model capability; strict routes use the native
media contract and current authorization, egress and Attempt accounting.

The native request contains the original audio file, the route in `model`,
and optional `prompt`, `temperature` and `response_format`. Only the model
binding and declared absent-only defaults may change on strict routes. File
bytes, filename, content type, part ordering and caller field text remain
unchanged. Temperature accepts finite numbers from zero through one. The
response formats are `json`, `verbose_json`, `text`, `srt` and `vtt`; no
transcription language, streaming or diarization controls are introduced.
Strict JSON replies retain original number spellings and native metadata.

Provider configuration exposes operation-owned prompt, temperature and format
defaults. The route editor, usage history and Advanced playground expose
Audio translation; its public upload form keeps the inference key in the tab,
labels billable execution and supports delivery cancellation. The no-inference
inspector checks the route tuple without staging an audio file.

`TestStrictAudioTranslationPreservesNativeFormatsAndAccounting` and
`TestAudioTranslationRejectsUnsupportedControlsBeforeDispatch` exercise exact
native preservation, all five response formats, accounting event serialization
and pre-dispatch refusal. `TestAudioTranslationPublicStrictRouteAndPinnedSDK`
publishes strict routes through management APIs and runs OpenAI JavaScript SDK
7.4.0 against each response format, checking captured provider fields, provider
dispatch counts, absent-only defaults and public pricing persistence.

The existing `0026_registered_unary_operations.sql` migration already accepts
bounded operation labels for pricing. No historical migration or frozen fixture
inventory is rewritten; translation adds one separately reviewed unary
certification tuple. Scripted qualification does not establish live-model
quality, translation accuracy or provider availability.

Validation on this feature branch passed the gateway, provider and connector
audio tests with race detection; the public strict-route/SDK suite passed all
five formats in 5.019 seconds. Media, routes, provider and usage package races
passed. The console passed 56 unit tests, eight existing playground component
tests and the new upload component test, plus Svelte/TypeScript and edited-file
ESLint checks. Management contracts were regenerated without contract changes;
the release inventory and historical compatibility matrix validators passed.

The committed upload component test follows file selection through multipart
dispatch to the native VTT result. A separate Chromium smoke exercised the same
component with a scripted successful response; the [reviewed screenshot](console-audio-translation.png)
shows only synthetic fixture content and a masked fixture key. That visual
receipt is separate from the public gateway/SDK execution above.
