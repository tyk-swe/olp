# Use a new route identity when crossing the strict boundary

An older gateway can reject a new snapshot while retaining its last supported
legacy route. Publishing strict behavior under that same slug would therefore
advertise a contract some live readers do not serve. A new flag cannot fence
binaries that do not understand it.

A published slug keeps its strict or non-strict identity. Moving between those
classes requires a reviewed draft under a previously unpublished slug. Existing
legacy and transformed behavior can still change explicitly within the non-strict
class. The old route remains available during client migration; old readers
cannot dispatch the new slug. Compatible strict revisions and historical resource
bindings retain their existing authority, with current revocation enforced.

The routes authority records this boundary. Database checks also stop older
writers from omitting an existing explicit transformed or legacy contract in a
new revision or runtime release. Public
activation, configuration promotion and migration tooling report the boundary
before publication. Historical snapshots and their digests remain unchanged.
This adds an explicit client cutover instead of promising an atomic mixed-version
fleet upgrade. Older binaries still cannot restart against a newer schema; a
rollback uses a compatible binary and retained legacy route with current authority.
