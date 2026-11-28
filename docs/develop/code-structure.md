# Code structure

## Structure rules

`make lint` enforces the Go layout through `.golangci.yml` and
`scripts/check-lint.sh`. Read the failure message and fix the code. Frontend
boundaries are enforced separately by ESLint and documented in
[`console/README.md`](../../console/README.md#boundaries).

| Rule | Scope | Constraint |
| --- | --- | --- |
| `serving` | `internal/gateway/**` (non-test) | Denies the root `internal/access$` management kit package so the gateway never takes management transactions or server state. `gateway/delegation` and `gateway/surface/guardrails` still import `internal/access/httpkit`, and `gateway/surface/playground` imports both `internal/access/httpkit` and `internal/access/authz`. |
| `management` | `internal/access/**` (non-test) | Denies all imports of `internal/gateway`. Only `cmd/olp` wires serving and management planes together. |
| `kernel` | `internal/gateway/kernel/**` (non-test) | Denies `github.com/jackc/pgx`, `internal/access/httpkit` and `internal/access/authz`. Persistence stays in owning store packages, faults convert in `cmd/olp`, and member authorization passes through a callback on `kernel.Member`. |
| `gateway_isolation` | `internal/**` outside `internal/gateway/**` (non-test) | Denies all imports of `internal/gateway`. Features receive narrow collaborators from `cmd/olp` instead of importing gateway packages directly. |
| `resources_isolation` | `internal/resources/**` outside `internal/resources/manage/**` (non-test) | Denies `internal/access` and `internal/management/contract` so resource persistence and background workers stay free of management HTTP handlers. |
| `media_isolation` | `internal/media/**` outside `internal/media/jobs/manage/**` (non-test) | Denies `internal/access` and `internal/management/contract` so media codecs, transport, persistence and workers stay free of management HTTP handlers. |
| `store_worker_validation` | Files named `*store.go`, `*stores.go`, `*worker.go`, `*workers.go`, `*validation.go` and `*validate.go` (non-test) | Denies `internal/access/httpkit` so stores, workers and validators return [problem values](management.md#problem-values) and leave the reply to the HTTP handler. |
| `domain` | `internal/domain/**` (non-test) | Allows only `internal/domain` inside `internal/`, and denies `net/http`, `database/sql`, `github.com/jackc/pgx` and `github.com/valkey-io`. Take values in as arguments and do I/O in the caller. |
| `platform` | `internal/platform/**` (non-test) | Allows only `internal/platform` and `internal/domain` inside `internal/` so shared infrastructure never imports a feature. |
| `library` | `internal/**` | Denies all imports of `cmd/`. |

Beyond those `depguard` rules, `scripts/check-lint.sh` verifies that top-level
modules under `internal/` never form an import cycle. When two modules need each
other, move the shared vocabulary into `internal/domain` or inject the
collaborator from `cmd/olp`. Every other directory under `internal/` is named
for the module that owns a behavior, including its SQL, validation and routes,
while `internal/access` remains the management kit with no feature logic inside
it. `funlen` caps functions at 100 lines and `revive` caps files at 500 lines of
code outside tests and generated `.gen.go` files. Split code by behavior rather
than by line count.

### Table ownership

Every table in the `olp` schema has one owning module, a directory directly
under `internal/` whose `tables.txt` lists the table by name, one per line. The
change that creates a table adds it to one of these files. Any module may read
any table, and only the owner writes it. A module that needs a row of another
module's table changed calls a function the owner exports that takes the
caller's `pgx.Tx`, and `cmd/olp` injects that function when a direct import
would form a cycle.

`make check-go` runs `scripts/check-tables.sh`, which reads the tables from the
migrations. It fails when a table has no owner or two, when an entry names a
table the migrations do not create, and when non-test Go code outside the owner
writes a table with `INSERT`, `UPDATE`, `DELETE`, `MERGE`, `TRUNCATE` or `COPY`,
or names it in a `pgx.Identifier`.

`scripts/check-tables.baseline` holds the cross-module writes that predate the
check. Each line is a module, a table and the number of writes, and those writes
are the only ones the check lets through. The numbers only go down. A new write
fails the check, and so does a line that allows more than the code writes, so the
change that moves SQL into the owner also lowers or deletes its line.

The check searches Go source text for table names written with the `olp.`
schema. It cannot see a name assembled at run time or writes made by database
functions, triggers and views, so review those by hand.
