import test from 'node:test';
import assert from 'node:assert/strict';
import { metricNames, validateRuns, parseRuns } from './continuation-candidate-benchmark.mjs';

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
      native_events: 456, observations: 312, actions: 48, ready_checks: 24, rejected: 0,
      history_bytes: size === 'large' ? 262144 : 0,
      state_bytes: size === 'large' ? 527000 : 2100,
      contract: 'negotiated-chat-anthropic-tools-v1/go-sdk-equivalent/1', metrics
    });
  }
  return runs;
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
    (runs) => { runs[0].observations--; },
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
