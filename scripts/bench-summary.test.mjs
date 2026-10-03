import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { caveats, render } from './bench-summary.mjs';

const s1 = {
  scenario: 'S1',
  workload: { stream_share: 0 },
  added_latency_ms: {
    all: { p50: 9, p95: 9, p99: 9 },
    unary: { p50: 0.5, p95: 1.25, p99: 3.1 },
    stream: null
  },
  ttft_overhead_ms: null,
  throughput: { rps_per_vcpu: 480, gateway_cpu_ms_per_request: 1.234 },
  resources: { rss_peak_mib: 211.4 },
  error_rate: { gateway: 0 },
  request_metadata_completeness: { completeness: 1 },
  targets: [
    { id: 'a', status: 'met' },
    { id: 'b', status: 'needs_comparison' },
    { id: 'c', status: 'needs_comparison' }
  ],
  validity: { valid: true },
  reference_conditions: { all: true }
};

const s6 = {
  scenario: 'S6',
  slow_readers: { peak_gateway_admitted: 40 },
  workload: { stream_share: 1 },
  added_latency_ms: { all: null, unary: null, stream: null },
  throughput: { rps_per_vcpu: 0 },
  resources: { rss_peak_mib: 4096 },
  error_rate: { gateway: 0.0005 },
  request_metadata_completeness: { completeness: 0.99995 },
  targets: [],
  validity: { valid: false, problems: ['the gateway held 40 of 10000 streams'] },
  reference_conditions: { all: false }
};

test('a row shows the headline latency of the mode the scenario exercises', () => {
  const [, , row] = render([s1]).split('\n');
  assert.match(row, /^S1\s+0\.50\s+1\.25\s+3\.10\s+-\s+480\.0\s+1\.23\s+211\s+0\.000\s+100\.000\s+met 1, comparison 2\s+yes\s+yes$/);
});

test('a figure a scenario does not produce is a dash, never zero', () => {
  const [, , , row] = render([s1, s6]).split('\n');
  assert.match(row, /^S6\s+-\s+-\s+-\s+-\s+-\s+-\s+4096\s+0\.050\s+99\.995\s+-\s+NO\s+no$/);
});

test('a streaming scenario reads the stream overhead and a mixed one all requests', () => {
  const streaming = { ...s1, scenario: 'S2', workload: { stream_share: 1 }, added_latency_ms: { stream: { p50: 7, p95: 8, p99: 9 } } };
  const mixed = { ...s1, scenario: 'S3', workload: { stream_share: 0.5 }, added_latency_ms: { all: { p50: 1, p95: 2, p99: 3 } } };
  const rows = render([streaming, mixed]).split('\n');
  assert.match(rows[2], /^S2\s+7\.00\s+8\.00\s+9\.00/);
  assert.match(rows[3], /^S3\s+1\.00\s+2\.00\s+3\.00/);
});

test('caveats name invalid runs, missed targets and what makes a run non-reference', () => {
  const notes = caveats([
    { ...s1, targets: [{ id: 's1-2vcpu-added-p95', status: 'missed' }] },
    s6
  ]);
  assert.deepEqual(notes.slice(0, 2), [
    'S1: target s1-2vcpu-added-p95 missed',
    'S6: the gateway held 40 of 10000 streams'
  ]);
  assert.match(notes.at(-1), /^Not a reference run/);
  assert.deepEqual(caveats([s1]), []);
});

test('the command reads results from a directory and fails when there are none', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bench-summary-'));
  try {
    const script = new URL('./bench-summary.mjs', import.meta.url).pathname;
    const empty = spawnSync('node', [script, dir, 'S1'], { encoding: 'utf8' });
    assert.equal(empty.status, 1);
    assert.match(empty.stdout, /S1: no result at /);
    writeFileSync(join(dir, 's1.json'), JSON.stringify(s1));
    const run = spawnSync('node', [script, dir, 'S1', 'S2'], { encoding: 'utf8' });
    assert.equal(run.status, 0);
    assert.match(run.stdout, /S2: no result at /);
    assert.match(run.stdout, /^S1\s+0\.50/m);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
