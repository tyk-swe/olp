#!/usr/bin/env node
// Additive randomized paired source benchmark. The v1 runner, oracle, baseline
// and budgets remain immutable; this file must be frozen before observing C.
import { spawn, spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { appendFileSync, existsSync, readFileSync, writeFileSync } from 'node:fs';
import { arch, cpus, platform, release, totalmem } from 'node:os';
import { isAbsolute, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { isDeepStrictEqual } from 'node:util';
import { summarize } from './fidelity-benchmark.mjs';

export const SCHEMA = 'openllmproxy.dev/fidelity-source-paired-r7';
export const SEED = 'oif-source-paired-r7-2026-09-23-sealed-c89a7421e6b5';
export const BLOCKS = 32;
export const SAMPLES = 64;
export const UPPER_INDEX = 22; // d_(23), zero-based.
export const LOWER_INDEX = 9; // d_(10), zero-based.
const B_PRODUCT = '8e52f775df815c3a1a25d75064c3ffd540fb07f5';
const PREFIX = 'OLP_SOURCE_PAIRED_V2 ';
const workloads = ['native_unary', 'native_stream_256', 'native_slow_stream_64', 'native_asset_png', 'translated_unary', 'rejected_extension'];
const groups = workloads.flatMap((workload) => [1, 8].map((concurrency) => `${workload}/c${concurrency}`));
const metricKinds = ['ns/op', 'process-cpu-ns/op', 'B/op', 'allocs/op', 'sampled-heap-growth-B',
  'latency-p50-us', 'latency-p95-us', 'latency-p99-us'];
const streamMetrics = ['first-event-p50-us', 'first-event-p95-us', 'first-event-p99-us',
  'max-inter-event-gap-p50-us', 'max-inter-event-gap-p95-us', 'max-inter-event-gap-p99-us'];
const latencyMetrics = ['latency-p50-us', 'latency-p95-us', 'latency-p99-us'];
const strictRouteContracts = Object.fromEntries(['native', 'translated', 'rejected'].map((category) => [category, { fidelity: { mode: 'strict' } }]));
const hash = (value) => createHash('sha256').update(value).digest('hex');
const digest = (path) => hash(readFileSync(path));
const json = (path) => JSON.parse(readFileSync(path, 'utf8'));
const median = (values) => {
  const sorted = [...values].sort((a, b) => a - b);
  return (sorted[(sorted.length - 1) >> 1] + sorted[sorted.length >> 1]) / 2;
};
const finite = (value) => typeof value === 'number' && Number.isFinite(value);
const invariant = (condition, message) => { if (!condition) throw new Error(message); };
const rank = (seed, label) => hash(`${seed}:${label}`);
const groupPaths = (group) => group.startsWith('rejected_extension/') ? ['gateway'] : ['relay', 'gateway'];
const metricList = (group) => group.includes('stream_') ? [...metricKinds, ...streamMetrics] : metricKinds;
const pathName = (group, path) => `${group}/${path}`;
const oldFiles = {
  baseline: 'docs/evidence/fidelity-performance/v1/baseline.json',
  budgets: 'docs/evidence/fidelity-performance/v1/replacement-budgets.json',
  harness: 'internal/gateway/fidelity_benchmark_test.go',
  runner: 'scripts/fidelity-benchmark.mjs',
  pairedHarness: 'internal/gateway/fidelity_paired_benchmark_test.go',
  pairedRunner: 'scripts/fidelity-paired-r7.mjs',
  r5Manifest: 'docs/evidence/fidelity-performance/source-paired-v2/manifest-r5.json',
  r6Manifest: 'docs/evidence/fidelity-performance/source-paired-v2/manifest-r6.json',
  r6Artifact: 'docs/evidence/fidelity-performance/source-paired-v2/paired-r6.json',
  r6Journal: 'docs/evidence/fidelity-performance/source-paired-v2/paired-r6.json.journal.jsonl'
};
const B_ONLY_EVIDENCE_PATH = 'docs/evidence/fidelity-performance/source-paired-v2/baseline-r7.json';
const B_ONLY_JOURNAL_PATH = `${B_ONLY_EVIDENCE_PATH}.journal.jsonl`;
const PAIRED_EVIDENCE_PATH = 'docs/evidence/fidelity-performance/source-paired-v2/paired-r7.json';
const PAIRED_JOURNAL_PATH = `${PAIRED_EVIDENCE_PATH}.journal.jsonl`;
const R5_INVALID_COMMIT = 'c104953720f7817d2367eb68f99cc2e812bdf91d';
const R5_INVALID_ARTIFACT_SHA256 = '68f7bb88ce70bcc4b179179749577faf085838c0fd2696c7e3b2f9ba3b239b06';
const R5_INVALID_JOURNAL_SHA256 = '5127ea75ec3688cbadf433eae8d021ce0d473b269f571fe03face683f7dd8295';
const R6_INVALID_ARTIFACT_SHA256 = '33f5efb6dc681b2be570e5e2352e53ea35f01d8adfbaff836a05083a8bf1f023';
const R6_INVALID_JOURNAL_SHA256 = '25f76f751721fa4252d4969e5c0583ea354a1deecd34eeb13e0953552c41bdde';
const C_PRODUCT_REVISION = 'bc325da50c575803c771531dc4e3aab1ac203a53';
const hostStabilityRule = {
  preflight_seconds: 60, preflight_interval_seconds: 5, preflight_samples: 13,
  preflight_max_load_1m_exclusive: 2, preflight_max_cpu_some_avg10_percent_exclusive: 5,
  preflight_max_external_busy_cores_exclusive: 1,
  capture_external_cores_single_block_invalid_at: 2,
  capture_external_cores_two_consecutive_blocks_invalid_at: 0.75,
  clock_ticks_per_second: 100,
  decision: 'before reservation require every preflight sample/window below limits; after each completed block invalidate the whole attempt on one >=2 external-core interval or two consecutive >=0.75 intervals; never filter or replace blocks'
};

export function sourceOrder(group, variant, baselineOnly = false, outer = 'relay') {
  invariant(variant === 'ABBA' || variant === 'BAAB', 'unknown order');
  invariant(outer === 'relay' || outer === 'gateway', 'unknown outer path');
  if (group.startsWith('rejected_extension/')) {
    return baselineOnly ? ['B/gateway', 'B/gateway'] : variant === 'ABBA'
      ? ['B/gateway', 'C/gateway', 'C/gateway', 'B/gateway']
      : ['C/gateway', 'B/gateway', 'B/gateway', 'C/gateway'];
  }
  // Treatment order is defined per relay path. Swapping B and C at every
  // position yields the opposite order. Path placement is independently
  // balanced so relay and gateway each occupy outer and inner positions.
  const orders = {
    relay: {
      ABBA: ['B/relay', 'C/relay', 'C/gateway', 'B/gateway', 'B/gateway', 'C/gateway', 'C/relay', 'B/relay'],
      BAAB: ['C/relay', 'B/relay', 'B/gateway', 'C/gateway', 'C/gateway', 'B/gateway', 'B/relay', 'C/relay']
    },
    gateway: {
      ABBA: ['C/gateway', 'B/gateway', 'B/relay', 'C/relay', 'C/relay', 'B/relay', 'B/gateway', 'C/gateway'],
      BAAB: ['B/gateway', 'C/gateway', 'C/relay', 'B/relay', 'B/relay', 'C/relay', 'C/gateway', 'B/gateway']
    }
  };
  const full = orders[outer][variant];
  return baselineOnly ? full.filter((arm) => arm.startsWith('B/')) : full;
}

export function sourceSchedule(seed = SEED) {
  const blocks = groups.flatMap((group) => {
    const indices = Array.from({ length: BLOCKS }, (_, index) => index)
      .sort((a, b) => rank(seed, `variant:${group}:${a}`).localeCompare(rank(seed, `variant:${group}:${b}`)));
    const variant = new Map(indices.map((index, ordinal) => [index, ordinal < 16 ? 'ABBA' : 'BAAB']));
    const outer = new Map();
    for (const treatment of ['ABBA', 'BAAB']) {
      const placed = indices.filter((index) => variant.get(index) === treatment)
        .sort((a, b) => rank(seed, `outer:${group}:${a}`).localeCompare(rank(seed, `outer:${group}:${b}`)));
      placed.forEach((index, ordinal) => outer.set(index, group.startsWith('rejected_extension/') ? 'gateway' : ordinal < 8 ? 'relay' : 'gateway'));
    }
    return Array.from({ length: BLOCKS }, (_, index) => ({ group, index, variant: variant.get(index), outer: outer.get(index) }));
  });
  blocks.sort((a, b) => rank(seed, `block:${a.group}:${a.index}`).localeCompare(rank(seed, `block:${b.group}:${b.index}`)));
  return blocks;
}

export function semanticGateOrder(paired) {
  return groups.flatMap((group) => groupPaths(group).flatMap((path) => {
    const name = pathName(group, path);
    return (paired ? ['C', 'B'] : ['B']).map((version) => ({ arm: `${version}/${path}`, name }));
  }));
}

export function validateStrictRouteContracts(contracts) {
  invariant(isDeepStrictEqual(contracts, strictRouteContracts), 'C requires exact explicit strict fidelity for native, translated and rejected routes');
  return contracts;
}

function committedEvidence(root, revision, path) {
  const result = spawnSync('git', ['show', `${revision}:${path}`], { cwd: root, maxBuffer: 128 << 20 });
  invariant(result.status === 0, `historical committed evidence unavailable: ${revision}:${path}`);
  return result.stdout;
}

export function makeManifest(root) {
  const full = (key) => resolve(root, oldFiles[key]);
  const baseline = json(full('baseline'));
  const budgets = json(full('budgets'));
  invariant(baseline.source_revision === B_PRODUCT && budgets.baseline_revision === B_PRODUCT, 'historical B revision changed');
  invariant(baseline.schema === 'openllmproxy.dev/fidelity-performance/v1' && budgets.schema === 'openllmproxy.dev/fidelity-performance-budget/v1', 'frozen v1 schema changed');
  invariant(baseline.repetitions === 3 && baseline.contract_mode === 'legacy' && baseline.runs.length === 66, 'historical B repetitions changed');
  invariant(budgets.baseline_artifact_sha256 === digest(full('baseline')), 'v1 baseline hash changed');
  invariant(baseline.harness_sha256 === digest(full('harness')) && budgets.baseline_harness_sha256 === baseline.harness_sha256, 'v1 fixture/oracle changed');
  invariant(baseline.runner_sha256 === digest(full('runner')) && budgets.baseline_runner_sha256 === baseline.runner_sha256, 'v1 runner changed');
  const summary = summarize(baseline.runs, 3);
  const expectedNames = groups.flatMap((group) => groupPaths(group).map((path) => pathName(group, path)));
  invariant(isDeepStrictEqual(Object.keys(budgets.workloads), expectedNames), 'v1 inventory or ordering changed');
  const comparisons = {};
  let metricCount = 0;
  for (const name of expectedNames) {
    const group = name.slice(0, name.lastIndexOf('/'));
    const maximum = budgets.workloads[name].maxima;
    invariant(isDeepStrictEqual(Object.keys(maximum).toSorted(), metricList(group).toSorted()), `v1 numeric metrics changed: ${name}`);
    comparisons[name] = {};
    for (const metric of metricList(group)) {
      const limit = maximum[metric];
      const historicalMedian = summary[name].metrics[metric].median;
      const margin = limit - historicalMedian;
      invariant(finite(limit) && finite(historicalMedian) && finite(margin) && margin > 0, `invalid frozen headroom: ${name} ${metric}`);
      comparisons[name][metric] = { old_limit: limit, old_B_median: historicalMedian, margin };
      metricCount++;
    }
  }
  invariant(metricCount === 224, `expected 224 path metrics, found ${metricCount}`);
  const addedLatency = {};
  for (const group of groups.filter((name) => !name.startsWith('rejected_extension/'))) {
    addedLatency[group] = Object.fromEntries(latencyMetrics.map((metric) => [metric, comparisons[pathName(group, 'gateway')][metric]]));
  }
  invariant(Object.values(addedLatency).flatMap(Object.keys).length === 30, 'added latency inventory changed');
  const r5Manifest = json(full('r5Manifest'));
  invariant(digest(full('r5Manifest')) === 'ffc6fa688a6394fe3f350d483587dfc0c13e23874d6d8f99d9b7355569966fca', 'r5 frozen manifest changed');
  invariant(isDeepStrictEqual(comparisons, r5Manifest.comparisons) && isDeepStrictEqual(addedLatency, r5Manifest.added_latency) &&
    isDeepStrictEqual(Object.fromEntries(expectedNames.map((name) => [name, budgets.workloads[name].exact])), r5Manifest.exact), 'r7 changed an old workload, metric, margin or exact outcome');
  const r5Artifact = committedEvidence(root, R5_INVALID_COMMIT, 'docs/evidence/fidelity-performance/source-paired-v2/baseline.json');
  const r5Journal = committedEvidence(root, R5_INVALID_COMMIT, 'docs/evidence/fidelity-performance/source-paired-v2/baseline.json.journal.jsonl');
  invariant(hash(r5Artifact) === R5_INVALID_ARTIFACT_SHA256 && hash(r5Journal) === R5_INVALID_JOURNAL_SHA256, 'r5 invalid evidence hashes changed');
  const r5Observed = JSON.parse(r5Artifact.toString());
  invariant(r5Observed.status === 'invalid' && r5Observed.blocks.length === 151 && r5Observed.C === null, 'r5 invalid B-only outcome changed');
  const r6Manifest = json(full('r6Manifest'));
  invariant(digest(full('r6Manifest')) === 'ca611cb5ad4f7716039319d910f561e1f2bf72ff0754ee0d3ee79179ef0d563a', 'r6 frozen manifest changed');
  invariant(isDeepStrictEqual(comparisons, r6Manifest.comparisons) && isDeepStrictEqual(addedLatency, r6Manifest.added_latency) &&
    isDeepStrictEqual(Object.fromEntries(expectedNames.map((name) => [name, budgets.workloads[name].exact])), r6Manifest.exact), 'r7 changed r6 numeric margins, inventory or exact outcome');
  invariant(digest(full('r6Artifact')) === R6_INVALID_ARTIFACT_SHA256 && digest(full('r6Journal')) === R6_INVALID_JOURNAL_SHA256, 'r6 invalid evidence hashes changed');
  const r6Observed = json(full('r6Artifact'));
  invariant(r6Observed.status === 'invalid' && r6Observed.blocks.length === 0 &&
    r6Observed.C?.revision === C_PRODUCT_REVISION && r6Observed.journal_sha256 === R6_INVALID_JOURNAL_SHA256 &&
    r6Observed.error === 'C: oracle, effect count or fixed sample count failed ', 'r6 adverse first-C result changed');
  const schedule = sourceSchedule();
  return {
    schema: `${SCHEMA}-manifest`, method_version: 3, manifest_revision: 'diagnostic-one-shot-r7', attempt_id: 'source-paired-r7',
    supersedes_manifest_sha256: 'ca611cb5ad4f7716039319d910f561e1f2bf72ff0754ee0d3ee79179ef0d563a',
    previous_invalid_attempts: {
      r5: { artifact_commit: R5_INVALID_COMMIT, artifact_sha256: R5_INVALID_ARTIFACT_SHA256,
        journal_sha256: R5_INVALID_JOURNAL_SHA256, completed_blocks: 151, C_observations: 0 },
      r6: { artifact_path: oldFiles.r6Artifact, artifact_sha256: R6_INVALID_ARTIFACT_SHA256,
        journal_path: oldFiles.r6Journal, journal_sha256: R6_INVALID_JOURNAL_SHA256,
        completed_blocks: 0, first_C_name: 'native_unary/c1/relay', C_revision: C_PRODUCT_REVISION,
        reason: 'adverse/incomplete first C semantic result; category cannot be recovered from r6 evidence' }
    },
    sequential_rule: 'R6 remains an adverse/incomplete C semantic attempt with zero complete numeric blocks and cannot be retried. R7 is one prospective write-once C opportunity. R7 d_(23)/d_(10) has standalone one-sided per-metric alpha 0.0100308035; the conservative sum with r6 d_(22) alpha 0.0250512299 is 0.0350820334 against a prospectively declared 0.05 target for the two opportunity decisions on a given metric. No claim of 2 times the r7 alpha, no across-metric familywise claim, and no later r7 retry. Every C semantic failure is an adverse candidate result.',
    host_stability: hostStabilityRule,
    B_only_evidence_path: B_ONLY_EVIDENCE_PATH, B_only_journal_path: B_ONLY_JOURNAL_PATH,
    paired_evidence_path: PAIRED_EVIDENCE_PATH, paired_journal_path: PAIRED_JOURNAL_PATH,
    chronology: 'paired B stays at measured r7 method commit M7 with fixed untracked reservations; a separate branch creates exact B7 JSON and journal together in normal commit E7 directly after M7; E7 is a strict ancestor of C7; C7 descends from bc325 and has byte-identical production Go blobs, with no invented product change',
    C_route_contract: strictRouteContracts, B_product_revision: B_PRODUCT, C_product_revision: C_PRODUCT_REVISION,
    old_baseline_sha256: digest(full('baseline')), old_budgets_sha256: digest(full('budgets')),
    old_harness_sha256: digest(full('harness')), old_runner_sha256: digest(full('runner')),
    paired_harness_sha256: digest(full('pairedHarness')), paired_runner_sha256: digest(full('pairedRunner')),
    seed: SEED, blocks_per_group: BLOCKS, samples_per_subrun: SAMPLES,
    semantic_gate_order_sha256: { B_only: hash(JSON.stringify(semanticGateOrder(false))), paired: hash(JSON.stringify(semanticGateOrder(true))) },
    order: { ABBA: 'B-C-C-B per relay path; C-B-B-C per gateway path', BAAB: 'C-B-B-C per relay path; B-C-C-B per gateway path',
      outer: 'within each treatment order, eight relay-outer and eight gateway-outer four-arm palindromes',
      rejected: 'B-C-C-B or C-B-B-C gateway', baseline: 'same schedule with C subruns omitted' },
    schedule_sha256: hash(JSON.stringify(schedule)),
    bound: { lower_order_statistic: 10, upper_order_statistic: 23, upper_coverage: 0.9899691964965314,
      primary: 'sorted 32 paired block C-minus-B differences: d_(23) < old frozen margin',
      relay_control: 'd_(10) > -margin and d_(23) < margin',
      B_envelope: 'median of all 64 B subrun metrics <= old frozen limit' },
    comparisons, added_latency: addedLatency,
    exact: Object.fromEntries(expectedNames.map((name) => [name, budgets.workloads[name].exact])),
    old_hardware: Object.fromEntries(['os', 'architecture', 'kernel', 'cpu', 'logical_cpus', 'total_memory_bytes', 'cpu_quota', 'memory_limit'].map((key) => [key, baseline.hardware[key]])),
    old_toolchain: baseline.toolchain, old_go_build_environment: baseline.go_build_environment,
    old_runtime_environment: baseline.runtime_environment,
    conditions: {
      network: 'IPv4 loopback HTTP/1.1 keep-alive, no inference TLS, same two-hop v1 fixture',
      authority: 'in-memory published route and credential slot; authenticated native relay; owned PostgreSQL/Valkey stopped',
      processes: 'B and C are separately prebuilt, long-lived Go test processes; compilation/setup and per-subrun checked warmup excluded',
      slow_reader: '100us requested sleep after every content event; actual OS timer may be coarser',
      resources: 'whole-process local client/provider/gateway or relay/instrumentation, not isolated gateway RSS',
      timing: 'after a fixed all-22 semantic gate: 64 requests/subrun, two subruns/arm/block, 32 complete blocks/group; gate durations excluded; no omissions or selected retries',
      semantics: 'frozen v1 provider-bound request, complete response/SSE event oracle, exact asset, authentication and zero-dispatch rejection; C gate/subrun failures are adverse candidate results with structured details'
    }
  };
}

function hardware() {
  const optional = (path) => existsSync(path) ? readFileSync(path, 'utf8').trim() : null;
  return { os: platform(), architecture: arch(), kernel: release(), cpu: cpus()[0]?.model, logical_cpus: cpus().length,
    total_memory_bytes: totalmem(), cpu_quota: optional('/sys/fs/cgroup/cpu.max'), memory_limit: optional('/sys/fs/cgroup/memory.max') };
}
function diagnostics() {
  const optional = (path) => existsSync(path) ? readFileSync(path, 'utf8').trim() : null;
  return { loadavg: optional('/proc/loadavg'), cpu_pressure: optional('/proc/pressure/cpu'), memory_pressure: optional('/proc/pressure/memory') };
}
function readHostSnapshot(pids = []) {
  const cpuLine = readFileSync('/proc/stat', 'utf8').split('\n')[0];
  const fields = cpuLine.trim().split(/\s+/);
  invariant(fields[0] === 'cpu' && fields.length >= 9, 'unavailable /proc/stat CPU counters');
  const counters = fields.slice(1, 9).map(Number);
  invariant(counters.every((value) => Number.isSafeInteger(value) && value >= 0), 'invalid /proc/stat CPU counters');
  const busyTicks = counters[0] + counters[1] + counters[2] + counters[5] + counters[6] + counters[7];
  const pressure = readFileSync('/proc/pressure/cpu', 'utf8').trim();
  const some = pressure.split('\n').find((line) => line.startsWith('some '));
  const pressureMatch = some?.match(/(?:^|\s)avg10=([0-9.]+)/);
  invariant(pressureMatch && finite(Number(pressureMatch[1])), 'unavailable CPU pressure avg10');
  const loadRaw = readFileSync('/proc/loadavg', 'utf8').trim();
  const load1m = Number(loadRaw.split(/\s+/)[0]);
  invariant(finite(load1m) && load1m >= 0, 'unavailable one-minute host load');
  const processTicks = {};
  for (const pid of pids) {
    invariant(Number.isSafeInteger(pid) && pid > 0, 'missing measured process PID');
    const raw = readFileSync(`/proc/${pid}/stat`, 'utf8');
    const close = raw.lastIndexOf(')');
    invariant(close > 0, `unavailable measured process CPU counters: ${pid}`);
    const values = raw.slice(close + 1).trim().split(/\s+/);
    const ticks = Number(values[11]) + Number(values[12]); // utime/stime, fields 14/15.
    invariant(Number.isSafeInteger(ticks) && ticks >= 0, `invalid measured process CPU counters: ${pid}`);
    processTicks[String(pid)] = ticks;
  }
  const runnerCPU = process.cpuUsage();
  return { monotonic_ns: process.hrtime.bigint().toString(), cpu_line: cpuLine, host_busy_ticks: busyTicks,
    process_ticks: processTicks, runner_user_us: runnerCPU.user, runner_system_us: runnerCPU.system,
    loadavg: loadRaw, load_1m: load1m, cpu_pressure: pressure, cpu_some_avg10_percent: Number(pressureMatch[1]) };
}

export function hostInterval(before, after, clockTicksPerSecond) {
  invariant(Number.isSafeInteger(clockTicksPerSecond) && clockTicksPerSecond > 0, 'invalid host clock tick denominator');
  const elapsedSeconds = Number(BigInt(after.monotonic_ns) - BigInt(before.monotonic_ns)) / 1e9;
  const hostBusyDelta = after.host_busy_ticks - before.host_busy_ticks;
  const beforePids = Object.keys(before.process_ticks).toSorted();
  invariant(isDeepStrictEqual(beforePids, Object.keys(after.process_ticks).toSorted()), 'measured process identity changed during host interval');
  const measuredTicksDelta = beforePids.reduce((sum, pid) => sum + after.process_ticks[pid] - before.process_ticks[pid], 0);
  const runnerMicrosecondsDelta = after.runner_user_us + after.runner_system_us - before.runner_user_us - before.runner_system_us;
  invariant(finite(elapsedSeconds) && elapsedSeconds > 0 && Number.isSafeInteger(hostBusyDelta) && hostBusyDelta >= 0 &&
    Number.isSafeInteger(measuredTicksDelta) && measuredTicksDelta >= 0 && Number.isSafeInteger(runnerMicrosecondsDelta) && runnerMicrosecondsDelta >= 0,
  'missing or regressed host CPU counters');
  const hostBusyCores = hostBusyDelta / clockTicksPerSecond / elapsedSeconds;
  const measuredBusyCores = measuredTicksDelta / clockTicksPerSecond / elapsedSeconds + runnerMicrosecondsDelta / 1e6 / elapsedSeconds;
  const externalBusyCores = Math.max(0, hostBusyCores - measuredBusyCores);
  invariant(finite(externalBusyCores), 'invalid external CPU estimate');
  return { before, after, elapsed_seconds: elapsedSeconds, clock_ticks_per_second: clockTicksPerSecond,
    host_busy_ticks_delta: hostBusyDelta, measured_process_ticks_delta: measuredTicksDelta,
    runner_cpu_microseconds_delta: runnerMicrosecondsDelta, host_busy_cores: hostBusyCores,
    measured_busy_cores: measuredBusyCores, external_busy_cores: externalBusyCores };
}

export function evaluateHostPreflight(samples, intervals, rule = hostStabilityRule) {
  invariant(samples.length === rule.preflight_samples && intervals.length === rule.preflight_samples - 1, 'incomplete fixed host preflight');
  const failures = [];
  for (let index = 0; index < samples.length; index++) {
    const sample = samples[index];
    if (!(sample.load_1m < rule.preflight_max_load_1m_exclusive)) failures.push(`sample ${index}: load ${sample.load_1m}`);
    if (!(sample.cpu_some_avg10_percent < rule.preflight_max_cpu_some_avg10_percent_exclusive)) failures.push(`sample ${index}: CPU pressure ${sample.cpu_some_avg10_percent}`);
  }
  for (let index = 0; index < intervals.length; index++) {
    if (!(intervals[index].external_busy_cores < rule.preflight_max_external_busy_cores_exclusive)) failures.push(`window ${index}: external cores ${intervals[index].external_busy_cores}`);
  }
  return { passed: failures.length === 0, failures };
}

export function evaluateCaptureHostInterval(interval, previousElevated, rule = hostStabilityRule) {
  const external = interval.external_busy_cores;
  invariant(finite(external) && external >= 0, 'missing per-block external CPU estimate');
  const elevated = external >= rule.capture_external_cores_two_consecutive_blocks_invalid_at;
  const invalid = external >= rule.capture_external_cores_single_block_invalid_at || (previousElevated && elevated);
  return { external_busy_cores: external, elevated, invalid,
    reason: invalid ? external >= rule.capture_external_cores_single_block_invalid_at
      ? `one block reached ${external} external busy cores` : `two consecutive blocks reached at least ${rule.capture_external_cores_two_consecutive_blocks_invalid_at} external busy cores` : null };
}

async function runHostPreflight(clockTicksPerSecond, rule) {
  const samples = [readHostSnapshot()];
  const intervals = [];
  for (let index = 1; index < rule.preflight_samples; index++) {
    await new Promise((resolveWait) => setTimeout(resolveWait, rule.preflight_interval_seconds * 1000));
    if (index === rule.preflight_samples - 1) {
      const elapsedSeconds = Number(process.hrtime.bigint() - BigInt(samples[0].monotonic_ns)) / 1e9;
      if (elapsedSeconds < rule.preflight_seconds) {
        await new Promise((resolveWait) => setTimeout(resolveWait, Math.ceil((rule.preflight_seconds - elapsedSeconds) * 1000)));
      }
    }
    samples.push(readHostSnapshot());
    intervals.push(hostInterval(samples[index - 1], samples[index], clockTicksPerSecond));
  }
  invariant(Number(BigInt(samples.at(-1).monotonic_ns) - BigInt(samples[0].monotonic_ns)) / 1e9 >= rule.preflight_seconds,
    'host preflight duration was shorter than the frozen full minute');
  const result = evaluateHostPreflight(samples, intervals, rule);
  return { samples, intervals, ...result };
}
function command(program, args, cwd) {
  const result = spawnSync(program, args, { cwd, encoding: 'utf8', maxBuffer: 1 << 20 });
  invariant(result.status === 0, `${program} failed: ${result.stderr}`);
  return result.stdout.trim();
}
function gitSucceeds(args, cwd) {
  return spawnSync('git', args, { cwd, encoding: 'utf8', maxBuffer: 1 << 20 }).status === 0;
}
export function validateOutputReservation(mode, output, root, manifest) {
  const evidence = mode === 'B-only' ? manifest.B_only_evidence_path : manifest.paired_evidence_path;
  const journal = mode === 'B-only' ? manifest.B_only_journal_path : manifest.paired_journal_path;
  invariant(mode === 'B-only' || mode === 'paired', 'unknown source study mode');
  invariant(resolve(output) === resolve(root, evidence), `${mode} output must use the single frozen evidence path`);
  invariant(resolve(`${output}.journal.jsonl`) === resolve(root, journal), `${mode} journal must use the single frozen reservation path`);
  invariant(!existsSync(output) && !existsSync(resolve(root, journal)), `${mode} output or journal already reserves this attempt`);
  return { output: resolve(root, evidence), journal: resolve(root, journal) };
}

export function validateUntrackedBReservation(root, manifest) {
  for (const path of [manifest.B_only_evidence_path, manifest.B_only_journal_path]) {
    invariant(existsSync(resolve(root, path)), `measured B reservation is missing: ${path}`);
    const status = command('git', ['status', '--porcelain', '--untracked-files=all', '--', path], root);
    invariant(status === `?? ${path}`, `paired B must retain ${path} as an untracked reservation on the measured method commit`);
  }
  return true;
}

function gitBlob(root, revision, path, cache) {
  const key = `${revision}:${path}`;
  if (cache.has(key)) return cache.get(key);
  const result = spawnSync('git', ['rev-parse', '--verify', key], { cwd: root, encoding: 'utf8', maxBuffer: 1 << 20 });
  const value = result.status === 0 ? result.stdout.trim() : null;
  cache.set(key, value);
  return value;
}

export function verifyJournal(capture, journalPath) {
  invariant(capture.journal_sha256 === digest(journalPath), 'capture does not bind its exact reservation journal bytes');
  const lines = readFileSync(journalPath, 'utf8').trimEnd().split('\n').map((line) => JSON.parse(line));
  invariant(lines.length === capture.blocks.length + 3, 'reservation journal gate/block count or terminal record changed');
  const header = lines[0]?.header;
  invariant(header?.schema === capture.schema && header.mode === capture.mode && header.status === 'in_progress' &&
    header.manifest_sha256 === capture.manifest_sha256 && isDeepStrictEqual(header.B, capture.B) &&
    isDeepStrictEqual(header.C, capture.C) && isDeepStrictEqual(header.provenance, capture.provenance), 'reservation journal header differs from captured source identity');
  invariant(isDeepStrictEqual(lines[1]?.semantic_gate, capture.semantic_gate), 'reservation journal semantic gate differs from JSON artifact');
  for (let index = 0; index < capture.blocks.length; index++) {
    invariant(isDeepStrictEqual(lines[index + 2]?.block, capture.blocks[index]), `reservation journal block ${index} differs from JSON artifact`);
  }
  invariant(lines.at(-1)?.completion?.status === 'complete' &&
    lines.at(-1).completion.block_count === capture.blocks.length, 'reservation journal lacks a complete terminal record');
  return true;
}

function productionGoInventory(root, revision) {
  return command('git', ['ls-tree', '-r', revision], root).split('\n').filter((line) => {
    const path = line.slice(line.indexOf('\t') + 1);
    return path.endsWith('.go') && !path.endsWith('_test.go');
  });
}

// Resolve against the recorded C revision, not the checkout's current HEAD:
// a later documentation-only descendant must not erase or create chronology.
export function resolvePairedProvenance(root, baselinePath, CRevision, manifest) {
  invariant(/^[0-9a-f]{40}$/.test(CRevision) && gitSucceeds(['cat-file', '-e', `${CRevision}^{commit}`], root), 'locked C commit is unavailable');
  const trackedPath = relative(root, resolve(baselinePath));
  invariant(trackedPath === manifest.B_only_evidence_path, 'B-only evidence path differs from frozen manifest');
  const journalPath = resolve(root, manifest.B_only_journal_path);
  invariant(existsSync(journalPath), 'B-only reservation journal is missing');
  const baselineSHA256 = digest(baselinePath);
  const journalSHA256 = digest(journalPath);
  const baselineBlob = command('git', ['hash-object', resolve(baselinePath)], root);
  const journalBlob = command('git', ['hash-object', journalPath], root);
  const cache = new Map();
  invariant(gitBlob(root, CRevision, trackedPath, cache) === baselineBlob, 'locked C does not contain the exact B-only artifact blob');
  invariant(gitBlob(root, CRevision, manifest.B_only_journal_path, cache) === journalBlob, 'locked C does not contain the exact B-only journal blob');
  const graph = command('git', ['rev-list', '--parents', CRevision], root).split('\n').filter(Boolean)
    .map((line) => { const [revision, ...parents] = line.split(/\s+/); return { revision, parents }; });
  const creations = { [trackedPath]: [], [manifest.B_only_journal_path]: [] };
  for (const { revision, parents } of graph) {
    for (const path of [trackedPath, manifest.B_only_journal_path]) {
      const current = gitBlob(root, revision, path, cache);
      const before = parents.map((parent) => gitBlob(root, parent, path, cache));
      if (!current) {
        invariant(before.every((blob) => !blob), `${path}: committed evidence was deleted in C ancestry`);
      } else if (before.every((blob) => !blob)) {
        creations[path].push(revision);
      } else {
        invariant(before.every((blob) => !blob || blob === current), `${path}: committed evidence was edited in C ancestry`);
      }
    }
  }
  invariant(creations[trackedPath].length === 1 && creations[manifest.B_only_journal_path].length === 1 &&
    creations[trackedPath][0] === creations[manifest.B_only_journal_path][0], 'B-only JSON and journal require one shared creation commit');
  const evidenceCommit = creations[trackedPath][0];
  invariant(graph.find((item) => item.revision === evidenceCommit)?.parents.length === 1, 'B-only evidence must be created in a normal commit, not a merge');
  invariant(gitBlob(root, evidenceCommit, trackedPath, cache) === baselineBlob &&
    gitBlob(root, evidenceCommit, manifest.B_only_journal_path, cache) === journalBlob, 'evidence creation commit has different JSON or journal bytes');
  invariant(evidenceCommit !== CRevision && gitSucceeds(['merge-base', '--is-ancestor', evidenceCommit, CRevision], root), 'B-only evidence must be a strict ancestor of C');
  invariant(gitSucceeds(['merge-base', '--is-ancestor', manifest.C_product_revision, CRevision], root), 'locked C must descend from the preregistered product');
  const productInventory = productionGoInventory(root, manifest.C_product_revision);
  invariant(isDeepStrictEqual(productionGoInventory(root, CRevision), productInventory), 'locked C production Go blobs differ from bc325');
  return { B_only_path: trackedPath, B_only_sha256: baselineSHA256, B_only_blob_oid: baselineBlob,
    B_journal_path: manifest.B_only_journal_path, B_journal_sha256: journalSHA256, B_journal_blob_oid: journalBlob,
    evidence_commit: evidenceCommit, product_revision: manifest.C_product_revision,
    product_go_inventory_sha256: hash(productInventory.join('\n')),
    C_revision: CRevision };
}

export function verifyPairedProvenance(capture, baseline, manifest, root, baselinePath) {
  invariant(capture.B_only_sha256 === digest(baselinePath) && capture.B_only_sha256 === hash(JSON.stringify(baseline, null, 2) + '\n'), 'B-only artifact bytes differ from capture');
  verifyJournal(baseline, resolve(root, manifest.B_only_journal_path));
  verifyJournal(capture, resolve(root, manifest.paired_journal_path));
  const expected = resolvePairedProvenance(root, baselinePath, capture.C?.revision, manifest);
  invariant(capture.B?.revision === baseline.B?.revision, 'paired B must reuse the measured historical method revision');
  const evidenceParents = command('git', ['rev-list', '--parents', '-n', '1', expected.evidence_commit], root).split(/\s+/);
  invariant(evidenceParents.length === 2 && baseline.B?.revision === evidenceParents[1], 'separate B-only evidence branch must start at the measured method revision');
  invariant(isDeepStrictEqual(capture.provenance, expected), 'paired artifact chronology or blob provenance differs from Git history');
  return expected;
}
function sourceIdentity(root, manifest, B, allowedUntracked = []) {
  const revision = command('git', ['rev-parse', 'HEAD'], root);
  const status = command('git', ['status', '--porcelain', '--untracked-files=normal'], root);
  const allowed = new Set(allowedUntracked.map((path) => relative(root, path)));
  const unexpected = status.split('\n').filter(Boolean).filter((line) => !line.startsWith('?? ') || !allowed.has(line.slice(3)));
  invariant(unexpected.length === 0, `${B ? 'B' : 'C'} source tree is dirty: ${unexpected.join(', ')}`);
  invariant(digest(resolve(root, oldFiles.harness)) === manifest.old_harness_sha256, 'frozen v1 harness differs');
  invariant(digest(resolve(root, oldFiles.runner)) === manifest.old_runner_sha256, 'frozen v1 runner differs');
  invariant(digest(resolve(root, oldFiles.baseline)) === manifest.old_baseline_sha256, 'frozen v1 baseline differs');
  invariant(digest(resolve(root, oldFiles.budgets)) === manifest.old_budgets_sha256, 'frozen v1 budgets differ');
  invariant(digest(resolve(root, oldFiles.pairedHarness)) === manifest.paired_harness_sha256, 'v2 harness differs');
  invariant(digest(resolve(root, oldFiles.pairedRunner)) === manifest.paired_runner_sha256, 'v2 runner differs');
  invariant(command('git', ['merge-base', manifest.B_product_revision, 'HEAD'], root) === manifest.B_product_revision, `${B ? 'B' : 'C'} does not descend from historical product`);
  if (B) {
    const changed = command('git', ['diff', '--name-only', manifest.B_product_revision, 'HEAD'], root).split('\n').filter(Boolean);
    invariant(changed.every((name) => name === oldFiles.pairedHarness || name === oldFiles.pairedRunner || name === oldFiles.baseline || name === oldFiles.budgets ||
      name === oldFiles.r6Artifact || name === oldFiles.r6Journal || name === 'scripts/fidelity-paired-v2.mjs' || name === 'scripts/fidelity-paired-v2.test.mjs' ||
      name === 'scripts/fidelity-paired-r7.test.mjs' || name === 'docs/evidence/fidelity-performance/source-paired-v2/manifest.json' ||
      name === 'docs/evidence/fidelity-performance/source-paired-v2/manifest-r2.json' || name === 'docs/evidence/fidelity-performance/source-paired-v2/manifest-r3.json' ||
      name === 'docs/evidence/fidelity-performance/source-paired-v2/manifest-r4.json' || name === 'docs/evidence/fidelity-performance/source-paired-v2/manifest-r5.json' ||
      name === oldFiles.r6Manifest || name === 'docs/evidence/fidelity-performance/source-paired-v2/manifest-r7.json' ||
      name === 'docs/evidence/fidelity-performance/source-paired-v2/README.md' || name === 'docs/evidence/fidelity-performance/source-paired-v2/README-r7.md'), `B product differs: ${changed}`);
  }
  return { revision, root, working_tree: status };
}

function buildBinary(binaryPath, root, manifest, B, allowedUntracked = []) {
  const absolute = resolve(binaryPath);
  const withinRoot = relative(root, absolute);
  invariant(isAbsolute(withinRoot) || withinRoot.startsWith('..'), 'benchmark binary output must be outside the source checkout');
  invariant(!existsSync(absolute), `refusing to reuse or overwrite benchmark binary: ${absolute}`);
  const before = sourceIdentity(root, manifest, B, allowedUntracked);
  const args = ['test', '-c', '-mod=readonly', '-o', absolute, './internal/gateway'];
  const result = spawnSync('go', args, { cwd: root, encoding: 'utf8', maxBuffer: 4 << 20 });
  invariant(result.status === 0 && existsSync(absolute), `fresh ${B ? 'B' : 'C'} test binary build failed: ${result.stderr}`);
  const after = sourceIdentity(root, manifest, B, allowedUntracked);
  invariant(isDeepStrictEqual(before, after), 'source checkout changed during binary build');
  return { ...before, binary_sha256: digest(absolute), build_command: ['go', ...args], built_at: new Date().toISOString() };
}

class GoArm {
  constructor(binary, label, environment) {
    this.binary = binary;
    this.label = label;
    this.pending = [];
    this.ready = new Promise((resolveReady, rejectReady) => { this.resolveReady = resolveReady; this.rejectReady = rejectReady; });
    this.readyTimer = setTimeout(() => { this.process.kill(); this.abort(new Error(`${label}: readiness timeout`)); }, 60_000);
    this.process = spawn(binary, ['-test.run=^TestFidelityPairedServer$', '-test.benchtime=64x', '-test.timeout=0'],
      { env: { ...process.env, ...environment, OLP_SOURCE_PAIRED_SERVER: '1', OLP_SOURCE_PAIRED_R7_FORCE_EFFECT_MISMATCH: '',
        GOMAXPROCS: '4', GOGC: '100', GOMEMLIMIT: 'off', GODEBUG: '' }, stdio: ['pipe', 'pipe', 'pipe'] });
    this.stderr = '';
    this.stdoutDiagnostics = '';
    this.process.stderr.on('data', (chunk) => { this.stderr = (this.stderr + chunk.toString()).slice(-4096); });
    let buffer = '';
    this.process.stdout.on('data', (chunk) => {
      buffer += chunk.toString();
      while (buffer.includes('\n')) {
        const index = buffer.indexOf('\n');
        const line = buffer.slice(0, index).trim();
        buffer = buffer.slice(index + 1);
        if (!line.startsWith(PREFIX)) { this.stdoutDiagnostics = (this.stdoutDiagnostics + line + '\n').slice(-4096); continue; }
        let reply;
        try { reply = JSON.parse(line.slice(PREFIX.length)); } catch (error) { this.abort(error); return; }
        if (reply.ready) { clearTimeout(this.readyTimer); this.resolveReady(); continue; }
        const pending = this.pending.shift();
        if (!pending) { this.abort(new Error(`${label}: unsolicited reply`)); return; }
        clearTimeout(pending.timer);
        if (reply.id !== pending.id) pending.reject(new Error(`${label}: reply ID mismatch`));
        else pending.resolve(reply);
      }
    });
    this.process.on('error', (error) => this.abort(error));
    this.process.on('exit', (code, signal) => this.abort(new Error(`${label} exited (${code ?? signal}): ${this.stderr}`)));
  }
  abort(error) {
    clearTimeout(this.readyTimer);
    this.rejectReady(error);
    for (const pending of this.pending.splice(0)) { clearTimeout(pending.timer); pending.reject(error); }
  }
  async request(id, name) {
    await this.ready;
    return new Promise((resolveReply, rejectReply) => {
      const timer = setTimeout(() => { this.process.kill(); rejectReply(new Error(`${this.label}: subrun timeout: ${name}`)); }, 120_000);
      this.pending.push({ id, resolve: resolveReply, reject: rejectReply, timer });
      this.process.stdin.write(JSON.stringify({ id, name }) + '\n');
    });
  }
  close() { this.process.stdin.end(); }
  kill() { this.process.kill(); }
}

function safeDiagnostic(value) {
  return String(value || '').replaceAll('benchmark-client-key', '<REDACTED>').replaceAll('benchmark-provider-key', '<REDACTED>').slice(-4096);
}

export class SemanticFailure extends Error {
  constructor(arm, name, detail) {
    super(`${arm} ${name}: semantic oracle, effect count or fixed sample count failed`);
    this.name = 'SemanticFailure';
    this.detail = { arm, name, ...detail };
  }
}

export const failureDisposition = (error, hasCandidate) => error instanceof SemanticFailure && hasCandidate ? 'failed' : 'invalid';

export function checkedSemanticReply(reply, arm, name, manifest, diagnostics = {}) {
  if (reply?.error) throw new SemanticFailure(arm, name, { stage: reply.failure?.stage || 'unclassified_adapter_failure',
    adapter_error: safeDiagnostic(reply.error), adapter_failure: reply.failure || null,
    stdout_tail: safeDiagnostic(diagnostics.stdout), stderr_tail: safeDiagnostic(diagnostics.stderr) });
  try { validateSubrun(reply, name, manifest); }
  catch (error) { throw new SemanticFailure(arm, name, { stage: 'independent_runner_oracle',
    runner_error: safeDiagnostic(error.message), stdout_tail: safeDiagnostic(diagnostics.stdout), stderr_tail: safeDiagnostic(diagnostics.stderr) }); }
  return reply;
}

async function requestChecked(process, id, arm, name, manifest) {
  let reply;
  try { reply = await process.request(id, name); }
  catch (error) { throw new SemanticFailure(arm, name, { stage: 'process_or_protocol_failure',
    process_error: safeDiagnostic(error.message), stdout_tail: safeDiagnostic(process.stdoutDiagnostics), stderr_tail: safeDiagnostic(process.stderr) }); }
  return checkedSemanticReply(reply, arm, name, manifest, { stdout: process.stdoutDiagnostics, stderr: process.stderr });
}

function percentile(values, p) {
  const sorted = [...values].sort((a, b) => a - b);
  return sorted[Math.ceil(p * sorted.length / 100) - 1];
}
export function validateSubrun(reply, name, manifest) {
  invariant(!reply?.error && !reply?.failure, `${name}: adapter reported a semantic failure`);
  const group = name.slice(0, name.lastIndexOf('/'));
  const rejected = group.startsWith('rejected_extension/');
  const events = group.startsWith('native_stream_256/') ? 256 : group.startsWith('native_slow_stream_64/') ? 64 : 0;
  const eventBytes = group.startsWith('native_stream_256/') ? 128 : group.startsWith('native_slow_stream_64/') ? 16384 : 0;
  invariant(reply.name === name && reply.iterations === SAMPLES && reply.samples?.length === SAMPLES, `${name}: missing fixed samples`);
  const metrics = reply.metrics;
  invariant(metrics && Object.keys(metrics).length === metricList(group).length + 5, `${name}: metric inventory changed`);
  for (const metric of metricList(group)) invariant(finite(metrics[metric]) && metrics[metric] >= 0, `${name}: invalid ${metric}`);
  invariant(metrics.dispatches === (rejected ? 0 : SAMPLES) && metrics.succeeded === (rejected ? 0 : SAMPLES) && metrics.rejected === (rejected ? SAMPLES : 0), `${name}: provider effects changed`);
  invariant(metrics['request-bytes'] === manifest.exact[name]['request-bytes'] && metrics['content-events/op'] === manifest.exact[name]['content-events/op'], `${name}: frozen request bytes/event count changed`);
  for (const sample of reply.samples) {
    invariant(finite(sample.latency_us) && sample.latency_us > 0 && sample.events === events && sample.text_bytes === events * eventBytes, `${name}: request/event oracle sample changed`);
    invariant(finite(sample.first_event_us) && finite(sample.max_inter_event_gap_us) && sample.first_event_us >= 0 && sample.max_inter_event_gap_us >= 0, `${name}: invalid event timing`);
    if (!events) invariant(sample.first_event_us === 0 && sample.max_inter_event_gap_us === 0, `${name}: unary event timing changed`);
  }
  for (const [prefix, field] of [['latency', 'latency_us'], ['first-event', 'first_event_us'], ['max-inter-event-gap', 'max_inter_event_gap_us']]) {
    if (prefix !== 'latency' && !events) continue;
    for (const p of [50, 95, 99]) invariant(Math.abs(metrics[`${prefix}-p${p}-us`] - percentile(reply.samples.map((sample) => sample[field]), p)) < 0.000001, `${name}: p${p} is not the fixed per-request order statistic`);
  }
  invariant(reply.runtime && ['num_gc_delta', 'pause_total_ns_delta', 'total_alloc_bytes_delta', 'heap_alloc_after_bytes', 'goroutines_after', 'provider_effect_wait_ns'].every((key) => Number.isSafeInteger(reply.runtime[key]) && reply.runtime[key] >= 0), `${name}: runtime/GC/effect-wait diagnostics missing`);
  invariant(Array.isArray(reply.scheduler?.latency_bucket_seconds) && reply.scheduler.latency_bucket_seconds.length > 1 &&
    Array.isArray(reply.scheduler.latency_counts_delta) && reply.scheduler.latency_counts_delta.length + 1 === reply.scheduler.latency_bucket_seconds.length &&
    reply.scheduler.latency_bucket_seconds.every((value) => typeof value === 'string') &&
    reply.scheduler.latency_counts_delta.every((value) => Number.isSafeInteger(value) && value >= 0), `${name}: scheduler diagnostics missing`);
}

function validateManifest(manifest, root) {
  invariant(isDeepStrictEqual(manifest, makeManifest(root)), 'manifest differs from frozen source, v1 artifacts or decision rule');
}
function hardwareMatches(actual, old) {
  return Object.keys(old).every((key) => actual[key] === old[key]);
}
function validateCapture(capture, manifest, baselineOnly) {
  invariant(capture.schema === SCHEMA && capture.mode === (baselineOnly ? 'B-only' : 'paired') && capture.manifest_sha256 === hash(JSON.stringify(manifest)), 'capture or manifest identity changed');
  invariant(capture.status === 'complete', 'incomplete capture cannot qualify');
  invariant(hardwareMatches(capture.hardware, manifest.old_hardware) && capture.toolchain === manifest.old_toolchain && capture.go_build_environment === manifest.old_go_build_environment, 'hardware or toolchain changed');
  invariant(isDeepStrictEqual(capture.runtime_environment, manifest.old_runtime_environment), 'Go runtime environment changed');
  invariant(isDeepStrictEqual(capture.conditions, manifest.conditions), 'source measurement conditions changed');
  invariant(isDeepStrictEqual(capture.host_stability_rule, manifest.host_stability) &&
    capture.clock_ticks_per_second === manifest.host_stability.clock_ticks_per_second, 'host-stability method or denominator changed');
  const preflight = capture.host_preflight;
  invariant(preflight?.samples?.length === manifest.host_stability.preflight_samples &&
    preflight.intervals?.length === manifest.host_stability.preflight_samples - 1, 'host preflight missing or incomplete');
  for (let index = 0; index < preflight.intervals.length; index++) {
    invariant(isDeepStrictEqual(preflight.intervals[index], hostInterval(preflight.samples[index], preflight.samples[index + 1], capture.clock_ticks_per_second)), `host preflight window ${index} counters changed`);
  }
  const preflightDuration = Number(BigInt(preflight.samples.at(-1).monotonic_ns) - BigInt(preflight.samples[0].monotonic_ns)) / 1e9;
  invariant(preflightDuration >= manifest.host_stability.preflight_seconds, 'host preflight did not sustain its full duration');
  const preflightDecision = evaluateHostPreflight(preflight.samples, preflight.intervals, manifest.host_stability);
  invariant(preflightDecision.passed && isDeepStrictEqual(preflightDecision, { passed: preflight.passed, failures: preflight.failures }), 'host preflight limit failed');
  invariant(typeof capture.B?.revision === 'string' && typeof capture.B?.binary_sha256 === 'string', 'B binary identity missing');
  if (!baselineOnly) {
    invariant(capture.C && capture.C.revision !== capture.B.revision, 'C revision missing');
    validateStrictRouteContracts(capture.C_route_contract);
    invariant(isDeepStrictEqual(capture.C_route_contract, manifest.C_route_contract), 'C route contract differs from frozen strict manifest');
    invariant(typeof capture.B_only_sha256 === 'string' && capture.B_only_sha256.length === 64, 'committed B-only artifact identity missing');
  }
  const gateOrder = semanticGateOrder(!baselineOnly);
  invariant(hash(JSON.stringify(gateOrder)) === manifest.semantic_gate_order_sha256[baselineOnly ? 'B_only' : 'paired'], 'semantic gate order hash changed');
  invariant(capture.semantic_gate?.length === gateOrder.length, 'complete all-22 semantic gate is missing');
  for (let i = 0; i < gateOrder.length; i++) {
    const expected = gateOrder[i], observed = capture.semantic_gate[i];
    invariant(observed.arm === expected.arm && observed.name === expected.name, `semantic gate ${i}: arm or name changed`);
    validateSubrun(observed.reply, expected.name, manifest);
  }
  for (const diagnostics of [capture.diagnostics_before, capture.diagnostics_after]) invariant(typeof diagnostics?.loadavg === 'string' && typeof diagnostics?.cpu_pressure === 'string', 'capture host diagnostics missing');
  const schedule = sourceSchedule(manifest.seed);
  invariant(capture.blocks.length === schedule.length && hash(JSON.stringify(schedule)) === manifest.schedule_sha256, 'block schedule or count changed');
  let previousElevated = false;
  for (let i = 0; i < schedule.length; i++) {
    const actual = capture.blocks[i], expected = schedule[i];
    invariant(actual.group === expected.group && actual.index === expected.index && actual.variant === expected.variant && actual.outer === expected.outer, `block ${i}: schedule changed`);
    for (const diagnostics of [actual.diagnostics_before, actual.diagnostics_after]) invariant(typeof diagnostics?.loadavg === 'string' && typeof diagnostics?.cpu_pressure === 'string', `block ${i}: host diagnostics missing`);
    invariant(isDeepStrictEqual(actual.host_cpu_interval, hostInterval(actual.host_cpu_interval.before, actual.host_cpu_interval.after, capture.clock_ticks_per_second)), `block ${i}: host CPU counters changed`);
    const hostDecision = evaluateCaptureHostInterval(actual.host_cpu_interval, previousElevated, manifest.host_stability);
    invariant(isDeepStrictEqual(actual.host_stability_decision, hostDecision) && !hostDecision.invalid, `block ${i}: external CPU invalidated the whole attempt`);
    previousElevated = hostDecision.elevated;
    const arms = sourceOrder(expected.group, expected.variant, baselineOnly, expected.outer);
    invariant(actual.subruns.length === arms.length && actual.subruns.every((subrun, j) => subrun.arm === arms[j]), `block ${i}: arm order/count changed`);
    for (const subrun of actual.subruns) validateSubrun(subrun.reply, pathName(expected.group, subrun.arm.split('/')[1]), manifest);
  }
}

function perName(capture, name, arm) {
  return capture.blocks.filter((block) => block.group === name.slice(0, name.lastIndexOf('/')))
    .flatMap((block) => block.subruns.filter((run) => run.arm === `${arm}/${name.slice(name.lastIndexOf('/') + 1)}`).map((run) => run.reply));
}
export function analyzeBaseline(capture, manifest) {
  validateCapture(capture, manifest, true);
  const envelope = {};
  const absoluteVariability = {};
  const failures = [];
  for (const [name, metrics] of Object.entries(manifest.comparisons)) {
    const runs = perName(capture, name, 'B');
    invariant(runs.length === 64, `${name}: B-only sample floor changed`);
    envelope[name] = {};
    absoluteVariability[name] = {};
    const group = name.slice(0, name.lastIndexOf('/'));
    const path = name.slice(name.lastIndexOf('/') + 1);
    for (const [metric, old] of Object.entries(metrics)) {
      const observed = median(runs.map((run) => run.metrics[metric]));
      const passed = observed <= old.old_limit;
      envelope[name][metric] = { B_median: observed, old_limit: old.old_limit, passed };
      if (!passed) failures.push(`${name} ${metric}: B-only median ${observed} > frozen ${old.old_limit}`);
      const values = capture.blocks.filter((block) => block.group === group).map((block) => {
        const pair = block.subruns.filter((run) => run.arm === `B/${path}`);
        invariant(pair.length === 2, `${name}: missing B-only block pair`);
        return Math.abs(pair[0].reply.metrics[metric] - pair[1].reply.metrics[metric]);
      });
      const sorted = [...values].sort((a, b) => a - b);
      absoluteVariability[name][metric] = { values, median: median(values), upper: sorted[UPPER_INDEX], old_margin: old.margin, diagnostic_only: true };
    }
  }
  return { passed: failures.length === 0, failures, envelope, absolute_within_block_variability: absoluteVariability,
    count: { blocks: capture.blocks.length, B_subruns: capture.blocks.reduce((n, block) => n + block.subruns.length, 0),
      B_requests: 22 * 64 * 64, B_semantic_gate_requests: 22 * 64 } };
}

function pairedValues(blocks, path, metric) {
  return blocks.map((block) => {
    const means = {};
    for (const arm of ['B', 'C']) {
      const runs = block.subruns.filter((run) => run.arm === `${arm}/${path}`);
      invariant(runs.length === 2, `${block.group}: ${arm}/${path} missing pair`);
      means[arm] = (runs[0].reply.metrics[metric] + runs[1].reply.metrics[metric]) / 2;
    }
    return means.C - means.B;
  });
}
function pairedResult(values, old, control) {
  invariant(values.length === BLOCKS && values.every(finite), 'incomplete paired statistic');
  const sorted = [...values].sort((a, b) => a - b);
  const lower = sorted[LOWER_INDEX], upper = sorted[UPPER_INDEX];
  return { values, median: median(values), lower, upper, ...old, passed: upper < old.margin,
    ...(control ? { control_passed: lower > -old.margin && upper < old.margin } : {}) };
}
export function analyzePaired(capture, baseline, manifest) {
  validateCapture(baseline, manifest, true);
  validateCapture(capture, manifest, false);
  invariant(isDeepStrictEqual(capture.hardware, baseline.hardware) && capture.manifest_sha256 === baseline.manifest_sha256, 'B-only and paired measurement conditions differ');
  invariant(capture.B?.revision === baseline.B?.revision && capture.B?.binary_sha256 === baseline.B?.binary_sha256, 'historical B method revision or binary changed');
  invariant(capture.B_only_sha256 === hash(JSON.stringify(baseline, null, 2) + '\n'), 'B-only artifact reference changed');
  const BOnly = analyzeBaseline(baseline, manifest);
  const failures = [...BOnly.failures.map((failure) => `B-only envelope: ${failure}`)];
  const controls = [];
  const primaries = [];
  const envelope = {};
  const results = {};
  const descriptiveAbsoluteC = {};
  const descriptiveAbsoluteFailures = [];
  for (const group of groups) {
    const blocks = capture.blocks.filter((block) => block.group === group);
    invariant(blocks.length === BLOCKS, `${group}: missing paired blocks`);
    for (const path of groupPaths(group)) {
      const name = pathName(group, path), runs = perName(capture, name, 'B');
      invariant(runs.length === 64 && perName(capture, name, 'C').length === 64, `${name}: missing B/C subruns`);
      results[name] = {};
      envelope[name] = {};
      descriptiveAbsoluteC[name] = {};
      for (const [metric, old] of Object.entries(manifest.comparisons[name])) {
        const BMedian = median(runs.map((run) => run.metrics[metric]));
        envelope[name][metric] = { B_median: BMedian, old_limit: old.old_limit, passed: BMedian <= old.old_limit };
        if (BMedian > old.old_limit) failures.push(`${name} ${metric}: contemporaneous B median ${BMedian} > frozen ${old.old_limit}`);
        const result = pairedResult(pairedValues(blocks, path, metric), old, path === 'relay');
        result.B_median = BMedian;
        result.C_median = median(perName(capture, name, 'C').map((run) => run.metrics[metric]));
        descriptiveAbsoluteC[name][metric] = { C_median: result.C_median, old_v1_limit: old.old_limit,
          within_old_v1_limit: result.C_median <= old.old_limit, diagnostic_only: true };
        if (result.C_median > old.old_limit) descriptiveAbsoluteFailures.push(`${name} ${metric}: C median ${result.C_median} > old v1 absolute limit ${old.old_limit}`);
        if (metric === 'ns/op') {
          result.B_requests_per_second = 1e9 / result.B_median;
          result.C_requests_per_second = 1e9 / result.C_median;
        }
        results[name][metric] = result;
        if (!result.passed) primaries.push(`${name} ${metric}: paired d_(23) ${result.upper} >= frozen margin ${old.margin}`);
        if (path === 'relay' && !result.control_passed) controls.push(`${name} ${metric}: relay median interval [${result.lower}, ${result.upper}] outside (-${old.margin}, ${old.margin})`);
      }
    }
  }
  const addedLatency = {};
  for (const group of groups.filter((name) => !name.startsWith('rejected_extension/'))) {
    const blocks = capture.blocks.filter((block) => block.group === group);
    addedLatency[group] = {};
    for (const [metric, old] of Object.entries(manifest.added_latency[group])) {
      const vectors = blocks.map((block) => {
        const mean = (arm, path) => {
          const runs = block.subruns.filter((run) => run.arm === `${arm}/${path}`);
          invariant(runs.length === 2, `${group}: incomplete added latency arm`);
          return (runs[0].reply.metrics[metric] + runs[1].reply.metrics[metric]) / 2;
        };
        return { B: mean('B', 'gateway') - mean('B', 'relay'), C: mean('C', 'gateway') - mean('C', 'relay') };
      });
      const values = vectors.map(({ B, C }) => C - B);
      const result = pairedResult(values, old, false);
      result.B_gateway_minus_relay_values = vectors.map(({ B }) => B);
      result.C_gateway_minus_relay_values = vectors.map(({ C }) => C);
      result.B_gateway_minus_relay_median = median(result.B_gateway_minus_relay_values);
      result.C_gateway_minus_relay_median = median(result.C_gateway_minus_relay_values);
      addedLatency[group][metric] = result;
      if (!result.passed) primaries.push(`${group} added ${metric}: paired d_(23) ${result.upper} >= frozen gateway margin ${old.margin}`);
    }
  }
  const controlFailed = failures.length > 0 || controls.length > 0;
  return { status: controlFailed ? 'inconclusive' : primaries.length ? 'failed' : 'passed',
    failures, controls, primaries, B_only: { passed: BOnly.passed, failures: BOnly.failures },
    envelope, results, added_latency: addedLatency,
    descriptive_absolute_C_vs_v1: { metrics: descriptiveAbsoluteC,
      within_old_limit: 224 - descriptiveAbsoluteFailures.length, above_old_limit: descriptiveAbsoluteFailures.length,
      above_old_limit_details: descriptiveAbsoluteFailures, diagnostic_only: true },
    count: { blocks: capture.blocks.length, source_primary_metrics: 224, added_latency_metrics: 30,
      B_successes: 81920, C_successes: 81920, B_dispatches: 81920, C_dispatches: 81920,
      B_rejections: 8192, C_rejections: 8192, B_semantic_gate_requests: 22 * 64,
      C_semantic_gate_requests: 22 * 64 } };
}

async function record(mode, output, manifestPath, baselinePath, Bbinary, Broot, Cbinary, Croot) {
  const manifest = json(manifestPath);
  validateManifest(manifest, Broot);
  const reservation = validateOutputReservation(mode, output, mode === 'B-only' ? Broot : Croot, manifest);
  const frozenBaseline = mode === 'paired' ? json(baselinePath) : null;
  if (frozenBaseline) {
    const baselineAnalysis = analyzeBaseline(frozenBaseline, manifest);
    invariant(baselineAnalysis.passed, 'frozen B-only baseline envelope failed; C capture is forbidden');
    const trackedPath = relative(Croot, resolve(baselinePath));
    invariant(trackedPath === manifest.B_only_evidence_path, 'B-only artifact must use the frozen evidence path in locked C');
    command('git', ['ls-files', '--error-unmatch', '--', trackedPath], Croot);
    command('git', ['ls-files', '--error-unmatch', '--', manifest.B_only_journal_path], Croot);
    invariant(command('git', ['status', '--porcelain', '--', trackedPath, manifest.B_only_journal_path], Croot) === '', 'B-only JSON and journal must be committed before C capture');
    invariant(frozenBaseline.journal_sha256 === digest(resolve(Croot, manifest.B_only_journal_path)), 'B-only JSON and journal bytes disagree');
  }
  const BReserved = mode === 'paired' ? [resolve(Broot, manifest.B_only_evidence_path), resolve(Broot, manifest.B_only_journal_path)] : [];
  if (mode === 'paired') {
    invariant(BReserved.every(existsSync), 'original B-only output and journal reservation are missing from B checkout');
    invariant(digest(BReserved[0]) === digest(baselinePath) && digest(BReserved[1]) === digest(resolve(Croot, manifest.B_only_journal_path)), 'B checkout reservations differ from committed B evidence');
    validateUntrackedBReservation(Broot, manifest);
  }
  const BSource = sourceIdentity(Broot, manifest, true, BReserved);
  const CSource = mode === 'paired' ? sourceIdentity(Croot, manifest, false) : null;
  const provenance = CSource ? resolvePairedProvenance(Croot, baselinePath, CSource.revision, manifest) : null;
  if (provenance) {
    invariant(BSource.revision === frozenBaseline.B?.revision, 'paired B must remain at the measured historical method commit');
    const parents = command('git', ['rev-list', '--parents', '-n', '1', provenance.evidence_commit], Croot).split(/\s+/);
    invariant(parents.length === 2 && frozenBaseline.B?.revision === parents[1], 'separate B-only evidence branch must start at the measured method commit');
  }
  if (mode === 'paired') invariant(resolve(Bbinary) !== resolve(Cbinary), 'B and C require distinct binary output paths');
  const routeContract = mode === 'paired' ? JSON.parse(process.env.OLP_FIDELITY_BENCH_ROUTE_CONTRACT || 'null') : null;
  const providerContract = mode === 'paired' ? JSON.parse(process.env.OLP_FIDELITY_BENCH_PROVIDER_CONTRACT || 'null') : null;
  if (mode === 'paired') {
    validateStrictRouteContracts(routeContract);
    invariant(isDeepStrictEqual(routeContract, manifest.C_route_contract), 'C route contract differs from frozen strict manifest');
  }
  const currentHardware = hardware();
  invariant(hardwareMatches(currentHardware, manifest.old_hardware), 'physical hardware differs from v1');
  const toolchain = command('go', ['version'], Broot);
  const goBuildEnvironment = command('go', ['env', 'GOFLAGS', 'GOAMD64', 'GOARCH', 'GOOS'], Broot);
  invariant(toolchain === manifest.old_toolchain && goBuildEnvironment === manifest.old_go_build_environment, 'toolchain/build environment differs from v1');
  const B = buildBinary(Bbinary, Broot, manifest, true, BReserved);
  const C = mode === 'paired' ? buildBinary(Cbinary, Croot, manifest, false) : null;
  invariant(mode === 'B-only' || C.revision !== B.revision, 'paired study requires a distinct locked C revision');
  const clockTicksPerSecond = Number(command('getconf', ['CLK_TCK'], Broot));
  invariant(clockTicksPerSecond === manifest.host_stability.clock_ticks_per_second, 'host CPU clock-tick denominator differs from frozen rule');
  const hostPreflight = await runHostPreflight(clockTicksPerSecond, manifest.host_stability);
  invariant(hostPreflight.passed, `host preflight failed before attempt reservation: ${hostPreflight.failures.join('; ')}`);
  const artifact = { schema: SCHEMA, mode, status: 'in_progress', started_at: new Date().toISOString(),
    manifest_sha256: hash(JSON.stringify(manifest)), schedule_sha256: manifest.schedule_sha256,
    B_only_sha256: frozenBaseline ? digest(baselinePath) : null,
    B, C, provenance, C_route_contract: routeContract, C_provider_contract: providerContract,
    hardware: currentHardware, toolchain, go_build_environment: goBuildEnvironment,
    runtime_environment: { GOMAXPROCS: '4', GOGC: '100', GOMEMLIMIT: 'off', GODEBUG: '' },
    clock_ticks_per_second: clockTicksPerSecond, host_stability_rule: manifest.host_stability,
    host_preflight: hostPreflight,
    conditions: manifest.conditions, diagnostics_before: diagnostics(), semantic_gate: [], blocks: [] };
  writeFileSync(reservation.journal, JSON.stringify({ header: { ...artifact, blocks: undefined } }) + '\n', { flag: 'wx' });
  const Bprocess = new GoArm(Bbinary, 'B', { OLP_FIDELITY_BENCH_ROUTE_CONTRACT: '', OLP_FIDELITY_BENCH_PROVIDER_CONTRACT: '' });
  const Cprocess = C ? new GoArm(Cbinary, 'C', { OLP_FIDELITY_BENCH_ROUTE_CONTRACT: JSON.stringify(routeContract), OLP_FIDELITY_BENCH_PROVIDER_CONTRACT: providerContract ? JSON.stringify(providerContract) : '' }) : null;
  try {
    await Bprocess.ready;
    if (Cprocess) await Cprocess.ready;
    const studyPids = [Bprocess.process.pid, ...(Cprocess ? [Cprocess.process.pid] : [])];
    for (const { arm, name } of semanticGateOrder(Boolean(Cprocess))) {
      const selected = arm.startsWith('B/') ? Bprocess : Cprocess;
      const reply = await requestChecked(selected, `gate:${artifact.semantic_gate.length}`, arm, name, manifest);
      artifact.semantic_gate.push({ arm, name, reply });
    }
    appendFileSync(reservation.journal, JSON.stringify({ semantic_gate: artifact.semantic_gate }) + '\n');
    let previousElevated = false;
    for (const block of sourceSchedule(manifest.seed)) {
      const entry = { ...block, diagnostics_before: diagnostics(), subruns: [] };
      const hostBefore = readHostSnapshot(studyPids);
      for (const arm of sourceOrder(block.group, block.variant, !Cprocess, block.outer)) {
        const [version, path] = arm.split('/');
        const name = pathName(block.group, path);
        const id = `${block.group}:${block.index}:${entry.subruns.length}`;
        const reply = await requestChecked(version === 'B' ? Bprocess : Cprocess, id, arm, name, manifest);
        entry.subruns.push({ arm, reply });
      }
      entry.diagnostics_after = diagnostics();
      entry.host_cpu_interval = hostInterval(hostBefore, readHostSnapshot(studyPids), clockTicksPerSecond);
      entry.host_stability_decision = evaluateCaptureHostInterval(entry.host_cpu_interval, previousElevated, manifest.host_stability);
      previousElevated = entry.host_stability_decision.elevated;
      artifact.blocks.push(entry);
      appendFileSync(reservation.journal, JSON.stringify({ block: entry }) + '\n');
      if (entry.host_stability_decision.invalid) throw new Error(`host stability invalidated whole attempt after block ${artifact.blocks.length}: ${entry.host_stability_decision.reason}`);
    }
    artifact.status = 'complete';
    artifact.completed_at = new Date().toISOString();
    artifact.diagnostics_after = diagnostics();
    appendFileSync(reservation.journal, JSON.stringify({ completion: { status: 'complete', block_count: artifact.blocks.length } }) + '\n');
    artifact.journal_sha256 = digest(reservation.journal);
    if (C) {
      invariant(sourceIdentity(Croot, manifest, false, [reservation.journal]).revision === C.revision, 'C checkout changed during paired capture');
      verifyPairedProvenance(artifact, frozenBaseline, manifest, Croot, baselinePath);
    }
    const analysis = mode === 'B-only' ? analyzeBaseline(artifact, manifest) : analyzePaired(artifact, frozenBaseline, manifest);
    artifact.analysis = analysis;
    writeFileSync(reservation.output, JSON.stringify(artifact, null, 2) + '\n', { flag: 'wx' });
    console.log(`${mode} capture ${analysis.status || (analysis.passed ? 'passed' : 'inconclusive')}: ${output}`);
    if (analysis.status !== 'passed' && !analysis.passed) process.exitCode = 1;
  } catch (error) {
    artifact.status = failureDisposition(error, Boolean(C));
    artifact.error = error.message;
    artifact.failure = error instanceof SemanticFailure ? error.detail : { stage: 'host_or_method', detail: safeDiagnostic(error.message) };
    artifact.completed_at = new Date().toISOString();
    artifact.diagnostics_after = diagnostics();
    appendFileSync(reservation.journal, JSON.stringify({ failure: { status: artifact.status, block_count: artifact.blocks.length,
      semantic_gate_count: artifact.semantic_gate.length, error: artifact.error, detail: artifact.failure } }) + '\n');
    artifact.journal_sha256 = digest(reservation.journal);
    writeFileSync(reservation.output, JSON.stringify(artifact, null, 2) + '\n', { flag: 'wx' });
    throw new Error(`${error.message}; retained ${reservation.output} and ${reservation.journal}`);
  } finally {
    Bprocess.kill();
    Cprocess?.kill();
  }
}

function main() {
  const [action, ...args] = process.argv.slice(2);
  if (action === 'freeze-manifest' && args.length === 1) {
    writeFileSync(args[0], JSON.stringify(makeManifest(process.cwd()), null, 2) + '\n', { flag: 'wx' });
    return console.log(`Frozen source r7 manifest: ${args[0]}`);
  }
  if (action === 'record-B' && args.length === 4) return record('B-only', args[0], args[1], null, args[2], args[3]);
  if (action === 'record-paired' && args.length === 7) return record('paired', ...args);
  if (action === 'compare-B' && args.length === 2) {
    const manifest = json(args[1]); validateManifest(manifest, process.cwd());
    invariant(resolve(args[0]) === resolve(process.cwd(), manifest.B_only_evidence_path), 'B-only comparison requires fixed evidence path');
    const capture = json(args[0]);
    verifyJournal(capture, resolve(process.cwd(), manifest.B_only_journal_path));
    const result = analyzeBaseline(capture, manifest);
    console.log(JSON.stringify(result, null, 2));
    if (!result.passed) process.exitCode = 1;
    return;
  }
  if (action === 'compare-paired' && args.length === 5) {
    const manifest = json(args[2]); validateManifest(manifest, args[3]);
    invariant(resolve(args[0]) === resolve(args[3], manifest.paired_evidence_path) &&
      resolve(args[1]) === resolve(args[3], manifest.B_only_evidence_path) &&
      resolve(args[4]) === resolve(args[3], manifest.B_only_evidence_path), 'paired comparison requires fixed JSON and journal paths');
    const capture = json(args[0]), baseline = json(args[1]);
    verifyPairedProvenance(capture, baseline, manifest, args[3], args[4]);
    const result = analyzePaired(capture, baseline, manifest);
    console.log(JSON.stringify(result, null, 2));
    if (result.status !== 'passed') process.exitCode = 1;
    return;
  }
  throw new Error('Usage: fidelity-paired-r7.mjs freeze-manifest OUT | record-B OUT MANIFEST B_BINARY B_ROOT | record-paired OUT MANIFEST B_BASELINE B_BINARY B_ROOT C_BINARY C_ROOT | compare-B B_CAPTURE MANIFEST | compare-paired PAIRED_CAPTURE B_CAPTURE MANIFEST C_REPO B_BASELINE');
}
if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try { await main(); } catch (error) { console.error(error.message); process.exitCode = 1; }
}
