import assert from 'node:assert/strict';
import { test } from 'node:test';
import { compareBudgets, freezeBudgets, parseBenchmarks, summarize } from './fidelity-benchmark.mjs';

function evidence() {
  const runs = [];
  for (const workload of ['native_unary', 'native_stream_256', 'native_slow_stream_64', 'native_asset_png', 'translated_unary', 'rejected_extension']) {
    for (const concurrency of [1, 8]) {
      for (const mode of workload === 'rejected_extension' ? ['gateway'] : ['relay', 'gateway']) {
        for (let repeat = 0; repeat < 3; repeat++) {
          const rejected = workload === 'rejected_extension';
          const events = workload === 'native_stream_256' ? 256 : workload === 'native_slow_stream_64' ? 64 : 0;
          const metrics = {
            'ns/op': 100_000, 'B/op': 50_000, 'allocs/op': 100, 'latency-p50-us': 80,
            'latency-p95-us': 90, 'latency-p99-us': 100, 'process-cpu-ns/op': 150_000,
            'sampled-heap-growth-B': 1_000_000, 'request-bytes': 100, 'content-events/op': events,
            dispatches: rejected ? 0 : 100, succeeded: rejected ? 0 : 100, rejected: rejected ? 100 : 0
          };
          if (events) for (const category of ['first-event', 'max-inter-event-gap']) for (const percentile of [50, 95, 99]) metrics[`${category}-p${percentile}-us`] = 10;
          runs.push({ name: `${workload}/c${concurrency}/${mode}`, iterations: 100, metrics });
        }
      }
    }
  }
  return { schema: 'openllmproxy.dev/fidelity-performance/v1', source_revision: 'baseline', contract_mode: 'legacy', route_contract: null, harness_sha256: 'fixture-hash', runner_sha256: 'fixture-runner', command: ['go', 'test'], conditions: { network: 'fixture' }, target_time_per_repetition: '2s', toolchain: 'fixture Go', runtime_environment: { GOGC: '100' }, go_build_environment: 'fixture flags', repetitions: 3, gomaxprocs: 4, hardware: { cpu: 'fixture', logical_cpus: 8, architecture: 'amd64', cpu_quota: 'max 100000' }, runs };
}

test('complete measured inventory retains successful and rejected workloads', () => {
  const artifact = evidence();
  assert.equal(Object.keys(summarize(artifact.runs, 3)).length, 22);
  assert.deepEqual(compareBudgets(artifact, freezeBudgets(artifact)), []);
});

test('missing workloads, fewer events, changed effects and invalid metrics cannot pass', () => {
  const original = evidence();
  const budgets = freezeBudgets(original);
  for (const mutate of [
    (candidate) => candidate.runs.splice(0, 3),
    (candidate) => { candidate.runs[0].metrics.dispatches = 0; },
    (candidate) => { candidate.runs.at(-1).metrics.succeeded = 100; },
    (candidate) => { candidate.runs.find((run) => run.name.startsWith('native_stream_256/')).metrics['content-events/op'] = 255; },
    (candidate) => { candidate.runs[0].metrics['B/op'] = Number.NaN; }
  ]) {
    const candidate = structuredClone(original);
    mutate(candidate);
    assert.throws(() => compareBudgets(candidate, budgets));
  }
});

test('slower and allocating replacements fail independently of response success', () => {
  const candidate = evidence();
  const budgets = freezeBudgets(candidate);
  for (const run of candidate.runs.filter((run) => run.name === 'native_unary/c1/gateway')) {
    run.metrics['latency-p99-us'] = 50_000;
    run.metrics['allocs/op'] = 5_000;
  }
  const failures = compareBudgets(candidate, budgets);
  assert.equal(failures.length, 2);
  assert.ok(failures.some((message) => message.includes('latency-p99-us')));
  assert.ok(failures.some((message) => message.includes('allocs/op')));
});

test('hardware changes and request reductions require separate qualification', () => {
  const candidate = evidence();
  const budgets = freezeBudgets(candidate);
  candidate.hardware.cpu_quota = '100000 100000';
  assert.throws(() => compareBudgets(candidate, budgets), /Hardware differs/);
  candidate.hardware.cpu_quota = budgets.hardware.cpu_quota;
  candidate.runs[0].metrics['request-bytes'] = 99;
  assert.match(compareBudgets(candidate, budgets)[0], /workload request-bytes changed/);
});

test('parser rejects incomplete metrics instead of treating partial output as evidence', () => {
  assert.throws(() => parseBenchmarks('BenchmarkFidelity/native_unary/c1/gateway-4 100 10 ns/op'), /counts changed/);
  assert.deepEqual(parseBenchmarks('goos: linux\nPASS\n'), []);
  assert.throws(() => summarize([], 3), /expected 3 repetitions/);
});

test('changed harness and implicit legacy cannot masquerade as explicit contract evidence', () => {
  const candidate = evidence();
  const budgets = freezeBudgets(candidate);
  candidate.harness_sha256 = 'weakened-oracle';
  assert.throws(() => compareBudgets(candidate, budgets), /harness\/oracle differs/);
  candidate.harness_sha256 = budgets.baseline_harness_sha256;
  assert.throws(() => compareBudgets(candidate, budgets, 'explicit'), /Expected explicit/);
  candidate.contract_mode = 'explicit';
  assert.throws(() => compareBudgets(candidate, budgets, 'explicit'), /contracts must be recorded/);
  candidate.route_contract = { native: { fidelity: 'native_identity' }, translated: { fidelity: 'qualified_interaction' }, rejected: { fidelity: 'qualified_interaction' } };
  assert.deepEqual(compareBudgets(candidate, budgets, 'explicit'), []);
});

test('changed runner or measurement conditions cannot reuse frozen budgets', () => {
  const original = evidence();
  const budgets = freezeBudgets(original);
  for (const mutate of [
    (candidate) => { candidate.runner_sha256 = 'changed'; },
    (candidate) => { candidate.command = ['different']; },
    (candidate) => { candidate.conditions = { network: 'one fewer hop' }; },
    (candidate) => { candidate.runtime_environment = { GOGC: 'off' }; },
    (candidate) => { candidate.hardware.memory_limit = 'unlimited'; }
  ]) {
    const candidate = structuredClone(original);
    mutate(candidate);
    assert.throws(() => compareBudgets(candidate, budgets));
  }
});
