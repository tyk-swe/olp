# Magpie compared with OLP code mode

Status, Draft research note. Facts below come from primary sources where possible, and each section names its source so a reader can check whether a claim is stale or broken.

## What Magpie is

Magpie describes itself as giving every agent every model, and it runs as a free and open source local gateway for coding agents on the developer machine. Sources are the [Magpie homepage](https://usemagpie.ai/), the [Magpie start guide](https://usemagpie.ai/docs/start), and the [Magpie source repository](https://github.com/yetone/magpie).

The gateway listens locally and translates between OpenAI Chat, OpenAI Responses, Anthropic Messages, and Google Gemini in both directions, including streaming, tool calls, and reasoning. Source is the [Magpie homepage](https://usemagpie.ai/) and the linked product docs.

A provider is added once by signing in or by pasting a key, and the same provider then becomes available to every supported agent. Subscriptions such as Claude and ChatGPT plans can act as providers for other agents, and Magpie warns that some vendors may suspend accounts used outside their own client. Sources are the [Magpie start guide](https://usemagpie.ai/docs/start) and the provider and import docs linked from it, including [Magpie import docs](https://usemagpie.ai/docs/import).

Magpie supports a very wide agent list, with the homepage listing more than 30 agents and the repository readme listing around 70 agents at fetch time. Sources are the [Magpie homepage](https://usemagpie.ai/) and the [Magpie source repository](https://github.com/yetone/magpie).

Routing groups pool several models or accounts behind one name, with strategies that include smart reset aware ordering, in order, round robin, least used first, and by intent. Failover can retry on another account before any byte reaches the agent, and rate limited accounts are passed over using vendor reset signals. Source is the routing material linked from the [Magpie homepage](https://usemagpie.ai/), with intent routing detailed at [Magpie intent docs](https://usemagpie.ai/docs/intent).

Intent routing classifies one turn with a small fast model and then keeps that turn on the chosen model through its tool rounds. Source is [Magpie intent docs](https://usemagpie.ai/docs/intent).

Privacy masking swaps secrets and personal data for stable placeholders before a request leaves the machine and swaps real values back into streamed text and tool calls. Source is [Magpie masking docs](https://usemagpie.ai/docs/masking), and the privacy position is stated at [Magpie privacy docs](https://usemagpie.ai/docs/privacy).

The Library shares instructions, MCP servers, skills, hooks, and subagents across agents by writing each agent native format. Sources are [Magpie library docs](https://usemagpie.ai/docs/library) and [Magpie MCP router docs](https://usemagpie.ai/docs/mcp-router).

Remote use covers LAN sharing, Docker, per client gateway keys, and token and cost caps. Sources are [Magpie remote docs](https://usemagpie.ai/docs/remote) and [Magpie Docker docs](https://usemagpie.ai/docs/docker).

The user surface covers a native desktop app, a tray or menu bar panel, a terminal UI, a command line, and a web UI for headless use. Install covers a download page, a shell installer, go install, and Docker. Source is the [Magpie homepage](https://usemagpie.ai/) and the [Magpie start guide](https://usemagpie.ai/docs/start).

Magpie has no paid tier on the fetched site, and the user brings their own keys or subscriptions. Sponsor tiles from API relays appear on the homepage. Source is the [Magpie homepage](https://usemagpie.ai/).

## What OLP code mode is today

OLP is a self hosted team gateway where operators connect providers and coding subscriptions once and then share them behind OLP keys, published routes, budgets, and attempt level accounting. Sources are the [OLP readme](/home/ubuntu/code/olp/1/olp/README.md) and the [OLP glossary](/home/ubuntu/code/olp/1/olp/GLOSSARY.md).

Code mode is the route specific contract where coding clients use authorized accounts, models, and conversation state, and native forwarding is the default. Compatibility behavior needs explicit published route policy. Sources are the [OLP glossary](/home/ubuntu/code/olp/1/olp/GLOSSARY.md), the [code mode guide](/home/ubuntu/code/olp/1/olp/docs/use/code-mode.md), and [ADR 0001 on explicit code mode compatibility](/home/ubuntu/code/olp/1/olp/docs/adr/0001-explicit-code-mode-compatibility.md).

An adapter is the subscription family of a code account, and it decides served paths, upstream host, credential placement, and generated client config. Documented adapters cover Codex, Claude subscription, OpenCode Go, and the GLM, MiniMax, Kimi Code, and Qwen coding plans, each with global and mainland China profiles where applicable. Antigravity exists for controlled fixtures with no public ingress. Source is the [code mode guide](/home/ubuntu/code/olp/1/olp/docs/use/code-mode.md).

Accounts sit in pools, pools bind accounts and keys with optional per account weights, and routes publish immutable revisions that freeze connection config, models, and policy. Conversation affinity pins a conversation tree to an account per model and refuses revoked pins instead of silently switching accounts. Sources are the [code mode guide](/home/ubuntu/code/olp/1/olp/docs/use/code-mode.md) and [ADR 0002 on state aware code account recovery](/home/ubuntu/code/olp/1/olp/docs/adr/0002-state-aware-code-account-recovery.md).

Explicit opt ins cover exact model aliases, ordered model pools, Messages to Codex and Chat to Codex translation, Codex client identity, and paid API targets with monetary policy. Recovery to another account needs a proven complete rejection and positively portable input, and accepted or partially streamed work never replays. Source is the [code mode guide](/home/ubuntu/code/olp/1/olp/docs/use/code-mode.md).

Generated client config covers Codex, Claude Code, and OpenCode today, and the operator flow runs through enrollment, account and pool and route creation, publication, and key handoff. Source is the [code mode guide](/home/ubuntu/code/olp/1/olp/docs/use/code-mode.md), with renderers in the [generated client config code](/home/ubuntu/code/olp/1/olp/internal/codeclient/configuration.go).

Qualification is the largest stated gap, because controlled client journeys no longer run in CI or as release gates and live acceptance for several adapters and translations remains pending or manual. Sources are the [qualification guide](/home/ubuntu/code/olp/1/olp/docs/develop/qualification.md) and the [code mode guide](/home/ubuntu/code/olp/1/olp/docs/use/code-mode.md).

Other stated gaps include no proven native Codex generation bound for hard token budgets, no code specific manual usage resolution for paid targets, refused translation for signed reasoning and several other semantics, and no served Claude Code hello probe or count tokens endpoint. Source is the [code mode guide](/home/ubuntu/code/olp/1/olp/docs/use/code-mode.md).

## Where they differ

| Attribute | Magpie | OLP code mode |
| --- | --- | --- |
| Primary unit | One developer machine | One team installation |
| Default posture | Translate so any agent can use any model | Native forwarding, translation only by explicit policy |
| Agent breadth | Homepage over 30, repo around 70 | Generated config for Codex, Claude Code, OpenCode |
| Provider breadth | Around 20 presets plus any compatible URL, plus subscriptions as providers | Named adapters and coding plans, plus explicit paid targets |
| Routing | Groups with smart, intent, and failover before first byte | Pools with weights, affinity pins, narrow proven rejection recovery |
| Shared config | Library writes instructions, MCP, skills, hooks per agent | Route generated client config fragments only |
| Extras | Masking, intent routing, MCP hub, web search, plugins, profiles | Budgets, attempt accounting, certification, audit, FinOps on the wider gateway |
| Deployment | Local app, tray, TUI, CLI, web, Docker, LAN sharing | Self hosted Go service with PostgreSQL, Valkey, and console |
| Price | Free and open source, bring your own keys | Self hosted, AGPL, pre 1.0 |

The table compresses the sections above, and the sources for each row are the primary links already cited in those sections.

## Open questions for the interview

The word better is unresolved, because Magpie wins on local breadth and immediacy while OLP wins on team governance and accounting, and copying one into the other would trade away the reason the other exists.

Whether OLP should chase the individual developer at all, or stay a team gateway and beat Magpie only on the team job, is unresolved.

Which Magpie capabilities are in scope as parity targets, which are deliberate no go areas under the native forwarding default in [ADR 0001 on explicit code mode compatibility](/home/ubuntu/code/olp/1/olp/docs/adr/0001-explicit-code-mode-compatibility.md), and which need new glossary terms before any design, is unresolved.
