# OpenLLMProxy

OpenLLMProxy, or OLP, is a self-hosted AI gateway and control plane. It lets a
team connect model providers, certify the exact capabilities it will serve,
publish stable route names and manage access, budgets and attempt-level
accounting from one installation.

The gateway serves OpenAI, Anthropic, Gemini and Bedrock client interfaces. Its
persisted request history and telemetry are content-free by default.
Provider-retained resources, response caching and optional external capture each
have a separate and explicit storage contract.

## Start here

Read the [handbook](docs/README.md) for the task guides. To install OLP and make
a first request, follow [Getting Started](docs/getting-started.md). The
[Compatibility](docs/use/compatibility.md) guide states which operations are
supported and where protocol translation changes or refuses a request.

Production operators should continue with
[Deployment](docs/operate/deployment.md) and
[Operations](docs/operate/operations.md). Contributors should start with
[Contributing](CONTRIBUTING.md). Test suite selection is covered in
[Testing](tests/README.md).

## Project status

OpenLLMProxy 0.1.0 is still a work in progress. During 0.x, releases may change
the management API, configuration and storage formats. OLP therefore makes no
compatibility, upgrade, mixed-version or rollback promises.

## License and participation

OpenLLMProxy is licensed under AGPL-3.0-only. Report vulnerabilities privately
under the [security policy](SECURITY.md). Participation follows the
[Code of Conduct](CODE_OF_CONDUCT.md).
