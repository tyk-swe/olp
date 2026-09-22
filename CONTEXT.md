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

**Serving identity**:
The selected model and its serving environment, including the API profile,
upstream principal, region, resource scope, and observable revisions.

**Interaction contract**:
The scoped promise that execution, client observation, permitted continuation,
and effects are preserved for a particular operation and client behavior.

**Continuation**:
A permitted next interaction whose retained native dependencies correspond to
the client's visible history and selected branch.
