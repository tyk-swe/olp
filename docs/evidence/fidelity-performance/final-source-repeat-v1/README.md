# Final-source repeat lock

After the P10 product fix is integrated and before any new timed C capture,
create one write-once `product-lock.json` using
`node scripts/fidelity-final-source-lock.mjs freeze <exact-P10-commit>` and
commit it. The lifecycle-v3 and paired-barrier attempt-3 C methods require
that lock commit as a strict ancestor, the exact P10 product revision as a
strict ancestor, and byte-identical production Go blobs at the measured C
source. The full tracked diff from P10 to C may contain only
`docs/evidence/fidelity-performance/` files. This also freezes `go.mod`,
`go.sum`, every migration SQL and Lua script, `openapi/management.json`,
all Go test harnesses and embedded fixtures; none are hidden behind a
production-Go-only fingerprint. Tree comparisons disable Git rename detection
and inspect both sides of an identical-file move across the evidence boundary.
An unmerged, unmeasured draft at `68ca0708` was abandoned because its default
rename-aware diff could hide a removed build input. The earlier unmerged
`25a75179` draft was abandoned for a journal attempt-ID mismatch. Make a clean descendant source commit after the lock commit before
reserving either C artifact. The old bc325 passing C artifacts remain unchanged. This lock only
binds source identity; it cannot change either frozen numeric budget.
