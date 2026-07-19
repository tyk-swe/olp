# Enterprise contracts

This directory is the versioned decision record for the enterprise control
plane. Milestone M0 freezes the contracts that later milestones implement; it
does not make the installation tenant-aware by itself.

## Contract set

| Decision or gate | Source of truth | Linear evidence |
| --- | --- | --- |
| Resource hierarchy and ownership | [`adr/0001-enterprise-resource-hierarchy.md`](adr/0001-enterprise-resource-hierarchy.md), [`contracts/resource-ownership.json`](contracts/resource-ownership.json) | XOD-83 |
| Request context and runtime authority | [`adr/0002-request-context-and-runtime-authority.md`](adr/0002-request-context-and-runtime-authority.md) | XOD-84 |
| Connector and policy trust boundaries | [`adr/0003-extension-trust-boundaries.md`](adr/0003-extension-trust-boundaries.md), [`contracts/connector-v1.json`](contracts/connector-v1.json), [`contracts/policy-v1.json`](contracts/policy-v1.json), [`policy program schema`](contracts/policy/policy-program-v1.schema.json), [`policy golden vectors`](contracts/policy/policy-v1-golden.json), [`Connector v1 protobuf`](../../proto/olp/connector/v1/connector.proto) | XOD-85 |
| Migration and public compatibility | [`adr/0004-compatibility-and-migrations.md`](adr/0004-compatibility-and-migrations.md), [`contracts/compatibility.json`](contracts/compatibility.json) | XOD-86 |
| Threats and residual-risk dispositions | [`threat-model.md`](threat-model.md), [`contracts/threat-register.json`](contracts/threat-register.json) | XOD-87 |
| Capacity, propagation, SLO, and recovery targets | [`contracts/capacity-envelope.json`](contracts/capacity-envelope.json) | XOD-87 |
| Enterprise-beta gates | [`contracts/enterprise-beta-scorecard.json`](contracts/enterprise-beta-scorecard.json) | XOD-87 |
| Approval and change control | [`approvals.md`](approvals.md) | XOD-83–XOD-87 |

The human-readable ADRs explain why a decision was made. Machine-readable
contracts provide stable identifiers and CI-testable invariants. CI validates
the enumerated structural, repository, compatibility, and cross-reference
rules in `scripts/check-enterprise-contracts.sh`; it cannot infer that arbitrary
prose has the same meaning as JSON. A semantic disagreement blocks review and
must be resolved in the same change.

## Status vocabulary

- **Accepted target** means the design text is the selected implementation
  target. It is not evidence that accountable owners approved the exact bytes.
- **Approved** means every authority and evidence requirement in
  [`approvals.md`](approvals.md) has a reviewable record.
- **Implemented** means the relevant later-milestone code and migrations have
  landed and their conformance evidence passes.
- **Qualified** means the beta scorecard contains the required measured,
  rehearsed, externally reviewed, or explicitly accepted evidence.

These states are independent. An accepted target can remain approval-pending,
unimplemented, and unqualified. The scorecard records approval and
qualification evidence separately so documentation cannot imply that the
current installation-global runtime already provides enterprise isolation.

## Change control

1. Keep identifiers stable. Rename display text, not resource, threat, profile,
   or gate IDs.
2. Change an accepted target only through a superseding ADR linked to the
   affected Linear issue and downstream milestones.
3. Classify compatibility before implementation. Breaking public changes
   require the major-version and deprecation rules in ADR 0004.
4. Update the machine-readable contract, documentation, tests, and scorecard
   evidence in the same change.
5. Record approval only after protected repository review and completion of
   the linked Linear decision issue. Until both exist, keep the scorecard state
   `approval_pending`; do not substitute unauditable names or signatures.

Run `scripts/check-enterprise-contracts.sh` before review. The required CI tier
runs the same validation.
