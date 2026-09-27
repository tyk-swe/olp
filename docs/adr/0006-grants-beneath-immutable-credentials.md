# Hold rotating upstream tokens as grants beneath immutable credential versions

Subscription backends can rotate refresh tokens on every refresh, while
published revisions pin a credential version until the provider is activated
again. Grant enrollment therefore creates an ordinary immutable credential
version recording the plugin, observed principal and grant facts. The rotating
tokens live beneath it as a grant, encrypted by the secret authority and advanced
only by refresh. A credential version per refresh would force constant
reactivation; rewriting the version in place would break the immutability that
revisions and attempts rely on.

A worker refreshes each grant ahead of expiry under a Postgres advisory lock, so
a rotating refresh token is spent once. Gateways receive only current access
tokens through their authority poll and never hold refresh tokens; a gateway
credential failure requests an early refresh. Mounted gateways, which run without
the master key, refuse credentials that have grants; static plugin credentials
still mount.

A refresh that fails permanently lapses the grant. Its credential version's slots
become ineligible instead of cooling for a minute, routing fails over, and the
operator is notified; only a new grant enrollment replaces it. The observed
principal is part of serving identity: every slot in a provider revision observes
the same principal, so re-enrolling that account is a credential rotation and
enrolling another is a serving-identity change. Several accounts are pooled
through several provider targets on a route. Exports carry a credential
reference and never grant material, so an imported plugin-backed provider needs a
new grant enrollment.
