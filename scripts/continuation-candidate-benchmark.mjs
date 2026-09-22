#!/usr/bin/env node
import { spawnSync } from 'node:child_process';
import { readFileSync, writeFileSync, existsSync } from 'node:fs';
import { cpus, totalmem, release, arch, platform } from 'node:os';
import { createHash } from 'node:crypto';
import { isDeepStrictEqual } from 'node:util';
import { fileURLToPath } from 'node:url';
import { resolve } from 'node:path';
import { compare as validateReference } from './continuation-barrier-benchmark.mjs';

const schema = 'openllmproxy.dev/continuation-candidate-performance/v1';
const contract = 'negotiated-chat-anthropic-tools-v1/go-sdk-equivalent/1';
const runner = 'scripts/continuation-candidate-benchmark.mjs';
const harness = 'tests/integration/continuation_candidate_benchmark_test.go';
const reference = 'docs/evidence/fidelity-performance/barrier-v1/baseline.json';
const frozenBudgets = 'docs/evidence/fidelity-performance/barrier-v1/budgets.json';
const sharedSources = [
  'tests/integration/continuation_barrier_benchmark_test.go',
  'tests/integration/fidelity_lifecycle_performance_test.go',
  'tests/integration/access_test.go',
  'tests/integration/strict_generation_test.go',
  'tests/fidelity/reference.go',
  'tests/fixtures/fidelity/v1/anthropic-tool-next-request.json',
  'tests/fixtures/fidelity/v1/anthropic-tool-workflow.sse'
];
const names = ['small', 'large'].flatMap((size) => [1, 8].map((c) => `${size}/c${c}/translated`));
const commandArgs = ['test', '-mod=readonly', '-tags=integration', '-run', '^TestNegotiatedContinuationCandidateBenchmark$', '-count=1', '-v', '-timeout=10m', './tests/integration'];
const runtimeEnvironment = { GOMAXPROCS: '4', GOGC: '100', GOMEMLIMIT: 'off', GODEBUG: '', OLP_CONTINUATION_CANDIDATE_MEASURE: '1' };
const conditions = {
  network: 'IPv4 loopback, two warm HTTP/1.1 inference hops, no inference TLS',
  authority: 'Public strict Anthropic Messages profile, same disposable PostgreSQL release and provider fixture, state-enabled API key and existing encrypted resource authority',
  workflow: 'Go SDK-equivalent parser of the versioned OpenAI Chat chunks and standard assistant/tool history; pinned JavaScript/Python official SDKs qualified separately with the same native oracle',
  history: 'Original user turn or the same additional 262144 ASCII bytes in both provider requests and the encrypted complete dependency',
  reference_relation: 'Same frozen native first/next request and 19-event provider oracle. Candidate action-ready means first actionable projected tool chunk followed by a separate authenticated public GET that decrypts committed ready state, before either fixture action. Native wire-tool is earlier and is not a candidate observation.',
  outcomes: '24 complete two-turn workflows, 48 provider dispatches, 456 native events, 312 projected observations, 48 fixture actions and 24 ready checks per repetition. Any unexpected provider request or response fails the test.',
  resources: 'Process CPU, allocations and 1ms sampled heap growth include Go client, provider, gateway, oracle and instrumentation; exclude separate PostgreSQL process; no actual SDK process CPU or isolated gateway RSS',
  ordering: 'Fixed size/concurrency order, one warmup per repetition, three repetitions of 24 successful workflows at concurrency 1/8'
};
const comparisonScope = [
  'ns/op', 'process-cpu-ns/op', 'B/op', 'allocs/op', 'sampled-heap-growth-B',
  ...[50, 95, 99].flatMap((p) => [`workflow-p${p}-us`, `action-ready-p${p}-us`])
];
const hash = (path) => createHash('sha256').update(readFileSync(path)).digest('hex');
const optional = (path) => existsSync(path) ? readFileSync(path, 'utf8').trim() : null;
const median = (values) => values.toSorted((a, b) => a - b)[Math.floor(values.length / 2)];
function command(name, args) {
  const result = spawnSync(name, args, { encoding: 'utf8', maxBuffer: 16 << 20 });
  if (result.status !== 0) throw new Error(`${name} failed: ${result.stderr}`);
  return result.stdout.trim();
}
function referenceEvidence() {
  const baseline = JSON.parse(readFileSync(reference, 'utf8'));
  const budgets = JSON.parse(readFileSync(frozenBudgets, 'utf8'));
  const failures = validateReference(baseline, budgets);
  if (failures.length) throw new Error(`Frozen reference does not self-compare: ${failures.join('; ')}`);
  for (const [path, digest] of Object.entries(baseline.dependency_sha256)) {
    if (hash(path) !== digest) throw new Error(`Frozen independent source changed: ${path}`);
  }
  if (hash(sharedSources[0]) !== baseline.harness_sha256 || hash('scripts/continuation-barrier-benchmark.mjs') !== baseline.runner_sha256) {
    throw new Error('Frozen reference harness or runner changed');
  }
  return { baseline, budgets };
}
export function metricNames() {
  return ['elapsed_ns', 'ns/op', 'process-cpu-ns/op', 'B/op', 'allocs/op', 'sampled-heap-growth-B',
    ...['workflow', 'first-event', 'tool-visible', 'action-ready'].flatMap((phase) => [50, 95, 99].map((p) => `${phase}-p${p}-us`))];
}
export function validateRuns(runs) {
  if (!Array.isArray(runs) || runs.length !== names.length * 3) throw new Error('Complete 12-repetition candidate inventory required');
  const seen = new Set();
  const sizes = new Map();
  for (const run of runs) {
    if (!names.includes(run.name) || ![0, 1, 2].includes(run.repetition) || seen.has(`${run.name}/${run.repetition}`) || run.contract !== contract) throw new Error('Unknown or duplicate candidate workload');
    seen.add(`${run.name}/${run.repetition}`);
    if (run.samples !== 24 || run.dispatches !== 48 || run.first_requests !== 24 || run.next_requests !== 24 || run.native_events !== 456 || run.observations !== 312 || run.actions !== 48 || run.ready_checks !== 24 || run.rejected !== 0) throw new Error('Candidate accepted work or semantic inventory changed');
    const large = run.name.startsWith('large/');
    if (run.history_bytes !== (large ? 262144 : 0) || !Number.isSafeInteger(run.state_bytes) || run.state_bytes <= run.history_bytes || run.state_bytes > (5 << 20)) throw new Error('Candidate encrypted state is missing or unbounded');
    if (sizes.has(large) && sizes.get(large) !== run.state_bytes) throw new Error('Candidate state size varies across repetitions');
    sizes.set(large, run.state_bytes);
    if (!isDeepStrictEqual(Object.keys(run.metrics).sort(), metricNames().sort())) throw new Error('Candidate metric inventory changed');
    for (const value of Object.values(run.metrics)) if (!Number.isFinite(value) || value < 0) throw new Error('Invalid candidate metric');
    for (const p of [50, 95, 99]) {
      const first = run.metrics[`first-event-p${p}-us`];
      const visible = run.metrics[`tool-visible-p${p}-us`];
      const ready = run.metrics[`action-ready-p${p}-us`];
      const workflow = run.metrics[`workflow-p${p}-us`];
      if (!(first > 0 && first <= visible && visible <= ready && ready <= workflow)) throw new Error('Candidate actionability order changed');
    }
  }
  return Object.fromEntries(names.map((name) => [name, Object.fromEntries(metricNames().map((metric) => {
    const values = runs.filter((run) => run.name === name).map((run) => run.metrics[metric]);
    return [metric, { median: median(values), minimum: Math.min(...values), maximum: Math.max(...values) }];
  }))]));
}
export function parseRuns(output) {
  const marker = 'CANDIDATE_MEASUREMENT ';
  return output.split('\n').filter((line) => line.includes(marker)).map((line) => JSON.parse(line.slice(line.indexOf(marker) + marker.length)));
}
function record(path) {
  if (existsSync(path)) throw new Error('Refusing to overwrite candidate measurement');
  if (command('git', ['status', '--short']) !== '') throw new Error('Commit candidate sources before measurement');
  const { baseline } = referenceEvidence();
  const startedAt = new Date().toISOString();
  const loadBefore = optional('/proc/loadavg');
  const result = spawnSync('go', commandArgs, { encoding: 'utf8', maxBuffer: 16 << 20, env: { ...process.env, ...runtimeEnvironment } });
  process.stdout.write(result.stdout ?? '');
  process.stderr.write(result.stderr ?? '');
  if (result.status !== 0) throw new Error('Candidate run failed; no passing artifact written');
  const runs = parseRuns(result.stdout);
  const summary = validateRuns(runs);
  const marker = 'CANDIDATE_SETUP ';
  const setup = result.stdout.split('\n').find((line) => line.includes(marker));
  if (!setup) throw new Error('Candidate storage condition missing');
  const artifact = {
    schema, contract, started_at: startedAt, completed_at: new Date().toISOString(),
    source_revision: command('git', ['rev-parse', 'HEAD']), working_tree: command('git', ['status', '--short']),
    harness_sha256: hash(harness), runner_sha256: hash(runner),
    shared_source_sha256: Object.fromEntries(sharedSources.map((source) => [source, hash(source)])),
    reference_sha256: hash(reference), frozen_budget_sha256: hash(frozenBudgets), reference_revision: baseline.source_revision,
    command: ['go', ...commandArgs], runtime_environment: runtimeEnvironment, toolchain: command('go', ['version']),
    go_build_environment: command('go', ['env', 'GOFLAGS', 'GOAMD64', 'GOARCH', 'GOOS']),
    hardware: { os: platform(), architecture: arch(), kernel: release(), cpu: cpus()[0]?.model, logical_cpus: cpus().length, total_memory_bytes: totalmem(), cpu_quota: optional('/sys/fs/cgroup/cpu.max'), memory_limit: optional('/sys/fs/cgroup/memory.max') },
    system_load: { before: loadBefore, after: optional('/proc/loadavg') },
    storage: JSON.parse(setup.slice(setup.indexOf(marker) + marker.length)), conditions,
    repetitions: 3, samples_per_repetition: 24, concurrency: [1, 8], runs, summary,
    comparison_scope: comparisonScope,
    excluded_from_direct_comparison: ['first-event: native first event versus first projected client observation', 'tool-visible: native pre-barrier wire tool versus post-commit projected tool', 'reference claim/journal/ready phase timings: candidate uses production transactions without phase instrumentation', 'actual JS/Python SDK process CPU'],
    unmeasured: ['WAN/TLS inference', 'isolated gateway RSS', 'live-model quality'], raw_output: result.stdout
  };
  writeFileSync(path, JSON.stringify(artifact, null, 2) + '\n', { flag: 'wx' });
  console.log(`Recorded ${runs.length} negotiated continuation candidate repetitions`);
}
export function compareCandidate(candidate, baseline, budgets) {
  if (candidate.schema !== schema || candidate.contract !== contract || candidate.working_tree !== '') throw new Error('Unknown or dirty candidate evidence');
  if (candidate.reference_sha256 !== hash(reference) || candidate.frozen_budget_sha256 !== hash(frozenBudgets) || candidate.reference_revision !== baseline.source_revision) throw new Error('Candidate compared to a changed reference');
  if (candidate.harness_sha256 !== hash(harness) || candidate.runner_sha256 !== hash(runner)) throw new Error('Candidate harness or runner changed');
  for (const [source, digest] of Object.entries(candidate.shared_source_sha256)) if (hash(source) !== digest) throw new Error(`Candidate dependency changed: ${source}`);
  if (!isDeepStrictEqual(candidate.conditions, conditions) || !isDeepStrictEqual(candidate.runtime_environment, runtimeEnvironment) || !isDeepStrictEqual(candidate.command, ['go', ...commandArgs])) throw new Error('Candidate method changed');
  if (candidate.toolchain !== baseline.toolchain || !isDeepStrictEqual(candidate.storage, baseline.storage) || candidate.hardware.cpu !== baseline.hardware.cpu || candidate.hardware.logical_cpus !== baseline.hardware.logical_cpus) throw new Error('Reference and candidate runtime conditions differ');
  if (!isDeepStrictEqual(candidate.comparison_scope, comparisonScope)) throw new Error('Candidate comparison scope changed');
  const summary = validateRuns(candidate.runs);
  const failures = [];
  for (const name of names) {
    const referenceName = name.replace('/translated', '/reference');
    for (const metric of comparisonScope) {
      const limit = budgets.maxima[referenceName]?.[metric];
      if (!Number.isFinite(limit) || limit < 0) throw new Error(`Missing frozen limit: ${referenceName}/${metric}`);
      if (summary[name][metric].median > limit) failures.push(`${name}: ${metric} ${summary[name][metric].median} > ${limit}`);
    }
  }
  return failures;
}
if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const [action, path] = process.argv.slice(2);
    if (action === 'record' && path) record(path);
    else if (action === 'compare' && path) {
      const { baseline, budgets } = referenceEvidence();
      const failures = compareCandidate(JSON.parse(readFileSync(path, 'utf8')), baseline, budgets);
      console.log(JSON.stringify({ passed: failures.length === 0, failures }, null, 2));
      if (failures.length) process.exitCode = 1;
    } else throw new Error('Usage: record <artifact> | compare <artifact>');
  } catch (error) { console.error(error.message); process.exitCode = 1; }
}
