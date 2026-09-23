#!/usr/bin/env node
// Prospective, additive paired experiment. Frozen v1 evidence and its runner are
// read-only inputs; this runner never edits them or changes the product oracle.
import { spawn, spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { existsSync, readFileSync, writeFileSync } from 'node:fs';
import { arch, cpus, platform, release, totalmem, tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { isDeepStrictEqual } from 'node:util';
import { compare as compareFrozenReference } from './continuation-barrier-benchmark.mjs';

export const anchor = '29e18268ac23f7290d1881cbda3af2cc2ccab018';
export const schema = 'openllmproxy.dev/continuation-barrier-paired/v2';
export const seed = 'OLP-BARRIER-V2-2026-09-23-32x4';
export const samples = 24;
export const blocksPerStratum = 32;
export const strata = ['small/c1', 'small/c8', 'large/c1', 'large/c8'];
export const referencePaths = ['relay', 'gateway', 'reference'];
export const primaryMetrics = [
  'ns/op', 'process-cpu-ns/op', 'B/op', 'allocs/op', 'sampled-heap-growth-B',
  ...['workflow', 'action-ready'].flatMap((phase) => [50, 95, 99].map((p) => `${phase}-p${p}-us`))
];
export const referenceHarness = 'tests/integration/continuation_barrier_paired_v2_test.go';
export const candidateHarness = 'tests/integration/continuation_candidate_paired_v2_test.go';
export const frozenReference = 'docs/evidence/fidelity-performance/barrier-v1/baseline.json';
export const frozenBudget = 'docs/evidence/fidelity-performance/barrier-v1/budgets.json';
export const criteriaPath = 'docs/evidence/fidelity-performance/paired-barrier-v2/criteria.json';
export const runnerPath = 'scripts/continuation-barrier-paired-v2.mjs';
export const corpusPaths = [
  'tests/integration/continuation_barrier_benchmark_test.go',
  'tests/fidelity/reference.go',
  'tests/fixtures/fidelity/v1/anthropic-tool-next-request.json',
  'tests/fixtures/fidelity/v1/anthropic-tool-workflow.sse'
];
const runtimeEnvironment = { GOMAXPROCS: '4', GOGC: '100', GOMEMLIMIT: 'off', GODEBUG: '' };
export const conditions = { network: 'IPv4 loopback, two warm HTTP/1.1 inference hops, no inference TLS', reference: 'Historical native strict gateway plus independently authored encrypted submission/journal/atomic-ready barrier', candidate: 'Production negotiated OpenAI Chat-to-Anthropic translated continuation', resources: 'Client, provider, gateway or relay, oracle and instrumentation in arm process; PostgreSQL process excluded', exclusions: ['WAN/TLS', 'isolated gateway RSS', 'live-model quality', 'actual SDK process CPU'] };
const digest = (data) => createHash('sha256').update(data).digest('hex');
const fileHash = (path) => digest(readFileSync(path));
const readJSON = (path) => JSON.parse(readFileSync(path, 'utf8'));
const sorted = (values) => [...values].sort((a, b) => a - b);
const median = (values) => { const a = sorted(values); return (a[(a.length - 1) >> 1] + a[a.length >> 1]) / 2; };
const command = (name, args, cwd = process.cwd()) => {
  const result = spawnSync(name, args, { cwd, encoding: 'utf8', maxBuffer: 32 << 20 });
  if (result.status !== 0) throw new Error(`${name} ${args[0]} failed: ${result.stderr?.trim() || result.stdout?.trim()}`);
  return result.stdout.trim();
};
const git = (args, cwd = process.cwd()) => command('git', args, cwd);
const gitBlobHash = (revision, path) => {
  const result = spawnSync('git', ['show', `${revision}:${path}`], { maxBuffer: 16 << 20 });
  if (result.status !== 0) throw new Error(`Unreachable committed source: ${revision}:${path}`);
  return digest(result.stdout);
};
const optional = (path) => existsSync(path) ? readFileSync(path, 'utf8').trim() : null;
const check = (ok, message) => { if (!ok) throw new Error(message); };

export function deriveCriteria(baseline = readJSON(frozenReference), budgets = readJSON(frozenBudget)) {
  check(baseline.source_revision === anchor && budgets.baseline_revision === anchor, 'Historical reference revision changed');
  check(compareFrozenReference(baseline, budgets).length === 0, 'Frozen native reference no longer self-compares');
  check(baseline.harness_sha256 === budgets.harness_sha256 && baseline.runner_sha256 === budgets.runner_sha256, 'Frozen v1 method changed');
  const margins = {}, limits = {}, historicalMedians = {};
  for (const stratum of strata) for (const path of referencePaths) {
    const name = `${stratum}/${path}`;
    const observations = baseline.runs.filter((run) => run.name === name);
    check(observations.length === 3 && isDeepStrictEqual(observations.map((run) => run.repetition).sort(), [0, 1, 2]), `Missing historical runs: ${name}`);
    const metrics = Object.keys(budgets.maxima[name]).sort();
    margins[name] = {}; limits[name] = {}; historicalMedians[name] = {};
    for (const metric of metrics) {
      const values = observations.map((run) => run.metrics[metric]);
      const limit = budgets.maxima[name][metric];
      check(values.every((value) => Number.isFinite(value)) && Number.isFinite(limit), `Invalid historical metric: ${name}/${metric}`);
      const historicalMedian = median(values);
      const margin = limit - historicalMedian;
      check(margin > 0, `Nonpositive pre-candidate margin: ${name}/${metric}`);
      margins[name][metric] = margin;
      limits[name][metric] = limit;
      historicalMedians[name][metric] = historicalMedian;
    }
  }
  for (const stratum of strata) for (const metric of primaryMetrics) check(Number.isFinite(margins[`${stratum}/reference`][metric]), `Missing primary metric: ${stratum}/${metric}`);
  return {
    schema: 'openllmproxy.dev/continuation-barrier-paired-criteria/v2',
    historical_revision: anchor, historical_baseline_sha256: fileHash(frozenReference), historical_budget_sha256: fileHash(frozenBudget),
    seed, blocks_per_stratum: blocksPerStratum, samples_per_subrun: samples, reference_paths: referencePaths, strata,
    arm_orders: {
      forward: ['relay', 'gateway', 'reference', 'translated', 'translated', 'reference', 'gateway', 'relay'],
      reverse: ['translated', 'reference', 'gateway', 'relay', 'relay', 'gateway', 'reference', 'translated']
    },
    warmup: 'One complete checked workflow before each 24-workflow subrun, excluded from metrics and exact measured counts',
    primary_metrics: primaryMetrics,
    decision: 'Per metric 32 paired block differences, C mean of two minus B mean of two. Strict pass iff sorted d[21] < frozen M. B same-source absolute repeat control strict pass iff sorted a[21] < own frozen M. Median of 64 B subruns must be <= old L. All metrics, controls and exact semantics conjunctive; no block filtering or replacement.',
    interval: { paired_median_one_sided_upper_zero_based_index: 21, diagnostic_two_sided_lower_zero_based_index: 10, diagnostic_two_sided_upper_zero_based_index: 21, coverage_percent: 97.494877 },
    limits, historical_medians: historicalMedians, margins
  };
}
export function verifyCriteria(criteria = readJSON(criteriaPath)) {
  check(isDeepStrictEqual(criteria, deriveCriteria()), 'Paired criteria differ from unchanged pre-candidate evidence');
  return criteria;
}

function randomWords() {
  let counter = 0;
  return () => createHash('sha256').update(`${seed}:${counter++}`).digest().readUInt32BE(0);
}
function shuffle(values, word) {
  const output = [...values];
  for (let i = output.length - 1; i > 0; i--) {
    const j = word() % (i + 1);
    [output[i], output[j]] = [output[j], output[i]];
  }
  return output;
}
export function schedule(criteria = verifyCriteria()) {
  const word = randomWords();
  const blocks = [];
  for (const stratum of criteria.strata) {
    const orientations = shuffle([...Array(16).fill('forward'), ...Array(16).fill('reverse')], word);
    for (let block = 0; block < blocksPerStratum; block++) blocks.push({ stratum, block, orientation: orientations[block], arms: criteria.arm_orders[orientations[block]] });
  }
  return shuffle(blocks, word);
}

const referenceMetricNames = (name, criteria) => Object.keys(criteria.limits[name]).sort();
const candidateMetricNames = [
  'elapsed_ns', 'ns/op', 'process-cpu-ns/op', 'B/op', 'allocs/op', 'sampled-heap-growth-B',
  ...['workflow', 'first-event', 'tool-visible', 'action-ready'].flatMap((phase) => [50, 95, 99].map((p) => `${phase}-p${p}-us`))
].sort();
const rawPhases = { workflow: 'workflow_us', 'first-event': 'first_event_us', 'wire-tool': 'wire_tool_us', 'tool-visible': 'tool_visible_us', 'action-ready': 'action_ready_us', 'claim-commit': 'claim_us', 'dispatch-journal': 'journal_us', 'ready-commit': 'ready_us' };
export function validateRun(run, criteria = verifyCriteria()) {
  check(run && typeof run === 'object', 'Missing subrun');
  const parts = run.name?.split('/');
  check(parts?.length === 3 && strata.includes(`${parts[0]}/${parts[1]}`) && [...referencePaths, 'translated'].includes(parts[2]), 'Unknown workload');
  const translated = parts[2] === 'translated';
  check(Number.isInteger(run.block) && run.block >= 0 && run.block < blocksPerStratum && Number.isInteger(run.position) && run.position >= 0 && run.position < 8, 'Invalid block position');
  check(run.samples === 24 && run.dispatches === 48 && run.first_requests === 24 && run.next_requests === 24 && run.native_events === 456 && run.actions === 48 && run.rejected === 0, 'Lost, duplicate or rejected native work');
  check(run.history_bytes === (parts[0] === 'large' ? 262144 : 0), 'Changed history size');
  check(translated ? (run.first_turn_observations === 312 && run.final_observations === 48 && run.ready_checks === 24) : (run.first_turn_observations === 0 && run.final_observations === 0 && run.ready_checks === (parts[2] === 'reference' ? 24 : 0)), 'Changed client observation or ready-read count');
  const historicalState = parts[0] === 'large' ? 263050 : 904;
  check(translated ? (Number.isSafeInteger(run.state_bytes) && run.state_bytes > run.history_bytes && run.state_bytes <= (5 << 20)) : run.state_bytes === historicalState, 'Missing or changed encrypted dependency state');
  const expectedMetrics = translated ? candidateMetricNames : ['elapsed_ns', ...referenceMetricNames(run.name, criteria)].sort();
  check(isDeepStrictEqual(Object.keys(run.metrics ?? {}).sort(), expectedMetrics), 'Changed metric inventory');
  for (const value of Object.values(run.metrics)) check(Number.isFinite(value) && value >= 0, 'Invalid metric value');
  check(Math.abs(run.metrics['ns/op'] * 24 - run.metrics.elapsed_ns) < 0.01, 'Elapsed time and ns/op disagree');
  check(Array.isArray(run.observations) && run.observations.length === samples, 'Missing raw workflow timings');
  for (const observation of run.observations) {
    check(observation.events === 19 && observation.actions === 2, 'Changed per-workflow conservation');
    const tool = translated ? observation.tool_visible_us : observation.wire_tool_us;
    check(observation.first_event_us > 0 && observation.first_event_us <= tool && tool <= observation.action_ready_us && observation.action_ready_us <= observation.workflow_us, 'Client actionability order changed');
    if (parts[2] === 'reference') check(observation.claim_us > 0 && observation.journal_us > 0 && observation.ready_us > 0, 'Missing reference persistence phase');
  }
  for (const [phase, key] of Object.entries(rawPhases)) {
    if (translated && ['wire-tool', 'claim-commit', 'dispatch-journal', 'ready-commit'].includes(phase)) continue;
    if (!translated && phase === 'tool-visible') continue;
    if (!translated && parts[2] !== 'reference' && ['claim-commit', 'dispatch-journal', 'ready-commit'].includes(phase)) continue;
    const values = sorted(run.observations.map((observation) => observation[key]));
    for (const p of [50, 95, 99]) check(Math.abs(run.metrics[`${phase}-p${p}-us`] - values[Math.floor((samples - 1) * p / 100)]) < 0.000001, `Pooled or inconsistent percentile: ${phase}/p${p}`);
  }
  check(Number.isSafeInteger(run.gc_count) && run.gc_count >= 0 && Number.isSafeInteger(run.gc_pause_ns) && run.gc_pause_ns >= 0 && Number.isInteger(run.goroutines_before) && run.goroutines_before > 0 && Number.isInteger(run.goroutines_after) && run.goroutines_after > 0, 'Missing runtime diagnostics');
  check(Number.isSafeInteger(run.scheduler_waits) && run.scheduler_waits >= 0 && Number.isFinite(run.scheduler_p99_us) && run.scheduler_p99_us >= 0, 'Missing scheduler latency diagnostic');
  return run;
}

export function validateInventory(runs, mode, criteria = verifyCriteria()) {
  check(['baseline', 'paired'].includes(mode), 'Unknown capture mode');
  const expected = schedule(criteria).flatMap(({ stratum, block, arms }) => arms.flatMap((arm, position) => mode === 'baseline' && arm === 'translated' ? [] : [{ name: `${stratum}/${arm}`, block, position }]));
  check(Array.isArray(runs) && runs.length === expected.length, `Incomplete ${mode} inventory: ${runs?.length}/${expected.length}`);
  const stateSizes = new Map();
  for (let i = 0; i < expected.length; i++) {
    const run = validateRun(runs[i], criteria);
    check(run.name === expected[i].name && run.block === expected[i].block && run.position === expected[i].position, `Order, arm, block or subrun changed at ${i}`);
    const stateKey = `${run.name.split('/')[0]}/${run.name.split('/')[2]}`;
    if (stateSizes.has(stateKey)) check(stateSizes.get(stateKey) === run.state_bytes, `Encrypted state size varies across subruns: ${stateKey}`);
    else stateSizes.set(stateKey, run.state_bytes);
  }
  return true;
}

const blockRuns = (runs, stratum, block, path) => runs.filter((run) => run.name === `${stratum}/${path}` && run.block === block).sort((a, b) => a.position - b.position);
const estimate = (values) => { const a = sorted(values); return { block_differences: values, median: median(values), lower_median_bound: a[10], upper_median_bound: a[21] }; };
export function analyze(runs, mode, criteria = verifyCriteria()) {
  validateInventory(runs, mode, criteria);
  const failures = [], envelope = {}, controls = {}, primary = {};
  for (const stratum of strata) {
    for (const path of referencePaths) {
      const name = `${stratum}/${path}`;
      envelope[name] = {}; controls[name] = {};
      for (const metric of referenceMetricNames(name, criteria)) {
        const values = runs.filter((run) => run.name === name).map((run) => run.metrics[metric]);
        check(values.length === 64, `Missing B envelope: ${name}/${metric}`);
        const value = median(values), limit = criteria.limits[name][metric], margin = criteria.margins[name][metric];
        envelope[name][metric] = { median: value, frozen_limit: limit, passed: value <= limit };
        if (value > limit) failures.push(`B envelope ${name}/${metric}: ${value} > ${limit}`);
        const absolute = Array.from({ length: 32 }, (_, block) => {
          const pair = blockRuns(runs, stratum, block, path);
          check(pair.length === 2, `Incomplete B control pair: ${name}/${block}`);
          return Math.abs(pair[1].metrics[metric] - pair[0].metrics[metric]);
        });
        const bound = estimate(absolute);
        controls[name][metric] = { ...bound, frozen_margin: margin, passed: bound.upper_median_bound < margin };
        if (!(bound.upper_median_bound < margin)) failures.push(`B control ${name}/${metric}: ${bound.upper_median_bound} >= ${margin}`);
      }
    }
    if (mode === 'paired') {
      const reference = `${stratum}/reference`, candidate = `${stratum}/translated`;
      primary[stratum] = {};
      for (const metric of primaryMetrics) {
        const differences = Array.from({ length: 32 }, (_, block) => {
          const b = blockRuns(runs, stratum, block, 'reference');
          const c = blockRuns(runs, stratum, block, 'translated');
          check(b.length === 2 && c.length === 2, `Incomplete paired block: ${stratum}/${block}`);
          return (c[0].metrics[metric] + c[1].metrics[metric] - b[0].metrics[metric] - b[1].metrics[metric]) / 2;
        });
        const bound = estimate(differences), margin = criteria.margins[reference][metric];
        primary[stratum][metric] = { ...bound, frozen_margin: margin, frozen_limit: criteria.limits[reference][metric], historical_median: criteria.historical_medians[reference][metric], passed: bound.upper_median_bound < margin };
        if (!(bound.upper_median_bound < margin)) failures.push(`C paired ${candidate}/${metric}: ${bound.upper_median_bound} >= ${margin}`);
      }
    }
  }
  const b = runs.filter((run) => !run.name.endsWith('/translated'));
  const c = runs.filter((run) => run.name.endsWith('/translated'));
  const sum = (group, key) => group.reduce((total, run) => total + run[key], 0);
  return { passed: failures.length === 0, failures, envelope, controls, primary,
    totals: {
      reference_workflows: sum(b, 'samples'), candidate_workflows: sum(c, 'samples'),
      native_dispatches: sum(runs, 'dispatches'), native_events: sum(runs, 'native_events'),
      fixture_actions: sum(runs, 'actions'), rejected: sum(runs, 'rejected'),
      reference_ready_reads: sum(b, 'ready_checks'), candidate_ready_reads: sum(c, 'ready_checks'),
      candidate_first_turn_observations: sum(c, 'first_turn_observations'), candidate_final_observations: sum(c, 'final_observations')
    } };
}

const hardware = () => ({ os: platform(), architecture: arch(), kernel: release(), cpu: cpus()[0]?.model, logical_cpus: cpus().length, total_memory_bytes: totalmem(), cpu_quota: optional('/sys/fs/cgroup/cpu.max'), memory_limit: optional('/sys/fs/cgroup/memory.max') });
const load = () => ({ loadavg: optional('/proc/loadavg'), cpu_pressure: optional('/proc/pressure/cpu') });
const toolchain = () => command('go', ['version']);
const buildEnvironment = () => command('go', ['env', 'GOFLAGS', 'GOAMD64', 'GOARCH', 'GOOS']);
const sourceRevision = (cwd) => git(['rev-parse', 'HEAD'], cwd);
const clean = (cwd) => check(git(['status', '--short'], cwd) === '', `Dirty checkout: ${cwd}`);
const tracked = (cwd, path) => git(['ls-files', '--error-unmatch', path], cwd);

function verifyReferenceCheckout(root) {
  clean(root);
  const changed = git(['diff', '--name-only', anchor, 'HEAD'], root).split('\n').filter(Boolean);
  check(isDeepStrictEqual(changed, [referenceHarness]), 'Historical B product or oracle changed beyond measurement overlay');
  check(fileHash(join(root, referenceHarness)) === fileHash(referenceHarness), 'Historical B measurement overlay differs');
  const frozen = readJSON(frozenReference);
  check(fileHash(join(root, 'tests/integration/continuation_barrier_benchmark_test.go')) === frozen.harness_sha256, 'Historical B v1 oracle differs');
  for (const path of ['tests/fidelity/reference.go', 'tests/fixtures/fidelity/v1/anthropic-tool-next-request.json', 'tests/fixtures/fidelity/v1/anthropic-tool-workflow.sse']) {
    check(fileHash(join(root, path)) === fileHash(path), `Native fixture differs across B/C: ${path}`);
  }
}

function buildBinary(root, path) {
  command('go', ['test', '-mod=readonly', '-tags=integration', '-c', '-o', path, './tests/integration'], root);
}
function launchArm(root, binary, testName, role) {
  const child = spawn(binary, ['-test.run', `^${testName}$`, '-test.v', '-test.timeout=2h'], { cwd: root, env: { ...process.env, ...runtimeEnvironment }, stdio: ['pipe', 'pipe', 'pipe'] });
  let buffer = '', stderr = '', setup, ready = false, pending, dead;
  let readyResolve, readyReject;
  const readyPromise = new Promise((resolveReady, rejectReady) => { readyResolve = resolveReady; readyReject = rejectReady; });
  let exitResolve, exitReject;
  const exitPromise = new Promise((resolveExit, rejectExit) => { exitResolve = resolveExit; exitReject = rejectExit; });
  const fail = (error) => { if (dead) return; dead = error; readyReject(error); if (pending) { pending.reject(error); pending = undefined; } };
  child.stderr.on('data', (chunk) => { stderr += chunk.toString(); });
  child.stdout.on('data', (chunk) => {
    buffer += chunk.toString();
    let newline;
    while ((newline = buffer.indexOf('\n')) !== -1) {
      const line = buffer.slice(0, newline); buffer = buffer.slice(newline + 1);
      if (line.startsWith('PAIRED_BARRIER_SETUP ')) {
        try { setup = JSON.parse(line.slice('PAIRED_BARRIER_SETUP '.length)); }
        catch (error) { fail(error); return; }
      }
      if (line === `PAIRED_BARRIER_READY ${role}`) { ready = true; readyResolve(setup); }
      if (line.startsWith('PAIRED_BARRIER_RUN ')) {
        if (!pending) { fail(new Error(`Unsolicited ${role} subrun`)); return; }
        try { pending.resolve(JSON.parse(line.slice('PAIRED_BARRIER_RUN '.length))); } catch (error) { pending.reject(error); }
        pending = undefined;
      }
    }
  });
  child.stdin.on('error', fail);
  child.on('error', (error) => { fail(error); exitReject(error); });
  child.on('exit', (code, signal) => {
    if (!ready || code !== 0) {
      const error = new Error(`${role} test process exited ${code ?? signal}: ${stderr.slice(-4096)}`);
      fail(error); exitReject(error);
    } else exitResolve();
  });
  const timeout = setTimeout(() => fail(new Error(`${role} benchmark startup timed out`)), 120000);
  return {
    async ready() { try { return await readyPromise; } finally { clearTimeout(timeout); } },
    async run(work) {
      if (dead) throw dead;
      check(ready && !pending, `${role} process not ready`);
      const answer = new Promise((resolveRun, rejectRun) => { pending = { resolve: resolveRun, reject: rejectRun }; });
      child.stdin.write(`${JSON.stringify(work)}\n`);
      return answer;
    },
    close() { child.stdin.end(); return exitPromise; },
    kill() { child.kill(); },
    get stderr() { return stderr; }
  };
}

function assertConditions(bSetup, cSetup) {
  const frozen = readJSON(frozenReference);
  const storage = frozen.storage;
  const valid = (setup) => setup?.postgresql_server_version === storage.postgresql_server_version && setup?.postgresql_tls === storage.postgresql_tls && typeof setup?.database_name === 'string' && setup.database_name.startsWith('access_');
  check(valid(bSetup) && (!cSetup || valid(cSetup)), 'PostgreSQL version, TLS or isolated database changed');
  if (cSetup) check(bSetup.database_name !== cSetup.database_name, 'B and C share a benchmark database');
  check(isDeepStrictEqual(hardware(), frozen.hardware), 'Hardware, kernel, quota or memory differs from historical reference');
  check(toolchain() === frozen.toolchain && buildEnvironment() === frozen.go_build_environment, 'Go toolchain or build environment changed');
}

async function capture(mode, output, baselinePath) {
  const prescribedOutput = resolve(`docs/evidence/fidelity-performance/paired-barrier-v2/${mode === 'baseline' ? 'baseline.json' : 'candidate.json'}`);
  check(resolve(output) === prescribedOutput, 'Use the preregistered write-once artifact path');
  check(!existsSync(output) && !existsSync(`${output}.failed.json`), 'Refusing a second or overwritten paired attempt');
  const criteria = verifyCriteria();
  clean(process.cwd()); tracked(process.cwd(), criteriaPath);
  const bRoot = resolve(process.env.OLP_PAIRED_B_ROOT || '/tmp/olp-worktrees/paired-barrier-reference-v2');
  verifyReferenceCheckout(bRoot);
  let baseline;
  if (mode === 'paired') {
    check(baselinePath && existsSync(baselinePath), 'Committed B-only baseline is required before C measurement');
    tracked(process.cwd(), baselinePath);
    baseline = readJSON(baselinePath);
    check(baseline.mode === 'baseline' && baseline.analysis?.passed && baseline.criteria_sha256 === fileHash(criteriaPath) && baseline.runner_sha256 === fileHash(runnerPath) && baseline.reference_harness_sha256 === fileHash(referenceHarness), 'Frozen B-only baseline or method is unavailable/failed');
    validateArtifact(baseline, 'baseline', criteria);
    check(baseline.method_revision && baseline.method_revision !== sourceRevision(process.cwd()), 'C must follow the committed pre-candidate method');
    const ancestor = spawnSync('git', ['merge-base', '--is-ancestor', baseline.method_revision, 'HEAD']);
    check(ancestor.status === 0, 'Frozen B-only method is not an ancestor of candidate C');
    const changedProduct = git(['diff', '--name-only', baseline.method_revision, 'HEAD']).split('\n').some((path) => path.startsWith('internal/') && path.endsWith('.go'));
    check(changedProduct, 'No post-baseline product implementation change is present');
  }
  const bBinary = join(tmpdir(), `olp-paired-barrier-b-${process.pid}`);
  const cBinary = join(tmpdir(), `olp-paired-barrier-c-${process.pid}`);
  buildBinary(bRoot, bBinary);
  if (mode === 'paired') buildBinary(process.cwd(), cBinary);
  const b = launchArm(bRoot, bBinary, 'TestPairedBarrierReferenceV2', 'reference');
  const c = mode === 'paired' ? launchArm(process.cwd(), cBinary, 'TestPairedBarrierCandidateV2', 'translated') : null;
  const artifact = {
    schema, mode, started_at: new Date().toISOString(), completed_at: null,
    criteria_sha256: fileHash(criteriaPath), runner_sha256: fileHash(runnerPath),
    historical_baseline_sha256: fileHash(frozenReference), historical_budget_sha256: fileHash(frozenBudget),
    reference_product_revision: anchor, reference_overlay_revision: sourceRevision(bRoot), candidate_revision: c ? sourceRevision(process.cwd()) : null,
    method_revision: mode === 'baseline' ? sourceRevision(process.cwd()) : baseline.method_revision,
    reference_harness_sha256: fileHash(referenceHarness), candidate_harness_sha256: c ? fileHash(candidateHarness) : null,
    original_oracle_sha256: fileHash('tests/integration/continuation_barrier_benchmark_test.go'),
    fixture_sha256: Object.fromEntries(corpusPaths.filter((path) => existsSync(path)).map((path) => [path, fileHash(path)])),
    candidate_workflow_sha256: c ? fileHash('tests/integration/continuation_candidate_benchmark_test.go') : null,
    toolchain: toolchain(), go_build_environment: buildEnvironment(), runtime_environment: runtimeEnvironment,
    hardware: hardware(), system_load: { before: load(), after: null }, storage: null, schedule: schedule(criteria),
    blocks: [], runs: [], analysis: null, error: null,
    conditions
  };
  try {
    const [bSetup, cSetup] = await Promise.all([b.ready(), c ? c.ready() : Promise.resolve(null)]);
    assertConditions(bSetup, cSetup);
    artifact.storage = { reference: bSetup, candidate: cSetup };
    for (const block of artifact.schedule) {
      const diagnostic = { stratum: block.stratum, block: block.block, orientation: block.orientation, before: load(), after: null };
      for (let position = 0; position < block.arms.length; position++) {
        const arm = block.arms[position];
        if (mode === 'baseline' && arm === 'translated') continue;
        const processArm = arm === 'translated' ? c : b;
        const run = await processArm.run({ name: `${block.stratum}/${arm}`, block: block.block, position });
        validateRun(run, criteria);
        artifact.runs.push(run);
      }
      diagnostic.after = load(); artifact.blocks.push(diagnostic);
    }
    artifact.analysis = analyze(artifact.runs, mode, criteria);
  } catch (error) {
    artifact.error = error?.message || String(error);
  } finally {
    try { await Promise.all([b.close(), ...(c ? [c.close()] : [])]); }
    catch (error) { artifact.error ||= error?.message || String(error); }
    artifact.completed_at = new Date().toISOString(); artifact.system_load.after = load();
    const destination = artifact.error ? `${output}.failed.json` : output;
    writeFileSync(destination, `${JSON.stringify(artifact, null, 2)}\n`, { flag: 'wx' });
    if (artifact.error) { b.kill(); c?.kill(); throw new Error(`${artifact.error}; retained ${destination}`); }
    console.log(`${mode}: ${artifact.runs.length} complete subruns, ${artifact.analysis.passed ? 'PASS' : 'INCONCLUSIVE'}; ${destination}`);
    if (!artifact.analysis.passed) process.exitCode = 1;
  }
}

export function validateArtifact(artifact, mode, criteria = verifyCriteria()) {
  check(artifact.schema === schema && artifact.mode === mode && artifact.error === null, 'Incomplete paired artifact');
  check(artifact.criteria_sha256 === fileHash(criteriaPath) && artifact.runner_sha256 === fileHash(runnerPath) && artifact.historical_baseline_sha256 === fileHash(frozenReference) && artifact.historical_budget_sha256 === fileHash(frozenBudget), 'Frozen paired method changed');
  check(artifact.reference_product_revision === anchor && artifact.original_oracle_sha256 === readJSON(frozenReference).harness_sha256 && artifact.reference_harness_sha256 === fileHash(referenceHarness), 'Reference product/oracle changed');
  check(artifact.method_revision && gitBlobHash(artifact.method_revision, runnerPath) === artifact.runner_sha256 && gitBlobHash(artifact.method_revision, criteriaPath) === artifact.criteria_sha256, 'Pre-candidate method commit changed');
  check(isDeepStrictEqual(git(['diff', '--name-only', anchor, artifact.reference_overlay_revision]).split('\n').filter(Boolean), [referenceHarness]), 'B overlay touched product or oracle');
  check(gitBlobHash(artifact.reference_overlay_revision, referenceHarness) === artifact.reference_harness_sha256, 'Recorded B overlay changed');
  check(isDeepStrictEqual(artifact.conditions, conditions), 'Measurement conditions changed');
  check(isDeepStrictEqual(Object.keys(artifact.fixture_sha256 ?? {}).sort(), corpusPaths.slice().sort()), 'Fixture hash inventory changed');
  for (const path of corpusPaths) {
    check(gitBlobHash(anchor, path) === artifact.fixture_sha256[path], `Fixture or workflow oracle changed: ${path}`);
  }
  check(mode === 'baseline' ? artifact.candidate_revision === null && artifact.candidate_harness_sha256 === null && artifact.candidate_workflow_sha256 === null : artifact.candidate_revision && artifact.candidate_harness_sha256 === fileHash(candidateHarness) && gitBlobHash(artifact.candidate_revision, candidateHarness) === artifact.candidate_harness_sha256 && artifact.candidate_workflow_sha256 === gitBlobHash(artifact.candidate_revision, 'tests/integration/continuation_candidate_benchmark_test.go'), 'Candidate source/harness changed');
  if (mode === 'paired') {
    const ancestor = spawnSync('git', ['merge-base', '--is-ancestor', artifact.method_revision, artifact.candidate_revision]);
    check(ancestor.status === 0, 'Candidate does not descend from frozen method');
    check(git(['diff', '--name-only', artifact.method_revision, artifact.candidate_revision]).split('\n').some((path) => path.startsWith('internal/') && path.endsWith('.go')), 'No candidate product implementation change after method');
  }
  check(isDeepStrictEqual(artifact.schedule, schedule(criteria)), 'Schedule changed');
  check(artifact.blocks?.length === strata.length * blocksPerStratum && artifact.blocks.every((block, i) => block.stratum === artifact.schedule[i].stratum && block.block === artifact.schedule[i].block && block.orientation === artifact.schedule[i].orientation && block.before && block.after), 'Missing block diagnostics');
  check(isDeepStrictEqual(artifact.hardware, readJSON(frozenReference).hardware) && artifact.toolchain === readJSON(frozenReference).toolchain && artifact.go_build_environment === readJSON(frozenReference).go_build_environment && isDeepStrictEqual(artifact.runtime_environment, runtimeEnvironment), 'Incomparable host/runtime');
  check(artifact.storage?.reference?.postgresql_server_version === '180006' && artifact.storage.reference.postgresql_tls === false && (mode === 'baseline' ? artifact.storage.candidate === null : artifact.storage.candidate?.postgresql_server_version === '180006' && artifact.storage.candidate.postgresql_tls === false && artifact.storage.candidate.database_name !== artifact.storage.reference.database_name), 'Changed or shared storage');
  const result = analyze(artifact.runs, mode, criteria);
  check(isDeepStrictEqual(artifact.analysis, result), 'Saved paired calculation differs');
  return result;
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const [action, path, extra] = process.argv.slice(2);
    if (action === 'derive-criteria' && path && !extra) writeFileSync(path, `${JSON.stringify(deriveCriteria(), null, 2)}\n`, { flag: 'wx' });
    else if (action === 'record-baseline' && path && !extra) await capture('baseline', path);
    else if (action === 'record-paired' && path && extra) await capture('paired', path, extra);
    else if (action === 'compare' && path && !extra) {
      const result = validateArtifact(readJSON(path), readJSON(path).mode);
      console.log(JSON.stringify({ passed: result.passed, failures: result.failures, totals: result.totals }, null, 2));
      if (!result.passed) process.exitCode = 1;
    } else throw new Error('Usage: derive-criteria <new-file> | record-baseline <new-file> | record-paired <new-file> <committed-baseline> | compare <artifact>');
  } catch (error) { console.error(error.message); process.exitCode = 1; }
}
