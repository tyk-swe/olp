# Management tokens act within their creator's authority

A management token authorizes an operation only when the token's scopes include
it and its creator could perform it now. Every request resolves the creator
with the token: an inactive or OIDC-deauthorized creator makes the token fail
authentication, a changed role removes the operations that role no longer
holds, and an assigned access scope limits the token to the creator's current
projects and project roles. Stored tokens are never rewritten.

Only owners create tokens, so a token issued with the `access` scope carries
owner authority. Without this bound, an owner who was demoted, disabled, or lost
their identity-provider mapping kept that authority through their tokens and
could, for example, promote another member to owner. Evaluating the creator on
every request covers every way a member loses authority, including OIDC role
synchronization, provisioning and direct database changes, without a revocation
step at each of them.

Because nothing is revoked, a creator who regains authority also restores their
tokens. Revoke a token explicitly to retire it permanently, and issue automation
tokens from an owner account that outlives any one person's access.
