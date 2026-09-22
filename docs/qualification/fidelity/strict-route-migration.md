# Reviewed strict route migration across mixed versions

A published route slug carries its strict or non-strict promise. An old gateway
can reject a release whose fidelity fields it cannot decode while continuing to
serve its cached legacy route. In-place legacy-to-strict activation would leave
that slug with two simultaneous meanings. [ADR 0004](../../adr/0004-migrate-strict-routes-to-new-identities.md)
therefore requires a previously unpublished slug when crossing the strict
boundary. Legacy and transformed modes can still change explicitly within the
non-strict class. Existing historical snapshots and digests remain untouched.

Forward migration 0027 adds the route's sealed strict identity, checks new
revisions and rejects old-shaped runtime publications that would omit a route's
active contract. Existing explicit transformed or legacy contracts cannot be
silently omitted by older revision writers either. The migration rejects an already mixed legacy/strict history
rather than silently declaring it safe. A failed migration rolls back its
schema and history; after the underlying published history is repaired from a
reviewed source, retry succeeds. These checks also protect against an older
writer that omits the new fidelity field.

The management API rejects a same-slug boundary crossing during draft validation
and activation with `route_fidelity_migration_required`. Configuration plan
reports that conflict, and apply does not stage it. `POST
/api/v3/routes/{route_id}/migration-draft` creates a review draft under a new
slug with the source route's ETag, idempotency, project authorization, current
revision, targets and policies. It does not activate or perform inference. A
copied redaction policy remains visible and blocks strict validation until a
user resolves the conflict. Retired slugs cannot be reused.

The Routes console has a **Create strict migration draft** action on non-strict
published routes. The form makes the new slug explicit and opens the copied
review draft; it does not publish it. The [reviewed packaged-console
screenshot](screenshots/strict-route-review-draft.png) shows the form and
its no-inference explanation.

## Qualification

The public integration test provisions a compatible provider and an old route,
rejects in-place strict publication, creates and replays a reviewed draft under
a new slug, validates and activates it, then checks both routes' provider calls.
Database checks reject old writer revision insertion, flag mutation and an
old-shaped runtime release. A second source with a redaction policy proves the
copy preserves it and strict activation fails with `fidelity_policy_conflict`.
The unsafe-history test proves migration failure rolls back and a repaired
history migrates without changing its legacy identity.

The mixed-version test builds the fixed pre-fidelity binary at
`8580b39905dc4da9278de8e53ceac7d2412ad6a5` and starts it on a migrated
historical installation. The current binary applies forward migration and
publishes a new strict slug. The old process **actually observes** a snapshot
digest mismatch, keeps serving only its old legacy slug, and never dispatches
the new slug. It refuses startup against the newer schema. Current API-key and
credential revocation remain enforced by that live old process, with provider
dispatch counts observed independently. The integration runner builds the fixed
old binary; explicitly selecting this test without its binary fails.

The existing provider-configuration browser journey runs against packaged and
Vite consoles on separate PostgreSQL databases. It creates a published legacy
route, opens the new migration form, checks the submitted body and strong ETag,
opens the resulting strict review draft, and verifies no provider request was
added by draft creation. Both modes passed in the focused run. Earlier attempts
caught and fixed an invalid HTML pattern; the passing capture has no browser
console error. The focused API/component tests also check the generated client
request and route-list action. No live-model calls or empirical quality claims
are part of this evidence.

Remaining whole-spec qualification includes the #214 continuation, #215
operations, #216 media/lifecycle, #217 broader console evidence/playgrounds and
#218 final gates. This migration proof alone does not establish G7.
