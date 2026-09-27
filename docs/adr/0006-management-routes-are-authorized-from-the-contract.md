# Authorize management routes from the contract

Each management operation's `security` requirement in
`openapi/management.json` names the operations it needs and whether it needs
an installation-wide scope, for browser sessions and, when those operations
can be delegated, for management tokens. `internal/access` mounts every
management route through `Route`, `Stream`, or `Public`, which read that
requirement at registration and admit the caller before the handler runs. The
policy table in `internal/access/policy.go` decides which roles and tokens hold
each operation. The contract decides what a route needs and the policy decides
who holds it; neither restates the other.

Previously every handler named its operation inline, about 130 times, beside
ad-hoc role and scope checks. The contract advertised management tokens on 18
operations while the code accepted them on about 120, and no test tied the two
together. Declaring the requirement once, where API clients already read it,
makes the published contract the authorization record. It also means no
handler code runs for a caller the route refuses, which removed reads and body
decoding ahead of authorization.

A route the contract does not declare, or declares with a different
visibility, cannot be mounted, and the loader refuses a contract that does not
read truthfully. Handlers may demand more than the entry requirement through
`Principal.Authorize`, such as the operation a record's project implies, but
never less. Mutations call `Reauthorize` inside their transaction, after the
installation lock, so they commit only under authority that is still current.
Golden files pin the policy matrix and the callers every route admits; review
a diff to either, or to a `security` block, as an authorization change.
