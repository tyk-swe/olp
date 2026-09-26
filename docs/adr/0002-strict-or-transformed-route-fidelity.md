# Declare route fidelity as strict or transformed, defaulting to strict

A route's fidelity is either strict or transformed. Strict preserves execution,
observation, permitted continuation and effects relative to the selected
target's native invocation. Transformed permits changing an invocation or its
observed result, such as translating between dialects or redacting content. Two
modes give route authors two distinct promises, and any other value is rejected
with a typed field error.

An omitted fidelity, `null` or `{}` means strict in the management API,
configuration plan and apply, and the console. Nothing is inherited from an
earlier draft or revision, so a draft body or configuration entry is the whole
truth and exports round-trip exactly. We chose strict as the default because it
fails closed: a route that cannot keep the strict promise fails validation or
activation, with guidance to declare it transformed, instead of silently
serving translated or redacted traffic.

Strict routes reject redaction at draft validation, activation and
configuration promotion. Activation and release installation compile an
interaction template for each strict target, and requests bind values to those
templates before semantic eligibility and ranking. Operations, clients, effects
and continuation mechanisms without a complete contract fail closed.

A published slug moves between strict and transformed through an ordinary new
revision, so clients keep their model name. Stored strict responses, files,
batches, continuations and video jobs are refused while their route is
transformed, because the route no longer promises the contract they were stored
under.
