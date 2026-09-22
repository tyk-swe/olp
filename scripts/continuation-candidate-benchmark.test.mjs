import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { metricNames, validateRuns, parseRuns, compareCandidate, referenceEvidence, verifyHistoricalSources, historicalHash, sharedSources, commandArgs, runtimeEnvironment, conditions, comparisonScope } from './continuation-candidate-benchmark.mjs';

const hash = (path) => createHash('sha256').update(readFileSync(path)).digest('hex');
const baselinePath = 'docs/evidence/fidelity-performance/barrier-v1/baseline.json';
const budgetPath = 'docs/evidence/fidelity-performance/barrier-v1/budgets.json';
const baseline = JSON.parse(readFileSync(baselinePath, 'utf8'));
const budgets = JSON.parse(readFileSync(budgetPath, 'utf8'));

function completeRuns() {
  const runs = [];
  for (const size of ['small', 'large']) for (const concurrency of [1, 8]) for (let repetition = 0; repetition < 3; repetition++) {
    const metrics = Object.fromEntries(metricNames().map((name) => [name, 1]));
    for (const p of [50, 95, 99]) {
      metrics[`first-event-p${p}-us`] = 100;
      metrics[`tool-visible-p${p}-us`] = 200;
      metrics[`action-ready-p${p}-us`] = 300;
      metrics[`workflow-p${p}-us`] = 400;
    }
    runs.push({
      name: `${size}/c${concurrency}/translated`, repetition, samples: 24,
      dispatches: 48, first_requests: 24, next_requests: 24,
      native_events: 456, first_turn_observations: 312, final_observations: 48,
      actions: 48, ready_checks: 24, rejected: 0,
      history_bytes: size === 'large' ? 262144 : 0,
      state_bytes: size === 'large' ? 527000 : 2100,
      contract: 'negotiated-chat-anthropic-tools-v1/go-sdk-equivalent/2', metrics
    });
  }
  return runs;
}

function completeEvidence() {
  return {
    schema: 'openllmproxy.dev/continuation-candidate-performance/v2',
    contract: 'negotiated-chat-anthropic-tools-v1/go-sdk-equivalent/2',
    working_tree: '',
    reference_sha256: hash(baselinePath), frozen_budget_sha256: hash(budgetPath),
    reference_revision: baseline.source_revision,
    harness_sha256: hash('tests/integration/continuation_candidate_benchmark_test.go'),
    runner_sha256: hash('scripts/continuation-candidate-benchmark.mjs'),
    shared_source_sha256: Object.fromEntries(sharedSources.map((source) => [source, hash(source)])),
    conditions, runtime_environment: runtimeEnvironment, command: ['go', ...commandArgs],
    repetitions: 3, samples_per_repetition: 24, concurrency: [1, 8],
    toolchain: baseline.toolchain, go_build_environment: baseline.go_build_environment,
    storage: structuredClone(baseline.storage), hardware: structuredClone(baseline.hardware),
    comparison_scope: comparisonScope, runs: completeRuns()
  };
}

test('complete candidate observations have a fixed comparison inventory', () => {
  const summary = validateRuns(completeRuns());
  assert.equal(Object.keys(summary).length, 4);
  assert.equal(summary['small/c1/translated']['action-ready-p99-us'].median, 300);
});
test('missing work, rejected provider calls and duplicate repetitions fail', () => {
  for (const change of [
    (runs) => runs.pop(),
    (runs) => { runs[0].dispatches--; },
    (runs) => { runs[0].ready_checks--; },
    (runs) => { runs[0].rejected++; },
    (runs) => { runs[0].repetition = 1; }
  ]) {
    const runs = completeRuns();
    change(runs);
    assert.throws(() => validateRuns(runs));
  }
});
test('changed history, state, observations or metric inventory fail', () => {
  for (const change of [
    (runs) => { runs[6].history_bytes--; },
    (runs) => { runs[6].state_bytes = 262144; },
    (runs) => { runs[0].first_turn_observations--; },
    (runs) => { runs[0].final_observations--; },
    (runs) => { delete runs[0].metrics['process-cpu-ns/op']; },
    (runs) => { runs[0].metrics['action-ready-p99-us'] = -1; }
  ]) {
    const runs = completeRuns();
    change(runs);
    assert.throws(() => validateRuns(runs));
  }
});
test('first observation, projected tool, recoverable action and workflow stay ordered', () => {
  const runs = completeRuns();
  runs[0].metrics['tool-visible-p95-us'] = 50;
  assert.throws(() => validateRuns(runs));
});
test('candidate parser accepts only recorded measurement markers', () => {
  assert.deepEqual(parseRuns('other {"x":1}\nCANDIDATE_MEASUREMENT {"name":"small"}\n'), [{ name: 'small' }]);
});
test('frozen runtime identity and source inventory are required before comparison', () => {
  assert.equal(referenceEvidence().baseline.source_revision, baseline.source_revision);
  assert.deepEqual(compareCandidate(completeEvidence(), baseline, budgets), []);
  for (const change of [
    (evidence) => { evidence.hardware.total_memory_bytes--; },
    (evidence) => { evidence.hardware.kernel = 'other'; },
    (evidence) => { evidence.go_build_environment += '\n'; },
    (evidence) => { delete evidence.shared_source_sha256[sharedSources[0]]; },
    (evidence) => { evidence.shared_source_sha256['unfrozen-oracle'] = 'x'; },
    (evidence) => { evidence.repetitions = 2; },
    (evidence) => { evidence.samples_per_repetition = 8; },
    (evidence) => { evidence.concurrency = [8, 1]; }
  ]) {
    const evidence = completeEvidence();
    change(evidence);
    assert.throws(() => compareCandidate(evidence, baseline, budgets));
  }
});
test('missing history and corrupt historical source fail closed', () => {
  const source = 'tests/integration/fidelity_lifecycle_performance_test.go';
  assert.throws(() => historicalHash('0'.repeat(40), source), /Frozen source is unavailable/);
  const corrupt = structuredClone(baseline);
  corrupt.dependency_sha256[source] = '0'.repeat(64);
  assert.throws(() => verifyHistoricalSources(corrupt), /Frozen historical source changed/);
});
