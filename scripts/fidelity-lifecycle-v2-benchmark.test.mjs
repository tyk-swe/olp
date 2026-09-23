import assert from 'node:assert/strict';
import { test } from 'node:test';
import { readFileSync } from 'node:fs';
import { mkdtempSync, rmSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';
import { names } from './fidelity-lifecycle-benchmark.mjs';
import { parseRuns, validateV2Runs, historicalAddedLimits, freeze, compare, candidateSetupSHA256,
  evidencePaths, reserveCapture, completeCapture, readCapture, verifyBaselineReceipt, verifyCandidateLineage,
  selectEvidenceAncestor, verifyOfflineSource } from './fidelity-lifecycle-v2-benchmark.mjs';
const sha256 = (path) => createHash('sha256').update(readFileSync(path)).digest('hex');
const syntheticCaptureSHA = 'a'.repeat(64);
const originalBudget = JSON.parse(readFileSync('docs/evidence/fidelity-performance/lifecycle-v1/replacement-budgets.json'));
const originalBaseline = JSON.parse(readFileSync('docs/evidence/fidelity-performance/lifecycle-v1/baseline.json'));

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
    harness_sha256: sha256('tests/integration/fidelity_lifecycle_v2_test.go'),
    runner_sha256: sha256('scripts/fidelity-lifecycle-v2-benchmark.mjs'),
    runner_test_sha256: sha256('scripts/fidelity-lifecycle-v2-benchmark.test.mjs'),
    frozen_harness_sha256: originalBudget.harness_sha256,
    historical_baseline_sha256: sha256('docs/evidence/fidelity-performance/lifecycle-v1/baseline.json'),
    historical_budget_sha256: sha256('docs/evidence/fidelity-performance/lifecycle-v1/replacement-budgets.json'),
    command: ['go', 'test'], runtime_environment: { GOMAXPROCS: '4' }, toolchain: 'go version',
    go_build_environment: 'amd64', hardware: { cpu: 'host' }, storage: { postgresql_server_version: '180006' },
    setup_sha256: sha256('tests/integration/access_test.go'),
    conditions: { ordering: 'fixed' }, repetitions: 3, samples_per_repetition: 24, concurrency: [1, 4], runs: runs() };
}
function historical() {
  return structuredClone(originalBudget);
}
function candidate(a) {
  return { ...structuredClone(a), contract: { mode: 'strict' }, source_revision: 'candidate', reference_product_diff: null,
    setup_sha256: candidateSetupSHA256, baseline_evidence_commit: 'b'.repeat(40) };
}
test('the complete v2 native inventory and strict candidate compare to pre-change limits', () => {
  const b = baseline();
  assert.equal(Object.keys(validateV2Runs(b.runs)).length, 16);
  assert.deepEqual(parseRuns(b.runs.map((r) => `x: LIFECYCLE_V2_MEASUREMENT ${JSON.stringify(r)}`).join('\n')), b.runs);
  const budget = freeze(b, historical(), syntheticCaptureSHA);
  assert.equal(Object.keys(budget.added_latency_maxima).length, 8);
  assert.equal(Object.values(budget.added_latency_maxima).flatMap(Object.values).length, 24);
  assert.deepEqual(budget.added_latency_maxima, historicalAddedLimits(originalBaseline));
  assert.deepEqual(compare(candidate(b), budget), []);
  assert.equal(b.runs.reduce((n, r) => n + r.succeeded, 0), 1152);
  assert.equal(b.runs.reduce((n, r) => n + r.retrieval_checks, 0), 288);
});
test('coverage and oracle mutations fail before numeric comparison', () => {
  const budget = freeze(baseline(), historical(), syntheticCaptureSHA);
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
  const budget = freeze(baseline(), historical(), syntheticCaptureSHA);
  for (const mutate of [
    (a) => a.contract = null, (a) => a.harness_sha256 = 'changed',
    (a) => a.frozen_harness_sha256 = 'changed', (a) => a.hardware.cpu = 'other',
    (a) => a.storage.postgresql_server_version = '190001', (a) => a.working_tree = 'dirty',
    (a) => a.source_revision = 'reference', (a) => a.historical_budget_sha256 = 'changed',
    (a) => a.setup_sha256 = 'changed'
  ]) {
    const a = candidate(baseline()); mutate(a); assert.throws(() => compare(a, budget));
  }
  const a = candidate(baseline()); a.runs.forEach((r) => { if (r.name === 'durable_unary/c1/gateway') r.metrics['publication-p99-us'] = budget.maxima[r.name]['publication-p99-us'] + 1; });
  assert.equal(compare(a, budget).length, 1);
  budget.maxima[names[0]]['B/op'] = NaN;
  assert.throws(() => compare(candidate(baseline()), budget));
  const setupBudget = freeze(baseline(), historical(), syntheticCaptureSHA);
  setupBudget.expected_candidate_setup_sha256 = 'changed';
  assert.throws(() => compare(candidate(baseline()), setupBudget));
});
test('every added-latency limit is candidate-independent and mutation guarded', () => {
  const budget = freeze(baseline(), historical(), syntheticCaptureSHA);
  const label = 'durable_unary/c1';
  const metric = 'latency-p99-us';
  const a = candidate(baseline());
  for (const run of a.runs) if (run.name === `${label}/gateway`) run.metrics[metric] = budget.added_latency_maxima[label][metric] + 101;
  assert.ok(compare(a, budget).some((failure) => failure.includes(`${label}: added ${metric}`)));
  delete budget.added_latency_maxima[label][metric];
  assert.throws(() => compare(candidate(baseline()), budget));
});
test('B-only freeze rejects changed historical product or source-envelope failures', () => {
  for (const mutate of [
    (a) => a.reference_product_diff.push('internal/gateway/responses.go'),
    (a) => a.contract = { mode: 'strict' },
    (a) => a.working_tree = 'modified',
    (a) => a.harness_sha256 = 'changed',
    (a) => a.runner_sha256 = 'changed',
    (a) => a.setup_sha256 = 'changed',
    (a) => a.runs.forEach((r) => { if (r.name === 'durable_unary/c1/gateway') r.metrics['ns/op'] = originalBudget.maxima[r.name]['ns/op'] + 1; }),
  ]) {
    const a = baseline(); mutate(a); assert.throws(() => freeze(a, historical(), syntheticCaptureSHA));
  }
});
test('fixed evidence paths reject caller-selected retry paths before Go starts', () => {
  assert.equal(evidencePaths.baseline, 'docs/evidence/fidelity-performance/lifecycle-v2/baseline.jsonl');
  assert.equal(evidencePaths.candidate, 'docs/evidence/fidelity-performance/lifecycle-v2/strict-candidate.jsonl');
  for (const action of ['record-baseline', 'record-strict', 'freeze', 'compare']) {
    const result = spawnSync(process.execPath, ['scripts/fidelity-lifecycle-v2-benchmark.mjs', action, '/tmp/selective-retry.json'], { encoding: 'utf8' });
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /Only fixed preregistered evidence paths/);
  }
});
test('reservation is exclusive and failed or partial append-only attempts remain visible', () => {
  const dir = mkdtempSync(join(tmpdir(), 'olp-lifecycle-v2-test-'));
  try {
    const path = join(dir, 'capture.jsonl');
    const startedAt = '2026-09-23T00:00:00.000Z';
    const id = reserveCapture(path, 'reference', null, startedAt);
    assert.throws(() => readCapture(path), /Incomplete/);
    assert.throws(() => reserveCapture(path, 'reference', null, startedAt), /EEXIST/);
    completeCapture(path, id, 'failed', { source_revision: 'reference', contract: null, started_at: startedAt,
      reason: 'Go failed', raw_output: 'LIFECYCLE_V2_MEASUREMENT partial' });
    assert.equal(readCapture(path).phase, 'failed');
    const budget = freeze(baseline(), historical(), syntheticCaptureSHA);
    assert.throws(() => verifyBaselineReceipt(readCapture(path), budget, syntheticCaptureSHA), /complete B-only/);
    completeCapture(path, id, 'complete', { source_revision: 'reference', contract: null, started_at: startedAt });
    assert.throws(() => readCapture(path), /Incomplete/);
  } finally { rmSync(dir, { recursive: true, force: true }); }
});
test('complete capture requires matching reservation identity, source and contract', () => {
  const dir = mkdtempSync(join(tmpdir(), 'olp-lifecycle-v2-complete-'));
  try {
    const path = join(dir, 'candidate.jsonl');
    const startedAt = '2026-09-23T00:00:00.000Z';
    const contract = { mode: 'strict' };
    const id = reserveCapture(path, 'candidate', contract, startedAt);
    completeCapture(path, id, 'complete', { source_revision: 'candidate', contract, started_at: startedAt });
    assert.equal(readCapture(path).phase, 'complete');
    const wrong = join(dir, 'wrong.jsonl');
    const wrongID = reserveCapture(wrong, 'candidate', contract, startedAt);
    completeCapture(wrong, wrongID, 'complete', { source_revision: 'different', contract, started_at: startedAt });
    assert.throws(() => readCapture(wrong), /Invalid append-only/);
  } finally { rmSync(dir, { recursive: true, force: true }); }
});
test('committed B receipt requires exact capture SHA, source, conditions and self-compare', () => {
  const b = baseline();
  const budget = freeze(b, historical(), syntheticCaptureSHA);
  const capture = { phase: 'complete', artifact: b };
  assert.equal(verifyBaselineReceipt(capture, budget, syntheticCaptureSHA), b);
  assert.throws(() => verifyBaselineReceipt(capture, budget, 'b'.repeat(64)), /capture SHA/);
  const mutated = structuredClone(capture);
  mutated.artifact.source_revision = 'different';
  assert.throws(() => verifyBaselineReceipt(mutated, budget, syntheticCaptureSHA), /disagree/);
  mutated.artifact.source_revision = b.source_revision;
  for (const run of mutated.artifact.runs) if (run.name === 'durable_unary/c1/gateway') {
    run.metrics['ns/op'] = budget.maxima['durable_unary/c1/gateway']['ns/op'] + 1;
  }
  assert.throws(() => verifyBaselineReceipt(mutated, budget, syntheticCaptureSHA), /self-compare/);
  const changed = structuredClone(budget);
  delete changed.added_latency_maxima['durable_unary/c1'];
  assert.throws(() => verifyBaselineReceipt(capture, changed, syntheticCaptureSHA), /numeric method/);
});
test('candidate requires all method, historical B, and strict product ancestry', () => {
  const complete = { methodAncestor: true, productAncestor: true, baselineAncestor: true, productDiff: true };
  assert.doesNotThrow(() => verifyCandidateLineage(complete));
  for (const field of Object.keys(complete)) {
    assert.throws(() => verifyCandidateLineage({ ...complete, [field]: false }), /ancestry or strict product change/);
  }
});
test('B artifact and budget must exist with exact blobs in a strict ancestor of C', () => {
  const head = 'c'.repeat(40);
  const older = 'b'.repeat(40);
  const bBlob = 'b-blob';
  const budgetBlob = 'budget-blob';
  assert.equal(selectEvidenceAncestor(head, bBlob, budgetBlob, [
    { commit: head, baselineBlob: bBlob, budgetBlob },
    { commit: older, baselineBlob: bBlob, budgetBlob }
  ]), older);
  assert.throws(() => selectEvidenceAncestor(head, bBlob, budgetBlob,
    [{ commit: head, baselineBlob: bBlob, budgetBlob }]), /strict ancestor/);
  assert.throws(() => selectEvidenceAncestor(head, bBlob, budgetBlob,
    [{ commit: older, baselineBlob: 'different', budgetBlob }]), /strict ancestor/);
  assert.throws(() => selectEvidenceAncestor(head, bBlob, budgetBlob,
    [{ commit: older, baselineBlob: bBlob, budgetBlob: 'different' }]), /strict ancestor/);
  const budget = freeze(baseline(), historical(), syntheticCaptureSHA);
  const c = candidate(baseline());
  delete c.baseline_evidence_commit;
  assert.throws(() => compare(c, budget), /B evidence commit/);
});
test('offline comparison accepts a docs-only descendant but rejects unrelated or changed source', () => {
  const c = candidate(baseline());
  const sourceHashes = Object.fromEntries(['harness_sha256', 'runner_sha256', 'runner_test_sha256', 'frozen_harness_sha256', 'setup_sha256'].map((field) => [field, c[field]]));
  const currentHashes = Object.fromEntries(['harness_sha256', 'runner_sha256', 'runner_test_sha256'].map((field) => [field, c[field]]));
  const valid = { candidateAncestor: true, baselineBlobsMatch: true, evidenceCommit: c.baseline_evidence_commit, sourceHashes, currentHashes };
  // The verifier takes ancestry, not HEAD equality: a later evidence/docs commit is valid.
  assert.doesNotThrow(() => verifyOfflineSource(c, valid));
  assert.throws(() => verifyOfflineSource(c, { ...valid, candidateAncestor: false }), /unrelated/);
  assert.throws(() => verifyOfflineSource(c, { ...valid, baselineBlobsMatch: false }), /changed/);
  assert.throws(() => verifyOfflineSource(c, { ...valid, evidenceCommit: 'e'.repeat(40) }), /changed/);
  for (const field of Object.keys(sourceHashes)) {
    assert.throws(() => verifyOfflineSource(c, { ...valid, sourceHashes: { ...sourceHashes, [field]: 'mutated' } }), /committed source/);
  }
  assert.throws(() => verifyOfflineSource(c, { ...valid, currentHashes: { ...currentHashes, runner_sha256: 'mutated' } }), /Current comparator/);
});
