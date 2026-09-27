# OpenLLMProxy

OpenLLMProxy routes inference requests through published provider configurations.
This glossary distinguishes the request from the attempts made to serve it.

## Language

**Inference request**:
A caller's request for an operation through a published route, with one identity
across all attempts made to serve it.

**Route target**:
A provider model configured on a route, with the priority, weight and timeout
that govern its selection.

**Credential slot**:
A configured provider access choice, optionally bound to a credential version,
with its own eligibility and quota constraints.

**Attempt**:
One budgeted try to serve an inference request through a route target and
credential slot, including a local target quota refusal before upstream dispatch.
_Avoid_: Upstream call (an attempt need not reach the provider).

**Attempt budget**:
The maximum number of attempts permitted for an inference request; candidates
skipped because they are revoked, cooled or unmeterable do not consume it.

**Failover**:
Another attempt to serve the same inference request after an eligible failure,
permitted only before response commitment and when the upstream outcome is not
classified as ambiguous.

**OIF (OpenLLMProxy Internal Format)**:
The source-preserving representation of an operation's request, result, or event,
including its native meaning and the obligations required to preserve it.

**Dialect**:
A versioned API language defining an operation's requests, results, events, and
continuation rules, independently of the environment that hosts it.

**Provider profile**:
A versioned, compatible combination of dialect, hosting, authentication, and
transport for a provider-model binding.

**Automatic provider**:
A provider without a provider profile, whose endpoints follow from its provider
kind; strict routes refuse it.
_Avoid_: Legacy configuration.

**Provider plugin**:
An operator-installed extension that supplies a provider profile's
authentication and hosting around a built-in dialect, and its transport when
unconfined; it never defines a dialect.
_Avoid_: Connector plugin, auth plugin, coding plan.

**Confined plugin**:
A provider plugin that can reach only the capabilities OpenLLMProxy grants it;
the default for every provider plugin.

**Unconfined plugin**:
An experimental provider plugin that runs with native process privileges and may
carry upstream traffic itself, permitted only by an installation's deployment
opt-in.
_Avoid_: Trusted plugin (every plugin is operator-chosen; confinement differs).

**Hosting adaptation**:
A provider plugin's declared placement of a built-in dialect at an upstream:
its address, headers, envelope and declared rewrites of the dialect body, with
the upstream's model discovery and failure classification.

**Plugin option**:
A non-secret provider setting a plugin profile declares, such as an account,
region or project, which the operator sets per provider and which its hosting
adaptation and plugin code receive.
_Avoid_: Connection option (names any provider's connector settings).

**Grant enrollment**:
The operator's authorization of an upstream account through a provider plugin,
by loopback paste-back, out-of-band code or device authorization.
_Avoid_: Enrollment (names OIDC sign-in-method enrollment), login, OAuth setup.

**Grant**:
The rotating upstream authorization held beneath one credential version,
advanced only by OpenLLMProxy's refresh and ended by that version's revocation.
_Avoid_: OAuth token, session.

**Grant facts**:
Non-secret values a provider plugin reports about a grant, such as the upstream
account, project or base URL, available to its hosting adaptation.

**Lapsed grant**:
A grant that can no longer be refreshed; its credential version's slots are
ineligible until a new grant enrollment replaces it.
_Avoid_: Expired grant (access-token expiry is routine), revoked credential
(revocation is an operator act).

**Observed principal**:
An upstream principal reported by grant enrollment rather than declared by the
operator; enrolling a different account changes the serving identity.

**Serving identity**:
The selected model and its serving environment, including the API profile,
upstream principal, region, resource scope, and observable revisions.

**Interaction contract**:
The scoped promise that execution, client observation, permitted continuation,
and effects are preserved for a particular operation and client behavior.

**Continuation**:
A permitted next interaction whose retained native dependencies correspond to
the client's visible history and selected branch.

**Continuation handle**:
An opaque reference to the complete native dependencies for a permitted next
interaction; possession does not replace authorization.

**Submission identity**:
A caller's identity for one inference request whose retries refer to the same
accepted work and delivery, rather than another inference request.

**Delivery replay**:
Delivering already-recorded output from the same accepted work without
performing inference again.

**Route fidelity**:
A route's declared strict or transformed treatment of invocation semantics;
native identity and qualified interaction describe individual plans.
_Avoid_: Legacy (not a fidelity mode).

**Strict route**:
A route whose admitted interactions preserve execution, observation, permitted
continuation, and effects relative to the selected target's native invocation.

**Upstream acceptance**:
What is known about provider work: not sent, outcome unknown, accepted, or
terminal. Absence of client-visible bytes does not establish absence of work.

**Client observation**:
What the caller may have received: unobserved, partially observed, actionable,
or terminal. This is independent of upstream acceptance and response commitment.

**Transformed route**:
A route permitted to change an invocation or its observed result, such as
translating between dialects or redacting content.
_Avoid_: Legacy route, best-effort route.
