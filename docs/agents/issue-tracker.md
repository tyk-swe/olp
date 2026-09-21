# Issue tracker: GitHub

Issues and specs live in GitHub Issues for `tyk-swe/olp`. Use the `gh` CLI
from this clone; outside it, pass `--repo tyk-swe/olp`.

## Conventions

Write issue bodies and comments to a temporary Markdown file and pass
`--body-file <path>` to preserve newlines and literal text.

- **Create / publish**: `gh issue create --title "<title>" --body-file <path>`.
- **Read / fetch the relevant ticket**: `gh issue view <number> --comments`;
  use `--json number,title,body,labels,comments,assignees` for structured data.
- **List**: `gh issue list --state open --json number,title,body,labels,comments,assignees`.
  Apply the relevant label/state filters and set `--limit` to cover the queue.
- **Update a body**: `gh issue edit <number> --body-file <path>`.
- **Comment**: `gh issue comment <number> --body-file <path>`.
- **Apply / remove labels**: `gh issue edit <number> --add-label "<label>"`
  or `--remove-label "<label>"`. Use the mapping in `docs/agents/triage-labels.md`.
- **Close**: post any resolution comment, then `gh issue close <number>`.

## Pull requests as a triage surface

**PRs as a request surface: no.**

GitHub shares one number space for issues and PRs. Resolve an ambiguous
`#<number>` with `gh pr view <number>`, falling back to `gh issue view <number>`.

## Wayfinding operations

Used by `/wayfinder`. The map is a single issue; child issues are its tickets.

- **Map**: create an issue labelled `wayfinder:map` with a Notes /
  Decisions-so-far / Fog body.
- **Child ticket**: create an issue with `--parent <map-number>` and a
  `wayfinder:<type>` label (`research`, `prototype`, `grilling`, or `task`).
  If sub-issues are unavailable, link it in the map's task list and put
  `Part of #<map-number>` at the top of its body.
- **Blocking**: use native dependencies:
  `gh issue edit <child-number> --add-blocked-by <blocker-number>`.
  If dependencies are unavailable, put `Blocked by: #<number>, #<number>`
  at the top of the child body. A ticket is unblocked when every blocker is closed.
- **Frontier**: read the map's children in map order; select the first open,
  unassigned ticket with no open blockers. Use the map's task list when
  sub-issues are unavailable.
- **Claim**: `gh issue edit <number> --add-assignee @me`, the session's first write.
- **Resolve**: comment with the answer, close the ticket, then append a gist
  and link to the map's Decisions-so-far.
