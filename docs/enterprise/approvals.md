# M0 approval requirements and evidence

This file defines how M0 approval is earned; it does not claim that approval has
already happened. The selected ADRs are accepted targets, while the machine
scorecard remains `approval_pending` until every required record exists.

Each M0 decision needs both of these auditable controls:

1. completion of its corresponding Linear decision issue after its acceptance
   criteria and owner review are recorded; and
2. protected repository review of the exact contract bytes that are merged.

The scorecard stores those records per decision. A Linear issue moving to a
completed state proves only that issue's recorded decision; it does not stand in
for repository review or for another authority. This file deliberately does not
imitate handwritten or cryptographic signatures.

| Authority | Accountable evidence | Scope |
| --- | --- | --- |
| Architecture/domain | XOD-83 through XOD-87 as named by each decision, plus repository review | hierarchy, ownership, request/runtime authority, extension boundaries, compatibility, repository architecture |
| Gateway/security | XOD-83, XOD-84, XOD-85, and XOD-87 completion plus repository review | ownership security, authentication, isolation, extensions, threat model, accepted risks |
| Product/console | XOD-83 and XOD-86 completion plus repository review | hierarchy workflows and public compatibility |
| Storage/API | XOD-83, XOD-85, XOD-86, and XOD-87 completion plus repository review | scoped integrity, policy/connector persistence boundaries, schema evolution, management contracts, capacity |
| Provider/platform | XOD-85 and XOD-87 completion plus repository review | connector identity, isolation, lifecycle, reference-connector contract, connector capacity, and provider-related accepted risks |
| Policy/economics | XOD-85 and XOD-87 completion plus repository review | deterministic policy phases, composition, economic-control semantics, and policy-related accepted risks |
| Reliability | XOD-87 completion plus repository review | capacity, propagation, durability, RPO, and RTO targets |
| Operations | XOD-87 completion plus repository review | operational recovery, audit-integrity, support-access, and residual-risk controls |
| Product/release | XOD-87 completion plus repository review | capacity claims, residual-risk disposition, beta gates, and release evidence |

The project lead may hold more than one authority in the current team. Evidence
remains role-specific so a later ownership split does not require an ADR
rewrite. A completed Linear issue records its actor and time; protected review
records reviewer identity and the exact commit. Neither is inferred from an
author name in Markdown.

## Evidence model

`enterprise-beta-scorecard.json` records each decision as either
`approval_pending` or `approved`.

- `approval_pending` has no approval evidence and makes no approval claim.
- `approved` requires one `linear_completion` record for the matching XOD issue
  and one `repository_review` record for the reviewed commit.
- Each record names its authority, accountable identity, immutable locator, and
  timestamp. Repository review also binds a full commit SHA.
- The aggregate M0 gate may be `approved` only when every decision is approved.

CI validates this structure and rejects an approval state with missing,
mismatched, or placeholder evidence. CI does not query Linear or GitHub; human
review remains responsible for confirming that external locators are genuine.

## Approval rules

- `Accepted` ADRs cannot be implemented as optional guidance.
- A material contract change requires a superseding ADR and renewed approval
  from every affected authority.
- Capacity or security evidence may be delegated, but the accountable owner
  still accepts residual risk.
- A scorecard gate cannot become `passed` from a plan, target, or unreviewed
  local result. Every requirement needs an immutable passing evidence record;
  a test definition or capacity profile is not its own result.
- M0 completion approves the contracts and qualification plan. It does not
  pre-approve the M8 enterprise-beta go/no-go decision.
