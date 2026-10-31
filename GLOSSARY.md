# OLP

OLP is a gateway that lets teams share model provider accounts and coding subscriptions behind their own keys, routes and budgets. This glossary records the canonical domain language for that gateway, while [Concepts](docs/concepts.md) explains how the gateway applies the general terms.

## Language

### Gateway

**Route slug**:
The public model name that identifies a published route independently from the provider deployment serving it.
_Avoid_: Upstream model name

**Certification**:
Evidence that one exact provider, model, operation, surface and mode passed bounded serving probes.
_Avoid_: Discovery, model listing

**Runtime generation**:
One numbered and immutable publication of the provider, route and policy revisions that gateways serve together.

**Attempt**:
One provider execution made for a client request, with its own pinned revisions, usage and outcome.

**Cost**:
The estimated amount a provider bills the installation for an attempt.

**Charge**:
The amount a consumer owes for an attempt under the applicable rate card.

### Code mode

These definitions include accepted target contracts. The [baseline and roadmap](docs/develop/plans/code-mode-parity.md) separates implemented behavior from planned capabilities.

**Code mode**:
The route-specific contract through which coding clients use authorized accounts, models and conversation state. Native forwarding is its default, while compatibility behavior requires explicit route policy.

**Native forwarding**:
The code-mode contract that preserves the client's native request and response semantics, including unknown fields and stream ordering, within the documented authentication and transport exceptions.

**Adapter**:
The subscription family a code account belongs to. It decides which client paths the account serves and which clients get generated configuration.
_Avoid_: Vendor, provider (in code-mode contexts)

**Coding plan**:
A vendor subscription for coding tools that an operator enrolls by pasting a non-expiring API key the vendor's key page issues. Vendor brand names such as "Token Plan" or "Kimi Code" are display names of individual coding plans.
_Avoid_: Token plan (as a generic term), API-key subscription

**Profile**:
One account system of an adapter, such as a vendor's global or mainland China service, each with its own key page and upstream host.
_Avoid_: Region, endpoint

**Paid API target**:
A metered inference destination governed by monetary spend policy. It is distinct from a subscription adapter or a coding plan even when the same provider offers both.
_Avoid_: Coding plan, subscription account

**Protocol translation**:
Explicitly enabled conversion between a coding client's protocol and an account's native protocol while preserving supported semantics.

**Model alias**:
A public model name that identifies one exact underlying model by default. A model pool or fallback policy explicitly permits choosing a different model.

**Model pool**:
A route policy that deliberately permits choosing among different underlying models exposed through one public model name.

**Identity or prompt compatibility**:
Changes to client identity signals or prompt content for upstream compatibility, authorized separately from protocol translation and model rewriting.

**Route administrator**:
A person with authority to configure and publish a route, including its compatibility policies for eligible shared accounts.

**Conversation affinity**:
The association between a conversation and the account authority needed to continue it. Affinity does not grant permission to use revoked accounts or move account-owned state.
_Avoid_: Sticky authorization

**Safe recovery**:
Recovery to another eligible account when the request is known not to have executed and its continuation state is valid on that account.

**Usage uncertainty**:
An unresolved consumption outcome retained when final upstream usage is unavailable. It is not zero usage.

**Fixture qualification**:
Evidence from controlled peers and repeatable client journeys for a stated revision and scope. It does not establish acceptance by a real subscription service.

**Live qualification**:
Evidence from an authorized end-user workflow against a real upstream account, scoped to the recorded client version, platform, adapter, model, protocol and transport.
