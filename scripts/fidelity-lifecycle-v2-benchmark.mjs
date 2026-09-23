#!/usr/bin/env node
import { spawnSync } from 'node:child_process';
import { readFileSync, writeFileSync, existsSync } from 'node:fs';
import { cpus, totalmem, release, arch, platform } from 'node:os';
import { createHash } from 'node:crypto';
import { isDeepStrictEqual } from 'node:util';
import { fileURLToPath } from 'node:url';
import { resolve } from 'node:path';
import { names, validateRuns } from './fidelity-lifecycle-benchmark.mjs';

const schema = 'openllmproxy.dev/fidelity-lifecycle-performance/v2';
const budgetSchema = 'openllmproxy.dev/fidelity-lifecycle-budget/v2';
const historicalProduct = '033c611f'; // pre-#216 source with frozen v1 evidence
const historicalBaseline = 'docs/evidence/fidelity-performance/lifecycle-v1/baseline.json';
const historicalBudget = 'docs/evidence/fidelity-performance/lifecycle-v1/replacement-budgets.json';
const harness = 'tests/integration/fidelity_lifecycle_v2_test.go';
const frozenHarness = 'tests/integration/fidelity_lifecycle_performance_test.go';
const runner = 'scripts/fidelity-lifecycle-v2-benchmark.mjs';
const args = ['test', '-mod=readonly', '-tags=integration', '-run', '^TestFidelityLifecycleV2Performance$', '-count=1', '-v', '-timeout=10m', './tests/integration'];
const runtimeEnvironment = { GOMAXPROCS: '4', GOGC: '100', GOMEMLIMIT: 'off', GODEBUG: '', OLP_LIFECYCLE_V2_MEASURE: '1' };
const conditions = {
  network: 'IPv4 loopback; two HTTP/1.1/WebSocket hops; warm pools; no TLS on measured HTTP hops',
  authority: 'Publicly provisioned Azure v1 Responses profile revision 1; isolated PostgreSQL resources; owner-scoped stateful API key and separate valid cross-owner key; no distributed quotas',
  resources: 'CPU/allocations and 1ms sampled heap growth include client, scripted provider, gateway/relay, independent fixture validation and instrumentation; exclude separate PostgreSQL process; retrieval and cross-owner checks occur after measured interval',
  durable: 'Exact original ordered input/store/stream controls; complete 1-message result; stream has created + 64 exact text deltas + complete terminal. Publication is native provider emission to client-observed qualified ID before SQL validation. Every gateway ID checks kind, owner, route, native upstream mapping and strict ciphertext where applicable, then exactly one public retrieval outside timings.',
  duplex: '64 exact bidirectional native JSON event exchanges per session; session establishment, audio bytes and order checked; slow-reader variant sleeps 500us before each read. Event RTT includes client pacing.',
  cancellation: 'Abrupt client socket close after all exchanges; provider remains reading until closure. Client-to-provider cancellation delay measured independently of session duration.',
  ordering: 'Fixed sequential workload/path/concurrency order; three repetitions and 24 sessions or requests per repetition; one checked warmup per repetition; no live provider calls',
  scope: 'Native durable/duplex local scripted performance and strict qualified-ID preservation, not empirical model quality or translated-tool recoverability'
};
const command = (name, args) => {
  const r = spawnSync(name, args, { encoding: 'utf8', maxBuffer: 32 << 20 });
  if (r.status !== 0) throw new Error(`${name} failed: ${r.stderr}`);
  return r.stdout.trim();
};
const optional = (path) => existsSync(path) ? readFileSync(path, 'utf8').trim() : null;
const hash = (path) => createHash('sha256').update(readFileSync(path)).digest('hex');
const median = (values) => values.toSorted((a, b) => a - b)[Math.floor(values.length / 2)];

export function parseRuns(output) {
  return output.split('\n').filter((line) => line.includes('LIFECYCLE_V2_MEASUREMENT ')).map((line) => JSON.parse(line.slice(line.indexOf('LIFECYCLE_V2_MEASUREMENT ') + 25)));
}
export function validateV2Runs(runs) {
  const summary = validateRuns(runs);
  for (const run of runs) {
    const gatewayDurable = run.name.endsWith('/gateway') && run.name.startsWith('durable_');
    if (run.mapping_checks !== (gatewayDurable ? 24 : 0) || run.retrieval_checks !== (gatewayDurable ? 24 : 0) ||
        run.negative_controls !== (gatewayDurable ? 1 : 0) || run.negative_dispatches !== 0 || run.ambiguous_outcomes !== 0) {
      throw new Error(`Native ID, retrieval, rejection or ambiguity coverage changed: ${run.name}`);
    }
  }
  return summary;
}
function measurement(a) {
  return { command: a.command, runtime_environment: a.runtime_environment, toolchain: a.toolchain,
    go_build_environment: a.go_build_environment, hardware: a.hardware, storage: a.storage,
    conditions: a.conditions, repetitions: a.repetitions, samples_per_repetition: a.samples_per_repetition,
    concurrency: a.concurrency };
}
function referenceProductDiff() {
  return command('git', ['diff', '--name-only', historicalProduct, 'HEAD']).split('\n').filter(Boolean).sort();
}
function record(path, strict, budgetPath) {
  if (existsSync(path)) throw new Error('Refusing to overwrite measured evidence');
  if (strict) {
    if (!budgetPath || !existsSync(budgetPath)) throw new Error('Committed B-only v2 budget required before strict candidate measurement');
    const tracked = command('git', ['ls-files', '--error-unmatch', budgetPath]);
    if (!tracked || command('git', ['status', '--short', '--', budgetPath]) !== '') throw new Error('B-only v2 budget must be committed and clean');
    const budget = JSON.parse(readFileSync(budgetPath, 'utf8'));
    if (budget.schema !== budgetSchema || budget.baseline.contract !== null || !budget.baseline.source_revision || budget.harness_sha256 !== hash(harness) || budget.runner_sha256 !== hash(runner)) throw new Error('Unfrozen or mismatched B-only v2 budget');
    if (process.env.OLP_LIFECYCLE_ROUTE_FIDELITY !== '{"mode":"strict"}') throw new Error('Explicit strict route fidelity required');
  } else if (budgetPath || process.env.OLP_LIFECYCLE_ROUTE_FIDELITY) {
    throw new Error('Historical B reference must use the legacy route and no candidate budget');
  }
  const startedAt = new Date().toISOString();
  const loadBefore = optional('/proc/loadavg');
  const result = spawnSync('go', args, { encoding: 'utf8', maxBuffer: 32 << 20, env: { ...process.env, ...runtimeEnvironment } });
  process.stdout.write(result.stdout ?? ''); process.stderr.write(result.stderr ?? '');
  if (result.status !== 0) throw new Error('Lifecycle-v2 run failed; no passing artifact written');
  const runs = parseRuns(result.stdout);
  const summary = validateV2Runs(runs);
  const setup = result.stdout.split('\n').find((line) => line.includes('LIFECYCLE_V2_SETUP '));
  if (!setup) throw new Error('Required storage condition missing');
  const artifact = {
    schema, contract: strict ? { mode: 'strict' } : null, started_at: startedAt, completed_at: new Date().toISOString(),
    source_revision: command('git', ['rev-parse', 'HEAD']), working_tree: command('git', ['status', '--short']),
    reference_product_diff: strict ? null : referenceProductDiff(),
    harness_sha256: hash(harness), frozen_harness_sha256: hash(frozenHarness), runner_sha256: hash(runner),
    historical_baseline_sha256: hash(historicalBaseline), historical_budget_sha256: hash(historicalBudget),
    setup_sha256: hash('tests/integration/access_test.go'), command: ['go', ...args],
    runtime_environment: runtimeEnvironment, toolchain: command('go', ['version']),
    go_build_environment: command('go', ['env', 'GOFLAGS', 'GOAMD64', 'GOARCH', 'GOOS']),
    hardware: { os: platform(), architecture: arch(), kernel: release(), cpu: cpus()[0]?.model, logical_cpus: cpus().length, total_memory_bytes: totalmem(), cpu_quota: optional('/sys/fs/cgroup/cpu.max'), memory_limit: optional('/sys/fs/cgroup/memory.max') },
    system_load: { before: loadBefore, after: optional('/proc/loadavg') },
    storage: JSON.parse(setup.slice(setup.indexOf('LIFECYCLE_V2_SETUP ') + 19)), conditions,
    repetitions: 3, samples_per_repetition: 24, concurrency: [1, 4], runs, summary,
    counts: { repetitions: runs.length, successes: runs.reduce((n, r) => n + r.succeeded, 0), provider_dispatches: runs.reduce((n, r) => n + r.dispatches, 0),
      mapping_checks: runs.reduce((n, r) => n + r.mapping_checks, 0), retrieval_checks: runs.reduce((n, r) => n + r.retrieval_checks, 0), negative_controls: runs.reduce((n, r) => n + r.negative_controls, 0), negative_dispatches: 0, ambiguous_outcomes: 0 },
    unmeasured: ['encrypted translated-tool state barrier', 'WAN/TLS inference hops', 'isolated gateway RSS', 'live-model quality'], raw_output: result.stdout
  };
  writeFileSync(path, JSON.stringify(artifact, null, 2) + '\n', { flag: 'wx' });
  console.log(`Recorded ${runs.length} lifecycle-v2 repetitions`);
}
export function freeze(baseline, historical) {
  if (baseline.schema !== schema || baseline.contract !== null || baseline.working_tree !== '' ||
      historical.schema !== 'openllmproxy.dev/fidelity-lifecycle-budget/v1' || historical.baseline_revision !== '373c58467a3a17da97d2b48b16473ffd715afeac') {
    throw new Error('Clean historical pre-#216 native reference required');
  }
  if (baseline.historical_baseline_sha256 !== hash(historicalBaseline) || baseline.historical_budget_sha256 !== hash(historicalBudget) ||
      baseline.frozen_harness_sha256 !== historical.harness_sha256) throw new Error('Original pre-change lifecycle anchors changed');
  const allowed = [harness, runner, 'scripts/fidelity-lifecycle-v2-benchmark.test.mjs', 'docs/evidence/fidelity-performance/lifecycle-v2/README.md'];
  if (!baseline.reference_product_diff.every((path) => allowed.includes(path)) || !baseline.reference_product_diff.includes(harness) || !baseline.reference_product_diff.includes(runner)) {
    throw new Error('Historical B product source differs from pre-#216 revision');
  }
  const summary = validateV2Runs(baseline.runs);
  if (!isDeepStrictEqual(Object.keys(historical.maxima).sort(), [...names].sort())) throw new Error('Frozen v1 workload inventory changed');
  const failures = [];
  for (const name of names) {
    const expectedMetrics = Object.keys(summary[name]).filter((metric) => metric !== 'elapsed_ns').sort();
    if (!isDeepStrictEqual(Object.keys(historical.maxima[name]).sort(), expectedMetrics)) throw new Error('Original frozen metric inventory changed');
    for (const [metric, limit] of Object.entries(historical.maxima[name])) {
      if (!Number.isFinite(limit) || limit < 0 || !summary[name][metric] || summary[name][metric].median > limit) failures.push(`${name}: B ${metric} exceeds original frozen native limit`);
    }
  }
  if (failures.length) throw new Error(`Historical B envelope failed: ${failures.join('; ')}`);
  return { schema: budgetSchema, declared_at: new Date().toISOString(), method: 'B-only reference predeclared before strict C: use every unchanged original lifecycle-v1 native limit, derived from three pre-#216 repetitions; require v2 B median inside each original limit and strict C median <= same limit. Fixed 24 successes and dispatches per repetition, exact bytes/events/cancellation, 24 owner/native mapping and retrieval checks plus one authenticated zero-dispatch denial per gateway durable repetition. Never reset to fit C.',
    baseline: { source_revision: baseline.source_revision, contract: null, harness_sha256: baseline.harness_sha256, runner_sha256: baseline.runner_sha256 },
    historical_baseline_sha256: baseline.historical_baseline_sha256, historical_budget_sha256: baseline.historical_budget_sha256,
    frozen_harness_sha256: baseline.frozen_harness_sha256, harness_sha256: baseline.harness_sha256, runner_sha256: baseline.runner_sha256,
    measurement: structuredClone(measurement(baseline)), maxima: structuredClone(historical.maxima) };
}
export function compare(candidate, budget) {
  if (candidate.schema !== schema || budget.schema !== budgetSchema || !isDeepStrictEqual(candidate.contract, { mode: 'strict' }) || candidate.working_tree !== '') throw new Error('Clean strict lifecycle-v2 candidate required');
  if (candidate.source_revision === budget.baseline.source_revision || candidate.harness_sha256 !== budget.harness_sha256 || candidate.runner_sha256 !== budget.runner_sha256 || candidate.frozen_harness_sha256 !== budget.frozen_harness_sha256 || candidate.historical_baseline_sha256 !== budget.historical_baseline_sha256 || candidate.historical_budget_sha256 !== budget.historical_budget_sha256) throw new Error('Fixture, historical anchor or measured source changed');
  if (!isDeepStrictEqual(measurement(candidate), budget.measurement)) throw new Error('Measurement conditions changed');
  if (!isDeepStrictEqual(Object.keys(budget.maxima).sort(), [...names].sort())) throw new Error('Frozen workload inventory changed');
  const summary = validateV2Runs(candidate.runs);
  const failures = [];
  for (const name of names) {
    const metrics = Object.keys(summary[name]).filter((metric) => metric !== 'elapsed_ns');
    if (!isDeepStrictEqual(Object.keys(budget.maxima[name]).sort(), metrics.sort())) throw new Error('Frozen metric inventory changed');
    for (const metric of metrics) {
      const limit = budget.maxima[name][metric];
      if (!Number.isFinite(limit) || limit < 0) throw new Error('Invalid frozen limit');
      if (summary[name][metric].median > limit) failures.push(`${name}: ${metric} ${summary[name][metric].median} > ${limit}`);
    }
  }
  return failures;
}
if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const [action, path, other] = process.argv.slice(2);
    if (action === 'record-baseline' && path && !other) record(path, false);
    else if (action === 'freeze' && path && other) writeFileSync(other, JSON.stringify(freeze(JSON.parse(readFileSync(path, 'utf8')), JSON.parse(readFileSync(historicalBudget, 'utf8'))), null, 2) + '\n', { flag: 'wx' });
    else if (action === 'record-strict' && path && other) record(path, true, other);
    else if (action === 'compare' && path && other) {
      const failures = compare(JSON.parse(readFileSync(path, 'utf8')), JSON.parse(readFileSync(other, 'utf8')));
      console.log(JSON.stringify({ passed: failures.length === 0, failures }, null, 2)); if (failures.length) process.exitCode = 1;
    } else throw new Error('Usage: record-baseline <artifact> | freeze <baseline> <budgets> | record-strict <artifact> <committed-budgets> | compare <candidate> <budgets>');
  } catch (error) { console.error(error.message); process.exitCode = 1; }
}
