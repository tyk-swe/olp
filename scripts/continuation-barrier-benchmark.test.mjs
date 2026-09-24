import assert from 'node:assert/strict';
import { test } from 'node:test';
import { names, metrics, validateRuns, parseRuns, freeze, compare } from './continuation-barrier-benchmark.mjs';

function evidence() {
  const runs = names.flatMap((name) => [0, 1, 2].map((repetition) => ({
    name, repetition, samples: 24, first_requests: 24, next_requests: 24,
    dispatches: 48, events: 456, actions: 48,
    history_bytes: name.startsWith('large/') ? 262144 : 0,
    state_bytes: name.startsWith('large/') ? 263050 : 904,
    metrics: Object.fromEntries(metrics(name).map((metric) => [metric, 100]))
  })));
  return {
    schema: 'openllmproxy.dev/continuation-barrier-performance/v1',
    contract: 'native-wire-with-reference-side-encrypted-barrier/1',
    working_tree: '', harness_sha256: 'fixture', runner_sha256: 'fixture',
    dependency_sha256: { corpus: 'fixture' }, hardware: { cpu: 'fixture' },
    storage: { postgresql_server_version: '180006' }, runs
  };
}
test('all native workflows and encrypted reference phases survive recording and comparison', () => {
  const a = evidence();
  assert.equal(Object.keys(validateRuns(a.runs)).length, 12);
  assert.deepEqual(compare(a, freeze(a)), []);
  assert.deepEqual(parseRuns(a.runs.map((r) => ` test: BARRIER_MEASUREMENT ${JSON.stringify(r)}`).join('\n')), a.runs);
});
test('missing requests, events, actions, history, phases or repetitions cannot qualify', () => {
  const mutations = [
    (a) => a.runs.pop(), (a) => a.runs[0].samples--,
    (a) => a.runs[0].first_requests--, (a) => a.runs[0].next_requests--,
    (a) => a.runs[0].dispatches--, (a) => a.runs[0].events--,
    (a) => a.runs[0].actions--, (a) => a.runs[0].history_bytes++,
    (a) => a.runs[0].state_bytes++,
    (a) => delete a.runs.find((r) => r.name.endsWith('/reference')).metrics['ready-commit-p99-us'],
    (a) => a.runs[0].metrics['B/op'] = NaN,
    (a) => a.runs[1] = structuredClone(a.runs[0]),
    (a) => a.runs[0].metrics['action-ready-p99-us'] = 1
  ];
  for (const mutate of mutations) {
    const a = evidence(); const b = freeze(a);
    mutate(a);
    assert.throws(() => compare(a, b));
  }
});
test('delayed recoverable tool availability fails independently of native wire visibility', () => {
  const a = evidence(); const b = freeze(a);
  for (const run of a.runs) if (run.name.endsWith('/reference')) {
    run.metrics['action-ready-p99-us'] = 5000;
    run.metrics['workflow-p99-us'] = 5100;
  }
  const failures = compare(a, b);
  assert.equal(failures.length, 8);
  assert.equal(failures.filter((f) => f.includes('action-ready')).length, 4);
});
test('edited conditions, identities and translated semantics require separately reviewed evidence', () => {
  for (const mutate of [
    (a) => a.harness_sha256 = 'changed', (a) => a.runner_sha256 = 'changed',
    (a) => a.dependency_sha256.corpus = 'changed', (a) => a.hardware.cpu = 'different',
    (a) => a.storage.postgresql_server_version = '190001',
    (a) => a.contract = 'translated', (a) => a.runs.forEach((r) => r.state_bytes++)
  ]) {
    const a = evidence(); const b = freeze(a); mutate(a);
    assert.throws(() => compare(a, b));
  }
  const a = evidence(); a.working_tree = 'modified'; assert.throws(() => freeze(a));
});
test('edited budget inventory and invalid bounds cannot remove required comparisons', () => {
  for (const mutate of [
    (b) => delete b.maxima[names[0]],
    (b) => delete b.maxima[names[0]]['B/op'],
    (b) => b.maxima[names[0]]['B/op'] = NaN
  ]) {
    const a = evidence(); const b = freeze(a); mutate(b);
    assert.throws(() => compare(a, b));
  }
});
