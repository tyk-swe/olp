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

export const SCHEMA = 'openllmproxy.dev/fidelity-source-paired-v2';
export const SEED = 'oif-source-paired-v2-2026-09-23-sealed-7f093a602b';
export const BLOCKS = 32;
export const SAMPLES = 64;
export const UPPER_INDEX = 21; // d_(22), zero-based.
export const LOWER_INDEX = 10; // d_(11), zero-based.
const B_PRODUCT = '8e52f775df815c3a1a25d75064c3ffd540fb07f5';
const PREFIX = 'OLP_SOURCE_PAIRED_V2 ';
const workloads = ['native_unary', 'native_stream_256', 'native_slow_stream_64', 'native_asset_png', 'translated_unary', 'rejected_extension'];
const groups = workloads.flatMap((workload) => [1, 8].map((concurrency) => `${workload}/c${concurrency}`));
const metricKinds = ['ns/op', 'process-cpu-ns/op', 'B/op', 'allocs/op', 'sampled-heap-growth-B',
  'latency-p50-us', 'latency-p95-us', 'latency-p99-us'];
const streamMetrics = ['first-event-p50-us', 'first-event-p95-us', 'first-event-p99-us',
  'max-inter-event-gap-p50-us', 'max-inter-event-gap-p95-us', 'max-inter-event-gap-p99-us'];
const latencyMetrics = ['latency-p50-us', 'latency-p95-us', 'latency-p99-us'];
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
  pairedRunner: 'scripts/fidelity-paired-v2.mjs'
};

export function sourceOrder(group, variant, baselineOnly = false) {
  invariant(variant === 'ABBA' || variant === 'BAAB', 'unknown order');
  if (group.startsWith('rejected_extension/')) {
    return baselineOnly ? ['B/gateway', 'B/gateway'] : variant === 'ABBA'
      ? ['B/gateway', 'C/gateway', 'C/gateway', 'B/gateway']
      : ['C/gateway', 'B/gateway', 'B/gateway', 'C/gateway'];
  }
  // A four-arm palindrome. Each path separately receives 16 B-C-C-B and
  // 16 C-B-B-C blocks, while both arms occupy symmetric positions.
  const full = variant === 'ABBA'
    ? ['B/relay', 'C/relay', 'C/gateway', 'B/gateway', 'B/gateway', 'C/gateway', 'C/relay', 'B/relay']
    : ['C/gateway', 'B/gateway', 'B/relay', 'C/relay', 'C/relay', 'B/relay', 'B/gateway', 'C/gateway'];
  return baselineOnly ? full.filter((arm) => arm.startsWith('B/')) : full;
}

export function sourceSchedule(seed = SEED) {
  const blocks = groups.flatMap((group) => {
    const indices = Array.from({ length: BLOCKS }, (_, index) => index)
      .sort((a, b) => rank(seed, `variant:${group}:${a}`).localeCompare(rank(seed, `variant:${group}:${b}`)));
    const variant = new Map(indices.map((index, ordinal) => [index, ordinal < 16 ? 'ABBA' : 'BAAB']));
    return Array.from({ length: BLOCKS }, (_, index) => ({ group, index, variant: variant.get(index) }));
  });
  blocks.sort((a, b) => rank(seed, `block:${a.group}:${a.index}`).localeCompare(rank(seed, `block:${b.group}:${b.index}`)));
  return blocks;
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
  const schedule = sourceSchedule();
  return {
    schema: `${SCHEMA}-manifest`, method_version: 2, B_product_revision: B_PRODUCT,
    old_baseline_sha256: digest(full('baseline')), old_budgets_sha256: digest(full('budgets')),
    old_harness_sha256: digest(full('harness')), old_runner_sha256: digest(full('runner')),
    paired_harness_sha256: digest(full('pairedHarness')), paired_runner_sha256: digest(full('pairedRunner')),
    seed: SEED, blocks_per_group: BLOCKS, samples_per_subrun: SAMPLES,
    order: { ABBA: 'B-C-C-B per relay path; C-B-B-C per gateway path', BAAB: 'C-B-B-C per relay path; B-C-C-B per gateway path',
      rejected: 'B-C-C-B or C-B-B-C gateway', baseline: 'same schedule with C subruns omitted' },
    schedule_sha256: hash(JSON.stringify(schedule)),
    bound: { lower_order_statistic: 11, upper_order_statistic: 22, upper_coverage: 0.97494877,
      primary: 'sorted 32 paired block C-minus-B differences: d_(22) < old frozen margin',
      relay_control: 'd_(11) > -margin and d_(22) < margin',
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
      timing: '64 requests/subrun, two subruns/arm/block, 32 complete blocks/group; no omissions or selected retries',
      semantics: 'frozen v1 provider-bound request, complete response/SSE event oracle, exact asset, authentication and zero-dispatch rejection'
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
function command(program, args, cwd) {
  const result = spawnSync(program, args, { cwd, encoding: 'utf8', maxBuffer: 1 << 20 });
  invariant(result.status === 0, `${program} failed: ${result.stderr}`);
  return result.stdout.trim();
}
function sourceIdentity(root, manifest, B) {
  const revision = command('git', ['rev-parse', 'HEAD'], root);
  const status = command('git', ['status', '--porcelain', '--untracked-files=normal'], root);
  invariant(status === '', `${B ? 'B' : 'C'} source tree is dirty: ${status}`);
  invariant(digest(resolve(root, oldFiles.harness)) === manifest.old_harness_sha256, 'frozen v1 harness differs');
  invariant(digest(resolve(root, oldFiles.runner)) === manifest.old_runner_sha256, 'frozen v1 runner differs');
  invariant(digest(resolve(root, oldFiles.pairedHarness)) === manifest.paired_harness_sha256, 'v2 harness differs');
  invariant(digest(resolve(root, oldFiles.pairedRunner)) === manifest.paired_runner_sha256, 'v2 runner differs');
  if (B) {
    invariant(command('git', ['merge-base', manifest.B_product_revision, 'HEAD'], root) === manifest.B_product_revision, 'B does not descend from historical product');
    const changed = command('git', ['diff', '--name-only', manifest.B_product_revision, 'HEAD'], root).split('\n').filter(Boolean);
    invariant(changed.every((name) => name === oldFiles.pairedHarness || name === oldFiles.pairedRunner || name === oldFiles.baseline || name === oldFiles.budgets || name === 'scripts/fidelity-paired-v2.test.mjs' || name === 'docs/evidence/fidelity-performance/source-paired-v2/manifest.json' || name === 'docs/evidence/fidelity-performance/source-paired-v2/README.md'), `B product differs: ${changed}`);
  }
  return { revision, root, working_tree: status };
}

class GoArm {
  constructor(binary, label, environment) {
    this.binary = binary;
    this.label = label;
    this.pending = [];
    this.ready = new Promise((resolveReady, rejectReady) => { this.resolveReady = resolveReady; this.rejectReady = rejectReady; });
    this.readyTimer = setTimeout(() => { this.process.kill(); this.abort(new Error(`${label}: readiness timeout`)); }, 60_000);
    this.process = spawn(binary, ['-test.run=^TestFidelityPairedServer$', '-test.benchtime=64x', '-test.timeout=0'],
      { env: { ...process.env, ...environment, OLP_SOURCE_PAIRED_SERVER: '1', GOMAXPROCS: '4', GOGC: '100', GOMEMLIMIT: 'off', GODEBUG: '' }, stdio: ['pipe', 'pipe', 'pipe'] });
    this.stderr = '';
    this.process.stderr.on('data', (chunk) => { this.stderr += chunk.toString(); });
    let buffer = '';
    this.process.stdout.on('data', (chunk) => {
      buffer += chunk.toString();
      while (buffer.includes('\n')) {
        const index = buffer.indexOf('\n');
        const line = buffer.slice(0, index).trim();
        buffer = buffer.slice(index + 1);
        if (!line.startsWith(PREFIX)) continue;
        let reply;
        try { reply = JSON.parse(line.slice(PREFIX.length)); } catch (error) { this.abort(error); return; }
        if (reply.ready) { clearTimeout(this.readyTimer); this.resolveReady(); continue; }
        const pending = this.pending.shift();
        if (!pending) { this.abort(new Error(`${label}: unsolicited reply`)); return; }
        clearTimeout(pending.timer);
        if (reply.id !== pending.id || reply.error) pending.reject(new Error(`${label}: ${reply.error || 'reply ID mismatch'} ${this.stderr}`));
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

function percentile(values, p) {
  const sorted = [...values].sort((a, b) => a - b);
  return sorted[Math.ceil(p * sorted.length / 100) - 1];
}
export function validateSubrun(reply, name, manifest) {
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
  invariant(reply.runtime && ['num_gc_delta', 'pause_total_ns_delta', 'total_alloc_bytes_delta', 'heap_alloc_after_bytes', 'goroutines_after'].every((key) => Number.isSafeInteger(reply.runtime[key]) && reply.runtime[key] >= 0), `${name}: runtime/GC diagnostics missing`);
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
  invariant(typeof capture.B?.revision === 'string' && typeof capture.B?.binary_sha256 === 'string', 'B binary identity missing');
  if (!baselineOnly) {
    invariant(capture.C && capture.C.revision !== capture.B.revision && ['native', 'translated', 'rejected'].every((name) => Object.keys(capture.C_route_contract?.[name] || {}).length > 0), 'C revision or strict route contract missing');
    invariant(typeof capture.B_only_sha256 === 'string' && capture.B_only_sha256.length === 64, 'committed B-only artifact identity missing');
  }
  for (const diagnostics of [capture.diagnostics_before, capture.diagnostics_after]) invariant(typeof diagnostics?.loadavg === 'string' && typeof diagnostics?.cpu_pressure === 'string', 'capture host diagnostics missing');
  const schedule = sourceSchedule(manifest.seed);
  invariant(capture.blocks.length === schedule.length && hash(JSON.stringify(schedule)) === manifest.schedule_sha256, 'block schedule or count changed');
  for (let i = 0; i < schedule.length; i++) {
    const actual = capture.blocks[i], expected = schedule[i];
    invariant(actual.group === expected.group && actual.index === expected.index && actual.variant === expected.variant, `block ${i}: schedule changed`);
    for (const diagnostics of [actual.diagnostics_before, actual.diagnostics_after]) invariant(typeof diagnostics?.loadavg === 'string' && typeof diagnostics?.cpu_pressure === 'string', `block ${i}: host diagnostics missing`);
    const arms = sourceOrder(expected.group, expected.variant, baselineOnly);
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
  const failures = [];
  for (const [name, metrics] of Object.entries(manifest.comparisons)) {
    const runs = perName(capture, name, 'B');
    invariant(runs.length === 64, `${name}: B-only sample floor changed`);
    envelope[name] = {};
    for (const [metric, old] of Object.entries(metrics)) {
      const observed = median(runs.map((run) => run.metrics[metric]));
      const passed = observed <= old.old_limit;
      envelope[name][metric] = { B_median: observed, old_limit: old.old_limit, passed };
      if (!passed) failures.push(`${name} ${metric}: B-only median ${observed} > frozen ${old.old_limit}`);
    }
  }
  return { passed: failures.length === 0, failures, envelope, count: { blocks: capture.blocks.length, B_subruns: capture.blocks.reduce((n, block) => n + block.subruns.length, 0), B_requests: 22 * 64 * 64 } };
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
  invariant(capture.B?.revision === baseline.B?.revision && capture.B?.binary_sha256 === baseline.B?.binary_sha256, 'historical B binary/revision changed');
  invariant(capture.B_only_sha256 === hash(JSON.stringify(baseline, null, 2) + '\n'), 'B-only artifact reference changed');
  const BOnly = analyzeBaseline(baseline, manifest);
  const failures = [...BOnly.failures.map((failure) => `B-only envelope: ${failure}`)];
  const controls = [];
  const primaries = [];
  const envelope = {};
  const results = {};
  for (const group of groups) {
    const blocks = capture.blocks.filter((block) => block.group === group);
    invariant(blocks.length === BLOCKS, `${group}: missing paired blocks`);
    for (const path of groupPaths(group)) {
      const name = pathName(group, path), runs = perName(capture, name, 'B');
      invariant(runs.length === 64 && perName(capture, name, 'C').length === 64, `${name}: missing B/C subruns`);
      results[name] = {};
      envelope[name] = {};
      for (const [metric, old] of Object.entries(manifest.comparisons[name])) {
        const BMedian = median(runs.map((run) => run.metrics[metric]));
        envelope[name][metric] = { B_median: BMedian, old_limit: old.old_limit, passed: BMedian <= old.old_limit };
        if (BMedian > old.old_limit) failures.push(`${name} ${metric}: contemporaneous B median ${BMedian} > frozen ${old.old_limit}`);
        const result = pairedResult(pairedValues(blocks, path, metric), old, path === 'relay');
        result.B_median = BMedian;
        result.C_median = median(perName(capture, name, 'C').map((run) => run.metrics[metric]));
        if (metric === 'ns/op') {
          result.B_requests_per_second = 1e9 / result.B_median;
          result.C_requests_per_second = 1e9 / result.C_median;
        }
        results[name][metric] = result;
        if (!result.passed) primaries.push(`${name} ${metric}: paired d_(22) ${result.upper} >= frozen margin ${old.margin}`);
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
      if (!result.passed) primaries.push(`${group} added ${metric}: paired d_(22) ${result.upper} >= frozen gateway margin ${old.margin}`);
    }
  }
  const controlFailed = failures.length > 0 || controls.length > 0;
  return { status: controlFailed ? 'inconclusive' : primaries.length ? 'failed' : 'passed',
    failures, controls, primaries, B_only: { passed: BOnly.passed, failures: BOnly.failures },
    envelope, results, added_latency: addedLatency,
    count: { blocks: capture.blocks.length, source_primary_metrics: 224, added_latency_metrics: 30,
      B_successes: 81920, C_successes: 81920, B_dispatches: 81920, C_dispatches: 81920,
      B_rejections: 8192, C_rejections: 8192 } };
}

async function record(mode, output, manifestPath, baselinePath, Bbinary, Broot, Cbinary, Croot) {
  invariant(!existsSync(output) && !existsSync(`${output}.journal.jsonl`), 'output or journal already exists');
  const manifest = json(manifestPath);
  validateManifest(manifest, Broot);
  const frozenBaseline = mode === 'paired' ? json(baselinePath) : null;
  if (frozenBaseline) {
    const baselineAnalysis = analyzeBaseline(frozenBaseline, manifest);
    invariant(baselineAnalysis.passed, 'frozen B-only baseline envelope failed; C capture is forbidden');
    const trackedPath = relative(Croot, resolve(baselinePath));
    invariant(!isAbsolute(trackedPath) && !trackedPath.startsWith('..'), 'B-only artifact must be in the locked C checkout');
    command('git', ['ls-files', '--error-unmatch', '--', trackedPath], Croot);
    invariant(command('git', ['status', '--porcelain', '--', trackedPath], Croot) === '', 'B-only artifact must be committed before C capture');
  }
  const B = { ...sourceIdentity(Broot, manifest, true), binary_sha256: digest(Bbinary) };
  const C = mode === 'paired' ? { ...sourceIdentity(Croot, manifest, false), binary_sha256: digest(Cbinary) } : null;
  invariant(mode === 'B-only' || (C && C.revision !== B.revision), 'paired study requires a distinct locked C revision');
  const routeContract = mode === 'paired' ? JSON.parse(process.env.OLP_FIDELITY_BENCH_ROUTE_CONTRACT || 'null') : null;
  const providerContract = mode === 'paired' ? JSON.parse(process.env.OLP_FIDELITY_BENCH_PROVIDER_CONTRACT || 'null') : null;
  if (mode === 'paired') invariant(routeContract && ['native', 'translated', 'rejected'].every((name) => Object.keys(routeContract[name] || {}).length > 0), 'C requires explicit native, translated and rejected route contracts');
  const currentHardware = hardware();
  invariant(hardwareMatches(currentHardware, manifest.old_hardware), 'physical hardware differs from v1');
  const toolchain = command('go', ['version'], Broot);
  const goBuildEnvironment = command('go', ['env', 'GOFLAGS', 'GOAMD64', 'GOARCH', 'GOOS'], Broot);
  invariant(toolchain === manifest.old_toolchain && goBuildEnvironment === manifest.old_go_build_environment, 'toolchain/build environment differs from v1');
  const artifact = { schema: SCHEMA, mode, status: 'in_progress', started_at: new Date().toISOString(),
    manifest_sha256: hash(JSON.stringify(manifest)), schedule_sha256: manifest.schedule_sha256,
    B_only_sha256: frozenBaseline ? digest(baselinePath) : null,
    B, C, C_route_contract: routeContract, C_provider_contract: providerContract,
    hardware: currentHardware, toolchain, go_build_environment: goBuildEnvironment,
    runtime_environment: { GOMAXPROCS: '4', GOGC: '100', GOMEMLIMIT: 'off', GODEBUG: '' },
    conditions: manifest.conditions, diagnostics_before: diagnostics(), blocks: [] };
  writeFileSync(`${output}.journal.jsonl`, JSON.stringify({ header: { ...artifact, blocks: undefined } }) + '\n', { flag: 'wx' });
  const Bprocess = new GoArm(Bbinary, 'B', { OLP_FIDELITY_BENCH_ROUTE_CONTRACT: '', OLP_FIDELITY_BENCH_PROVIDER_CONTRACT: '' });
  const Cprocess = C ? new GoArm(Cbinary, 'C', { OLP_FIDELITY_BENCH_ROUTE_CONTRACT: JSON.stringify(routeContract), OLP_FIDELITY_BENCH_PROVIDER_CONTRACT: providerContract ? JSON.stringify(providerContract) : '' }) : null;
  try {
    await Bprocess.ready;
    if (Cprocess) await Cprocess.ready;
    for (const block of sourceSchedule(manifest.seed)) {
      const entry = { ...block, diagnostics_before: diagnostics(), subruns: [] };
      for (const arm of sourceOrder(block.group, block.variant, !Cprocess)) {
        const [version, path] = arm.split('/');
        const name = pathName(block.group, path);
        const id = `${block.group}:${block.index}:${entry.subruns.length}`;
        const reply = await (version === 'B' ? Bprocess : Cprocess).request(id, name);
        validateSubrun(reply, name, manifest);
        entry.subruns.push({ arm, reply });
      }
      entry.diagnostics_after = diagnostics();
      artifact.blocks.push(entry);
      appendFileSync(`${output}.journal.jsonl`, JSON.stringify({ block: entry }) + '\n');
    }
    artifact.status = 'complete';
    artifact.completed_at = new Date().toISOString();
    artifact.diagnostics_after = diagnostics();
    const analysis = mode === 'B-only' ? analyzeBaseline(artifact, manifest) : analyzePaired(artifact, frozenBaseline, manifest);
    artifact.analysis = analysis;
    writeFileSync(output, JSON.stringify(artifact, null, 2) + '\n', { flag: 'wx' });
    console.log(`${mode} capture ${analysis.status || (analysis.passed ? 'passed' : 'inconclusive')}: ${output}`);
    if (analysis.status !== 'passed' && !analysis.passed) process.exitCode = 1;
  } catch (error) {
    artifact.status = 'invalid';
    artifact.error = error.message;
    artifact.completed_at = new Date().toISOString();
    artifact.diagnostics_after = diagnostics();
    const failurePath = `${output}.failed-${Date.now()}.json`;
    writeFileSync(failurePath, JSON.stringify(artifact, null, 2) + '\n', { flag: 'wx' });
    throw new Error(`${error.message}; retained ${failurePath}`);
  } finally {
    Bprocess.kill();
    Cprocess?.kill();
  }
}

function main() {
  const [action, ...args] = process.argv.slice(2);
  if (action === 'freeze-manifest' && args.length === 1) {
    writeFileSync(args[0], JSON.stringify(makeManifest(process.cwd()), null, 2) + '\n', { flag: 'wx' });
    return console.log(`Frozen source v2 manifest: ${args[0]}`);
  }
  if (action === 'record-B' && args.length === 4) return record('B-only', args[0], args[1], null, args[2], args[3]);
  if (action === 'record-paired' && args.length === 7) return record('paired', ...args);
  if (action === 'compare-B' && args.length === 2) {
    const manifest = json(args[1]); validateManifest(manifest, process.cwd());
    const result = analyzeBaseline(json(args[0]), manifest);
    console.log(JSON.stringify(result, null, 2));
    if (!result.passed) process.exitCode = 1;
    return;
  }
  if (action === 'compare-paired' && args.length === 3) {
    const manifest = json(args[2]); validateManifest(manifest, process.cwd());
    const result = analyzePaired(json(args[0]), json(args[1]), manifest);
    console.log(JSON.stringify(result, null, 2));
    if (result.status !== 'passed') process.exitCode = 1;
    return;
  }
  throw new Error('Usage: fidelity-paired-v2.mjs freeze-manifest OUT | record-B OUT MANIFEST B_BINARY B_ROOT | record-paired OUT MANIFEST B_BASELINE B_BINARY B_ROOT C_BINARY C_ROOT | compare-B B_CAPTURE MANIFEST | compare-paired PAIRED_CAPTURE B_CAPTURE MANIFEST');
}
if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try { await main(); } catch (error) { console.error(error.message); process.exitCode = 1; }
}
