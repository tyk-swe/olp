# Risk register

Reviewed at every Friday scoring. A risk that materialises gets an issue and
a backlog item; a risk that retires gets a one-line note here and stays for
the record.

| ID | Risk | Likelihood | Impact | Early-warning signal | Mitigation | Owner |
|---|---|---|---|---|---|---|
| R-01 | Single maintainer; any week slips when life happens | High | Medium | Two Fridays without scoring | Milestones 3–8 are independent of each other; drop a whole week rather than half of two; exit criteria make partial delivery visible | maintainer |
| R-02 | Upstream crate yank or advisory turns `main` red again | High | Medium | Monday scheduled run fails `Quality` | Keep `yanked = "deny"`; the fix is `cargo update -p <crate>`; HYG-06 surfaces it weekly instead of on the next unrelated push | maintainer |
| R-03 | Live-provider keys leak or overspend | Low | High | Provider spend alert; unexpected job triggers | Environment-scoped secrets with required reviewer; provider-side monthly caps; OIDC federation for AWS and GCP instead of static keys; cheapest models only | maintainer |
| R-04 | Provider API drift breaks translation silently | Medium | High | `provider-drift` issue opens; SDK smoke fails | TEST-01 weekly live job; conformance fixtures updated as contract changes, never replaced | maintainer |
| R-05 | OTLP dependency drags in `tonic`, inflating build time and the ban list | Medium | Low | `cargo tree` shows `tonic`; `make deny` bans fail | Use `opentelemetry-otlp` with `http-proto` + `reqwest-client` features; check the tree before the first commit | maintainer |
| R-06 | Budget semantics misunderstood (unpriced attempts accrue 0) | Medium | Medium | Support questions about "budget not enforced" | The semantics sentence appears in README, CHANGELOG, the key detail view, and the 429 body; `unpriced_attempts` is visible per key | maintainer |
| R-07 | Native arm64 runners unavailable or slow | Low | Low | `release.yml` arm64 job queues for > 30 min | Fall back to the existing QEMU build with its 90-minute timeout | maintainer |
| R-08 | GHCR anonymous pull limits hurt the quick start | Low | Medium | Users report pull failures | Package is public; document a Docker Hub mirror as a follow-up if it bites | maintainer |
| R-09 | Coverage floor of 80 becomes a tax on refactors | Medium | Low | PRs blocked on coverage with no behaviour change | Floor is a workspace line floor, not per-file; add tests with the refactor; never lower it | maintainer |
| R-10 | Tracing accidentally records content | Low | High | Allowlist test fails; e2e "secret prompt" assertion fails | Attribute allowlist enforced by a unit test; e2e collector assertion; review of every new `span.record` call | maintainer |
| R-11 | Migration 0049 and the N-1 rehearsal disagree | Low | High | `upgrade-rehearsal` job fails | Rehearsal runs before tagging; `release-metadata.env` moves only in the release commit | maintainer |
| R-12 | Screenshot baselines drift again after UI work | High | Low | Full-tier Firefox/WebKit job fails post-merge | Weekly cadence rule: all four baselines regenerate in the same PR; CI-03 decides whether pixel snapshots stay cross-browser | maintainer |
