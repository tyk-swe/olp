# Magpie compared with OLP code mode

Research snapshot from the cited product pages and repository sources. [ADR 0005](../adr/0005-make-code-mode-better-than-magpie.md) resolves the original interview questions and records OLP's accepted priorities.

## Product comparison

| Area | Magpie | OLP code mode |
| --- | --- | --- |
| Audience | Local gateway for one developer, with LAN sharing and Docker support. [Overview](https://usemagpie.ai/), [remote use](https://usemagpie.ai/docs/remote). | Self-hosted team gateway sharing providers and subscriptions through keys, published routes, budgets and attempt accounting. [Overview](../../README.md). |
| Protocols | Bidirectional Chat, Responses, Messages and Gemini translation, including streaming, tools and reasoning. [Overview](https://usemagpie.ai/). | Native forwarding by default. Translation, model aliases, model pools, client identity changes and paid targets require explicit policy. [Code mode](../use/code-mode.md), [ADR 0001](../adr/0001-explicit-code-mode-compatibility.md). |
| Clients and providers | At research time, the homepage listed over 30 agents and the repository around 70, with around 20 provider presets plus compatible URLs. Login or key enrollment makes a provider available across agents, including subscription providers. [Source](https://github.com/yetone/magpie), [setup](https://usemagpie.ai/docs/start), [imports](https://usemagpie.ai/docs/import). | Named adapters for Codex, Claude subscription, OpenCode Go and the GLM, MiniMax, Kimi Code and Qwen coding plans, with global and mainland profiles where applicable. Antigravity has controlled fixtures and no public ingress. Generated configuration covers Codex, Claude Code and OpenCode. [Code mode](../use/code-mode.md), [renderers](../../internal/codeclient/configuration.go). |
| Routing | Groups pool models or accounts with reset-aware, ordered, round-robin, least-used and intent strategies. Failover can switch accounts before the first byte, using vendor resets to skip limited accounts. [Overview](https://usemagpie.ai/), [intent routing](https://usemagpie.ai/docs/intent). | Weighted account pools, immutable published revisions and conversation affinity per account and model. Revoked pins refuse service. Recovery needs a complete proven rejection and portable input. Accepted or partially streamed work never replays. [Code mode](../use/code-mode.md), [ADR 0002](../adr/0002-state-aware-code-account-recovery.md). |
| Shared configuration | The Library writes instructions, MCP servers, skills, hooks and subagents in each agent's native format. [Library](https://usemagpie.ai/docs/library), [MCP router](https://usemagpie.ai/docs/mcp-router). | Route-specific generated client configuration. Operators enroll accounts, create pools and routes, publish them and hand off keys. [Code mode](../use/code-mode.md). |
| Other capabilities | Privacy masking, intent routing, MCP hub, web search, plugins and profiles. Masking restores secret placeholders in streamed text and tool calls. Intent routing holds one model through a turn's tool rounds. [Masking](https://usemagpie.ai/docs/masking), [privacy](https://usemagpie.ai/docs/privacy), [intent](https://usemagpie.ai/docs/intent), [overview](https://usemagpie.ai/). | Team budgets, attempt accounting, certification, audit and FinOps in the wider gateway. [Handbook](../README.md). |
| Deployment and cost | Desktop app, tray, TUI, CLI and headless web UI. Download, shell installer, Go and Docker installation. Remote use has per-client keys and token and cost caps. Free and open source with user-supplied keys or subscriptions, no paid tier on the fetched site, and API-relay sponsor tiles. [Setup](https://usemagpie.ai/docs/start), [Docker](https://usemagpie.ai/docs/docker), [remote use](https://usemagpie.ai/docs/remote), [overview](https://usemagpie.ai/). | Go service with PostgreSQL, Valkey and a console. Self-hosted, AGPL and pre-1.0. [Overview](../../README.md). |

Magpie warns that vendors may suspend subscriptions used outside their own clients. Its broad compatibility posture differs from OLP's explicit authorization boundaries. [Magpie setup](https://usemagpie.ai/docs/start) and the [OLP glossary](../../GLOSSARY.md) define those contracts.

## OLP qualification gaps

At research time, controlled client journeys no longer ran in CI or as release gates, and live acceptance for several adapters and translations was pending or manual. The [qualification guide](../develop/qualification.md) and [code-mode guide](../use/code-mode.md) record that limitation.

The code-mode guide also records no proven native Codex generation bound for hard token budgets, no code-specific manual usage resolution for paid targets, refused translation for signed reasoning and other unsupported semantics, and no served Claude Code hello probe or count-tokens endpoint.

## Accepted direction

OLP prioritizes reliable team subscription pooling over local developer parity. [ADR 0005](../adr/0005-make-code-mode-better-than-magpie.md) combines better selection for new conversations with qualified rejection recovery, preserves native forwarding and hard pins, defers broader onboarding and shared configuration, and rejects a local single-developer mode.
