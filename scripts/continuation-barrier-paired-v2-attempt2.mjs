#!/usr/bin/env node
// Prospective, additive paired experiment. Frozen v1 evidence and its runner are
// read-only inputs; this runner never edits them or changes the product oracle.
import { spawn, spawnSync } from 'node:child_process';
import { createHash, randomUUID } from 'node:crypto';
import { closeSync, existsSync, fsyncSync, mkdtempSync, openSync, readFileSync, statSync, writeFileSync, writeSync } from 'node:fs';
import { arch, cpus, platform, release, totalmem, tmpdir } from 'node:os';
import { isAbsolute, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { isDeepStrictEqual } from 'node:util';
import { compare as compareFrozenReference } from './continuation-barrier-benchmark.mjs';

export const anchor = '29e18268ac23f7290d1881cbda3af2cc2ccab018';
export const schema = 'openllmproxy.dev/continuation-barrier-paired/v2-attempt2';
export const seed = 'OLP-BARRIER-V2-A2-2026-09-23-32x4';
export const attemptId = 'barrier-v2-attempt2-authority-poll';
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
export const criteriaPath = 'docs/evidence/fidelity-performance/paired-barrier-v2-attempt2/criteria.json';
export const bOnlyPath = 'docs/evidence/fidelity-performance/paired-barrier-v2-attempt2/baseline.json';
export const runnerPath = 'scripts/continuation-barrier-paired-v2-attempt2.mjs';
export const priorFailedPath = 'docs/evidence/fidelity-performance/paired-barrier-v2/baseline.json.failed.json';
export const priorJournalPath = 'docs/evidence/fidelity-performance/paired-barrier-v2/baseline.json.journal.jsonl';
export const corpusPaths = [
  'tests/integration/continuation_barrier_benchmark_test.go',
  'tests/fidelity/reference.go',
  'tests/fixtures/fidelity/v1/anthropic-tool-next-request.json',
  'tests/fixtures/fidelity/v1/anthropic-tool-workflow.sse'
];
export const candidateDependencies = [
  referenceHarness, candidateHarness,
  'tests/integration/continuation_barrier_benchmark_test.go',
  'tests/integration/continuation_candidate_benchmark_test.go',
  'tests/integration/access_test.go',
  'tests/integration/strict_generation_test.go',
  'tests/integration/fidelity_lifecycle_performance_test.go',
  'tests/integration/provider_parity_test.go',
  'tests/integration/provider_resources_test.go',
  'tests/integration/continuation_workflow_test.go',
  'tests/integration/responses_fixture_test.go',
  'tests/integration/services_test.go',
  'tests/integration/gateway_test.go',
  'tests/integration/route_fidelity_test.go',
  'tests/integration/resource_scope_test.go',
  'tests/fidelity/reference.go',
  'tests/fixtures/fidelity/fixtures.go',
  'tests/fixtures/fidelity/v1/anthropic-tool-next-request.json',
  'tests/fixtures/fidelity/v1/anthropic-tool-workflow.sse'
];
const runtimeEnvironment = { GOMAXPROCS: '4', GOGC: '100', GOMEMLIMIT: 'off', GODEBUG: '', OLP_PAIRED_BARRIER_COMMAND_MODE: '1' };
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
const gitTree = (revision) => git(['rev-parse', `${revision}^{tree}`]);
const gitBlobHashAt = (root, revision, path) => {
  const result = spawnSync('git', ['show', `${revision}:${path}`], { cwd: root, maxBuffer: 16 << 20 });
  if (result.status !== 0) throw new Error(`Unreachable committed source: ${revision}:${path}`);
  return digest(result.stdout);
};
const gitBlobHash = (revision, path) => gitBlobHashAt(process.cwd(), revision, path);
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
    schema: 'openllmproxy.dev/continuation-barrier-paired-criteria/v2-attempt2',
    attempt_id: attemptId,
    prior_failed_sha256: fileHash(priorFailedPath), prior_journal_sha256: fileHash(priorJournalPath),
    host_guard: hostGuard, materiality_reasons: materialityReasons,
    sequential_rule: 'One and only one replacement attempt is allowed for the diagnosed >60s measurement-harness authority expiry. Attempt 1 ended before any C observation, so no C hypothesis was tested and its one-sided alpha was not spent. Retain the original d_(22) 97.494877% bound, all L-H margins, B controls, sample floor and exact semantics. A2 B or C failure/inconclusive result ends this preregistered sequence; no further timed retry may be selected for a pass.',
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
export const hostGuard = {
  preflight_samples: 13, preflight_interval_ms: 5000,
  load1_exclusive_max: 2, cpu_some_avg10_percent_exclusive_max: 5,
  block_rule: 'Record raw whole-host, runner, B/C process, load and CPU pressure counters. PostgreSQL is an owned measured service in another process, so do not label host-minus-B/C CPU as unrelated automatically.',
  interference_rule: 'During collection only, an operator must immediately flag observed unrelated build, test or compute work. The runner fsyncs time/process/reason/load/pressure to the journal and invalidates the entire attempt before numeric analysis. No block is filtered and no retry is allowed.',
  unavailable_counters: 'Fail closed before or during capture.'
};
export const materialityReasons = {
  'unrelated-build': 'An unrelated build competes for host CPU, memory bandwidth or storage during the measured workflow.',
  'unrelated-test': 'An unrelated test suite competes for host CPU, memory bandwidth or storage during the measured workflow.',
  'unrelated-compute': 'An unrelated compute task materially competes with the measured workflow.'
};
let cachedHostHz;
const clockTicksPerSecond = () => {
  if (cachedHostHz !== undefined) return cachedHostHz;
  const hz = Number(command('getconf', ['CLK_TCK']));
  check(Number.isSafeInteger(hz) && hz > 0, 'Host clock-tick frequency is unavailable');
  cachedHostHz = hz;
  return cachedHostHz;
};
const numericTicks = (values) => values.map((value) => {
  const tick = Number(value);
  check(Number.isSafeInteger(tick) && tick >= 0, 'Invalid raw host CPU tick counter');
  return tick;
});
export function parseHostSnapshot(loadavgRaw, cpuPressureRaw, procStatRaw, observedAt, monotonicNs, runnerCPUus, processTicks, hz) {
  check(typeof loadavgRaw === 'string' && typeof cpuPressureRaw === 'string' && typeof procStatRaw === 'string', 'Missing required /proc host counters');
  const load1 = Number(loadavgRaw.trim().split(/\s+/)[0]);
  const pressure = cpuPressureRaw.match(/^some\s+avg10=([0-9.]+)/m);
  const someAvg10 = Number(pressure?.[1]);
  const cpuLine = procStatRaw.split('\n')[0];
  check(/^cpu\s+/.test(cpuLine), 'Missing aggregate /proc/stat CPU line');
  const ticks = numericTicks(cpuLine.trim().split(/\s+/).slice(1));
  check(ticks.length >= 8 && Number.isFinite(load1) && load1 >= 0 && pressure && Number.isFinite(someAvg10) && someAvg10 >= 0 && Number.isSafeInteger(hz) && hz > 0, 'Invalid host load, pressure or tick frequency');
  check(Number.isFinite(Date.parse(observedAt)) && /^\d+$/.test(monotonicNs) && Number.isSafeInteger(runnerCPUus) && runnerCPUus >= 0, 'Missing host observation clock or runner CPU');
  check(isDeepStrictEqual(Object.keys(processTicks ?? {}).sort(), ['reference', 'translated']), 'Missing measured-arm CPU counter inventory');
  for (const value of Object.values(processTicks)) check(value === null || Number.isSafeInteger(value) && value >= 0, 'Invalid measured-arm CPU ticks');
  return {
    observed_at: observedAt, monotonic_ns: monotonicNs, clock_hz: hz,
    loadavg_raw: loadavgRaw.trim(), load1,
    cpu_pressure_raw: cpuPressureRaw.trim(), cpu_some_avg10_percent: someAvg10,
    proc_stat_cpu_raw: cpuLine.trim(), host_busy_ticks: ticks[0] + ticks[1] + ticks[2] + ticks[5] + ticks[6] + ticks[7],
    host_total_ticks: ticks.reduce((sum, tick) => sum + tick, 0),
    runner_cpu_us: runnerCPUus, arm_cpu_ticks: processTicks
  };
}
function processStatFields(pid) {
  check(Number.isSafeInteger(pid) && pid > 0, 'Missing measured-arm process ID');
  const stat = readFileSync('/proc/' + pid + '/stat', 'utf8');
  const end = stat.lastIndexOf(')');
  check(end > 0, 'Invalid measured-arm /proc stat');
  return stat.slice(end + 2).trim().split(/\s+/);
}
function processCPUTicks(pid) {
  if (pid === null) return null;
  const fields = processStatFields(pid);
  return numericTicks([fields[11], fields[12]]).reduce((sum, ticks) => sum + ticks, 0);
}
const processStartTicks = (pid) => numericTicks([processStatFields(pid)[19]])[0];
function hostSnapshot(arms = { reference: null, translated: null }) {
  const cpu = process.cpuUsage();
  return parseHostSnapshot(
    readFileSync('/proc/loadavg', 'utf8'), readFileSync('/proc/pressure/cpu', 'utf8'), readFileSync('/proc/stat', 'utf8'),
    new Date().toISOString(), process.hrtime.bigint().toString(), cpu.user + cpu.system,
    { reference: processCPUTicks(arms.reference), translated: processCPUTicks(arms.translated) },
    clockTicksPerSecond()
  );
}
export function validateHostSnapshot(snapshot, requireArms = false) {
  check(snapshot && isDeepStrictEqual(snapshot, parseHostSnapshot(snapshot.loadavg_raw, snapshot.cpu_pressure_raw, snapshot.proc_stat_cpu_raw, snapshot.observed_at, snapshot.monotonic_ns, snapshot.runner_cpu_us, snapshot.arm_cpu_ticks, snapshot.clock_hz)), 'Host counter receipt changed');
  if (requireArms) check(snapshot.arm_cpu_ticks.reference !== null && (requireArms === 'paired' ? snapshot.arm_cpu_ticks.translated !== null : snapshot.arm_cpu_ticks.translated === null), 'Missing measured-arm CPU receipt');
  return true;
}
export function validateQuietPreflight(samples) {
  check(Array.isArray(samples) && samples.length === hostGuard.preflight_samples, 'Complete 60-second quiet preflight is required');
  for (let i = 0; i < samples.length; i++) {
    validateHostSnapshot(samples[i]);
    check(samples[i].clock_hz === samples[0].clock_hz, 'Host clock frequency changed during quiet preflight');
    check(samples[i].arm_cpu_ticks.reference === null && samples[i].arm_cpu_ticks.translated === null, 'Timed arm started during quiet preflight');
    check(samples[i].load1 < hostGuard.load1_exclusive_max && samples[i].cpu_some_avg10_percent < hostGuard.cpu_some_avg10_percent_exclusive_max, 'Quiet preflight host load or CPU pressure exceeded its frozen limit');
    if (i) check(BigInt(samples[i].monotonic_ns) - BigInt(samples[i - 1].monotonic_ns) >= BigInt(hostGuard.preflight_interval_ms) * 1000000n, 'Quiet preflight sample spacing was shortened');
  }
  return true;
}
export function validateBlockHostDiagnostics(before, after, mode) {
  check(mode === 'baseline' || mode === 'paired', 'Unknown host diagnostic mode');
  validateHostSnapshot(before, mode);
  validateHostSnapshot(after, mode);
  check(BigInt(after.monotonic_ns) > BigInt(before.monotonic_ns) && after.clock_hz === before.clock_hz && after.host_total_ticks >= before.host_total_ticks && after.host_busy_ticks >= before.host_busy_ticks && after.runner_cpu_us >= before.runner_cpu_us && after.arm_cpu_ticks.reference >= before.arm_cpu_ticks.reference, 'Host or reference CPU counters regressed within a block');
  if (mode === 'paired') check(after.arm_cpu_ticks.translated >= before.arm_cpu_ticks.translated, 'Candidate CPU counters regressed within a block');
  return true;
}
async function quietPreflight() {
  const samples = [hostSnapshot()];
  for (let i = 1; i < hostGuard.preflight_samples; i++) {
    const target = BigInt(samples.at(-1).monotonic_ns) + BigInt(hostGuard.preflight_interval_ms) * 1000000n;
    while (process.hrtime.bigint() < target) {
      const remainingMs = Number((target - process.hrtime.bigint()) / 1000000n);
      await new Promise((resolveWait) => setTimeout(resolveWait, Math.max(1, remainingMs)));
    }
    samples.push(hostSnapshot());
  }
  validateQuietPreflight(samples);
  return samples;
}
const toolchain = () => command('go', ['version']);
const buildEnvironment = () => command('go', ['env', 'GOFLAGS', 'GOAMD64', 'GOARCH', 'GOOS']);
const sourceRevision = (cwd) => git(['rev-parse', 'HEAD'], cwd);
const clean = (cwd) => check(git(['status', '--short'], cwd) === '', `Dirty checkout: ${cwd}`);
const tracked = (cwd, path) => git(['ls-files', '--error-unmatch', path], cwd);
export function verifyCandidateOracle(methodRevision, hashes = { harness: fileHash(candidateHarness), workflow: fileHash('tests/integration/continuation_candidate_benchmark_test.go') }) {
  check(hashes.harness === gitBlobHash(methodRevision, candidateHarness) && hashes.workflow === gitBlobHash(methodRevision, 'tests/integration/continuation_candidate_benchmark_test.go'), 'Candidate measurement or SDK-equivalent oracle changed after method freeze');
  return true;
}
export function verifyCandidateDependencies(methodRevision, hashes = Object.fromEntries(candidateDependencies.map((path) => [path, fileHash(path)]))) {
  check(isDeepStrictEqual(Object.keys(hashes ?? {}).sort(), candidateDependencies.slice().sort()), 'Candidate measurement dependency inventory changed');
  for (const path of candidateDependencies) check(hashes[path] === gitBlobHash(methodRevision, path), `Candidate measurement dependency changed after method freeze: ${path}`);
  return true;
}
const isAncestor = (older, newer, root = process.cwd()) => older !== newer && spawnSync('git', ['merge-base', '--is-ancestor', older, newer], { cwd: root }).status === 0;
export const isContinuationProductSource = (path) => /^internal\/(gateway|resources|interaction)\/.+\.go$/.test(path) && !path.endsWith('_test.go');
export function committedBaselineLineage(path, candidateRevision, root = process.cwd()) {
  const journalPath = journalPathFor(path);
  check(!isAbsolute(path) && existsSync(join(root, path)) && existsSync(join(root, journalPath)), 'Missing committed B-only artifact or reserved journal');
  const history = git(['log', '--format=%H', '--', path], root).split('\n').filter(Boolean);
  check(history.length === 1 && /^[a-f0-9]{40,64}$/.test(history[0]), 'B-only artifact was edited after its write-once creation');
  const baselineCommit = history[0];
  const journalHistory = git(['log', '--format=%H', '--', journalPath], root).split('\n').filter(Boolean);
  check(journalHistory.length === 1 && journalHistory[0] === baselineCommit, 'B-only reservation journal was not committed with its write-once artifact');
  const sha256 = digest(readFileSync(join(root, path)));
  const journalSha256 = digest(readFileSync(join(root, journalPath)));
  check(gitBlobHashAt(root, baselineCommit, path) === sha256, 'Working B-only artifact differs from its committed bytes');
  check(gitBlobHashAt(root, baselineCommit, journalPath) === journalSha256, 'Working B-only journal differs from its committed bytes');
  check(isAncestor(baselineCommit, candidateRevision, root), 'B-only artifact commit is not a strict ancestor of C');
  const intervening = git(['rev-list', '--reverse', '--ancestry-path', `${baselineCommit}..${candidateRevision}`], root).split('\n').filter(Boolean);
  const productChangeCommit = intervening.find((revision) => {
    if (!isAncestor(baselineCommit, revision, root)) return false;
    const changed = git(['diff-tree', '--root', '-m', '--no-commit-id', '--name-only', '-r', revision], root).split('\n');
    return changed.some(isContinuationProductSource);
  });
  check(productChangeCommit && isAncestor(baselineCommit, productChangeCommit, root) && (productChangeCommit === candidateRevision || isAncestor(productChangeCommit, candidateRevision, root)), 'No product-change commit strictly follows committed B-only evidence');
  return { b_only_path: path, b_only_sha256: sha256, b_only_journal_sha256: journalSha256, b_only_commit: baselineCommit, product_change_commit: productChangeCommit };
}
export function verifyPairedBaselineBinding(artifact, root = process.cwd()) {
  const binding = committedBaselineLineage(artifact.b_only_path, artifact.candidate_revision, root);
  check(artifact.b_only_sha256 === binding.b_only_sha256 && artifact.b_only_journal_sha256 === binding.b_only_journal_sha256 && artifact.b_only_commit === binding.b_only_commit && artifact.product_change_commit === binding.product_change_commit, 'Paired artifact is not bound to committed B-only bytes, reservation journal and later product change');
  return JSON.parse(readFileSync(join(root, artifact.b_only_path), 'utf8'));
}

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

const executablePattern = /^[a-f0-9]{64}$/;
const buildArgs = (path) => ['test', '-mod=readonly', '-tags=integration', '-c', '-o', path, './tests/integration'];
const runArgs = (role) => ['-test.run', role === 'reference' ? '^TestPairedBarrierReferenceV2$' : '^TestPairedBarrierCandidateV2$', '-test.v', '-test.timeout=2h'];
export function assertFreshBuildOutput(path) {
  check(isAbsolute(path) && !existsSync(path), `Refusing reused benchmark executable output: ${path}`);
  return true;
}
export function attestBuild(root, path, role, startedAt = new Date().toISOString()) {
  check(['reference', 'translated'].includes(role) && isAbsolute(path) && existsSync(path), 'Missing built benchmark executable');
  const stat = statSync(path);
  check(stat.isFile() && stat.size > 0 && (stat.mode & 0o111) !== 0, 'Benchmark output is not an executable file');
  const revision = sourceRevision(root);
  return {
    role, source_revision: revision, source_tree: gitTree(revision), working_directory: root,
    build_command: ['go', ...buildArgs(path)], run_command: [path, ...runArgs(role)],
    executable_path: path, executable_size_bytes: stat.size,
    executable_sha256: fileHash(path), executable_sha256_after: null,
    toolchain: toolchain(), go_build_environment: buildEnvironment(), started_at: startedAt, completed_at: new Date().toISOString()
  };
}
function buildBinary(root, path, role) {
  assertFreshBuildOutput(path);
  const startedAt = new Date().toISOString();
  command('go', buildArgs(path), root);
  return attestBuild(root, path, role, startedAt);
}
function attestUnchangedBinary(build) {
  check(existsSync(build.executable_path), 'Built benchmark executable vanished during measurement');
  build.executable_sha256_after = fileHash(build.executable_path);
  check(build.executable_sha256_after === build.executable_sha256 && statSync(build.executable_path).size === build.executable_size_bytes, 'Benchmark executable changed during measurement');
}
export function validateBuildEvidence(build, role, revision, expectedToolchain, expectedGoBuildEnvironment) {
  check(build?.role === role && build.source_revision === revision && isAbsolute(build.working_directory ?? '') && isAbsolute(build.executable_path ?? ''), 'Unknown benchmark build source');
  check(build.source_tree === gitTree(revision), 'Benchmark build source tree changed');
  check(isDeepStrictEqual(build.build_command, ['go', ...buildArgs(build.executable_path)]) && isDeepStrictEqual(build.run_command, [build.executable_path, ...runArgs(role)]), 'Benchmark build or execution command changed');
  check(Number.isSafeInteger(build.executable_size_bytes) && build.executable_size_bytes > 0 && executablePattern.test(build.executable_sha256 ?? '') && build.executable_sha256_after === build.executable_sha256, 'Missing, changed or malformed executable SHA-256');
  check(build.toolchain === expectedToolchain && build.go_build_environment === expectedGoBuildEnvironment && Number.isFinite(Date.parse(build.started_at)) && Number.isFinite(Date.parse(build.completed_at)) && Date.parse(build.started_at) <= Date.parse(build.completed_at), 'Benchmark build environment or chronology changed');
  if (existsSync(build.executable_path)) check(fileHash(build.executable_path) === build.executable_sha256 && statSync(build.executable_path).size === build.executable_size_bytes, 'Retained benchmark executable differs from measured SHA-256');
  return true;
}
const journalPathFor = (output) => `${output}.journal.jsonl`;
const repositoryRelative = (path) => relative(process.cwd(), resolve(path));
const interferencePathFor = (output) => output + '.interference.json';
export function validateMaterialInterferenceNotice(notice) {
  check(notice && Object.hasOwn(materialityReasons, notice.reason), 'Unknown or unregistered interference rationale');
  check(Number.isSafeInteger(notice.pid) && notice.pid > 0 && typeof notice.process === 'string' && /^[A-Za-z0-9._+-]{1,160}$/.test(notice.process), 'Interference process identity must be a bounded executable basename');
  check(typeof notice.observed_at === 'string' && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/.test(notice.observed_at) && Number.isFinite(Date.parse(notice.observed_at)), 'Interference observation time is missing');
  return true;
}
export function materialInterferenceEvent(notice, host) {
  validateMaterialInterferenceNotice(notice);
  validateHostSnapshot(host);
  return {
    event: 'material_interference', observed_at: notice.observed_at, recorded_at: new Date().toISOString(),
    process: { pid: notice.pid, name: notice.process }, reason: notice.reason,
    rationale: materialityReasons[notice.reason], host
  };
}
function flagMaterialInterference(output, reason, pidText, processName) {
  check([bOnlyPath, 'docs/evidence/fidelity-performance/paired-barrier-v2-attempt2/candidate.json'].includes(output), 'Unknown paired artifact for interference flag');
  const journalPath = journalPathFor(output);
  check(existsSync(journalPath), 'No active paired attempt reservation');
  const reservation = JSON.parse(readFileSync(journalPath, 'utf8').split('\n')[0]);
  check(reservation.event === 'reserved' && Number.isSafeInteger(reservation.runner_pid) && reservation.runner_pid > 0 && Number.isSafeInteger(reservation.runner_start_ticks) && reservation.runner_start_ticks === processStartTicks(reservation.runner_pid) && !existsSync(output) && !existsSync(`${output}.failed.json`), 'Paired attempt is not collecting under its original runner process');
  const notice = { reason, pid: Number(pidText), process: processName, observed_at: new Date().toISOString() };
  validateMaterialInterferenceNotice(notice);
  writeFileSync(interferencePathFor(output), JSON.stringify(notice) + '\n', { flag: 'wx', mode: 0o600 });
  process.kill(reservation.runner_pid, 'SIGUSR2');
}
export const goDiagnosticTailBytes = 64 << 10;
export function redactGoDiagnostic(value) {
  return String(value)
    .replace(/postgres(?:ql)?:\/\/[^\s"'<>]+/gi, '[redacted-database-url]')
    .replace(/([?&]key=)[^&\s"'<>]+/gi, '$1[redacted-query-key]')
    .replace(/(\\?["'](?:Authorization|X-Api-Key|X-Goog-Api-Key|Cookie|Set-Cookie)\\?["']\s*:\s*\\?["'])[^"'\\\r\n]*(\\?["'])/gi, '$1[redacted-header-value]$2')
    .replace(/\b(Authorization|X-Api-Key|X-Goog-Api-Key|Cookie|Set-Cookie)\s*[:=]\s*(["'])[^"'\r\n]*\2/gi, '$1: $2[redacted-header-value]$2')
    .replace(/(^|\n)([ \t]*(?:Authorization|X-Api-Key|X-Goog-Api-Key|Cookie|Set-Cookie)[ \t]*:[ \t]*)[^\r\n]*/gim, '$1$2[redacted-header-value]')
    .replace(/\b(Cookie|Set-Cookie)\s*[:=]\s*[^\r\n\]\}]+/gi, '$1: [redacted-header-value]')
    .replace(/\b(Authorization|X-Api-Key|X-Goog-Api-Key)\s*[:=]\s*(?:\[[^\]\r\n]*\]|(?:(?:Bearer|Basic)\s+)?[^\s"';,\]\}]+)/gi, '$1: [redacted-header-value]')
    .replace(/\b(Bearer|Basic)\s+[A-Za-z0-9._~+/%=-]+/gi, '$1 [redacted-token]')
    .replace(/olp_[A-Za-z0-9_-]+/g, '[redacted-api-key]')
    .replace(/vendor-secret-[A-Za-z0-9_-]+/g, '[redacted-provider-secret]');
}
export function boundedGoDiagnosticTail(previous, next) {
  const sanitized = redactGoDiagnostic(next);
  const combined = Buffer.concat([Buffer.from(previous), Buffer.from(sanitized)]);
  return combined.subarray(Math.max(0, combined.length - goDiagnosticTailBytes)).toString('utf8');
}
export function newBoundedGoDiagnosticLines() {
  let pending = '', dropping = false, tail = '', truncated = false;
  const append = (line) => {
    truncated ||= Buffer.byteLength(tail) + Buffer.byteLength(line) > goDiagnosticTailBytes;
    tail = boundedGoDiagnosticTail(tail, line);
  };
  return {
    push(chunk) {
      pending += String(chunk);
      let newline;
      while ((newline = pending.indexOf('\n')) !== -1) {
        const line = pending.slice(0, newline + 1);
        pending = pending.slice(newline + 1);
        if (dropping) {
          append('[redacted-overlong-diagnostic-line]\n');
          dropping = false;
        } else append(line);
      }
      if (Buffer.byteLength(pending) > goDiagnosticTailBytes) {
        pending = '';
        dropping = true;
        truncated = true;
      }
    },
    finish() {
      if (dropping) append('[redacted-overlong-diagnostic-line]\n');
      else if (pending) append(pending);
      pending = '';
      dropping = false;
    },
    snapshot() { return { tail, truncated }; }
  };
}
export function assertFreshAttemptOutput(output) {
  check(!existsSync(output) && !existsSync(`${output}.failed.json`) && !existsSync(journalPathFor(output)), 'Refusing an overwritten or interrupted write-once paired attempt');
  return true;
}
function writeJournalEntry(fd, entry) {
  const bytes = Buffer.from(`${JSON.stringify(entry)}\n`);
  for (let offset = 0; offset < bytes.length;) offset += writeSync(fd, bytes, offset, bytes.length - offset);
  fsyncSync(fd);
  return digest(bytes);
}
export function reserveCaptureJournal(output, reservation) {
  assertFreshAttemptOutput(output);
  const path = journalPathFor(output);
  const fd = openSync(path, 'wx', 0o600);
  const id = randomUUID();
  const first = { ...reservation, event: 'reserved', journal_id: id, artifact_path: repositoryRelative(output) };
  try {
    const reservationSha256 = writeJournalEntry(fd, first);
    return { id, path: repositoryRelative(path), reservation_sha256: reservationSha256,
      append(entry) { writeJournalEntry(fd, { journal_id: id, ...entry }); },
      close() { closeSync(fd); } };
  } catch (error) { closeSync(fd); throw error; }
}
export function validateJournalArtifact(artifact, artifactPath) {
  const path = journalPathFor(artifactPath);
  check(artifact.journal_path === repositoryRelative(path) && existsSync(path) && existsSync(artifactPath), 'Missing reserved paired attempt journal or artifact');
  const raw = readFileSync(path, 'utf8');
  check(raw.endsWith('\n'), 'Truncated paired attempt journal');
  const lines = raw.trimEnd().split('\n');
  const entries = lines.map((line) => JSON.parse(line));
  const first = entries[0], last = entries.at(-1);
  check(first?.event === 'reserved' && first.journal_id === artifact.journal_id && first.artifact_path === repositoryRelative(artifactPath) && digest(`${lines[0]}\n`) === artifact.journal_reservation_sha256, 'Journal reservation does not bind artifact');
  check(first.mode === artifact.mode && first.attempt_id === artifact.attempt_id && first.runner_sha256 === artifact.runner_sha256 && first.criteria_sha256 === artifact.criteria_sha256 && first.reference_revision === artifact.reference_overlay_revision && first.candidate_revision === artifact.candidate_revision && first.reference_executable_sha256 === artifact.builds?.reference?.executable_sha256 && first.candidate_executable_sha256 === (artifact.builds?.translated?.executable_sha256 ?? null), 'Journal reserved a different attempt, method, source or executable');
  check(first.host_preflight_sha256 === digest(JSON.stringify(artifact.host_preflight)) && first.host_guard_sha256 === digest(JSON.stringify(artifact.host_guard)) && Number.isSafeInteger(first.runner_pid) && first.runner_pid > 0 && first.runner_start_ticks === artifact.runner_start_ticks, 'Journal does not bind the quiet preflight, runner and host guard');
  const recordedRuns = entries.filter((entry) => entry.event === 'run').map((entry) => entry.run);
  const recordedBlocks = entries.filter((entry) => entry.event === 'block_complete').map((entry) => entry.block);
  check(isDeepStrictEqual(recordedRuns, artifact.runs) && isDeepStrictEqual(recordedBlocks, artifact.blocks), 'Journal lost or changed measured subruns/blocks');
  let cursor = 1, countedRuns = 0;
  for (const block of artifact.blocks) {
    const blockRuns = artifact.runs.filter((run) => run.block === block.block && run.name.startsWith(`${block.stratum}/`));
    for (const run of blockRuns) {
      check(entries[cursor]?.event === 'run' && entries[cursor].journal_id === artifact.journal_id && isDeepStrictEqual(entries[cursor].run, run), 'Journal run is missing or out of block order');
      cursor++; countedRuns++;
    }
    check(entries[cursor]?.event === 'block_complete' && entries[cursor].journal_id === artifact.journal_id && isDeepStrictEqual(entries[cursor].block, block), 'Journal block completion is missing or reordered');
    cursor++;
  }
  check(countedRuns === artifact.runs.length && cursor === entries.length - 1, 'Journal has incomplete or extra subrun entries');
  check(entries.filter((entry) => entry.event === 'reserved').length === 1 && entries.filter((entry) => ['complete', 'failed'].includes(entry.event)).length === 1 && last.event === 'complete' && last.journal_id === artifact.journal_id, 'Interrupted or duplicated paired attempt terminal');
  check(last.artifact_path === repositoryRelative(artifactPath) && last.artifact_sha256 === fileHash(artifactPath) && last.runs === artifact.runs.length && last.blocks === artifact.blocks.length && last.passed === artifact.analysis?.passed, 'Journal terminal does not bind complete artifact');
  check(entries.length === 2 + recordedRuns.length + recordedBlocks.length, 'Unexpected paired journal entries');
  return true;
}
function launchArm(root, build, role) {
  const child = spawn(build.executable_path, build.run_command.slice(1), { cwd: root, env: { ...process.env, ...runtimeEnvironment }, stdio: ['pipe', 'pipe', 'pipe'] });
  let buffer = '', stdoutTail = '', setup, ready = false, pending, dead;
  let stdoutTruncated = false;
  const stderrLines = newBoundedGoDiagnosticLines();
  let readyResolve, readyReject;
  const readyPromise = new Promise((resolveReady, rejectReady) => { readyResolve = resolveReady; readyReject = rejectReady; });
  let exitResolve, exitReject;
  const exitPromise = new Promise((resolveExit, rejectExit) => { exitResolve = resolveExit; exitReject = rejectExit; });
  const fail = (error) => { if (dead) return; dead = error; readyReject(error); if (pending) { pending.reject(error); pending = undefined; } };
  child.stderr.on('data', (chunk) => stderrLines.push(chunk.toString()));
  const flushDiagnosticFragments = () => {
    stderrLines.finish();
    if (buffer && !buffer.startsWith('PAIRED_BARRIER_RUN ')) {
      stdoutTruncated ||= Buffer.byteLength(stdoutTail) + Buffer.byteLength(buffer) > goDiagnosticTailBytes;
      stdoutTail = boundedGoDiagnosticTail(stdoutTail, buffer);
    }
    buffer = '';
  };
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
      if (!line.startsWith('PAIRED_BARRIER_RUN ') && !line.startsWith('PAIRED_BARRIER_SETUP ') && !line.startsWith('PAIRED_BARRIER_READY ')) {
        stdoutTruncated ||= Buffer.byteLength(stdoutTail) + Buffer.byteLength(line) + 1 > goDiagnosticTailBytes;
        stdoutTail = boundedGoDiagnosticTail(stdoutTail, `${line}\n`);
      }
    }
    if (Buffer.byteLength(buffer) > (1 << 20)) {
      fail(new Error(`${role} emitted an oversized unterminated Go output line`));
      child.kill();
    }
  });
  child.stdin.on('error', fail);
  child.on('error', (error) => { flushDiagnosticFragments(); fail(error); exitReject(error); });
  child.on('close', (code, signal) => {
    flushDiagnosticFragments();
    if (!ready || code !== 0) {
      const error = new Error(`${role} test process exited ${code ?? signal}; Go stdout: ${stdoutTail.slice(-4096)}; stderr: ${stderrLines.snapshot().tail.slice(-4096)}`);
      fail(error); exitReject(error);
    } else exitResolve();
  });
  const timeout = setTimeout(() => fail(new Error(`${role} benchmark startup timed out`)), 120000);
  return {
    pid: child.pid,
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
    diagnostics() { const stderr = stderrLines.snapshot(); return { stdout_tail: stdoutTail, stderr_tail: stderr.tail, stdout_truncated: stdoutTruncated, stderr_truncated: stderr.truncated }; }
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
  const prescribedOutput = resolve(`docs/evidence/fidelity-performance/paired-barrier-v2-attempt2/${mode === 'baseline' ? 'baseline.json' : 'candidate.json'}`);
  check(resolve(output) === prescribedOutput, 'Use the preregistered write-once artifact path');
  assertFreshAttemptOutput(output);
  const criteria = verifyCriteria();
  clean(process.cwd()); tracked(process.cwd(), criteriaPath);
  const bRoot = resolve(process.env.OLP_PAIRED_B_ROOT || '/tmp/olp-worktrees/paired-barrier-reference-attempt2');
  verifyReferenceCheckout(bRoot);
  let baseline, baselineBinding;
  if (mode === 'paired') {
    check(baselinePath === bOnlyPath && existsSync(baselinePath), 'Committed B-only baseline is required at its frozen path before C measurement');
    tracked(process.cwd(), baselinePath);
    baseline = readJSON(baselinePath);
    check(baseline.mode === 'baseline' && baseline.analysis?.passed && baseline.criteria_sha256 === fileHash(criteriaPath) && baseline.runner_sha256 === fileHash(runnerPath) && baseline.reference_harness_sha256 === fileHash(referenceHarness), 'Frozen B-only baseline or method is unavailable/failed');
    validateArtifact(baseline, 'baseline', criteria);
    check(baseline.method_revision && baseline.method_revision !== sourceRevision(process.cwd()), 'C must follow the committed pre-candidate method');
    verifyCandidateDependencies(baseline.method_revision);
    check(isAncestor(baseline.method_revision, sourceRevision(process.cwd())), 'Frozen B-only method is not a strict ancestor of candidate C');
    baselineBinding = committedBaselineLineage(baselinePath, sourceRevision(process.cwd()));
  }
  const buildDir = mkdtempSync(join(tmpdir(), 'olp-paired-barrier-v2-'));
  const bBinary = join(buildDir, 'reference.test');
  const cBinary = join(buildDir, 'translated.test');
  const bBuild = buildBinary(bRoot, bBinary, 'reference');
  const cBuild = mode === 'paired' ? buildBinary(process.cwd(), cBinary, 'translated') : null;
  const preflight = await quietPreflight();
  const artifact = {
    schema, mode, attempt_id: attemptId, started_at: new Date().toISOString(), completed_at: null,
    prior_failed_sha256: fileHash(priorFailedPath), prior_journal_sha256: fileHash(priorJournalPath),
    criteria_sha256: fileHash(criteriaPath), runner_sha256: fileHash(runnerPath),
    historical_baseline_sha256: fileHash(frozenReference), historical_budget_sha256: fileHash(frozenBudget),
    reference_product_revision: anchor, reference_overlay_revision: sourceRevision(bRoot), candidate_revision: mode === 'paired' ? sourceRevision(process.cwd()) : null,
    method_revision: mode === 'baseline' ? sourceRevision(process.cwd()) : baseline.method_revision,
    b_only_path: baselineBinding?.b_only_path ?? null, b_only_sha256: baselineBinding?.b_only_sha256 ?? null,
    b_only_journal_sha256: baselineBinding?.b_only_journal_sha256 ?? null,
    b_only_commit: baselineBinding?.b_only_commit ?? null, product_change_commit: baselineBinding?.product_change_commit ?? null,
    builds: { reference: bBuild, translated: cBuild },
    reference_harness_sha256: fileHash(referenceHarness), candidate_harness_sha256: mode === 'paired' ? fileHash(candidateHarness) : null,
    original_oracle_sha256: fileHash('tests/integration/continuation_barrier_benchmark_test.go'),
    fixture_sha256: Object.fromEntries(corpusPaths.filter((path) => existsSync(path)).map((path) => [path, fileHash(path)])),
    candidate_dependency_sha256: Object.fromEntries(candidateDependencies.map((path) => [path, fileHash(path)])),
    candidate_workflow_sha256: mode === 'paired' ? fileHash('tests/integration/continuation_candidate_benchmark_test.go') : null,
    toolchain: toolchain(), go_build_environment: buildEnvironment(), runtime_environment: runtimeEnvironment,
    hardware: hardware(), host_guard: hostGuard, materiality_reasons: materialityReasons,
    host_preflight: preflight, clock_hz: clockTicksPerSecond(),
    system_load: { before: hostSnapshot(), after: null }, storage: null, schedule: schedule(criteria),
    blocks: [], runs: [], analysis: null, error: null, process_diagnostics: null,
    runner_start_ticks: processStartTicks(process.pid),
    journal_id: null, journal_path: null, journal_reservation_sha256: null,
    conditions
  };
  const journal = reserveCaptureJournal(output, {
    schema, mode, attempt_id: attemptId, runner_sha256: artifact.runner_sha256, criteria_sha256: artifact.criteria_sha256,
    reference_revision: artifact.reference_overlay_revision, candidate_revision: artifact.candidate_revision,
    reference_executable_sha256: bBuild.executable_sha256, candidate_executable_sha256: cBuild?.executable_sha256 ?? null,
    host_preflight_sha256: digest(JSON.stringify(preflight)), host_guard_sha256: digest(JSON.stringify(hostGuard)),
    runner_pid: process.pid, runner_start_ticks: artifact.runner_start_ticks, reserved_at: new Date().toISOString()
  });
  artifact.journal_id = journal.id;
  artifact.journal_path = journal.path;
  artifact.journal_reservation_sha256 = journal.reservation_sha256;
  let b, c;
  let collectionOpen = true;
  let interference = null;
  const onMaterialInterference = () => {
    if (!collectionOpen || interference) return;
    try {
      const notice = readJSON(interferencePathFor(output));
      interference = materialInterferenceEvent(notice, hostSnapshot({ reference: b?.pid ?? null, translated: c?.pid ?? null }));
    } catch (error) {
      interference = { event: 'material_interference', observed_at: new Date().toISOString(), recorded_at: new Date().toISOString(),
        process: { pid: null, name: null }, reason: 'unattributed-signal',
        rationale: 'Operator interference signal lacked valid detail or host counters; the entire attempt is invalid.',
        host: { loadavg_raw: optional('/proc/loadavg'), cpu_pressure_raw: optional('/proc/pressure/cpu') },
        error: redactGoDiagnostic(error?.message || String(error)) };
    }
    journal.append(interference);
    artifact.error = 'material unrelated host work was prospectively reported during collection';
    b?.kill(); c?.kill();
  };
  process.on('SIGUSR2', onMaterialInterference);
  const checkInterference = () => {
    if (!interference && existsSync(interferencePathFor(output))) onMaterialInterference();
    if (interference) throw new Error('material unrelated host work invalidated the whole attempt');
  };
  try {
    b = launchArm(bRoot, bBuild, 'reference');
    c = mode === 'paired' ? launchArm(process.cwd(), cBuild, 'translated') : null;
    const [bSetup, cSetup] = await Promise.all([b.ready(), c ? c.ready() : Promise.resolve(null)]);
    checkInterference();
    assertConditions(bSetup, cSetup);
    artifact.storage = { reference: bSetup, candidate: cSetup };
    for (const block of artifact.schedule) {
      checkInterference();
      const armPids = { reference: b.pid, translated: c?.pid ?? null };
      const diagnostic = { stratum: block.stratum, block: block.block, orientation: block.orientation, before: hostSnapshot(armPids), after: null };
      for (let position = 0; position < block.arms.length; position++) {
        const arm = block.arms[position];
        if (mode === 'baseline' && arm === 'translated') continue;
        const processArm = arm === 'translated' ? c : b;
        const run = await processArm.run({ name: `${block.stratum}/${arm}`, block: block.block, position });
        validateRun(run, criteria);
        artifact.runs.push(run);
        journal.append({ event: 'run', run });
        checkInterference();
      }
      diagnostic.after = hostSnapshot(armPids);
      validateBlockHostDiagnostics(diagnostic.before, diagnostic.after, mode);
      artifact.blocks.push(diagnostic);
      journal.append({ event: 'block_complete', block: diagnostic });
    }
    await new Promise((resolveTick) => setImmediate(resolveTick));
    checkInterference();
    artifact.analysis = analyze(artifact.runs, mode, criteria);
  } catch (error) {
    artifact.error ||= error?.message || String(error);
  } finally {
    try { await Promise.all([...(b ? [b.close()] : []), ...(c ? [c.close()] : [])]); }
    catch (error) { artifact.error ||= error?.message || String(error); }
    try { attestUnchangedBinary(bBuild); if (cBuild) attestUnchangedBinary(cBuild); }
    catch (error) { artifact.error ||= error?.message || String(error); }
    if (artifact.error) artifact.process_diagnostics = { reference: b?.diagnostics() ?? null, translated: c?.diagnostics() ?? null };
    artifact.completed_at = new Date().toISOString();
    try { artifact.system_load.after = hostSnapshot(); }
    catch (error) {
      artifact.error ||= 'required host counters disappeared during final observation';
      artifact.system_load.after = { error: redactGoDiagnostic(error?.message || String(error)), loadavg_raw: optional('/proc/loadavg'), cpu_pressure_raw: optional('/proc/pressure/cpu') };
    }
    const destination = artifact.error ? `${output}.failed.json` : output;
    try {
      writeFileSync(destination, `${JSON.stringify(artifact, null, 2)}\n`, { flag: 'wx' });
      journal.append({ event: artifact.error ? 'failed' : 'complete', artifact_path: repositoryRelative(destination), artifact_sha256: fileHash(destination), runs: artifact.runs.length, blocks: artifact.blocks.length, passed: artifact.analysis?.passed ?? false, completed_at: artifact.completed_at });
    } finally { collectionOpen = false; journal.close(); }
    if (artifact.error) { b?.kill(); c?.kill(); throw new Error(`${artifact.error}; retained ${destination} and ${journal.path}`); }
    console.log(`${mode}: ${artifact.runs.length} complete subruns, ${artifact.analysis.passed ? 'PASS' : 'INCONCLUSIVE'}; ${destination}`);
    if (!artifact.analysis.passed) process.exitCode = 1;
  }
}

export function validateArtifact(artifact, mode, criteria = verifyCriteria(), artifactPath = mode === 'baseline' ? bOnlyPath : 'docs/evidence/fidelity-performance/paired-barrier-v2-attempt2/candidate.json') {
  check(artifact.schema === schema && artifact.mode === mode && artifact.error === null, 'Incomplete paired artifact');
  check(isDeepStrictEqual(artifact.host_guard, hostGuard) && isDeepStrictEqual(artifact.materiality_reasons, materialityReasons), 'Host stability guard changed');
  validateQuietPreflight(artifact.host_preflight);
  check(artifact.clock_hz === artifact.host_preflight[0].clock_hz, 'Host clock-tick frequency changed');
  validateHostSnapshot(artifact.system_load?.before);
  validateHostSnapshot(artifact.system_load?.after);
  check(artifact.system_load.before.clock_hz === artifact.clock_hz && artifact.system_load.after.clock_hz === artifact.clock_hz, 'Host clock frequency changed outside measured blocks');
  check(!existsSync(interferencePathFor(artifactPath)), 'A prospectively flagged unrelated process invalidated the whole attempt');
  check(artifact.attempt_id === attemptId && artifact.prior_failed_sha256 === fileHash(priorFailedPath) && artifact.prior_journal_sha256 === fileHash(priorJournalPath), 'Attempt 2 is not bound to the retained failed first attempt');
  validateJournalArtifact(artifact, artifactPath);
  check(artifact.criteria_sha256 === fileHash(criteriaPath) && artifact.runner_sha256 === fileHash(runnerPath) && artifact.historical_baseline_sha256 === fileHash(frozenReference) && artifact.historical_budget_sha256 === fileHash(frozenBudget), 'Frozen paired method changed');
  check(artifact.reference_product_revision === anchor && artifact.original_oracle_sha256 === readJSON(frozenReference).harness_sha256 && artifact.reference_harness_sha256 === fileHash(referenceHarness), 'Reference product/oracle changed');
  check(artifact.method_revision && gitBlobHash(artifact.method_revision, runnerPath) === artifact.runner_sha256 && gitBlobHash(artifact.method_revision, criteriaPath) === artifact.criteria_sha256, 'Pre-candidate method commit changed');
  validateBuildEvidence(artifact.builds?.reference, 'reference', artifact.reference_overlay_revision, artifact.toolchain, artifact.go_build_environment);
  check(mode === 'baseline' ? artifact.builds.translated === null : artifact.builds.translated !== null, 'Wrong benchmark executable inventory');
  if (mode === 'paired') validateBuildEvidence(artifact.builds.translated, 'translated', artifact.candidate_revision, artifact.toolchain, artifact.go_build_environment);
  verifyCandidateDependencies(artifact.method_revision, artifact.candidate_dependency_sha256);
  verifyCandidateDependencies(artifact.method_revision);
  check(isDeepStrictEqual(git(['diff', '--name-only', anchor, artifact.reference_overlay_revision]).split('\n').filter(Boolean), [referenceHarness]), 'B overlay touched product or oracle');
  check(gitBlobHash(artifact.reference_overlay_revision, referenceHarness) === artifact.reference_harness_sha256, 'Recorded B overlay changed');
  check(isDeepStrictEqual(artifact.conditions, conditions), 'Measurement conditions changed');
  check(isDeepStrictEqual(Object.keys(artifact.fixture_sha256 ?? {}).sort(), corpusPaths.slice().sort()), 'Fixture hash inventory changed');
  for (const path of corpusPaths) {
    check(gitBlobHash(anchor, path) === artifact.fixture_sha256[path], `Fixture or workflow oracle changed: ${path}`);
  }
  check(mode === 'baseline' ? artifact.candidate_revision === null && artifact.candidate_harness_sha256 === null && artifact.candidate_workflow_sha256 === null : artifact.candidate_revision && artifact.candidate_harness_sha256 === fileHash(candidateHarness) && gitBlobHash(artifact.candidate_revision, candidateHarness) === artifact.candidate_harness_sha256 && artifact.candidate_workflow_sha256 === gitBlobHash(artifact.candidate_revision, 'tests/integration/continuation_candidate_benchmark_test.go'), 'Candidate source/harness changed');
  if (mode === 'paired') {
    check(isAncestor(artifact.method_revision, artifact.candidate_revision), 'Candidate does not strictly follow frozen method');
    verifyCandidateOracle(artifact.method_revision, { harness: artifact.candidate_harness_sha256, workflow: artifact.candidate_workflow_sha256 });
    check(artifact.b_only_path === bOnlyPath, 'Unknown B-only artifact path');
    const bOnly = verifyPairedBaselineBinding(artifact);
    const bOnlyResult = validateArtifact(bOnly, 'baseline', criteria, artifact.b_only_path);
    check(bOnlyResult.passed && bOnly.method_revision === artifact.method_revision && bOnly.reference_overlay_revision === artifact.reference_overlay_revision, 'Committed B-only artifact is inconclusive or from a different method/reference');
  } else {
    check(artifact.b_only_path === null && artifact.b_only_sha256 === null && artifact.b_only_journal_sha256 === null && artifact.b_only_commit === null && artifact.product_change_commit === null, 'B-only artifact contains future candidate evidence');
  }
  check(isDeepStrictEqual(artifact.schedule, schedule(criteria)), 'Schedule changed');
  check(artifact.blocks?.length === strata.length * blocksPerStratum && artifact.blocks.every((block, i) => block.stratum === artifact.schedule[i].stratum && block.block === artifact.schedule[i].block && block.orientation === artifact.schedule[i].orientation && block.before && block.after), 'Missing block diagnostics');
  for (const block of artifact.blocks) {
    validateBlockHostDiagnostics(block.before, block.after, mode);
    check(block.before.clock_hz === artifact.clock_hz, 'Host clock frequency changed during a measured block');
  }
  check(isDeepStrictEqual(artifact.hardware, readJSON(frozenReference).hardware) && artifact.toolchain === readJSON(frozenReference).toolchain && artifact.go_build_environment === readJSON(frozenReference).go_build_environment && isDeepStrictEqual(artifact.runtime_environment, runtimeEnvironment), 'Incomparable host/runtime');
  check(artifact.storage?.reference?.postgresql_server_version === '180006' && artifact.storage.reference.postgresql_tls === false && (mode === 'baseline' ? artifact.storage.candidate === null : artifact.storage.candidate?.postgresql_server_version === '180006' && artifact.storage.candidate.postgresql_tls === false && artifact.storage.candidate.database_name !== artifact.storage.reference.database_name), 'Changed or shared storage');
  const result = analyze(artifact.runs, mode, criteria);
  check(isDeepStrictEqual(artifact.analysis, result), 'Saved paired calculation differs');
  return result;
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const [action, path, extra, more, detail] = process.argv.slice(2);
    if (action === 'derive-criteria' && path && !extra) writeFileSync(path, `${JSON.stringify(deriveCriteria(), null, 2)}\n`, { flag: 'wx' });
    else if (action === 'record-baseline' && path && !extra) await capture('baseline', path);
    else if (action === 'record-paired' && path && extra) await capture('paired', path, extra);
    else if (action === 'flag-interference' && path && extra && more && detail) flagMaterialInterference(path, extra, more, detail);
    else if (action === 'compare' && path && !extra) {
      const result = validateArtifact(readJSON(path), readJSON(path).mode, verifyCriteria(), path);
      console.log(JSON.stringify({ passed: result.passed, failures: result.failures, totals: result.totals }, null, 2));
      if (!result.passed) process.exitCode = 1;
    } else throw new Error('Usage: derive-criteria <new-file> | record-baseline <new-file> | record-paired <new-file> <committed-baseline> | flag-interference <active-artifact> <unrelated-build|unrelated-test|unrelated-compute> <pid> <process-name> | compare <artifact>');
  } catch (error) { console.error(error.message); process.exitCode = 1; }
}
