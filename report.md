# Explicit Code Mode Batching Evaluation

Date: 2026-07-24  
Project revision: `0b253ca12b9324995a7fef41ff06adc7a54da7b5`  
Codex CLI: `0.145.0`  
Model: `gpt-5.6-sol`

## Conclusion

The explicit batching instruction is useful at **high** and **max** reasoning
effort for reducing outer model cycles and input-context processing. It was not
useful at **medium** effort in this evaluation, and it was not a reliable
latency optimization.

- **High:** strongest result. Batching reduced outer cycles by 52%, total input
  by 57%, uncached input by 17%, and elapsed time by 5%.
- **Max:** batching reduced outer cycles by 42% and uncached input by 25%, but
  nearly doubled output tokens and took 19% longer.
- **Medium:** batching reduced outer cycles by only 11%, increased uncached
  input by 3%, increased output by 75%, and took 66% longer.

Across the three matched pairs, batching reduced outer cycles by 39%, total
input by 32%, and uncached input by 16%. It also increased output tokens by 65%,
shell inspections by 254%, and elapsed time by 21%.

The practical recommendation is to use the instruction for independent,
read-heavy repository investigation at high effort. At max effort, use it when
context efficiency matters more than latency. Do not use this exact instruction
as a blanket optimization at medium effort.

## Experimental setup

Each observation was a fresh `codex exec` invocation. The control and treatment
used:

- the same Git revision and clean worktree;
- the same read-only repository-analysis task;
- `gpt-5.6-sol`;
- the same reasoning effort within each pair;
- Code Mode and the Code Mode host enabled;
- `danger-full-access` for both variants, because nested sandboxing was
  incompatible with this environment;
- ignored user configuration while retaining authentication;
- no builds, tests, apps, MCP servers, connectors, web tools, plugins, skills,
  or subagents.

The task traced an inbound OpenAI-compatible streaming request through exactly
five areas—authentication, routing/runtime selection, provider normalization,
usage/cost persistence, and enforcing tests/documentation—and required exactly
three risks with file and symbol evidence.

The control had no prompt prefix. The treatment used the explicit Code Mode
batching prefix stored at:

`/home/ubuntu/.codex/skills/codex-project-eval/references/code-mode-batching-prefix.txt`

There was one control and one treatment observation at each effort level, for
six valid runs total. Execution order was alternated:

- medium: control, batching;
- high: batching, control;
- max: control, batching.

## Results

| Effort | Variant | Outer cycles | Commands | Total input | Cached input | Uncached input | Output | Reasoning output | Seconds |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|
| Medium | Control | 18 | 17 | 1,486,171 | 1,324,800 | 161,371 | 6,056 | 1,256 | 144.48 |
| Medium | Batching | 16 | 80 | 1,396,481 | 1,230,080 | 166,401 | 10,613 | 1,477 | 240.42 |
| High | Control | 29 | 24 | 3,016,651 | 2,803,712 | 212,939 | 9,269 | 3,196 | 223.58 |
| High | Batching | 14 | 100 | 1,289,373 | 1,112,832 | 176,541 | 10,499 | 2,579 | 212.78 |
| Max | Control | 43 | 41 | 3,860,325 | 3,560,192 | 300,133 | 12,590 | 4,291 | 417.50 |
| Max | Batching | 25 | 110 | 3,036,218 | 2,811,904 | 224,314 | 24,844 | 10,622 | 496.96 |

### Treatment changes

| Effort | Outer cycles | Commands | Total input | Cached input | Uncached input | Output | Reasoning output | Seconds |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| Medium | -11.1% | +370.6% | -6.0% | -7.1% | +3.1% | +75.2% | +17.6% | +66.4% |
| High | -51.7% | +316.7% | -57.3% | -60.3% | -17.1% | +13.3% | -19.3% | -4.8% |
| Max | -41.9% | +168.3% | -21.3% | -21.0% | -25.3% | +97.3% | +147.5% | +19.0% |
| All three pairs | -38.9% | +253.7% | -31.6% | -33.0% | -15.9% | +64.6% | +67.9% | +21.0% |

The high-effort result also reproduced an earlier matched pair on the same
project and task. That pair reduced outer cycles by 44.8%, total input by 49.5%,
and uncached input by 18.3%, with a 2.7% latency reduction.

## Quality review

All six reports:

- covered all five required areas;
- identified exactly three risks;
- stayed below the 1,200-word limit, at 505–723 words;
- cited 25–35 unique project paths;
- reached the same core architectural conclusions;
- distinguished confirmed findings from inference;
- made no file changes and ran no builds or tests.

The treatment did not show a systematic final-answer quality loss. However, it
performed 80–110 shell inspections versus 17–41 for the controls without
producing broader cited evidence. Much of the additional work was therefore
likely redundant.

## Interpretation

The instruction works through the claimed mechanism: independent shell
inspections move inside fewer `functions.exec` stages, reducing the number of
outer model/context cycles. It can simultaneously increase the amount of
low-level inspection, tool-call output, reasoning output, and wall-clock time.

This means “beneficial” depends on the objective:

- It was beneficial for outer-cycle and context efficiency at high and max.
- It was beneficial for latency only at high, and only modestly.
- It was harmful on most measured dimensions at medium.
- It did not establish subscription-credit savings because the service's
  weighting of cached input, uncached input, and output is not public.

A promising follow-up treatment would retain concurrent inspection while adding
a scope constraint such as:

> Do not broaden scope or repeat equivalent searches. Use bounded fan-out and
> stop once the evidence contract is satisfied.

This should be evaluated rather than assumed, because the current experiment
did not test it.

## Artifacts

- Medium summary: `/tmp/olp-batching-efforts.DVJUaF/medium-runs/summary.md`
- High summary: `/tmp/olp-batching-efforts.DVJUaF/high-runs/summary.md`
- Max summary: `/tmp/olp-batching-efforts.DVJUaF/max-runs/summary.md`
- Machine-readable summaries are in the corresponding `summary.json` files.
- Each run directory contains its final report, JSONL events, stderr, and parsed
  result.

No observations were excluded. All six exited successfully, stayed within the
timeout, avoided prohibited external tools, and left the Git worktree
unchanged.
