import { spawnSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { arch, cpus, platform, release, totalmem } from 'node:os';
import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { isDeepStrictEqual } from 'node:util';
import {
  compare as compareV2, completeCapture, evidencePaths, parseRuns, readCapture, reserveCapture,
  validateV2Runs, verifyBaselineReceipt
} from './fidelity-lifecycle-v2-benchmark.mjs';
import {
  ancestor, committedOnce, fileSHA256, gitBlobSHA256, helperPath, lockPath, requireClean,
  sourceRevision, verifyMethod, verifyProductLock
} from './fidelity-final-source-lock.mjs';

export const candidatePath = 'docs/evidence/fidelity-performance/lifecycle-v3/strict-candidate.jsonl';
export const runnerPath = 'scripts/fidelity-lifecycle-v3-benchmark.mjs';
export const testPath = 'scripts/fidelity-lifecycle-v3-benchmark.test.mjs';
export const priorCandidateSHA256 = 'cc22b2ff1f2c760ba1f1f9b782e1ad6fbf081abd6427aef89f369f736369a375';
const priorProduct = 'bc325da50c575803c771531dc4e3aab1ac203a53';
const schema = 'openllmproxy.dev/fidelity-lifecycle-performance/v3-repeat';
const v2Schema = 'openllmproxy.dev/fidelity-lifecycle-performance/v2';
const harness = 'tests/integration/fidelity_lifecycle_v2_test.go';
const frozenHarness = 'tests/integration/fidelity_lifecycle_performance_test.go';
const setupHelper = 'tests/integration/access_test.go';
const v2Runner = 'scripts/fidelity-lifecycle-v2-benchmark.mjs';
const v2Test = 'scripts/fidelity-lifecycle-v2-benchmark.test.mjs';
const historicalBaseline = 'docs/evidence/fidelity-performance/lifecycle-v1/baseline.json';
const historicalBudget = 'docs/evidence/fidelity-performance/lifecycle-v1/replacement-budgets.json';
const runtimeEnvironment = { GOMAXPROCS: '4', GOGC: '100', GOMEMLIMIT: 'off', GODEBUG: '', OLP_LIFECYCLE_V2_MEASURE: '1' };
const args = ['test', '-mod=readonly', '-tags=integration', '-run', '^TestFidelityLifecycleV2Performance$', '-count=1', '-v', '-timeout=10m', './tests/integration'];
const optional = (path) => { try { return readFileSync(path, 'utf8').trim(); } catch { return null; } };
const sourceHash = (revision, path) => gitBlobSHA256(revision, path);

export function comparableV2(candidate, budget) {
  // The frozen v2 comparison owns the unchanged numeric and inventory rules.
  // Only schema/runner identities differ in this separately named C repeat;
  // the artifact itself records and verifies its actual v3 method bytes.
  return { ...candidate, schema: v2Schema, runner_sha256: budget.runner_sha256,
    runner_test_sha256: budget.runner_test_sha256 };
}

export function verifyAnchors(source = sourceRevision()) {
  const method = verifyMethod(runnerPath, source);
  const testMethod = verifyMethod(testPath, source);
  if (method !== testMethod || method !== verifyMethod(helperPath, source)) throw new Error('Lifecycle-v3 method, test and product lock helper were not registered together');
  const lock = verifyProductLock(source);
  for (const path of [evidencePaths.baseline, evidencePaths.budget, evidencePaths.candidate,
    historicalBaseline, historicalBudget, v2Runner, v2Test, harness, frozenHarness, setupHelper]) {
    if (sourceHash(source, path) !== fileSHA256(path)) throw new Error(`Changed final-source lifecycle anchor: ${path}`);
  }
  const baselineCommit = committedOnce(evidencePaths.baseline, source);
  committedOnce(evidencePaths.budget, source);
  committedOnce(evidencePaths.candidate, source);
  const budget = JSON.parse(readFileSync(evidencePaths.budget, 'utf8'));
  const baseline = readCapture(evidencePaths.baseline);
  verifyBaselineReceipt(baseline, budget, fileSHA256(evidencePaths.baseline));
  if (budget.harness_sha256 !== fileSHA256(harness) || budget.frozen_harness_sha256 !== fileSHA256(frozenHarness) ||
      budget.runner_sha256 !== fileSHA256(v2Runner) || budget.runner_test_sha256 !== fileSHA256(v2Test) ||
      budget.expected_candidate_setup_sha256 !== fileSHA256(setupHelper)) throw new Error('Frozen lifecycle-v2 method or setup changed');
  if (fileSHA256(evidencePaths.candidate) !== priorCandidateSHA256) throw new Error('Passing bc325 C artifact changed');
  const previous = readCapture(evidencePaths.candidate);
  if (previous.phase !== 'complete' || previous.artifact.source_revision !== priorProduct ||
      compareV2(previous.artifact, budget).length !== 0 || !ancestor(priorProduct, lock.product_revision)) {
    throw new Error('Previous bc325 C did not pass unchanged v2 limits before product repeat');
  }
  return { budget, lock, method, baseline_commit: baselineCommit, previous_commit: committedOnce(evidencePaths.candidate, source) };
}

export function compareCandidate(candidate, anchors) {
  const { budget, lock, method, baseline_commit: baselineCommit, previous_commit: previousCommit } = anchors;
  if (candidate.schema !== schema || !isDeepStrictEqual(candidate.contract, { mode: 'strict' }) || candidate.working_tree !== '' ||
      candidate.method_revision !== method || candidate.product_lock_commit !== lock.lock_commit ||
      candidate.product_lock_sha256 !== lock.lock_sha256 || candidate.product_revision !== lock.product_revision ||
      candidate.production_go_sha256 !== lock.production_go_sha256 || candidate.previous_candidate_sha256 !== priorCandidateSHA256 ||
      candidate.previous_candidate_commit !== previousCommit || candidate.runner_sha256 !== fileSHA256(runnerPath) ||
      candidate.runner_test_sha256 !== fileSHA256(testPath) || candidate.harness_sha256 !== fileSHA256(harness) ||
      candidate.baseline_evidence_commit !== baselineCommit || candidate.lock_helper_sha256 !== fileSHA256(helperPath)) {
    throw new Error('Lifecycle-v3 C is not bound to the locked source, method and passing prior C');
  }
  const failures = compareV2(comparableV2(candidate, budget), budget);
  if (!isDeepStrictEqual(candidate.analysis, { passed: failures.length === 0, failures })) throw new Error('Recorded v3 comparison changed');
  return candidate.analysis;
}

function record() {
  requireClean();
  if (process.env.OLP_LIFECYCLE_ROUTE_FIDELITY !== '{"mode":"strict"}') throw new Error('Exact explicit strict route fidelity is required');
  const source = sourceRevision();
  const anchors = verifyAnchors(source);
  const startedAt = new Date().toISOString();
  const loadBefore = optional('/proc/loadavg');
  const captureID = reserveCapture(candidatePath, source, { mode: 'strict' }, startedAt);
  let result;
  try {
    result = spawnSync('go', args, { encoding: 'utf8', maxBuffer: 32 << 20, env: { ...process.env, ...runtimeEnvironment } });
    process.stdout.write(result.stdout ?? ''); process.stderr.write(result.stderr ?? '');
    if (result.status !== 0) throw new Error(`Go lifecycle run failed: exit=${result.status} signal=${result.signal}`);
    const runs = parseRuns(result.stdout);
    const summary = validateV2Runs(runs);
    const setup = result.stdout.split('\n').find((line) => line.includes('LIFECYCLE_V2_SETUP '));
    if (!setup) throw new Error('Required PostgreSQL condition missing');
    const artifact = {
      schema, contract: { mode: 'strict' }, started_at: startedAt, completed_at: new Date().toISOString(),
      source_revision: source, working_tree: '', method_revision: anchors.method,
      product_lock_commit: anchors.lock.lock_commit, product_lock_sha256: anchors.lock.lock_sha256,
      product_revision: anchors.lock.product_revision, production_go_sha256: anchors.lock.production_go_sha256,
      previous_candidate_sha256: priorCandidateSHA256, previous_candidate_commit: anchors.previous_commit,
      baseline_evidence_commit: anchors.baseline_commit,
      harness_sha256: fileSHA256(harness), frozen_harness_sha256: fileSHA256(frozenHarness),
      runner_sha256: fileSHA256(runnerPath), runner_test_sha256: fileSHA256(testPath),
      lock_helper_sha256: fileSHA256(helperPath),
      historical_baseline_sha256: fileSHA256(historicalBaseline), historical_budget_sha256: fileSHA256(historicalBudget),
      setup_sha256: fileSHA256(setupHelper), command: ['go', ...args], runtime_environment: runtimeEnvironment,
      toolchain: gitTool('go', ['version']), go_build_environment: gitTool('go', ['env', 'GOFLAGS', 'GOAMD64', 'GOARCH', 'GOOS']),
      hardware: { os: platform(), architecture: arch(), kernel: release(), cpu: cpus()[0]?.model, logical_cpus: cpus().length,
        total_memory_bytes: totalmem(), cpu_quota: optional('/sys/fs/cgroup/cpu.max'), memory_limit: optional('/sys/fs/cgroup/memory.max') },
      system_load: { before: loadBefore, after: optional('/proc/loadavg') },
      storage: JSON.parse(setup.slice(setup.indexOf('LIFECYCLE_V2_SETUP ') + 19)),
      conditions: anchors.budget.measurement.conditions,
      repetitions: 3, samples_per_repetition: 24, concurrency: [1, 4], runs, summary,
      counts: { repetitions: runs.length, successes: runs.reduce((n, r) => n + r.succeeded, 0),
        provider_dispatches: runs.reduce((n, r) => n + r.dispatches, 0), mapping_checks: runs.reduce((n, r) => n + r.mapping_checks, 0),
        retrieval_checks: runs.reduce((n, r) => n + r.retrieval_checks, 0), negative_controls: runs.reduce((n, r) => n + r.negative_controls, 0),
        negative_dispatches: 0, ambiguous_outcomes: 0 },
      unmeasured: ['encrypted translated-tool state barrier', 'WAN/TLS inference hops', 'isolated gateway RSS', 'live-model quality'],
      raw_output: result.stdout
    };
    const failures = compareV2(comparableV2(artifact, anchors.budget), anchors.budget);
    artifact.analysis = { passed: failures.length === 0, failures };
    completeCapture(candidatePath, captureID, 'complete', artifact);
    console.log(JSON.stringify(artifact.analysis));
    if (failures.length) process.exitCode = 1;
  } catch (error) {
    let partialRuns = [];
    try { partialRuns = parseRuns(result?.stdout ?? ''); } catch { /* raw output retained */ }
    completeCapture(candidatePath, captureID, 'failed', { schema, contract: { mode: 'strict' }, source_revision: source,
      method_revision: anchors.method, product_lock_commit: anchors.lock.lock_commit,
      completed_at: new Date().toISOString(), reason: error.message, exit_status: result?.status ?? null,
      signal: result?.signal ?? null, partial_runs: partialRuns, raw_output: result?.stdout ?? '', raw_error: result?.stderr ?? '',
      system_load: { before: loadBefore, after: optional('/proc/loadavg') } });
    throw new Error(`Lifecycle-v3 write-once attempt retained as failed: ${error.message}`);
  }
}
function gitTool(name, args) {
  const result = spawnSync(name, args, { encoding: 'utf8' });
  if (result.status !== 0) throw new Error(`${name} ${args[0]} failed`);
  return result.stdout.trim();
}
function compare() {
  const capture = readCapture(candidatePath);
  if (capture.phase !== 'complete') throw new Error('Incomplete or failed lifecycle-v3 attempt');
  const source = capture.artifact.source_revision;
  if (!ancestor(source, sourceRevision())) throw new Error('Captured C source must be a strict ancestor of offline comparison');
  const anchors = verifyAnchors(source);
  for (const path of [runnerPath, testPath, helperPath, harness, frozenHarness, setupHelper, lockPath]) {
    if (sourceHash(source, path) !== fileSHA256(path)) throw new Error(`Captured C source changed: ${path}`);
  }
  const result = compareCandidate(capture.artifact, anchors);
  console.log(JSON.stringify(result, null, 2));
  if (!result.passed) process.exitCode = 1;
}
if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const [action, unexpected] = process.argv.slice(2);
    if (unexpected || process.argv.length !== 3) throw new Error('Only fixed write-once lifecycle-v3 paths are accepted');
    if (action === 'record-strict') record();
    else if (action === 'compare') compare();
    else throw new Error('Usage: record-strict | compare');
  } catch (error) { console.error(error.message); process.exitCode = 1; }
}
