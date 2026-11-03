# Performance

The benchmark scenarios, the comparison harness, and the hot-path
microbenchmark process were subtracted with the rest of the test system.
Pre-merge validation no longer measures gateway throughput, latency, or
allocations. The scenario definitions, the reference numbers, and the
comparison record remain in git history before commit `99234cb0`.

This page stays as the pointer for that history. Treat every number in the
historical record as a measurement of the revision that produced it, not as
a property of the current code. New performance claims need new
measurements taken on the revision under discussion, with the machine, the
command, and the raw output recorded alongside the claim.

## Performance budget

There is no enforced performance budget. The former budget, which required
hot-path benchmarks to hold time and allocations within a fixed tolerance
and required unconfigured features to add no allocations or storage round
trips, is recorded in git history before commit `99234cb0`. The underlying
design intent still applies as review guidance. A change that adds work to
the request path should say what it adds and why, and reviewers may ask for
a local measurement when the cost is in doubt. Nothing checks the answer
automatically.
