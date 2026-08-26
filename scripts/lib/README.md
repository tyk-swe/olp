# Repository validation shell helpers

Invariant scripts source `repository-validation.sh` for input preflight and
fail-closed ripgrep handling.

`checked_rg_capture OUTPUT MATCHED OPERATION PATH [RG_ARGS...]` stores output
and sets `MATCHED` to `1` for a match or `0` for no match. A ripgrep exit code
greater than 1 is fatal and leaves caller variables unchanged.

`checked_rg_match MATCHED OPERATION PATH [RG_ARGS...]` provides the same
match/no-match status when output is unnecessary. Every scanned path is
required; future optional paths must be explicitly classified at their call
site before they may be skipped.

Self-tests that report TAP source `tap.sh` instead of restating it.
`run_test NAME COMMAND [ARGS...]` numbers each case and prints its `ok` or
`not ok` line; `tap_plan` closes the stream with `1..N`.
