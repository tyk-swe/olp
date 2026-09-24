import assert from 'node:assert/strict';
import { test } from 'node:test';
import { readFileSync } from 'node:fs';
import { compare as compareV2, evidencePaths, readCapture, verifyBaselineReceipt } from './fidelity-lifecycle-v2-benchmark.mjs';
import { fileSHA256 } from './fidelity-final-source-lock.mjs';
import { comparableV2, priorCandidateSHA256, candidatePath } from './fidelity-lifecycle-v3-benchmark.mjs';

test('v3 reuses the committed passing C and exact frozen B limits', () => {
  const budget = JSON.parse(readFileSync(evidencePaths.budget, 'utf8'));
  const baseline = readCapture(evidencePaths.baseline);
  verifyBaselineReceipt(baseline, budget, fileSHA256(evidencePaths.baseline));
  assert.equal(fileSHA256(evidencePaths.candidate), priorCandidateSHA256);
  const prior = readCapture(evidencePaths.candidate);
  assert.equal(prior.phase, 'complete');
  assert.deepEqual(compareV2(prior.artifact, budget), []);
  assert.equal(candidatePath, 'docs/evidence/fidelity-performance/lifecycle-v3/strict-candidate.jsonl');
  const normalized = comparableV2({ ...prior.artifact, schema: 'new-method', runner_sha256: 'new-runner', runner_test_sha256: 'new-test' }, budget);
  assert.deepEqual(compareV2(normalized, budget), []);
  const overLimit = structuredClone(normalized);
  for (const run of overLimit.runs.filter((run) => run.name === 'durable_unary/c1/gateway')) run.metrics['B/op'] = 1e9;
  assert.ok(compareV2(overLimit, budget).some((failure) => failure.includes('durable_unary/c1/gateway')));
});
