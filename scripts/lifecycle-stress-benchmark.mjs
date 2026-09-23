#!/usr/bin/env node
import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { existsSync, readFileSync, writeFileSync } from 'node:fs';
import { arch, cpus, platform, release, totalmem } from 'node:os';
import { resolve } from 'node:path';
import { isDeepStrictEqual } from 'node:util';
import { fileURLToPath } from 'node:url';

const schema = 'openllmproxy.dev/lifecycle-stress-performance/v1';
const budgetSchema = 'openllmproxy.dev/lifecycle-stress-budget/v1';
export const workloads = ['media_cycle_4m', 'media_slow_content_1m', 'duplex_jitter_64'];
export const concurrencies = (workload) => workload === 'duplex_jitter_64' ? [1, 4] : [1, 2];
export const names = workloads.flatMap((workload) =>
  concurrencies(workload).flatMap((concurrency) =>
    ['relay', 'gateway'].map((path) => `${workload}/c${concurrency}/${path}`)
  )
);
const repetitions = 3;
const samples = 8;
const events = 64;
const inputBytes = 4 << 20;
const contentBytes = 1 << 20;
const harness = 'tests/integration/lifecycle_stress_performance_test.go';
const fixture = 'tests/integration/fidelity_lifecycle_performance_test.go';
const runner = 'scripts/lifecycle-stress-benchmark.mjs';
const commandArgs = [
  'test', '-mod=readonly', '-tags=integration', '-run', '^TestLifecycleStressPerformance$',
  '-count=1', '-v', '-timeout=15m', './tests/integration'
];
const environment = {
  GOMAXPROCS: '4', GOGC: '100', GOMEMLIMIT: 'off', GODEBUG: '', OLP_LIFECYCLE_STRESS_MEASURE: '1'
};
const conditions = {
  network: 'IPv4 loopback, two HTTP/1.1 or WebSocket hops, warmed connections, no TLS on measured hops',
  authority: 'Publicly configured legacy Azure-v1 Responses realtime and video jobs; real isolated PostgreSQL, local scripted provider and bounded media spool. Concurrent 4MiB parsers use distinct equally authorized API keys because one API key may hold only one parser lease.',
  media: '4 MiB original image input reference; accepted video creation, one 50% partial status, one complete status, exact 1 MiB content retrieval. Slow reader delays 250us after each 8 KiB read.',
  duplex: '64 exact native JSON/audio exchanges with independent RTT and successive absolute RTT-difference jitter measurements; provider observes client closure',
  resources: 'Process CPU, allocations and sampled heap include client, fixture, gateway or relay and checks; PostgreSQL is separate. Spool high water and final reservation are observed from the public harness.',
  ordering: 'Fixed sequential workload/path/concurrency order, three repetitions, eight successful workflows each; negative controls run outside timed samples',
  scope: 'Scripted native transport and durable media lifecycle; not live-model quality, WAN, encrypted translated-tool state, or partial batch item errors'
};

const median = (values) => values.toSorted((a, b) => a - b)[Math.floor(values.length / 2)];
const optional = (path) => existsSync(path) ? readFileSync(path, 'utf8').trim() : null;
const sha256 = (path) => createHash('sha256').update(readFileSync(path)).digest('hex');
function git(...args) {
  const result = spawnSync('git', args, { encoding: 'utf8' });
  if (result.status !== 0) throw new Error(`git ${args.join(' ')} failed: ${result.stderr}`);
  return result.stdout.trim();
}
function workloadFor(name) { return name.split('/')[0]; }
function expected(name) {
  const workload = workloadFor(name);
  if (workload === 'media_cycle_4m') return {
    dispatches: 4 * samples, accepted: samples, partial: samples, retrieved: 2 * samples,
    content: samples, events: 0, rtt_observations: 0, jitter_observations: 0, uploaded_bytes: inputBytes * samples,
    downloaded_bytes: contentBytes * samples
  };
  if (workload === 'media_slow_content_1m') return {
    dispatches: samples, accepted: 0, partial: 0, retrieved: samples,
    content: samples, events: 0, rtt_observations: 0, jitter_observations: 0, uploaded_bytes: 0,
    downloaded_bytes: contentBytes * samples
  };
  return {
    dispatches: samples, accepted: 0, partial: 0, retrieved: 0,
    content: 0, events: events * samples, rtt_observations: events * samples,
    jitter_observations: (events - 1) * samples, uploaded_bytes: 0, downloaded_bytes: 0
  };
}
function metricNames(name) {
  const metrics = ['elapsed_ns', 'ns/op', 'process-cpu-ns/op', 'B/op', 'allocs/op', 'sampled-heap-growth-B', 'sampled-spool-peak-B', 'spool-final-B'];
  for (const category of ['latency', ...(workloadFor(name) === 'duplex_jitter_64' ? ['event-rtt', 'event-jitter', 'cancellation'] : ['first-byte', 'inter-read-gap'])]) {
    for (const percentile of [50, 95, 99]) metrics.push(`${category}-p${percentile}-us`);
  }
  return metrics.sort();
}
export function validateRuns(runs) {
  if (!Array.isArray(runs) || runs.length !== names.length * repetitions) throw new Error('Complete 36-repetition lifecycle stress inventory required');
  const seen = new Set();
  for (const run of runs) {
    if (!names.includes(run.name) || ![0, 1, 2].includes(run.repetition) || seen.has(`${run.name}/${run.repetition}`)) throw new Error('Unknown or duplicate workload/repetition');
    seen.add(`${run.name}/${run.repetition}`);
    if (run.samples !== samples || run.succeeded !== samples || run.rejected !== 0 || run.incomplete !== 0 || run.ambiguous !== 0) throw new Error('Successful/rejected/incomplete/ambiguous coverage changed');
    for (const [key, value] of Object.entries(expected(run.name))) if (run[key] !== value) throw new Error(`${run.name}: ${key} conservation changed`);
    if (!run.metrics || !isDeepStrictEqual(Object.keys(run.metrics).sort(), metricNames(run.name))) throw new Error('Metric inventory changed');
    for (const value of Object.values(run.metrics)) if (!Number.isFinite(value) || value < 0) throw new Error('Invalid measurement');
    if (run.metrics['spool-final-B'] !== 0) throw new Error('Spool reservation survived completed workflow');
  }
  return Object.fromEntries(names.map((name) => {
    const group = runs.filter((run) => run.name === name);
    return [name, Object.fromEntries(metricNames(name).map((metric) => [metric, {
      median: median(group.map((run) => run.metrics[metric])),
      maximum: Math.max(...group.map((run) => run.metrics[metric])),
      minimum: Math.min(...group.map((run) => run.metrics[metric]))
    }]))];
  }));
}
export function validateNegatives(negatives) {
  if (!Array.isArray(negatives) || negatives.length !== 3) throw new Error('All gateway media negative controls are required');
  const expectedControls = ['cancelled_upload', 'oversize_upload', 'parser_contention'];
  if (!isDeepStrictEqual(negatives.map((row) => row.name).sort(), expectedControls)) throw new Error('Negative-control inventory changed');
  for (const row of negatives) {
    if (row.path !== 'gateway' || row.admitted !== 0 || row.rejected !== 1 || row.provider_dispatches !== 0 || row.spool_final_bytes !== 0) throw new Error('Negative control was dropped, dispatched, or leaked spool bytes');
  }
  return true;
}
export function parse(output, marker) {
  return output.split('\n').filter((line) => line.includes(marker)).map((line) => JSON.parse(line.slice(line.indexOf(marker) + marker.length)));
}
function identity(artifact) {
  return {
    command: artifact.command, runtime_environment: artifact.runtime_environment,
    toolchain: artifact.toolchain, go_build_environment: artifact.go_build_environment,
    hardware: artifact.hardware, storage: artifact.storage, conditions: artifact.conditions,
    repetitions: artifact.repetitions, samples_per_repetition: artifact.samples_per_repetition,
    concurrency: artifact.concurrency
  };
}
function limit(metric, maximum) {
  let value = maximum * 1.5;
  if (metric.endsWith('-us')) value += 1000;
  if (metric === 'ns/op' || metric === 'process-cpu-ns/op') value += 1e6;
  if (metric === 'B/op') value += 16384;
  if (metric === 'sampled-heap-growth-B') value += 8 << 20;
  if (metric === 'sampled-spool-peak-B') value += 1 << 20;
  if (metric === 'allocs/op') value = maximum * 1.25 + 64;
  return Math.ceil(value);
}
export function freeze(artifact) {
  if (artifact.schema !== schema || artifact.contract !== null || artifact.working_tree !== '') throw new Error('Clean, complete legacy reference required before freeze');
  const summary = validateRuns(artifact.runs);
  validateNegatives(artifact.negatives);
  const maxima = Object.fromEntries(names.map((name) => [name, Object.fromEntries(
    Object.entries(summary[name]).filter(([metric]) => metric !== 'elapsed_ns').map(([metric, values]) => [metric, limit(metric, values.maximum)])
  )]));
  const addedLatency = Object.fromEntries(workloads.flatMap((workload) => concurrencies(workload).map((concurrency) => {
    const label = `${workload}/c${concurrency}`;
    const gateway = summary[`${label}/gateway`];
    const relay = summary[`${label}/relay`];
    return [label, Object.fromEntries([50, 95, 99].map((p) => {
      const metric = `latency-p${p}-us`;
      return [metric, Math.ceil(Math.max(0, gateway[metric].maximum - relay[metric].minimum) * 1.5 + 1000)];
    }))];
  })));
  return {
    schema: budgetSchema, declared_at: new Date().toISOString(), baseline_revision: artifact.source_revision,
    harness_sha256: artifact.harness_sha256, fixture_sha256: artifact.fixture_sha256,
    runner_sha256: artifact.runner_sha256, measurement: structuredClone(identity(artifact)),
    method: 'Pre-replacement: per-workload maximum of three native repetitions ×1.5 plus 1ms timing/CPU, 16KiB allocated bytes, 8MiB sampled heap, 1MiB sampled spool; allocations ×1.25 +64. Added latency is gateway minus relay with the same frozen path/concurrency, max positive baseline delta ×1.5 +1ms. All 8 successes, provider dispatches, partial statuses, media bytes, duplex events and negative controls required.',
    maxima, added_latency_maxima: addedLatency
  };
}
export function compare(artifact, budget, mode = 'legacy') {
  if (artifact.schema !== schema || budget.schema !== budgetSchema) throw new Error('Unknown evidence schema');
  if (artifact.harness_sha256 !== budget.harness_sha256 || artifact.fixture_sha256 !== budget.fixture_sha256 || artifact.runner_sha256 !== budget.runner_sha256) throw new Error('Harness, fixture or runner changed');
  if (!isDeepStrictEqual(identity(artifact), budget.measurement)) throw new Error('Measurement conditions changed');
  if (mode === 'legacy' ? artifact.contract !== null : !isDeepStrictEqual(artifact.contract, { mode: 'strict' })) throw new Error('Requested fidelity contract was not measured');
  const summary = validateRuns(artifact.runs);
  validateNegatives(artifact.negatives);
  if (!isDeepStrictEqual(Object.keys(budget.maxima).sort(), [...names].sort())) throw new Error('Budget workload inventory changed');
  const failures = [];
  for (const name of names) {
    const metrics = metricNames(name).filter((metric) => metric !== 'elapsed_ns');
    if (!isDeepStrictEqual(Object.keys(budget.maxima[name]).sort(), metrics)) throw new Error('Budget metric inventory changed');
    for (const metric of metrics) {
      const bound = budget.maxima[name][metric];
      if (!Number.isFinite(bound) || bound < 0) throw new Error('Invalid budget');
      if (summary[name][metric].median > bound) failures.push(`${name}: ${metric} ${summary[name][metric].median} > ${bound}`);
    }
  }
  const expectedAdded = workloads.flatMap((workload) => concurrencies(workload).map((concurrency) => `${workload}/c${concurrency}`));
  if (!isDeepStrictEqual(Object.keys(budget.added_latency_maxima).sort(), expectedAdded.sort())) throw new Error('Added-latency budget inventory changed');
  for (const label of expectedAdded) {
    if (!isDeepStrictEqual(Object.keys(budget.added_latency_maxima[label]).sort(), [50, 95, 99].map((p) => `latency-p${p}-us`).sort())) throw new Error('Added-latency metric inventory changed');
    for (const p of [50, 95, 99]) {
      const metric = `latency-p${p}-us`;
      const bound = budget.added_latency_maxima[label][metric];
      if (!Number.isFinite(bound) || bound < 0) throw new Error('Invalid added-latency budget');
      const added = Math.max(0, summary[`${label}/gateway`][metric].median - summary[`${label}/relay`][metric].median);
      if (added > bound) failures.push(`${label}: added ${metric} ${added} > ${bound}`);
    }
  }
  return failures;
}

function record(path) {
  if (existsSync(path)) throw new Error('Refusing to overwrite evidence');
  const contract = process.env.OLP_LIFECYCLE_STRESS_ROUTE_FIDELITY ? JSON.parse(process.env.OLP_LIFECYCLE_STRESS_ROUTE_FIDELITY) : null;
  if (contract !== null && !isDeepStrictEqual(contract, { mode: 'strict' })) throw new Error('Candidate route fidelity must be exactly strict');
  const started = new Date().toISOString();
  const loadBefore = optional('/proc/loadavg');
  const result = spawnSync('go', commandArgs, { encoding: 'utf8', maxBuffer: 32 << 20, env: { ...process.env, ...environment, OLP_LIFECYCLE_ROUTE_FIDELITY: JSON.stringify(contract ?? { mode: 'legacy' }) } });
  process.stdout.write(result.stdout ?? '');
  process.stderr.write(result.stderr ?? '');
  if (result.status !== 0) throw new Error('Lifecycle stress run failed; no passing artifact written');
  const runs = parse(result.stdout, 'LIFECYCLE_STRESS_MEASUREMENT ');
  const negatives = parse(result.stdout, 'LIFECYCLE_STRESS_NEGATIVE ');
  const setup = parse(result.stdout, 'LIFECYCLE_STRESS_SETUP ');
  const summary = validateRuns(runs);
  validateNegatives(negatives);
  if (setup.length !== 1) throw new Error('Exactly one required storage setup record expected');
  const artifact = {
    schema, contract, started_at: started, completed_at: new Date().toISOString(),
    source_revision: git('rev-parse', 'HEAD'), working_tree: git('status', '--short'),
    harness_sha256: sha256(harness), fixture_sha256: sha256(fixture), runner_sha256: sha256(runner),
    command: ['go', ...commandArgs], runtime_environment: environment,
    toolchain: spawnSync('go', ['version'], { encoding: 'utf8' }).stdout.trim(),
    go_build_environment: spawnSync('go', ['env', 'GOFLAGS', 'GOAMD64', 'GOARCH', 'GOOS'], { encoding: 'utf8' }).stdout.trim(),
    hardware: { os: platform(), architecture: arch(), kernel: release(), cpu: cpus()[0]?.model, logical_cpus: cpus().length, total_memory_bytes: totalmem(), cpu_quota: optional('/sys/fs/cgroup/cpu.max'), memory_limit: optional('/sys/fs/cgroup/memory.max') },
    system_load: { before: loadBefore, after: optional('/proc/loadavg') }, storage: setup[0], conditions,
    repetitions, samples_per_repetition: samples, concurrency: { media: [1, 2], duplex: [1, 4] }, runs, negatives, summary,
    unmeasured: ['live-model quality', 'WAN/TLS inference hops', 'isolated gateway RSS', 'partial batch item errors', 'encrypted translated-tool state barrier'],
    raw_output: result.stdout
  };
  writeFileSync(path, JSON.stringify(artifact, null, 2) + '\n', { flag: 'wx' });
  console.log(`Recorded ${runs.length} lifecycle stress repetitions`);
}
if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const [action, path, budget] = process.argv.slice(2);
    if (action === 'record' && path && !budget) record(path);
    else if (action === 'freeze' && path && budget) writeFileSync(budget, JSON.stringify(freeze(JSON.parse(readFileSync(path, 'utf8'))), null, 2) + '\n', { flag: 'wx' });
    else if (['compare', 'compare-strict'].includes(action) && path && budget) {
      const failures = compare(JSON.parse(readFileSync(path, 'utf8')), JSON.parse(readFileSync(budget, 'utf8')), action === 'compare-strict' ? 'strict' : 'legacy');
      console.log(JSON.stringify({ passed: failures.length === 0, failures }, null, 2));
      if (failures.length) process.exitCode = 1;
    } else throw new Error('Usage: record <artifact> | freeze <baseline> <budget> | compare[-strict] <candidate> <budget>');
  } catch (error) { console.error(error.message); process.exitCode = 1; }
}
