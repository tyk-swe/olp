# Public operation extension demonstrations

This versioned T10 fixture qualifies three separate trusted registration paths
through public management configuration, provider certification, a published
strict route, the real gateway and an independently scripted local provider.
It adds no production provider case, Generation field, or mandatory OpenAI
request representation. All fixture registrations are compiled test code; an
external API user cannot install a codec or unrestricted destination.

1. A new fixture provider profile composes existing `openai_compatible`
   direct-compatible hosting/authentication with the registered TEI embedding
   dialect. The exact native `/v1/embed` request and vector response survive.
2. A new fixture vector dialect owns its request/result schema and OIF source
   views. A trusted relative `vectors` address under direct-compatible hosting
   receives exact payload/task/model identity and returns an ordered native
   vector with negative zero and opaque extension state. An incompatible Azure
   hosting composition is rejected before publication.
3. A new `fixture_signal` operation and dialect own their own input identities
   and native scores. Public capability certification, strict route activation,
   native request and response, and the existing Attempt/accounting path work
   without enlarging Generation or a provider-specific gateway switch.

The scripted provider checks its endpoint and vendor credential on every
request. The tests separately compare provider-bound source bytes and the
client-visible result with hand-authored expectations. Unregistered profiles
and dialects fail before provider dispatch; malformed native results produce a
protocol violation after upstream work, never a success. Every successful
operation carries one shared Attempt and its own operation identity.

The suite lives behind the `extension` Go build tag because the trusted
in-process registries intentionally have no runtime unregister operation.
`scripts/integration.sh` executes it as a separate process after the ordinary
fixed-inventory service suite. Selecting the suite without required disposable
service settings fails rather than skipping. This separation leaves the
normal suite's historical profile counts and all frozen reference fixtures
unchanged. The existing connector kind is reused to show that a new provider
profile and dialect need no new provider transport engine.

The selected three-case public service suite passed with race detection in
7.075 seconds on the final source. All 19 automation script tests,
current release-inventory generation, integration-tagged Go vet and shell
syntax checks passed. Selecting the suite without its database configuration
failed immediately and explicitly. Local scripted providers make no paid
inference calls. These demonstrations establish the extension seams; full G7
and empirical quality remain unqualified.
