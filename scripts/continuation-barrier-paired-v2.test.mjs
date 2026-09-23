import assert from 'node:assert/strict';
import { test } from 'node:test';
import { readFileSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import {
  analyze, deriveCriteria, schedule, validateInventory, validateRun, verifyCandidateOracle, verifyCriteria,
  blocksPerStratum, candidateHarness, criteriaPath, primaryMetrics, referencePaths, samples, strata
} from './continuation-barrier-paired-v2.mjs';

const criteria = verifyCriteria();
const duration = { workflow_us: 500, first_event_us: 100, wire_tool_us: 200, tool_visible_us: 200, action_ready_us: 300, claim_us: 10, journal_us: 10, ready_us: 10, events: 19, actions: 2 };
function makeRun(name, block, position) {
  const translated = name.endsWith('/translated');
  const reference = name.endsWith('/reference');
  const phaseNames = translated ? ['workflow', 'first-event', 'tool-visible', 'action-ready'] : reference ? ['workflow', 'first-event', 'wire-tool', 'action-ready', 'claim-commit', 'dispatch-journal', 'ready-commit'] : ['workflow', 'first-event', 'wire-tool', 'action-ready'];
  const phaseValues = { workflow: 500, 'first-event': 100, 'tool-visible': 200, 'wire-tool': 200, 'action-ready': 300, 'claim-commit': 10, 'dispatch-journal': 10, 'ready-commit': 10 };
  const metrics = { elapsed_ns: 24000000, 'ns/op': 1000000, 'process-cpu-ns/op': 1000000, 'B/op': 1, 'allocs/op': 1, 'sampled-heap-growth-B': 1 };
  for (const phase of phaseNames) for (const p of [50, 95, 99]) metrics[`${phase}-p${p}-us`] = phaseValues[phase];
  return {
    name, block, position, samples, dispatches: 48, first_requests: 24, next_requests: 24, native_events: 456,
    first_turn_observations: translated ? 312 : 0, final_observations: translated ? 48 : 0,
    actions: 48, ready_checks: translated || reference ? 24 : 0, rejected: 0,
    history_bytes: name.startsWith('large/') ? 262144 : 0,
    state_bytes: translated ? (name.startsWith('large/') ? 263500 : 2000) : (name.startsWith('large/') ? 263050 : 904),
    metrics, observations: Array.from({ length: 24 }, () => ({ ...duration })),
    gc_count: 0, gc_pause_ns: 0, goroutines_before: 12, goroutines_after: 12,
    scheduler_waits: 0, scheduler_p99_us: 0
  };
}
function fixture(mode = 'paired') {
  return schedule(criteria).flatMap(({ stratum, block, arms }) => arms.flatMap((arm, position) => mode === 'baseline' && arm === 'translated' ? [] : [makeRun(`${stratum}/${arm}`, block, position)]));
}
function mutate(run, change) { const copy = structuredClone(run); change(copy); return copy; }

test('criteria are exactly derived from untouched three-run B history and frozen limits', () => {
  assert.deepEqual(criteria, deriveCriteria());
  assert.deepEqual(criteria, JSON.parse(readFileSync(criteriaPath, 'utf8')));
  assert.equal(Object.keys(criteria.margins).length, 12);
  assert.equal(primaryMetrics.length, 11);
  for (const stratum of strata) for (const path of referencePaths) for (const [metric, margin] of Object.entries(criteria.margins[`${stratum}/${path}`])) {
    assert.equal(margin, criteria.limits[`${stratum}/${path}`][metric] - criteria.historical_medians[`${stratum}/${path}`][metric]);
    assert.ok(margin > 0);
  }
  assert.ok(candidateHarness.endsWith('_paired_v2_test.go'));
});

test('sealed schedule has 32 balanced blocks per stratum and adjacent B-reference/C in both orientations', () => {
  const blocks = schedule(criteria);
  assert.equal(blocks.length, 128);
  assert.deepEqual(blocks, schedule(criteria));
  for (const stratum of strata) {
    const group = blocks.filter((block) => block.stratum === stratum);
    assert.equal(group.length, blocksPerStratum);
    assert.equal(group.filter((block) => block.orientation === 'forward').length, 16);
    assert.equal(group.filter((block) => block.orientation === 'reverse').length, 16);
    assert.deepEqual(group.map((block) => block.block).sort((a, b) => a - b), Array.from({ length: 32 }, (_, i) => i));
    for (const block of group) {
      for (const arm of [...referencePaths, 'translated']) assert.equal(block.arms.filter((value) => value === arm).length, 2);
      assert.ok(block.arms.some((arm, i) => arm === 'reference' && (block.arms[i - 1] === 'translated' || block.arms[i + 1] === 'translated')));
      assert.deepEqual(block.arms, [...block.arms].reverse());
    }
  }
});

test('complete B-only and paired synthetic inventories produce exact counts and strict bound passes', () => {
  const baseline = fixture('baseline'), paired = fixture();
  assert.equal(baseline.length, 768);
  assert.equal(paired.length, 1024);
  assert.equal(validateInventory(baseline, 'baseline', criteria), true);
  assert.equal(validateInventory(paired, 'paired', criteria), true);
  const a = analyze(baseline, 'baseline', criteria), b = analyze(paired, 'paired', criteria);
  assert.equal(a.passed, true); assert.equal(b.passed, true);
  assert.equal(a.totals.reference_workflows, 18432);
  assert.equal(b.totals.reference_workflows, 18432);
  assert.equal(b.totals.candidate_workflows, 6144);
  assert.equal(b.totals.native_dispatches, 49152);
  assert.equal(b.totals.native_events, 466944);
  assert.equal(b.totals.fixture_actions, 49152);
  assert.equal(b.totals.reference_ready_reads, 6144);
  assert.equal(b.totals.candidate_ready_reads, 6144);
  assert.equal(b.totals.candidate_first_turn_observations, 79872);
  assert.equal(b.totals.candidate_final_observations, 12288);
  assert.equal(Object.values(b.primary).reduce((n, row) => n + Object.keys(row).length, 0), 44);
  for (const row of Object.values(b.primary)) for (const metric of Object.values(row)) {
    assert.equal(metric.block_differences.length, 32);
    assert.equal(metric.upper_median_bound, 0);
  }
});

test('no missing, duplicate, reordered, incomplete or semantically changed workflow can qualify', () => {
  const base = fixture();
  const first = base.find((run) => !run.name.endsWith('/translated'));
  for (const [index, change] of [
    (run) => run.samples--, (run) => run.dispatches--, (run) => run.first_requests--,
    (run) => run.native_events--, (run) => run.actions--, (run) => run.rejected++,
    (run) => run.history_bytes++, (run) => run.state_bytes++,
    (run) => run.observations.pop(), (run) => run.observations[0].events--,
    (run) => run.observations[0].actions--, (run) => run.metrics['workflow-p99-us']++,
    (run) => run.metrics['action-ready-p95-us'] = 1,
    (run) => delete run.metrics['process-cpu-ns/op'],
    (run) => delete run.scheduler_waits
  ].entries()) {
    const changed = mutate(first, change);
    assert.throws(() => validateRun(changed, criteria), `mutation ${index}`);
  }
  for (const change of [
    (runs) => runs.pop(), (runs) => runs[1] = structuredClone(runs[0]),
    (runs) => [runs[0], runs[1]] = [runs[1], runs[0]],
    (runs) => runs[0].name = 'small/c1/translated',
    (runs) => runs[0].block++, (runs) => runs[0].position++,
    (runs) => runs.find((run) => run.name.endsWith('/translated')).state_bytes++
  ]) {
    const runs = structuredClone(base); change(runs);
    assert.throws(() => validateInventory(runs, 'paired', criteria));
  }
  const translated = base.find((run) => run.name.endsWith('/translated'));
  for (const change of [
    (run) => run.first_turn_observations--, (run) => run.final_observations--,
    (run) => run.ready_checks--, (run) => run.state_bytes = 0,
    (run) => run.observations[0].tool_visible_us = 350,
    (run) => run.metrics['tool-visible-p99-us'] = 199
  ]) assert.throws(() => validateRun(mutate(translated, change), criteria));
});

test('candidate bound touching margin and B absolute control bound touching margin both fail', () => {
  const runs = fixture();
  const stratum = 'small/c1', name = `${stratum}/reference`, metric = 'B/op';
  const synthetic = structuredClone(criteria);
  synthetic.margins[name][metric] = 1;
  for (const run of runs) if (run.name === `${stratum}/translated`) run.metrics[metric] = 2;
  const candidate = analyze(runs, 'paired', synthetic);
  assert.equal(candidate.primary[stratum][metric].upper_median_bound, 1);
  assert.equal(candidate.primary[stratum][metric].passed, false);
  assert.ok(candidate.failures.some((failure) => failure.startsWith('C paired small/c1/translated/B/op')));
  const controls = fixture();
  // Both orientations have their second reference position after the first.
  for (const block of schedule(synthetic).filter((value) => value.stratum === stratum && value.block < 22)) {
    const last = controls.find((run) => run.name === name && run.block === block.block && run.position === block.arms.lastIndexOf('reference'));
    last.metrics[metric] = 2;
  }
  const control = analyze(controls, 'paired', synthetic);
  assert.equal(control.controls[name][metric].upper_median_bound, 1);
  assert.equal(control.controls[name][metric].passed, false);
  assert.equal(control.envelope[name][metric].passed, true);
});

test('B envelope failure cannot be rescued by a favorable candidate', () => {
  const runs = fixture();
  const name = 'small/c1/reference', metric = 'B/op', limit = criteria.limits[name][metric];
  for (const run of runs) if (run.name === name) run.metrics[metric] = limit + 1;
  const result = analyze(runs, 'paired', criteria);
  assert.equal(result.envelope[name][metric].passed, false);
  assert.equal(result.passed, false);
});

test('order statistic uses the 22nd sorted block and does not select favorable blocks', () => {
  const runs = fixture(), synthetic = structuredClone(criteria);
  const stratum = 'small/c1', name = `${stratum}/reference`, metric = 'B/op';
  synthetic.margins[name][metric] = 1;
  for (const run of runs) if (run.name === `${stratum}/translated` && run.block >= 21) run.metrics[metric] = 2;
  const result = analyze(runs, 'paired', synthetic);
  assert.equal(result.primary[stratum][metric].block_differences.filter((value) => value === 1).length, 11);
  assert.equal(result.primary[stratum][metric].upper_median_bound, 1);
  assert.equal(result.passed, false);
});

test('criteria mutation cannot reset a frozen margin or erase a comparison', () => {
  const changed = structuredClone(criteria);
  changed.margins['small/c1/reference']['workflow-p99-us']++;
  assert.throws(() => verifyCriteria(changed));
  const erased = structuredClone(criteria);
  delete erased.margins['small/c1/reference']['workflow-p99-us'];
  assert.throws(() => verifyCriteria(erased));
});

test('post-baseline candidate harness or SDK-equivalent oracle edits are refused', () => {
  const method = execFileSync('git', ['rev-parse', 'HEAD'], { encoding: 'utf8' }).trim();
  const hash = (path) => createHash('sha256').update(readFileSync(path)).digest('hex');
  const harness = hash(candidateHarness), workflow = hash('tests/integration/continuation_candidate_benchmark_test.go');
  assert.equal(verifyCandidateOracle(method), true);
  assert.throws(() => verifyCandidateOracle(method, { harness: 'changed', workflow }));
  assert.throws(() => verifyCandidateOracle(method, { harness, workflow: 'changed' }));
});
