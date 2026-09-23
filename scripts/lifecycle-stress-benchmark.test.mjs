import assert from 'node:assert/strict';
import { test } from 'node:test';
import { existsSync, mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { compare, freeze, names, parse, validateNegatives, validateRuns, writeFailedCapture } from './lifecycle-stress-benchmark.mjs';

function evidence() {
  const runs = names.flatMap((name) => [0, 1, 2].map((repetition) => {
    const workload = name.split('/')[0];
    const mediaCycle = workload === 'media_cycle_4m';
    const mediaSlow = workload === 'media_slow_content_1m';
    const media = mediaCycle || mediaSlow;
    const metrics = Object.fromEntries([
      'elapsed_ns', 'ns/op', 'process-cpu-ns/op', 'B/op', 'allocs/op',
      'sampled-heap-growth-B', 'sampled-spool-peak-B', 'spool-final-B'
    ].map((metric) => [metric, metric === 'spool-final-B' ? 0 : 100]));
    for (const category of ['latency', ...(media ? ['first-byte', 'inter-read-gap'] : ['event-rtt', 'event-jitter', 'cancellation'])]) {
      for (const percentile of [50, 95, 99]) metrics[`${category}-p${percentile}-us`] = 100;
    }
    return {
      name, repetition, samples: 8, succeeded: 8, rejected: 0, incomplete: 0,
      ambiguous: 0, dispatches: mediaCycle ? 32 : 8, accepted: mediaCycle ? 8 : 0,
      partial: mediaCycle ? 8 : 0, retrieved: mediaCycle ? 16 : mediaSlow ? 8 : 0,
      content: media ? 8 : 0, events: media ? 0 : 512,
      rtt_observations: media ? 0 : 512, jitter_observations: media ? 0 : 504,
      uploaded_bytes: mediaCycle ? 8 * (4 << 20) : 0,
      downloaded_bytes: media ? 8 * (1 << 20) : 0, metrics
    };
  }));
  return {
    schema: 'openllmproxy.dev/lifecycle-stress-performance/v1', contract: null,
    working_tree: '', harness_sha256: 'harness', fixture_sha256: 'fixture',
    runner_sha256: 'runner', command: ['go', 'test'], runtime_environment: {},
    toolchain: 'go fixture', go_build_environment: 'fixture', hardware: { cpu: 'fixture' },
    storage: { postgresql_server_version: '180004' }, conditions: { network: 'fixture' },
    repetitions: 3, samples_per_repetition: 8, concurrency: { media: [1, 2], duplex: [1, 4] }, runs,
    negatives: [
      { name: 'oversize_upload', path: 'gateway', admitted: 0, rejected: 1, provider_dispatches: 0, spool_final_bytes: 0 },
      { name: 'cancelled_upload', path: 'gateway', admitted: 0, rejected: 1, provider_dispatches: 0, spool_final_bytes: 0 },
      { name: 'parser_contention', path: 'gateway', admitted: 0, rejected: 1, provider_dispatches: 0, spool_final_bytes: 0 }
    ]
  };
}

test('complete native inventory, controls, and direct/gateway comparison pass', () => {
  const artifact = evidence();
  assert.equal(Object.keys(validateRuns(artifact.runs)).length, 12);
  assert.equal(validateNegatives(artifact.negatives), true);
  assert.deepEqual(compare(artifact, freeze(artifact)), []);
  assert.deepEqual(parse(artifact.runs.map((run) => `prefix LIFECYCLE_STRESS_MEASUREMENT ${JSON.stringify(run)}`).join('\n'), 'LIFECYCLE_STRESS_MEASUREMENT '), artifact.runs);
});

test('dropped workload, accepted work, partial status, retrieval, bytes, or event is not silently excluded', () => {
  for (const change of [
    (a) => a.runs.pop(),
    (a) => a.runs[0].accepted--,
    (a) => a.runs[0].partial--,
    (a) => a.runs[0].retrieved--,
    (a) => a.runs[0].uploaded_bytes--,
    (a) => a.runs[0].dispatches--,
    (a) => a.runs.find((r) => r.name.startsWith('duplex_jitter_64')).events--,
    (a) => a.runs.find((r) => r.name.startsWith('duplex_jitter_64')).jitter_observations--,
    (a) => a.runs[0].rejected++,
    (a) => a.runs[0].incomplete++,
    (a) => a.runs[0].ambiguous++,
    (a) => a.runs[1] = structuredClone(a.runs[0])
  ]) {
    const artifact = evidence(); const budget = freeze(artifact); change(artifact);
    assert.throws(() => compare(artifact, budget));
  }
});

test('media spool leak, latency, RTT jitter, and resource regressions fail frozen limits', () => {
  const artifact = evidence(); const budget = freeze(artifact);
  artifact.runs[0].metrics['spool-final-B'] = 1;
  assert.throws(() => compare(artifact, budget));
  artifact.runs[0].metrics['spool-final-B'] = 0;
  for (const run of artifact.runs) {
    if (run.name.includes('/gateway')) {
      run.metrics['latency-p99-us'] = 10000;
      run.metrics['sampled-heap-growth-B'] = 100000000;
      if (run.name.startsWith('duplex_jitter_64')) run.metrics['event-jitter-p99-us'] = 10000;
    }
  }
  const failures = compare(artifact, budget);
  assert.ok(failures.some((f) => f.includes('added latency-p99-us')));
  assert.ok(failures.some((f) => f.includes('sampled-heap-growth-B')));
  assert.ok(failures.some((f) => f.includes('event-jitter-p99-us')));
});

test('negative control loss, provider dispatch, and altered evidence identity cannot qualify', () => {
  for (const change of [
    (a) => a.negatives.pop(),
    (a) => a.negatives[0].provider_dispatches++,
    (a) => a.negatives[0].spool_final_bytes++,
    (a) => a.harness_sha256 = 'changed',
    (a) => a.fixture_sha256 = 'changed',
    (a) => a.runner_sha256 = 'changed',
    (a) => a.hardware.cpu = 'different',
    (a) => a.storage.postgresql_server_version = '190001',
    (a) => a.contract = { mode: 'strict' }
  ]) {
    const artifact = evidence(); const budget = freeze(artifact); change(artifact);
    assert.throws(() => compare(artifact, budget));
  }
});

test('invalid or weakened budget inventory is rejected and strict requires actual strict route', () => {
  for (const change of [
    (b) => delete b.maxima[names[0]],
    (b) => delete b.maxima[names[0]]['B/op'],
    (b) => b.maxima[names[0]]['B/op'] = NaN,
    (b) => delete b.added_latency_maxima['media_cycle_4m/c1'],
    (b) => b.added_latency_maxima['media_cycle_4m/c1']['latency-p99-us'] = -1
  ]) {
    const artifact = evidence(); const budget = freeze(artifact); change(budget);
    assert.throws(() => compare(artifact, budget));
  }
  const artifact = evidence(); const budget = freeze(artifact);
  assert.throws(() => compare(artifact, budget, 'strict'));
  artifact.contract = { mode: 'strict' };
  assert.deepEqual(compare(artifact, budget, 'strict'), []);
  assert.throws(() => freeze(artifact));
});

test('incomplete timed output is retained separately without creating a passing artifact', () => {
  const directory = mkdtempSync(join(tmpdir(), 'lifecycle-stress-failed-'));
  try {
    const requested = join(directory, 'baseline.json');
    const failed = writeFailedCapture(requested, 'missing repetition', { status: 1, stdout: 'partial marker', stderr: 'fixture failed' }, '2026-09-23T00:00:00Z', '0.1 0.2 0.3');
    assert.equal(existsSync(requested), false);
    const retained = JSON.parse(readFileSync(failed, 'utf8'));
    assert.equal(retained.reason, 'missing repetition');
    assert.equal(retained.stdout, 'partial marker');
    assert.equal(retained.stderr, 'fixture failed');
    assert.equal(retained.exit_status, 1);
  } finally { rmSync(directory, { recursive: true, force: true }); }
});
