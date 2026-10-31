# OpenLLMProxy handbook

This handbook explains how to use, operate and extend OpenLLMProxy. Start with
[Getting Started](getting-started.md) to install OLP, connect a provider,
publish a route and make a request. Read [Concepts](concepts.md) next for the
route, publication, key and accounting model that the task guides rely on.

Guides describe current behavior. Internal plans and qualification evidence can
describe accepted targets or scoped test results, so they should not be read as
product documentation unless a guide links to them for that purpose.

## Use the gateway

| Task | Guide |
| --- | --- |
| Connect providers, certify models and publish routes | [Provider connections and routing](use/provider-routing.md) |
| Choose connection profiles, provider authentication and private networking | [Provider profiles](use/provider-profiles.md) |
| Connect Azure, Bedrock, SageMaker or watsonx | [Azure](use/providers/azure.md), [Bedrock](use/providers/bedrock.md), [SageMaker](use/providers/sagemaker.md) and [watsonx](use/providers/watsonx.md) |
| Serve or consume supported media operations | [Media providers](use/providers/media.md) |
| Check endpoint, protocol and translation support | [Compatibility](use/compatibility.md) |
| Configure SDKs, frameworks and coding clients | [Clients](use/clients.md) and [Code mode](use/code-mode.md) |
| Manage users, projects, keys, identity and budgets | [Access](use/access.md) |
| Enforce content and tool policies | [Guardrails](use/guardrails.md) |
| Connect named external guardrail services | [Guardrail integrations](use/guardrail-integrations.md) |
| Cache responses or control provider prompt caching | [Caching](use/caching.md) |
| Install provider plugins and register upstream MCP servers | [Plugins](use/plugins.md) and [MCP registration](use/mcp-servers.md) |
| Work in the console and publish a model catalog | [Console](use/console.md) |

## Operate an installation

| Task | Guide |
| --- | --- |
| Deploy, size and verify a production installation | [Deployment](operate/deployment.md) |
| Configure environment variables, file secrets and portable configuration | [Configuration](operate/configuration.md) |
| Monitor incidents, reconcile queues, back up and recover | [Operations](operate/operations.md) |
| Configure telemetry sinks, payload capture, tracing, metrics and alerts | [Observability](operate/observability.md) |
| Understand list prices, commitments, discounts and consumer charges | [Pricing](operate/pricing.md) |
| Export FOCUS and metering data and reconcile provider invoices | [FinOps](operate/finops.md) |
| Bind external secret versions or wrap master keys | [External secrets](operate/external-secrets.md) |
| Use the CLI, management MCP, GitHub Actions or infrastructure as code | [Automation](operate/automation.md) |

## Develop and qualify changes

[Contributing](../CONTRIBUTING.md) covers local setup and the normal change
workflow. [Testing](../tests/README.md) explains how to select a suite. The
remaining guides cover the implementation contracts behind a change.

| Task | Guide |
| --- | --- |
| Find feature owners and understand gateway execution | [Architecture](develop/architecture.md) |
| Review trust, authorization, secret and data boundaries | [Security](develop/security.md) |
| Author a provider or guardrail plugin | [Plugin authoring](develop/plugin-authoring.md) |
| Maintain signed model facts, prices and calibration data | [Catalog maintenance](develop/catalog.md) |
| Reproduce benchmarks and assess performance budgets | [Performance](develop/performance.md) |
| Qualify a candidate and identify outstanding external checks | [Qualification](develop/qualification.md) |
| Compare coding-client capabilities with accepted target contracts | [Code-mode baseline and roadmap](develop/plans/code-mode-parity.md) |
| Choose the next unfinished implementation outcome | [Roadmap](roadmap.md) |
| Work from the retained implementation designs | [Guardrails](develop/plans/guardrails.md), [API surfaces](develop/plans/api-surface.md) and [Agent gateway](develop/plans/agent-gateway.md) |

The seven retained console screenshots live in [assets](assets/). The provider,
console and external-secrets guides link to the screenshot that belongs to each
workflow. Browser test screenshots use their existing output locations.
