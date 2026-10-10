# Terraform and OpenTofu

The [OpenLLMProxy provider](https://github.com/tyk-swe/terraform-provider-openllmproxy)
lives in a separate repository. Its M11 implementation is in progress. Projects,
providers, credential slots, routes, routing policies, guardrails, export sinks, keys, budget groups,
notification destinations and notification rules currently have
live create, update, import and destroy qualification with Terraform 1.16.5 and
OpenTofu 1.13.1. The remaining M11 resources are being implemented before the
milestone is marked complete.

Connect with `OLP_MANAGEMENT_URL` and `OLP_MANAGEMENT_TOKEN_FILE`. The provider
uses the generated management contract and reads the private token file for
every operation. A token's creator role, project boundary and current scopes
remain authoritative. Project resources require an installation-wide
`manage_projects` token whose creator is an owner. Budget and notification
resources use their ordinary `keys` or installation `settings` requirements;
the provider does not require configuration-promotion permissions.

Each resource manages its own UUID, JSON `definition` and observed ETag.
Creation-only field changes require replacement; updates use the API's declared
mutable fields. Removing a nullable budget ceiling explicitly clears it.
Equivalent fixed-scale monetary values keep the configured representation,
avoiding repeated plans. Import discovers nonsecret creation fields from the
current server response. An external edit after a reviewed plan's observed
ETag refuses apply and requires a fresh review.

Notification signing values use a separate write-only `secret` attribute,
an ephemeral input and a positive `secret_version` change counter. Values are
excluded from saved plans and state. The acceptance suite checks that boundary
with both engines. The provider rejects credential values in the state-bearing
`definition` field.

Destroy is conditional. Dependent resources and retained accounting/delivery
history return a conflict. Delete notification rules before their destination
or budget group, and detach dependent resources before deleting a project.
Successful conditional deletion retains its audit and idempotent replay;
deleting a signing destination removes its stored secret. See
[access administration](access.md) for these API boundaries.

The provider repository documents source builds, development overrides,
required disposable-service environment and its CI acceptance suite. Its OLP
binary and SDK come from the same immutable dependency version. For
installation-wide artifact promotion through GitHub Actions, use the separate
[configuration-promotion workflow](configuration-actions.md).

API keys use `openllmproxy_key` and revoke on destroy, preserving retained
accounting. Their mutable ceiling fields are projected from budget windows,
separately from accrued spend. An optional absolute `secret_file` atomically
captures the one-time key into an owner-only file; state stores its path, not
its value. Changing that path replaces the key. Omit the option when importing
existing metadata, since an existing key's secret cannot be recovered. The
operator removes the local file after revocation. An output failure preserves
the created UUID in state so the authority can be reconciled or destroyed.

Providers manage draft configuration and validate write-only credential rotations
before staging a new version. Normalized configuration defaults are projected to
operator-selected fields. Credential slots use their own ETags and import with
`PROVIDER_UUID/SLOT_UUID`; creation uses the observed pool ETag. Deleting a draft
slot preserves published bindings and immutable versions. Parent deletion follows
only atomic transitions from the same client's slot writes; foreign edits still
refuse a saved destruction plan. See the provider README for the resource forms
and partial-validation recovery behavior.

`openllmproxy_route` publishes validated immutable revisions atomically, preserving
independent console drafts and existing routing policy. Target refresh preserves
chosen facts; UUID import uses canonical provider-model references. Destroy retires
the route and retains history and slug ownership. A project boundary change must
use a new slug; saved plans refuse external publications committed afterward.

Routing-policy resources use `scope` and `scope_id`: installation (nil UUID),
API key, or route draft. Import uses `SCOPE/UUID`. Their definitions replace the
complete policy; removing a constraint returns it to the default. Destroy removes
the explicit override while retaining a fresh ETag. The scoped API's settings,
key or configuration authority remains required. Draft policy changes become
live upon activation. Key/policy graphs coordinate only their own proven parent
writes; a foreign parent or policy edit still invalidates a saved plan.

`openllmproxy_guardrail` manages project-scoped `builtin.regex` definitions,
conditional immutable policy revisions, UUID import and retirement. Compose a
route's `content_policy` from
`jsondecode(openllmproxy_guardrail.filter.definition).policy` to review and
publish a selected copy. Existing serving revisions stay pinned; foreign saved
plans are refused before dependent publication. Retained guardrail history can
prevent project deletion. See [Named guardrails](guardrails.md).

`openllmproxy_sink` manages scoped HTTPS JSON exports of content-free durable
facts. Its UUID import reconstructs nonsecret definitions, and its sensitive
write-only signing input uses a change counter and ephemeral variables. Neither
saved plans nor state contain signing values. Actual installation workers deliver
stable event IDs with exact-byte signatures and bounded retries; delivery counters
do not change resource ETags. Destroy retires the destination and removes signing
material while retaining identity/history. A saved plan refuses external edits.
Project-owned history can prevent project deletion. See [Export sinks](export-sinks.md).
