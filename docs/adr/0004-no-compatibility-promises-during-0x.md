# Make no compatibility promises during 0.x

OpenLLMProxy has no installations or published data to protect yet, and every
promise to an earlier OLP version costs every later change: migration
discipline, retained formats and tests that depend on history. During 0.x, OLP
therefore makes no compatibility, upgrade, mixed-version or rollback promises.
Any 0.x release may change the management API, configuration artifacts, stored
data and the formats its processes exchange. Code handles only the formats the
current version writes.

Run one version across an installation. A binary refuses a database whose
schema is newer than its own, and migrations never run in reverse. Migrations
stay forward-only and sequential by convention, but no upgrade path between
0.x releases is guaranteed.

Compatibility with upstream providers and the official OpenAI, Anthropic and
Gemini SDKs is the product, not a promise between OLP versions, and this policy
does not relax it. Revisit this decision at 1.0.
