#!/usr/bin/env node
import { spawnSync } from 'node:child_process';
import { readFileSync, writeFileSync, appendFileSync, existsSync } from 'node:fs';
import { cpus, totalmem, release, arch, platform } from 'node:os';
import { createHash, randomUUID } from 'node:crypto';
import { isDeepStrictEqual } from 'node:util';
import { fileURLToPath } from 'node:url';
import { resolve } from 'node:path';
import { names, workloads, validateRuns } from './fidelity-lifecycle-benchmark.mjs';

const schema = 'openllmproxy.dev/fidelity-lifecycle-performance/v2';
const budgetSchema = 'openllmproxy.dev/fidelity-lifecycle-budget/v2';
const historicalProduct = '033c611f'; // pre-#216 source with frozen v1 evidence
const historicalBaseline = 'docs/evidence/fidelity-performance/lifecycle-v1/baseline.json';
const historicalBudget = 'docs/evidence/fidelity-performance/lifecycle-v1/replacement-budgets.json';
const harness = 'tests/integration/fidelity_lifecycle_v2_test.go';
const frozenHarness = 'tests/integration/fidelity_lifecycle_performance_test.go';
const runner = 'scripts/fidelity-lifecycle-v2-benchmark.mjs';
const captureSchema = 'openllmproxy.dev/fidelity-lifecycle-capture/v2';
export const evidencePaths = Object.freeze({
  baseline: 'docs/evidence/fidelity-performance/lifecycle-v2/baseline.jsonl',
  budget: 'docs/evidence/fidelity-performance/lifecycle-v2/replacement-budgets.json',
  candidate: 'docs/evidence/fidelity-performance/lifecycle-v2/strict-candidate.jsonl'
});
const methodCommit = 'd63e67bd05331757e25e11a18997950ac98d2126';
const strictProductCommit = 'dfa6420e';
// The reviewed source delta replaces the legacy unencrypted resource store
// with NewEncrypted and extracts the same harness constructor for migration
// tests. An unreviewed setup-helper revision invalidates this qualification.
export const candidateSetupSHA256 = '0f7230293d2e325dafcaf9dc56d4c02dc29b12c61ce105ce281dbc126954cb93';
const addedNames = workloads.flatMap((workload) => [1, 4].map((c) => `${workload}/c${c}`));
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
function isAncestor(ancestor) {
  const r = spawnSync('git', ['merge-base', '--is-ancestor', ancestor, 'HEAD'], { encoding: 'utf8' });
  return r.status === 0;
}
export function verifyCandidateLineage({ methodAncestor, productAncestor, baselineAncestor, productDiff }) {
  if (!methodAncestor || !productAncestor || !baselineAncestor || !productDiff) {
    throw new Error('Pre-candidate method/reference ancestry or strict product change missing');
  }
}
function committedClean(path) {
  if (!existsSync(path) || !command('git', ['ls-files', '--error-unmatch', path]) || command('git', ['status', '--short', '--', path]) !== '') {
    throw new Error(`Committed clean evidence required: ${path}`);
  }
}
export function readCapture(path) {
  const lines = readFileSync(path, 'utf8').trimEnd().split('\n');
  if (lines.length !== 2) throw new Error('Incomplete or edited append-only lifecycle capture');
  const start = JSON.parse(lines[0]);
  const terminal = JSON.parse(lines[1]);
  if (start.schema !== captureSchema || start.phase !== 'reserved' ||
      terminal.schema !== captureSchema || terminal.capture_id !== start.capture_id ||
      !['complete', 'failed'].includes(terminal.phase) || !terminal.artifact ||
      terminal.artifact.source_revision !== start.source_revision ||
      !isDeepStrictEqual(terminal.artifact.contract, start.contract) ||
      terminal.artifact.started_at !== start.started_at) {
    throw new Error('Invalid append-only lifecycle capture');
  }
  return terminal;
}
export function reserveCapture(path, source, contract, startedAt) {
  const id = randomUUID();
  writeFileSync(path, JSON.stringify({ schema: captureSchema, phase: 'reserved', capture_id: id,
    source_revision: source, contract, started_at: startedAt }) + '\n', { flag: 'wx' });
  return id;
}
export function completeCapture(path, id, phase, artifact) {
  appendFileSync(path, JSON.stringify({ schema: captureSchema, phase, capture_id: id, artifact }) + '\n');
}

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
    go_build_environment: a.go_build_environment, hardware: a.hardware, storage: a.storage, setup_sha256: a.setup_sha256,
    conditions: a.conditions, repetitions: a.repetitions, samples_per_repetition: a.samples_per_repetition,
    concurrency: a.concurrency };
}
function addedLatency(summary, label, metric) {
  return summary[`${label}/gateway`][metric].median - summary[`${label}/relay`][metric].median;
}
export function historicalAddedLimits(reference) {
  if (reference.schema !== 'openllmproxy.dev/fidelity-lifecycle-performance/v1' || reference.contract !== null ||
      reference.source_revision !== '373c58467a3a17da97d2b48b16473ffd715afeac' || reference.working_tree !== '') {
    throw new Error('Original native lifecycle reference changed');
  }
  validateRuns(reference.runs);
  return Object.fromEntries(addedNames.map((label) => [label, Object.fromEntries([50, 95, 99].map((p) => {
    const metric = `latency-p${p}-us`;
    const differences = [0, 1, 2].map((repetition) => {
      const gateway = reference.runs.find((r) => r.name === `${label}/gateway` && r.repetition === repetition);
      const relay = reference.runs.find((r) => r.name === `${label}/relay` && r.repetition === repetition);
      return gateway.metrics[metric] - relay.metrics[metric];
    });
    // Same pre-change v1 latency tolerance, applied to the original B
    // gateway-minus-relay descriptive difference. A negative maximum gets
    // only the fixed 1 ms tolerance, never a negative budget.
    return [metric, Math.ceil(Math.max(0, ...differences) * 1.5 + 1000)];
  }))]));
}
export function verifyBaselineReceipt(capture, budget, captureSHA) {
  if (capture.phase !== 'complete' || budget.schema !== budgetSchema ||
      !/^[0-9a-f]{64}$/.test(captureSHA) || budget.baseline_capture_sha256 !== captureSHA) {
    throw new Error('Committed complete B-only capture SHA is required');
  }
  const b = capture.artifact;
  if (!isDeepStrictEqual(budget.maxima, JSON.parse(readFileSync(historicalBudget, 'utf8')).maxima) ||
      !isDeepStrictEqual(budget.added_latency_maxima, historicalAddedLimits(JSON.parse(readFileSync(historicalBaseline, 'utf8'))))) {
    throw new Error('B-only numeric method changed after preregistration');
  }
  if (b.schema !== schema || b.contract !== null || b.working_tree !== '' ||
      b.source_revision !== budget.baseline.source_revision || b.harness_sha256 !== budget.harness_sha256 ||
      b.runner_sha256 !== budget.runner_sha256 || !isDeepStrictEqual(measurement(b), budget.measurement)) {
    throw new Error('Committed B-only capture and numeric receipt disagree');
  }
  const summary = validateV2Runs(b.runs);
  for (const name of names) for (const [metric, limit] of Object.entries(budget.maxima[name])) {
    if (summary[name][metric]?.median > limit) throw new Error(`B-only self-compare failed: ${name}/${metric}`);
  }
  for (const label of addedNames) for (const [metric, limit] of Object.entries(budget.added_latency_maxima[label])) {
    if (addedLatency(summary, label, metric) > limit) throw new Error(`B-only added-latency self-compare failed: ${label}/${metric}`);
  }
  return b;
}
function referenceProductDiff() {
  return command('git', ['diff', '--name-only', historicalProduct, 'HEAD']).split('\n').filter(Boolean).sort();
}
function record(strict) {
  const path = strict ? evidencePaths.candidate : evidencePaths.baseline;
  if (existsSync(path)) throw new Error(`Fixed write-once lifecycle capture already exists: ${path}`);
  const source = command('git', ['rev-parse', 'HEAD']);
  const workingTree = command('git', ['status', '--short']);
  if (workingTree !== '') throw new Error('A clean source worktree is required before reserving lifecycle evidence');
  if (strict) {
    committedClean(evidencePaths.baseline);
    committedClean(evidencePaths.budget);
    const budget = JSON.parse(readFileSync(evidencePaths.budget, 'utf8'));
    const b = readCapture(evidencePaths.baseline);
    verifyBaselineReceipt(b, budget, hash(evidencePaths.baseline));
    const original = JSON.parse(readFileSync(historicalBudget, 'utf8'));
    const added = historicalAddedLimits(JSON.parse(readFileSync(historicalBaseline, 'utf8')));
    if (budget.harness_sha256 !== hash(harness) || budget.runner_sha256 !== hash(runner) ||
        budget.expected_candidate_setup_sha256 !== candidateSetupSHA256 || hash('tests/integration/access_test.go') !== budget.expected_candidate_setup_sha256 ||
        !isDeepStrictEqual(budget.maxima, original.maxima) || !isDeepStrictEqual(budget.added_latency_maxima, added)) throw new Error('Unfrozen or mismatched B-only v2 budget/setup');
    verifyCandidateLineage({ methodAncestor: isAncestor(methodCommit), productAncestor: isAncestor(strictProductCommit),
      baselineAncestor: isAncestor(b.artifact.source_revision),
      productDiff: Boolean(command('git', ['diff', '--name-only', historicalProduct, 'HEAD', '--', 'internal/gateway/response_contract.go'])) });
    if (process.env.OLP_LIFECYCLE_ROUTE_FIDELITY !== '{"mode":"strict"}') throw new Error('Explicit strict route fidelity required');
  } else {
    if (process.env.OLP_LIFECYCLE_ROUTE_FIDELITY) throw new Error('Historical B reference must use the legacy route');
    const allowed = [harness, runner, 'scripts/fidelity-lifecycle-v2-benchmark.test.mjs', 'docs/evidence/fidelity-performance/lifecycle-v2/README.md'];
    const diff = referenceProductDiff();
    if (!diff.every((file) => allowed.includes(file)) || !diff.includes(harness) || !diff.includes(runner)) {
      throw new Error('Historical B product source differs from the pre-#216 revision');
    }
  }
  const contract = strict ? { mode: 'strict' } : null;
  const startedAt = new Date().toISOString();
  const loadBefore = optional('/proc/loadavg');
  const captureID = reserveCapture(path, source, contract, startedAt); // irreversible reservation precedes Go
  let result;
  try {
    result = spawnSync('go', args, { encoding: 'utf8', maxBuffer: 32 << 20, env: { ...process.env, ...runtimeEnvironment } });
    process.stdout.write(result.stdout ?? ''); process.stderr.write(result.stderr ?? '');
    if (result.status !== 0) throw new Error(`Go lifecycle run failed: exit=${result.status} signal=${result.signal}`);
    const runs = parseRuns(result.stdout);
    const summary = validateV2Runs(runs);
    const setup = result.stdout.split('\n').find((line) => line.includes('LIFECYCLE_V2_SETUP '));
    if (!setup) throw new Error('Required storage condition missing');
    const artifact = {
      schema, contract, started_at: startedAt, completed_at: new Date().toISOString(),
      source_revision: source, working_tree: workingTree,
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
    completeCapture(path, captureID, 'complete', artifact);
    console.log(`Recorded ${runs.length} lifecycle-v2 repetitions at fixed path ${path}`);
  } catch (error) {
    let partialRuns = [];
    try { partialRuns = parseRuns(result?.stdout ?? ''); } catch { /* raw output is preserved below */ }
    completeCapture(path, captureID, 'failed', { schema, contract, source_revision: source, started_at: startedAt,
      completed_at: new Date().toISOString(), reason: error.message, exit_status: result?.status ?? null,
      signal: result?.signal ?? null, partial_runs: partialRuns, raw_output: result?.stdout ?? '', raw_error: result?.stderr ?? '',
      system_load: { before: loadBefore, after: optional('/proc/loadavg') } });
    throw new Error(`Lifecycle-v2 attempt retained as failed at ${path}: ${error.message}`);
  }
}
export function freeze(baseline, historical, captureSHA) {
  if (!/^[0-9a-f]{64}$/.test(captureSHA)) throw new Error('Frozen B-only capture SHA required');
  if (baseline.schema !== schema || baseline.contract !== null || baseline.working_tree !== '' ||
      historical.schema !== 'openllmproxy.dev/fidelity-lifecycle-budget/v1' || historical.baseline_revision !== '373c58467a3a17da97d2b48b16473ffd715afeac') {
    throw new Error('Clean historical pre-#216 native reference required');
  }
  if (baseline.historical_baseline_sha256 !== hash(historicalBaseline) || baseline.historical_budget_sha256 !== hash(historicalBudget) ||
      baseline.frozen_harness_sha256 !== historical.harness_sha256 || baseline.setup_sha256 !== hash('tests/integration/access_test.go') ||
      baseline.harness_sha256 !== hash(harness) || baseline.runner_sha256 !== hash(runner)) throw new Error('Original or current B measurement anchors changed');
  if (!isDeepStrictEqual(historical.maxima, JSON.parse(readFileSync(historicalBudget, 'utf8')).maxima)) throw new Error('Original native limits changed');
  const allowed = [harness, runner, 'scripts/fidelity-lifecycle-v2-benchmark.test.mjs', 'docs/evidence/fidelity-performance/lifecycle-v2/README.md'];
  if (!baseline.reference_product_diff.every((path) => allowed.includes(path)) || !baseline.reference_product_diff.includes(harness) || !baseline.reference_product_diff.includes(runner)) {
    throw new Error('Historical B product source differs from pre-#216 revision');
  }
  const summary = validateV2Runs(baseline.runs);
  const added = historicalAddedLimits(JSON.parse(readFileSync(historicalBaseline, 'utf8')));
  if (!isDeepStrictEqual(Object.keys(historical.maxima).sort(), [...names].sort())) throw new Error('Frozen v1 workload inventory changed');
  const failures = [];
  for (const name of names) {
    const expectedMetrics = Object.keys(summary[name]).filter((metric) => metric !== 'elapsed_ns').sort();
    if (!isDeepStrictEqual(Object.keys(historical.maxima[name]).sort(), expectedMetrics)) throw new Error('Original frozen metric inventory changed');
    for (const [metric, limit] of Object.entries(historical.maxima[name])) {
      if (!Number.isFinite(limit) || limit < 0 || !summary[name][metric] || summary[name][metric].median > limit) failures.push(`${name}: B ${metric} exceeds original frozen native limit`);
    }
  }
  for (const label of addedNames) for (const [metric, limit] of Object.entries(added[label])) {
    const value = addedLatency(summary, label, metric);
    if (value > limit) failures.push(`${label}: B added ${metric} exceeds pre-change limit`);
  }
  if (failures.length) throw new Error(`Historical B envelope failed: ${failures.join('; ')}`);
  return { schema: budgetSchema, declared_at: new Date().toISOString(), method: 'B-only reference predeclared before strict C: use every unchanged original lifecycle-v1 native limit plus 24 added-latency limits derived only from the three original pre-#216 gateway-minus-relay repetition differences as ceil(max(0,max difference)*1.5+1000us). Require v2 B and strict C medians within all limits. Fixed 24 successes and dispatches per repetition, exact bytes/events/cancellation, 24 owner/native mapping and retrieval checks plus one authenticated zero-dispatch denial per gateway durable repetition. Never reset to fit C.',
    baseline: { source_revision: baseline.source_revision, contract: null, harness_sha256: baseline.harness_sha256, runner_sha256: baseline.runner_sha256 },
    baseline_capture_sha256: captureSHA,
    historical_baseline_sha256: baseline.historical_baseline_sha256, historical_budget_sha256: baseline.historical_budget_sha256,
    frozen_harness_sha256: baseline.frozen_harness_sha256, harness_sha256: baseline.harness_sha256, runner_sha256: baseline.runner_sha256,
    expected_candidate_setup_sha256: candidateSetupSHA256,
    measurement: structuredClone(measurement(baseline)), maxima: structuredClone(historical.maxima), added_latency_maxima: added };
}
export function compare(candidate, budget) {
  if (candidate.schema !== schema || budget.schema !== budgetSchema || !isDeepStrictEqual(candidate.contract, { mode: 'strict' }) || candidate.working_tree !== '') throw new Error('Clean strict lifecycle-v2 candidate required');
  if (candidate.source_revision === budget.baseline.source_revision || candidate.harness_sha256 !== budget.harness_sha256 || candidate.runner_sha256 !== budget.runner_sha256 || candidate.frozen_harness_sha256 !== budget.frozen_harness_sha256 || candidate.historical_baseline_sha256 !== budget.historical_baseline_sha256 || candidate.historical_budget_sha256 !== budget.historical_budget_sha256) throw new Error('Fixture, historical anchor or measured source changed');
  if (candidate.setup_sha256 !== budget.expected_candidate_setup_sha256) throw new Error('Unreviewed candidate setup helper changed');
  const candidateMeasurement = measurement(candidate);
  candidateMeasurement.setup_sha256 = budget.measurement.setup_sha256;
  if (!isDeepStrictEqual(candidateMeasurement, budget.measurement)) throw new Error('Measurement conditions changed');
  const original = JSON.parse(readFileSync(historicalBudget, 'utf8'));
  if (!isDeepStrictEqual(budget.maxima, original.maxima) || !isDeepStrictEqual(budget.added_latency_maxima, historicalAddedLimits(JSON.parse(readFileSync(historicalBaseline, 'utf8'))))) throw new Error('Pre-change numeric limits changed');
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
  for (const label of addedNames) for (const [metric, limit] of Object.entries(budget.added_latency_maxima[label])) {
    const value = addedLatency(summary, label, metric);
    if (value > limit) failures.push(`${label}: added ${metric} ${value} > ${limit}`);
  }
  return failures;
}
if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const [action, unexpected] = process.argv.slice(2);
    if (unexpected || process.argv.length !== 3) throw new Error('Only fixed preregistered evidence paths are accepted');
    if (action === 'record-baseline') record(false);
    else if (action === 'freeze') {
      const b = readCapture(evidencePaths.baseline);
      if (b.phase !== 'complete') throw new Error('Failed or partial B capture cannot be frozen');
      writeFileSync(evidencePaths.budget, JSON.stringify(freeze(b.artifact,
        JSON.parse(readFileSync(historicalBudget, 'utf8')), hash(evidencePaths.baseline)), null, 2) + '\n', { flag: 'wx' });
    } else if (action === 'record-strict') record(true);
    else if (action === 'compare') {
      committedClean(evidencePaths.baseline);
      committedClean(evidencePaths.budget);
      const budget = JSON.parse(readFileSync(evidencePaths.budget, 'utf8'));
      verifyBaselineReceipt(readCapture(evidencePaths.baseline), budget, hash(evidencePaths.baseline));
      const c = readCapture(evidencePaths.candidate);
      if (c.phase !== 'complete') throw new Error('Failed or partial C capture remains failed');
      const failures = compare(c.artifact, budget);
      console.log(JSON.stringify({ passed: failures.length === 0, failures }, null, 2)); if (failures.length) process.exitCode = 1;
    } else throw new Error('Usage: record-baseline | freeze | record-strict | compare (fixed lifecycle-v2 paths only)');
  } catch (error) { console.error(error.message); process.exitCode = 1; }
}
