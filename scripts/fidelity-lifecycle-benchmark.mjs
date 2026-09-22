#!/usr/bin/env node
import { spawnSync } from 'node:child_process';
import { readFileSync, writeFileSync, existsSync } from 'node:fs';
import { cpus, totalmem, release, arch, platform } from 'node:os';
import { createHash } from 'node:crypto';
import { isDeepStrictEqual } from 'node:util';
import { fileURLToPath } from 'node:url';
import { resolve } from 'node:path';

const schema = 'openllmproxy.dev/fidelity-lifecycle-performance/v1';
export const workloads = ['durable_unary', 'durable_stream_64', 'duplex_64', 'duplex_slow_64'];
export const names = workloads.flatMap((w) => [1, 4].flatMap((c) => ['relay', 'gateway'].map((p) => `${w}/c${c}/${p}`)));
const harness = 'tests/integration/fidelity_lifecycle_performance_test.go';
const runner = 'scripts/fidelity-lifecycle-benchmark.mjs';
const args = ['test', '-mod=readonly', '-tags=integration', '-run', '^TestFidelityLifecyclePerformance$', '-count=1', '-v', '-timeout=10m', './tests/integration'];
const runtimeEnvironment = { GOMAXPROCS: '4', GOGC: '100', GOMEMLIMIT: 'off', GODEBUG: '', OLP_LIFECYCLE_MEASURE: '1' };
const conditions = {
  network: 'IPv4 loopback; two HTTP/1.1/WebSocket hops; warm pools; no TLS on measured HTTP hops',
  authority: 'Publicly provisioned Azure v1 Responses profile revision 1; actual isolated PostgreSQL resources; current API-key authority; no distributed quotas',
  resources: 'CPU/allocations and 1ms sampled heap growth include client, scripted provider, gateway/relay, independent fixture validation and instrumentation; exclude separate PostgreSQL process',
  durable: 'Exact original input/store/stream controls; complete 1-message result; stream has created + 64 exact text deltas + complete terminal. Publication delay is provider emission to client-observed ID, including network and mapping. Every gateway ID is checked in committed PostgreSQL state.',
  duplex: '64 exact bidirectional native JSON event exchanges per session; session establishment, audio bytes and order checked; slow-reader variant sleeps 500us before each read (OS timer may be coarser). Event RTT includes client pacing, not a one-way network estimate.',
  cancellation: 'Abrupt client socket close after all exchanges; provider remains reading until closure. Client-to-provider cancellation delay measured independently of session duration.',
  ordering: 'Fixed sequential workload/path/concurrency order; three repetitions and 24 sessions or requests per repetition; no live provider calls',
  scope: 'Existing native durable mapping and duplex overhead, not encrypted translated-tool recoverability or live-model quality'
};
const median = (xs) => xs.toSorted((a, b) => a - b)[Math.floor(xs.length / 2)];
const optional = (path) => existsSync(path) ? readFileSync(path, 'utf8').trim() : null;
const hash = (path) => createHash('sha256').update(readFileSync(path)).digest('hex');
function command(name, args) {
  const r = spawnSync(name, args, { encoding: 'utf8', maxBuffer: 16 << 20 });
  if (r.status !== 0) throw new Error(`${name} failed: ${r.stderr}`);
  return r.stdout.trim();
}
export function validateRuns(runs) {
  if (!Array.isArray(runs) || runs.length !== names.length * 3) throw new Error('Complete 48-repetition lifecycle inventory required');
  const seen = new Set();
  for (const run of runs) {
    if (!names.includes(run.name) || ![0, 1, 2].includes(run.repetition) || seen.has(`${run.name}/${run.repetition}`)) throw new Error('Unknown or duplicate workload/repetition');
    seen.add(`${run.name}/${run.repetition}`);
    if (run.samples !== 24 || run.succeeded !== 24 || run.dispatches !== 24) throw new Error('Success, sample or dispatch coverage changed');
    const durable = run.name.startsWith('durable');
    const expectedEvents = run.name.startsWith('durable_unary') ? 0 : run.name.startsWith('durable_stream') ? 66 : 64;
    if (run.events_per_request !== expectedEvents) throw new Error('Event conservation coverage changed');
    const required = ['elapsed_ns', 'ns/op', 'process-cpu-ns/op', 'B/op', 'allocs/op', 'sampled-heap-growth-B'];
    for (const category of ['latency', ...(durable ? ['publication'] : ['cancellation', 'event-roundtrip'])]) {
      for (const p of [50, 95, 99]) required.push(`${category}-p${p}-us`);
    }
    if (!isDeepStrictEqual(Object.keys(run.metrics).sort(), required.sort())) throw new Error('Measured metric inventory changed');
    for (const value of Object.values(run.metrics)) if (!Number.isFinite(value) || value < 0) throw new Error('Invalid measured metric');
  }
  return Object.fromEntries(names.map((name) => {
    const matching = runs.filter((r) => r.name === name);
    return [name, Object.fromEntries(Object.keys(matching[0].metrics).map((metric) => [metric, {
      median: median(matching.map((r) => r.metrics[metric])),
      maximum: Math.max(...matching.map((r) => r.metrics[metric])),
      minimum: Math.min(...matching.map((r) => r.metrics[metric]))
    }]))];
  }));
}
export function parseRuns(output) {
  return output.split('\n').filter((line) => line.includes('LIFECYCLE_MEASUREMENT ')).map((line) => JSON.parse(line.slice(line.indexOf('LIFECYCLE_MEASUREMENT ') + 22)));
}
function record(path) {
  if (existsSync(path)) throw new Error('Refusing to overwrite measured evidence');
  const contract = process.env.OLP_LIFECYCLE_ROUTE_FIDELITY ? JSON.parse(process.env.OLP_LIFECYCLE_ROUTE_FIDELITY) : null;
  const startedAt = new Date().toISOString();
  const loadBefore = optional('/proc/loadavg');
  const result = spawnSync('go', args, { encoding: 'utf8', maxBuffer: 16 << 20, env: { ...process.env, ...runtimeEnvironment } });
  process.stdout.write(result.stdout ?? ''); process.stderr.write(result.stderr ?? '');
  if (result.status !== 0) throw new Error('Lifecycle run failed; no passing artifact written');
  const runs = parseRuns(result.stdout);
  const summary = validateRuns(runs);
  const setup = result.stdout.split('\n').find((line) => line.includes('LIFECYCLE_SETUP '));
  if (!setup) throw new Error('Required storage condition missing');
  const artifact = {
    schema, contract, started_at: startedAt, completed_at: new Date().toISOString(),
    source_revision: command('git', ['rev-parse', 'HEAD']), working_tree: command('git', ['status', '--short']),
    harness_sha256: hash(harness), runner_sha256: hash(runner), setup_sha256: hash('tests/integration/access_test.go'),
    command: ['go', ...args], runtime_environment: runtimeEnvironment, toolchain: command('go', ['version']),
    go_build_environment: command('go', ['env', 'GOFLAGS', 'GOAMD64', 'GOARCH', 'GOOS']),
    hardware: { os: platform(), architecture: arch(), kernel: release(), cpu: cpus()[0]?.model, logical_cpus: cpus().length, total_memory_bytes: totalmem(), cpu_quota: optional('/sys/fs/cgroup/cpu.max'), memory_limit: optional('/sys/fs/cgroup/memory.max') },
    system_load: { before: loadBefore, after: optional('/proc/loadavg') },
    storage: JSON.parse(setup.slice(setup.indexOf('LIFECYCLE_SETUP ') + 16)),
    conditions, repetitions: 3, samples_per_repetition: 24, concurrency: [1, 4], runs, summary,
    unmeasured: ['encrypted translated-tool state barrier', 'WAN/TLS on inference hops', 'isolated gateway RSS', 'live-model quality'], raw_output: result.stdout
  };
  writeFileSync(path, JSON.stringify(artifact, null, 2) + '\n', { flag: 'wx' });
  console.log(`Recorded ${runs.length} lifecycle repetitions`);
}
function measurement(a) {
  return { command: a.command, runtime_environment: a.runtime_environment, toolchain: a.toolchain, go_build_environment: a.go_build_environment, hardware: a.hardware, storage: a.storage, conditions: a.conditions, repetitions: a.repetitions, samples_per_repetition: a.samples_per_repetition, concurrency: a.concurrency };
}
export function freeze(a) {
  if (a.schema !== schema || a.contract !== null || a.working_tree !== '') throw new Error('Complete clean legacy baseline required before freezing');
  const summary = validateRuns(a.runs);
  const maxima = Object.fromEntries(names.map((name) => [name, Object.fromEntries(Object.entries(summary[name]).filter(([metric]) => metric !== 'elapsed_ns').map(([metric, values]) => {
    let limit = values.maximum * 1.5;
    if (metric.endsWith('-us')) limit += 1000;
    if (metric === 'ns/op' || metric === 'process-cpu-ns/op') limit += 1e6;
    if (metric === 'B/op') limit += 16384;
    if (metric === 'sampled-heap-growth-B') limit += 8 << 20;
    if (metric === 'allocs/op') limit = values.maximum * 1.25 + 64;
    return [metric, Math.ceil(limit)];
  }))]));
  return { schema: schema.replace('performance/', 'budget/'), declared_at: new Date().toISOString(), baseline_revision: a.source_revision, harness_sha256: a.harness_sha256, runner_sha256: a.runner_sha256, measurement: structuredClone(measurement(a)), method: 'Before lifecycle replacement: maximum of 3 native-baseline repetitions ×1.5 +1ms timing/CPU, +16KiB allocated bytes, +8MiB sampled heap; allocation counts ×1.25 +64. Fixed 24 successes per repetition, all provider dispatches and exact events required. Never reset budgets to fit candidates.', maxima };
}
export function compare(a, b, mode = 'legacy') {
  if (a.schema !== schema || b.schema !== schema.replace('performance/', 'budget/')) throw new Error('Unknown evidence schema');
  if (a.harness_sha256 !== b.harness_sha256 || a.runner_sha256 !== b.runner_sha256) throw new Error('Harness or runner changed; original evidence must remain frozen');
  if (!isDeepStrictEqual(measurement(a), b.measurement)) throw new Error('Measurement conditions changed');
  if (mode === 'legacy' ? a.contract !== null : !isDeepStrictEqual(a.contract, { mode: 'strict' })) throw new Error('Requested fidelity contract was not measured');
  if (!isDeepStrictEqual(Object.keys(b.maxima).sort(), [...names].sort())) throw new Error('Frozen workload inventory changed');
  const summary = validateRuns(a.runs); const failures = [];
  for (const name of names) {
    const metrics = Object.keys(summary[name]).filter((metric) => metric !== 'elapsed_ns');
    if (!isDeepStrictEqual(Object.keys(b.maxima[name]).sort(), metrics.sort())) throw new Error('Frozen metric inventory changed');
    for (const metric of metrics) {
      const limit = b.maxima[name][metric];
      if (!Number.isFinite(limit) || limit < 0) throw new Error('Invalid frozen limit');
      if (summary[name][metric].median > limit) failures.push(`${name}: ${metric} ${summary[name][metric].median} > ${limit}`);
    }
  }
  return failures;
}
if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const [action, path, budget] = process.argv.slice(2);
    if (action === 'record' && path && !budget) record(path);
    else if (action === 'freeze' && path && budget) writeFileSync(budget, JSON.stringify(freeze(JSON.parse(readFileSync(path, 'utf8'))), null, 2) + '\n', { flag: 'wx' });
    else if (['compare', 'compare-strict'].includes(action) && path && budget) {
      const failures = compare(JSON.parse(readFileSync(path, 'utf8')), JSON.parse(readFileSync(budget, 'utf8')), action === 'compare-strict' ? 'strict' : 'legacy');
      console.log(JSON.stringify({ passed: failures.length === 0, failures }, null, 2)); if (failures.length) process.exitCode = 1;
    } else throw new Error('Usage: record <artifact> | freeze <baseline> <budgets> | compare[-strict] <candidate> <budgets>');
  } catch (error) { console.error(error.message); process.exitCode = 1; }
}
