# Repository validation shell helpers

Repository invariant scripts source `repository-validation.sh` to preflight
their inputs and to run ripgrep without treating scan failures as valid
no-match results.

`checked_rg_capture OUTPUT MATCHED OPERATION PATH [RG_ARGS...]` stores ripgrep
output in `OUTPUT`. It stores `1` in `MATCHED` for ripgrep exit 0 and `0` for
exit 1. An exit greater than 1 is fatal, identifies the script, operation, and
path, and does not update either caller variable.

`checked_rg_match MATCHED OPERATION PATH [RG_ARGS...]` provides the same
fail-closed status handling when a caller needs only the match/no-match result.

Paths scanned by the migrated invariant scripts are required. There are
currently no optional scan paths. A future optional path must be documented at
its call site before it may be skipped; an unclassified missing, unreadable, or
wrong-type path is a preflight failure.
