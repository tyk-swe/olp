# OpenLLMProxy

OpenLLMProxy is a self-hosted AI gateway and control plane built with Go,
SvelteKit, PostgreSQL and Valkey. Connect providers, certify model capabilities,
publish stable routes and manage access, budgets and attempt accounting across
OpenAI, Anthropic, Gemini and Bedrock client interfaces.

**[Read the handbook](docs/README.md)** or **[install and make your first request](docs/getting-started.md)**.
For supported operations and translation limits, see
[Compatibility](docs/use/compatibility.md). Persisted telemetry stays content-free;
provider-retained state, response caching and opt-in external capture have
explicit storage contracts.

OpenLLMProxy 0.1.0 is a work in progress. During 0.x it makes no compatibility,
upgrade, mixed-version or rollback promises: releases may change the management
API, configuration and storage. [Roadmap](docs/roadmap.md) tracks unfinished work
and qualification gates.

For local development, follow [Contributing](CONTRIBUTING.md); suite selection is
in [Testing](tests/README.md). Production operators should start with
[Deployment](docs/operate/deployment.md) and [Operations](docs/operate/operations.md).

Licensed under AGPL-3.0-only. Report vulnerabilities through the
[security policy](SECURITY.md); participation follows the [Code of Conduct](CODE_OF_CONDUCT.md).
