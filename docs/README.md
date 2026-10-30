# OpenLLMProxy handbook

Use OpenLLMProxy to connect providers, publish model routes and control access,
cost and operations from one self-hosted gateway. Start with
[Getting Started](getting-started.md), then use the guide for your task.
[Concepts](concepts.md) explains routes, revisions, keys and accounting.

## Use the gateway

| Task | Guide |
| --- | --- |
| Connect providers, certify models and publish routes | [Provider connections and routing](use/provider-routing.md) |
| Choose profiles, authentication and private networking | [Provider profiles](use/provider-profiles.md) |
| Connect Azure, Bedrock, SageMaker or watsonx; serve media | [Azure](use/providers/azure.md), [Bedrock](use/providers/bedrock.md), [SageMaker](use/providers/sagemaker.md), [watsonx](use/providers/watsonx.md), [media](use/providers/media.md) |
| Check endpoint support and translation limits | [Compatibility](use/compatibility.md) |
| Configure SDKs, frameworks and coding clients | [Clients](use/clients.md), [code mode](use/code-mode.md) |
| Manage people, keys, identity and budgets | [Access](use/access.md) |
| Enforce content and tool policies | [Guardrails](use/guardrails.md) |
| Cache responses or insert provider cache controls | [Caching](use/caching.md) |
| Install provider plugins or register upstream tools | [Plugins](use/plugins.md), [MCP registration](use/mcp-servers.md) |
| Work in the console and publish a model catalog | [Console](use/console.md) |

## Operate an installation

| Task | Guide |
| --- | --- |
| Deploy, size and verify a production installation | [Deployment](operate/deployment.md) |
| Set variables, file secrets and portable configuration | [Configuration](operate/configuration.md) |
| Monitor incidents, reconcile queues, back up and recover | [Operations](operate/operations.md) |
| Configure sinks, payload capture, tracing, metrics and alerts | [Observability](operate/observability.md) |
| Understand prices, commitments, discounts and charge | [Pricing](operate/pricing.md) |
| Export FOCUS/metering data and reconcile invoices | [FinOps](operate/finops.md) |
| Bind external secret versions or wrap master keys | [External secrets](operate/external-secrets.md) |
| Use the CLI, management MCP, GitHub Actions or infrastructure as code | [Automation](operate/automation.md) |

## Develop and qualify changes

[Contributing](../CONTRIBUTING.md) owns local setup and change workflow;
[Testing](../tests/README.md) covers suite selection.

| Task | Guide |
| --- | --- |
| Find feature owners and understand execution | [Architecture](develop/architecture.md) |
| Review trust, authorization, secrets and data boundaries | [Security](develop/security.md) |
| Author a provider or guardrail plugin | [Plugin authoring](develop/plugin-authoring.md) |
| Maintain signed model facts, prices and calibration | [Catalog maintenance](develop/catalog.md) |
| Reproduce benchmarks and assess performance budgets | [Performance](develop/performance.md) |
| Qualify a candidate and find outstanding external checks | [Qualification](develop/qualification.md) |
| Compare coding-client capabilities and planned contracts | [Code-mode baseline and roadmap](develop/plans/code-mode-parity.md) |
| Choose the next unfinished outcome | [Roadmap](roadmap.md) |
| Implement remaining designs | [Guardrails](develop/plans/guardrails.md), [API surfaces](develop/plans/api-surface.md), [agent gateway](develop/plans/agent-gateway.md) |

The seven retained console screenshots live in [assets](assets/). Their use is
linked from the relevant provider, console and external-secrets guides. Browser
test screenshots continue to use their existing output locations.
