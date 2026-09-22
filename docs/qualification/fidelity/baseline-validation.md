# Initial baseline execution record

Date: 2026-09-22. Production source baseline:
`8580b39905dc4da9278de8e53ceac7d2412ad6a5`. The implementation branch adds
references, an independent test oracle and tests; it does not modify the
production request path. Toolchain: Go 1.27.1 linux/amd64, Node 26.8.2,
pnpm 11.24.0, Docker daemon 29.8.1. All upstream traffic was local synthetic
fixture traffic. No paid live inference or production changes occurred.

## Executed checks

| Check | Actual result |
| --- | --- |
| Baseline `go test -mod=readonly -timeout=5m ./internal/protocols/... ./internal/gateway` | Passed; protocol packages cached, gateway 46.688s |
| `go test -mod=readonly -v ./tests/fidelity` | Passed seven native/translation characterization cases and oracle checks; reproduced all five recorded legacy losses |
| `go test -mod=readonly -race -count=1 ./tests/fidelity` | Passed, including precision/Unicode, state, order, terminal and ID inheritance/reset mutation checks |
| `go test -mod=readonly -run '^$' -fuzz '^FuzzReferenceRetainsOpaqueState$' -fuzztime=5s ./tests/fidelity` | Passed; 78,192 executions, six initial seeds, 88 new interesting inputs, 6.050s elapsed |
| `go vet ./tests/fidelity ./tests/fixtures/fidelity` | Passed |
| `go test -mod=readonly -race -tags=integration -run '^TestFidelityPublicNativeAndTranslatedConservationControls$' -count=1 -timeout=3m -v ./tests/integration` | Passed both OpenAI and Gemini subtests against disposable PostgreSQL 18; 3.998s |
| `gofmt` and `git diff --check` | Passed |

The integration test provisions through management APIs and performs three
inference checks: one native positive, one translated positive and one
translated incompatibility. Captured effective requests match independent
expectations; the incompatible case causes zero additional provider dispatch.
Setup discovery/certification calls are excluded from those inference counts.
The disposable database container was stopped and removed after the run.

The first integration attempt revealed that the fixture needed a Responses
certification endpoint in addition to Chat, because the existing OpenAI
capability proof checks both. Adding the independently scripted Responses
reply fixed the fixture; no production validation was bypassed.

## Scope and unexecuted qualification

Seven native positives passing does not mean the five legacy translated losses
are qualified. Those five cases explicitly remain characterized losses until
strict preserve-or-reject admission is implemented and tested. The existing
legacy route behavior is not silently changed by this slice.

The frozen representative denominator contains 47 rows. This slice does not
execute all 47 rows or claim to qualify their profiles. The Anthropic streamed
thinking/tool response and next-request assets are independently authored
fixtures with tested corruption detection, not completed SDK-next-turn or
restart evidence. This slice did not run live quality trials, complete SDK
continuation journeys, process recovery, full service qualification or browser
journeys. Empirical quality remains unknown for every row.

Performance measurements and predeclared replacement budgets have their own
artifact and execution record; the oracle runs above establish no latency,
throughput or resource-boundedness result.
