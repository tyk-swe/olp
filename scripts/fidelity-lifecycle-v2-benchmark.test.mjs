import assert from 'node:assert/strict';
import { test } from 'node:test';
import { readFileSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { names } from './fidelity-lifecycle-benchmark.mjs';
import { parseRuns, validateV2Runs, freeze, compare } from './fidelity-lifecycle-v2-benchmark.mjs';
const sha256 = (path) => createHash('sha256').update(readFileSync(path)).digest('hex');

function runs() {
  return names.flatMap((name) => [0, 1, 2].map((repetition) => {
    const metrics = Object.fromEntries(['elapsed_ns', 'ns/op', 'process-cpu-ns/op', 'B/op', 'allocs/op', 'sampled-heap-growth-B'].map((metric) => [metric, 100]));
    for (const category of ['latency', ...(name.startsWith('durable') ? ['publication'] : ['cancellation', 'event-roundtrip'])]) {
      for (const p of [50, 95, 99]) metrics[`${category}-p${p}-us`] = 100;
    }
    const gatewayDurable = name.endsWith('/gateway') && name.startsWith('durable_');
    return { name, repetition, samples: 24, succeeded: 24, dispatches: 24,
      events_per_request: name.startsWith('durable_unary') ? 0 : name.startsWith('durable_stream') ? 66 : 64,
      mapping_checks: gatewayDurable ? 24 : 0, retrieval_checks: gatewayDurable ? 24 : 0,
      negative_controls: gatewayDurable ? 1 : 0, negative_dispatches: 0, ambiguous_outcomes: 0, metrics };
  }));
}
function baseline() {
  return { schema: 'openllmproxy.dev/fidelity-lifecycle-performance/v2', contract: null, source_revision: 'reference',
    working_tree: '', reference_product_diff: ['scripts/fidelity-lifecycle-v2-benchmark.mjs', 'tests/integration/fidelity_lifecycle_v2_test.go'],
    harness_sha256: 'harness', runner_sha256: 'runner', frozen_harness_sha256: 'v1harness',
    historical_baseline_sha256: sha256('docs/evidence/fidelity-performance/lifecycle-v1/baseline.json'),
    historical_budget_sha256: sha256('docs/evidence/fidelity-performance/lifecycle-v1/replacement-budgets.json'),
    command: ['go', 'test'], runtime_environment: { GOMAXPROCS: '4' }, toolchain: 'go version',
    go_build_environment: 'amd64', hardware: { cpu: 'host' }, storage: { postgresql_server_version: '180006' },
    conditions: { ordering: 'fixed' }, repetitions: 3, samples_per_repetition: 24, concurrency: [1, 4], runs: runs() };
}
function historical() {
  return { schema: 'openllmproxy.dev/fidelity-lifecycle-budget/v1', baseline_revision: '373c58467a3a17da97d2b48b16473ffd715afeac', harness_sha256: 'v1harness',
    maxima: Object.fromEntries(names.map((name) => [name, Object.fromEntries(Object.keys(runs().find((r) => r.name === name).metrics).filter((m) => m !== 'elapsed_ns').map((m) => [m, 1000]))])) };
}
function candidate(a) {
  return { ...structuredClone(a), contract: { mode: 'strict' }, source_revision: 'candidate', reference_product_diff: null };
}
test('the complete v2 native inventory and strict candidate compare to pre-change limits', () => {
  const b = baseline();
  assert.equal(Object.keys(validateV2Runs(b.runs)).length, 16);
  assert.deepEqual(parseRuns(b.runs.map((r) => `x: LIFECYCLE_V2_MEASUREMENT ${JSON.stringify(r)}`).join('\n')), b.runs);
  const budget = freeze(b, historical());
  assert.deepEqual(compare(candidate(b), budget), []);
  assert.equal(b.runs.reduce((n, r) => n + r.succeeded, 0), 1152);
  assert.equal(b.runs.reduce((n, r) => n + r.retrieval_checks, 0), 288);
});
test('coverage and oracle mutations fail before numeric comparison', () => {
  const budget = freeze(baseline(), historical());
  for (const mutate of [
    (a) => a.runs.pop(), (a) => a.runs[0].samples--, (a) => a.runs[0].dispatches--,
    (a) => a.runs[1].events_per_request--, (a) => a.runs[1].mapping_checks--,
    (a) => a.runs[1].retrieval_checks--, (a) => a.runs[1].negative_controls--,
    (a) => a.runs[1].negative_dispatches++, (a) => a.runs[1].ambiguous_outcomes++,
    (a) => delete a.runs[1].metrics['publication-p99-us'],
  ]) {
    const a = candidate(baseline()); mutate(a); assert.throws(() => compare(a, budget));
  }
});
test('changed identity, conditions, source or budget cannot pass', () => {
  const budget = freeze(baseline(), historical());
  for (const mutate of [
    (a) => a.contract = null, (a) => a.harness_sha256 = 'changed',
    (a) => a.frozen_harness_sha256 = 'changed', (a) => a.hardware.cpu = 'other',
    (a) => a.storage.postgresql_server_version = '190001', (a) => a.working_tree = 'dirty',
    (a) => a.source_revision = 'reference', (a) => a.historical_budget_sha256 = 'changed'
  ]) {
    const a = candidate(baseline()); mutate(a); assert.throws(() => compare(a, budget));
  }
  const a = candidate(baseline()); a.runs.forEach((r) => { if (r.name === 'durable_unary/c1/gateway') r.metrics['publication-p99-us'] = 1001; });
  assert.equal(compare(a, budget).length, 1);
  budget.maxima[names[0]]['B/op'] = NaN;
  assert.throws(() => compare(candidate(baseline()), budget));
});
test('B-only freeze rejects changed historical product or source-envelope failures', () => {
  for (const mutate of [
    (a) => a.reference_product_diff.push('internal/gateway/responses.go'),
    (a) => a.contract = { mode: 'strict' },
    (a) => a.working_tree = 'modified',
    (a) => a.runs.forEach((r) => { if (r.name === 'durable_unary/c1/gateway') r.metrics['ns/op'] = 1001; }),
  ]) {
    const a = baseline(); mutate(a); assert.throws(() => freeze(a, historical()));
  }
});
