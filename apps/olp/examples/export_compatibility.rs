//! Build tooling, not a usage example: emits the generated section of the
//! compatibility matrix to stdout. `make compat` splices it between the
//! markers in `docs/compatibility.md`;
//! `apps/olp/tests/integration/compatibility_drift.rs` fails CI when the
//! committed copy is stale.

fn main() {
    print!("{}", olp::gateway::compatibility::markdown());
}
