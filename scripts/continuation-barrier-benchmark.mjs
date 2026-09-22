#!/usr/bin/env node
import { spawnSync } from 'node:child_process';
import { readFileSync, writeFileSync, existsSync } from 'node:fs';
import { cpus, totalmem, release, arch, platform } from 'node:os';
import { createHash } from 'node:crypto';
import { isDeepStrictEqual } from 'node:util';
import { fileURLToPath } from 'node:url';
import { resolve } from 'node:path';

const schema = 'openllmproxy.dev/continuation-barrier-performance/v1';
const contract = 'native-wire-with-reference-side-encrypted-barrier/1';
export const names = ['small', 'large'].flatMap((size) =>
  [1, 8].flatMap((c) => ['relay', 'gateway', 'reference'].map((path) => `${size}/c${c}/${path}`))
);
const harness = 'tests/integration/continuation_barrier_benchmark_test.go';
const runner = 'scripts/continuation-barrier-benchmark.mjs';
const dependencies = [
  'tests/integration/fidelity_lifecycle_performance_test.go',
  'tests/integration/access_test.go',
  'tests/integration/strict_generation_test.go',
  'tests/fidelity/reference.go',
  'tests/fixtures/fidelity/v1/anthropic-tool-next-request.json',
  'tests/fixtures/fidelity/v1/anthropic-tool-workflow.sse'
];
const args = ['test', '-mod=readonly', '-tags=integration', '-run', '^TestContinuationBarrierReference$', '-count=1', '-v', '-timeout=10m', './tests/integration'];
const runtimeEnvironment = { GOMAXPROCS: '4', GOGC: '100', GOMEMLIMIT: 'off', GODEBUG: '', OLP_BARRIER_MEASURE: '1' };
const conditions = {
  network: 'IPv4 loopback; two warm HTTP/1.1 inference hops; authorized fixed relay destination; no inference TLS',
  authority: 'Publicly provisioned strict Anthropic Messages profile; actual isolated PostgreSQL; API-key state permission; existing KeyRing secret/resource transactions; no distributed quotas',
  workflow: 'Go native-wire client; fixed separately SDK-qualified Anthropic corpus: thinking/signature, text, two tool calls, text; all 19 events and complete second request verified independently; two dispatches and two enabled fixture actions per workflow',
  history: 'Original user turn, or original user turn plus 262144 ASCII history bytes, present in both requests and retained encrypted dependencies',
  reference: 'Reference-side encrypted submission claim, dispatch journal before HTTP, atomic ready payload/state after complete native stream, then independent committed-state decryption before tool action and second request. This is not the production translated carrier.',
  observation: 'wire-tool is first native tool-use start frame. action-ready is complete validated history and, for reference, separately readable encrypted ready state; only action-ready is comparable to a durable translated tool-action barrier.',
  resources: 'Process CPU, allocations and 1ms sampled heap growth include client, provider, gateway/relay, oracle and instrumentation; exclude separate PostgreSQL process; not isolated gateway RSS',
  ordering: 'Fixed size/concurrency/path order; one warmup per repetition; three repetitions of 24 complete workflows; local fixture tool results only',
  scope: 'Native gateway plus an independent minimal persistence reference; no production translated carrier, actual SDK process, recovery fault injection, WAN or live-model quality measurement'
};
const optional = (path) => existsSync(path) ? readFileSync(path, 'utf8').trim() : null;
const hash = (path) => createHash('sha256').update(readFileSync(path)).digest('hex');
const median = (values) => values.toSorted((a, b) => a - b)[Math.floor(values.length / 2)];
function command(name, args) {
  const result = spawnSync(name, args, { encoding: 'utf8', maxBuffer: 16 << 20 });
  if (result.status !== 0) throw new Error(`${name} failed: ${result.stderr}`);
  return result.stdout.trim();
}
export function metrics(name) {
  const required = ['elapsed_ns', 'ns/op', 'process-cpu-ns/op', 'B/op', 'allocs/op', 'sampled-heap-growth-B'];
  const phases = ['workflow', 'first-event', 'wire-tool', 'action-ready'];
  if (name.endsWith('/reference')) phases.push('claim-commit', 'dispatch-journal', 'ready-commit');
  for (const phase of phases) for (const p of [50, 95, 99]) required.push(`${phase}-p${p}-us`);
  return required;
}
export function validateRuns(runs) {
  if (!Array.isArray(runs) || runs.length !== names.length * 3) throw new Error('Complete 36-repetition barrier inventory required');
  const seen = new Set();
  const stateSizes = new Map();
  for (const run of runs) {
    if (!names.includes(run.name) || ![0, 1, 2].includes(run.repetition) || seen.has(`${run.name}/${run.repetition}`)) throw new Error('Unknown or duplicate workload/repetition');
    seen.add(`${run.name}/${run.repetition}`);
    if (run.samples !== 24 || run.first_requests !== 24 || run.next_requests !== 24 || run.dispatches !== 48 || run.events !== 456 || run.actions !== 48) throw new Error('Native workflow coverage changed');
    const large = run.name.startsWith('large/');
    if (run.history_bytes !== (large ? 262144 : 0) || !Number.isSafeInteger(run.state_bytes) || run.state_bytes <= run.history_bytes || run.state_bytes > run.history_bytes + 2048) throw new Error('Retained history bounds changed');
    if (stateSizes.has(large) && stateSizes.get(large) !== run.state_bytes) throw new Error('State size varies across paths or repetitions');
    stateSizes.set(large, run.state_bytes);
    if (!isDeepStrictEqual(Object.keys(run.metrics).sort(), metrics(run.name).sort())) throw new Error('Measured metric inventory changed');
    for (const value of Object.values(run.metrics)) if (!Number.isFinite(value) || value < 0) throw new Error('Invalid measured metric');
    for (const p of [50, 95, 99]) {
      const first = run.metrics[`first-event-p${p}-us`];
      const tool = run.metrics[`wire-tool-p${p}-us`];
      const ready = run.metrics[`action-ready-p${p}-us`];
      const workflow = run.metrics[`workflow-p${p}-us`];
      if (!(first > 0 && first <= tool && tool <= ready && ready <= workflow)) throw new Error('Observation order changed');
    }
  }
  return Object.fromEntries(names.map((name) => [name, Object.fromEntries(metrics(name).map((metric) => {
    const values = runs.filter((r) => r.name === name).map((r) => r.metrics[metric]);
    return [metric, { median: median(values), maximum: Math.max(...values), minimum: Math.min(...values) }];
  }))]));
}
export function parseRuns(output) {
  const marker = 'BARRIER_MEASUREMENT ';
  return output.split('\n').filter((line) => line.includes(marker)).map((line) => JSON.parse(line.slice(line.indexOf(marker) + marker.length)));
}
function record(path) {
  if (existsSync(path)) throw new Error('Refusing to overwrite measured evidence');
  if (command('git', ['status', '--short']) !== '') throw new Error('Commit measurement sources before recording');
  const startedAt = new Date().toISOString();
  const loadBefore = optional('/proc/loadavg');
  const result = spawnSync('go', args, { encoding: 'utf8', maxBuffer: 16 << 20, env: { ...process.env, ...runtimeEnvironment } });
  process.stdout.write(result.stdout ?? '');
  process.stderr.write(result.stderr ?? '');
  if (result.status !== 0) throw new Error('Barrier run failed; no passing artifact written');
  const runs = parseRuns(result.stdout);
  const summary = validateRuns(runs);
  const marker = 'BARRIER_SETUP ';
  const setup = result.stdout.split('\n').find((line) => line.includes(marker));
  if (!setup) throw new Error('Required storage condition missing');
  const artifact = {
    schema, contract, started_at: startedAt, completed_at: new Date().toISOString(),
    source_revision: command('git', ['rev-parse', 'HEAD']), working_tree: command('git', ['status', '--short']),
    harness_sha256: hash(harness), runner_sha256: hash(runner), dependency_sha256: Object.fromEntries(dependencies.map((path) => [path, hash(path)])),
    command: ['go', ...args], runtime_environment: runtimeEnvironment, toolchain: command('go', ['version']),
    go_build_environment: command('go', ['env', 'GOFLAGS', 'GOAMD64', 'GOARCH', 'GOOS']),
    hardware: { os: platform(), architecture: arch(), kernel: release(), cpu: cpus()[0]?.model, logical_cpus: cpus().length, total_memory_bytes: totalmem(), cpu_quota: optional('/sys/fs/cgroup/cpu.max'), memory_limit: optional('/sys/fs/cgroup/memory.max') },
    system_load: { before: loadBefore, after: optional('/proc/loadavg') },
    storage: JSON.parse(setup.slice(setup.indexOf(marker) + marker.length)), conditions,
    repetitions: 3, samples_per_repetition: 24, concurrency: [1, 8], runs, summary,
    unmeasured: ['production translated continuation carrier', 'actual SDK process CPU', 'recovery fault injection', 'WAN/TLS inference', 'isolated gateway RSS', 'live-model quality'], raw_output: result.stdout
  };
  writeFileSync(path, JSON.stringify(artifact, null, 2) + '\n', { flag: 'wx' });
  console.log(`Recorded ${runs.length} encrypted barrier reference repetitions`);
}
function measurement(a) {
  return { contract: a.contract, command: a.command, runtime_environment: a.runtime_environment, toolchain: a.toolchain, go_build_environment: a.go_build_environment, hardware: a.hardware, storage: a.storage, conditions: a.conditions, repetitions: a.repetitions, samples_per_repetition: a.samples_per_repetition, concurrency: a.concurrency, dependency_sha256: a.dependency_sha256, state_bytes: Object.fromEntries(a.runs.map((r) => [r.name, r.state_bytes])) };
}
export function freeze(a) {
  if (a.schema !== schema || a.contract !== contract || a.working_tree !== '') throw new Error('Complete clean independent native reference required');
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
  return { schema: schema.replace('performance/', 'budget/'), declared_at: new Date().toISOString(), baseline_revision: a.source_revision, harness_sha256: a.harness_sha256, runner_sha256: a.runner_sha256, measurement: structuredClone(measurement(a)), method: 'Before translated carrier measurement: maximum of 3 reference repetitions ×1.5 +1ms timing/CPU, +16KiB allocated bytes, +8MiB sampled heap; allocation counts ×1.25 +64. Fixed 24 complete two-turn workflows per repetition. Candidate medians must meet each frozen limit. Never reset criteria to fit a candidate.', maxima };
}
export function compare(a, b) {
  if (a.schema !== schema || b.schema !== schema.replace('performance/', 'budget/')) throw new Error('Unknown evidence schema');
  if (a.harness_sha256 !== b.harness_sha256 || a.runner_sha256 !== b.runner_sha256) throw new Error('Harness or runner changed; reference evidence remains frozen');
  validateRuns(a.runs);
  if (!isDeepStrictEqual(measurement(a), b.measurement)) throw new Error('Measurement conditions changed');
  if (!isDeepStrictEqual(Object.keys(b.maxima).sort(), [...names].sort())) throw new Error('Frozen workload inventory changed');
  const summary = validateRuns(a.runs);
  const failures = [];
  for (const name of names) {
    const required = metrics(name).filter((metric) => metric !== 'elapsed_ns');
    if (!isDeepStrictEqual(Object.keys(b.maxima[name]).sort(), required.sort())) throw new Error('Frozen metric inventory changed');
    for (const metric of required) {
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
    else if (action === 'compare' && path && budget) {
      const failures = compare(JSON.parse(readFileSync(path, 'utf8')), JSON.parse(readFileSync(budget, 'utf8')));
      console.log(JSON.stringify({ passed: failures.length === 0, failures }, null, 2));
      if (failures.length) process.exitCode = 1;
    } else throw new Error('Usage: record <artifact> | freeze <baseline> <budgets> | compare <candidate> <budgets>');
  } catch (error) { console.error(error.message); process.exitCode = 1; }
}
