# Domain docs

This repo uses a single-context layout:

- `CONTEXT.md` at the repo root: shared domain glossary.
- `docs/adr/`: architectural decision records.

## Before exploring

Read `CONTEXT.md` and any ADRs that touch the area you are about to work in.
If a root `CONTEXT-MAP.md` is introduced later, follow its pointers to the
relevant context glossaries and context-specific ADR directories, and read
applicable system-wide ADRs in `docs/adr/`.

If these files are absent, proceed silently. The `/domain-modeling` skill
creates them lazily when terms or decisions are resolved; it is also reached
through `/grill-with-docs` and `/improve-codebase-architecture`.

## Use the glossary's vocabulary

Use terms as defined in `CONTEXT.md` in issue titles, proposals, hypotheses,
and test names. Respect any synonyms the glossary explicitly excludes.

If a needed concept is missing, first check whether an existing term fits;
record a real vocabulary gap for `/domain-modeling`.

## Flag ADR conflicts

When a proposal or output contradicts an existing ADR, identify the ADR and
explain why reopening the decision is warranted before proceeding.
