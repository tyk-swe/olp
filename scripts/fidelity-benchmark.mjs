#!/usr/bin/env node
import { spawnSync } from 'node:child_process';
import { readFileSync, writeFileSync, existsSync } from 'node:fs';
import { cpus, totalmem, release, arch, platform } from 'node:os';
import { fileURLToPath } from 'node:url';
import { resolve } from 'node:path';
import { createHash } from 'node:crypto';

const schema = 'openllmproxy.dev/fidelity-performance/v1';
const workloadNames = ['native_unary', 'native_stream_256', 'native_slow_stream_64', 'native_asset_png', 'translated_unary', 'rejected_extension'];
const expectedNames = workloadNames.flatMap((name) => [1, 8].flatMap((concurrency) =>
  (name === 'rejected_extension' ? ['gateway'] : ['relay', 'gateway']).map((mode) => `${name}/c${concurrency}/${mode}`)));

export function parseBenchmarks(output) {
  const runs = [];
  for (const line of output.split('\n')) {
    if (!/^BenchmarkFidelity\//.test(line)) continue;
    const columns = line.trim().split(/\s+/);
    if (columns.length < 4) continue;
    const name = columns[0].replace(/^BenchmarkFidelity\//, '').replace(/-\d+$/, '');
    const iterations = Number(columns[1]);
    if (!expectedNames.includes(name) || !Number.isSafeInteger(iterations) || iterations <= 0 || columns.length % 2 !== 0) throw new Error(`Malformed benchmark: ${line}`);
    const metrics = {};
    for (let i = 2; i < columns.length; i += 2) {
      const value = Number(columns[i]);
      if (!Number.isFinite(value) || value < 0 || Object.hasOwn(metrics, columns[i + 1])) throw new Error(`Malformed metric: ${line}`);
      metrics[columns[i + 1]] = value;
    }
    const rejected = name.startsWith('rejected_extension/');
    if (metrics.dispatches !== (rejected ? 0 : iterations) || metrics.succeeded !== (rejected ? 0 : iterations) || metrics.rejected !== (rejected ? iterations : 0)) throw new Error(`Workload outcome or dispatch counts changed: ${name}`);
    for (const metric of ['ns/op', 'B/op', 'allocs/op', 'latency-p50-us', 'latency-p95-us', 'latency-p99-us', 'process-cpu-ns/op', 'sampled-heap-growth-B', 'request-bytes', 'content-events/op']) {
      if (!Object.hasOwn(metrics, metric)) throw new Error(`Missing ${metric}: ${name}`);
    }
    const events = name.startsWith('native_stream_256/') ? 256 : name.startsWith('native_slow_stream_64/') ? 64 : 0;
    if (metrics['content-events/op'] !== events) throw new Error(`Stream coverage shrank: ${name}`);
    if (events) {
      for (const category of ['first-event', 'max-inter-event-gap']) {
        for (const percentile of [50, 95, 99]) {
          if (!Object.hasOwn(metrics, `${category}-p${percentile}-us`)) throw new Error(`Missing stream metric: ${name}`);
        }
      }
    }
    runs.push({ name, iterations, metrics });
  }
  return runs;
}

function median(values) {
  const sorted = values.toSorted((a, b) => a - b);
  const middle = Math.floor(sorted.length / 2);
  return sorted.length % 2 ? sorted[middle] : (sorted[middle - 1] + sorted[middle]) / 2;
}

export function summarize(runs, repeats) {
  const summaries = {};
  // Revalidate imported artifacts as well as fresh Go output.
  for (const run of runs) {
    const line = `BenchmarkFidelity/${run.name}-4 ${run.iterations} ${Object.entries(run.metrics).map(([metric, value]) => `${value} ${metric}`).join(' ')}`;
    if (parseBenchmarks(line).length !== 1) throw new Error('Invalid imported benchmark');
  }
  for (const name of expectedNames) {
    const matching = runs.filter((run) => run.name === name);
    if (matching.length !== repeats) throw new Error(`${name}: expected ${repeats} repetitions, found ${matching.length}`);
    summaries[name] = { iterations: matching.map((run) => run.iterations), metrics: {} };
    for (const metric of Object.keys(matching[0].metrics)) {
      const values = matching.map((run) => run.metrics[metric]);
      summaries[name].metrics[metric] = { median: median(values), maximum: Math.max(...values), minimum: Math.min(...values) };
    }
    summaries[name].requests_per_second = 1e9 / summaries[name].metrics['ns/op'].median;
  }
  return summaries;
}

function command(program, args) {
  const result = spawnSync(program, args, { encoding: 'utf8', maxBuffer: 16 << 20 });
  if (result.status !== 0) throw new Error(`${program} failed: ${result.stderr}`);
  return result.stdout.trim();
}

function optionalFile(path) {
  return existsSync(path) ? readFileSync(path, 'utf8').trim() : null;
}

function digest(path) { return createHash('sha256').update(readFileSync(path)).digest('hex'); }

function record(path) {
  if (existsSync(path)) throw new Error(`Refusing to overwrite evidence: ${path}`);
  const args = ['test', '-mod=readonly', '-run', '^$', '-bench', '^BenchmarkFidelity$', '-benchmem', '-benchtime=2s', '-count=3', '-cpu=4', '-timeout=15m', './internal/gateway'];
  const startedAt = new Date().toISOString();
  const loadBefore = optionalFile('/proc/loadavg');
  const routeContract = process.env.OLP_FIDELITY_BENCH_ROUTE_CONTRACT ? JSON.parse(process.env.OLP_FIDELITY_BENCH_ROUTE_CONTRACT) : null;
  const providerContract = process.env.OLP_FIDELITY_BENCH_PROVIDER_CONTRACT ? JSON.parse(process.env.OLP_FIDELITY_BENCH_PROVIDER_CONTRACT) : null;
  if (providerContract && !routeContract) throw new Error('A provider contract requires explicit route contracts');
  const run = spawnSync('go', args, { encoding: 'utf8', maxBuffer: 16 << 20, env: { ...process.env, GOMAXPROCS: '4' } });
  process.stdout.write(run.stdout ?? '');
  process.stderr.write(run.stderr ?? '');
  if (run.status !== 0) throw new Error(`Benchmark failed (${run.status}); no passing artifact written`);
  const runs = parseBenchmarks(run.stdout);
  const summary = summarize(runs, 3);
  const relayDifferences = {};
  for (const name of expectedNames.filter((name) => name.endsWith('/gateway') && !name.startsWith('rejected_extension/'))) {
    const relay = summary[name.replace('/gateway', '/relay')];
    relayDifferences[name] = Object.fromEntries(['latency-p50-us', 'latency-p95-us', 'latency-p99-us', 'B/op', 'allocs/op', 'process-cpu-ns/op'].map((metric) => [metric, summary[name].metrics[metric].median - relay.metrics[metric].median]));
  }
  const artifact = {
    schema, contract_mode: routeContract ? 'explicit' : 'legacy', route_contract: routeContract, provider_contract: providerContract, started_at: startedAt, completed_at: new Date().toISOString(), source_revision: command('git', ['rev-parse', 'HEAD']), working_tree: command('git', ['status', '--short']),
    harness_sha256: digest('internal/gateway/fidelity_benchmark_test.go'), runner_sha256: digest('scripts/fidelity-benchmark.mjs'), command: ['go', ...args],
    hardware: { os: platform(), architecture: arch(), kernel: release(), cpu: cpus()[0]?.model, logical_cpus: cpus().length, total_memory_bytes: totalmem(), cpu_quota: optionalFile('/sys/fs/cgroup/cpu.max'), memory_limit: optionalFile('/sys/fs/cgroup/memory.max'), load_before: loadBefore, load_after: optionalFile('/proc/loadavg') },
    toolchain: command('go', ['version']), gomaxprocs: 4, concurrency: [1, 8], repetitions: 3, target_time_per_repetition: '2s',
    conditions: { network: 'IPv4 loopback; two HTTP/1.1 hops; keep-alive; warm connection; no injected provider delay', tls: 'none; explicit loopback-only egress exception', authority: 'in-memory published runtime and credential slots; authenticated fixed-route native relay; no PostgreSQL/Valkey', resources: 'Whole-process client + local provider + gateway/relay + independent fixture validation + instrumentation; sampled heap growth, not absolute RSS or exact peak', stream: '256 x 128-byte text events; slow workload 64 x 16384-byte events and 100us requested sleep after each read (OS timer may be coarser); exact content count and terminal markers validated', asset: '512x512 deterministic RGB PNG, approximately 1MiB JSON/base64 request; original provider-bound document checked', ordering: 'Workloads sequential in fixed declaration order; repetitions serial; randomized model-quality trials are separate', scope: 'Legacy runtime performance baseline only; no intelligence claim, external network/TLS, durable state barrier or realtime jitter measurement' },
    runs, summary, median_gateway_minus_relay: relayDifferences,
    unmeasured: ['durable continuation barrier', 'duplex realtime jitter', 'WAN/TLS', 'isolated gateway RSS', 'live-model quality'],
    raw_output: run.stdout
  };
  writeFileSync(path, JSON.stringify(artifact, null, 2) + '\n', { flag: 'wx' });
  console.log(`Recorded ${runs.length} measured repetitions in ${path}`);
}

export function freezeBudgets(artifact) {
  if (artifact.schema !== schema || artifact.repetitions !== 3) throw new Error('Expected a complete v1 baseline with three repetitions');
  const summary = summarize(artifact.runs, artifact.repetitions);
  const workloads = {};
  for (const name of expectedNames) {
    const metrics = summary[name].metrics;
    const maxima = {};
    for (const [metric, values] of Object.entries(metrics)) {
      if (metric.endsWith('-us')) maxima[metric] = Math.ceil(values.maximum * 1.5 + 1000);
      if (metric === 'ns/op' || metric === 'process-cpu-ns/op') maxima[metric] = Math.ceil(values.maximum * 1.5 + 1e6);
      if (metric === 'B/op' || metric === 'sampled-heap-growth-B') maxima[metric] = Math.ceil(values.maximum * 1.5 + (metric === 'B/op' ? 16384 : 8 << 20));
      if (metric === 'allocs/op') maxima[metric] = Math.ceil(values.maximum * 1.25 + 64);
    }
    workloads[name] = { maxima, exact: { 'request-bytes': metrics['request-bytes'].median, 'content-events/op': metrics['content-events/op'].median }, minimum_samples: Math.min(20, ...summary[name].iterations) };
  }
  return { schema: 'openllmproxy.dev/fidelity-performance-budget/v1', declared_at: new Date().toISOString(), baseline_revision: artifact.source_revision, baseline_harness_sha256: artifact.harness_sha256, baseline_runner_sha256: artifact.runner_sha256, hardware: structuredClone(artifact.hardware), gomaxprocs: 4, repetitions: 3, method: 'Before replacement: per-workload maximum of 3 baseline runs ×1.5 plus 1ms timing/CPU allowance, 16KiB allocation allowance or 8MiB sampled heap allowance. Allocation counts ×1.25 +64. These tolerances are change-regression budgets, not service SLOs. Never auto-rebase budgets on candidate results.', workloads };
}

export function compareBudgets(candidate, budgets, mode = 'legacy') {
  if (candidate.schema !== schema || budgets.schema !== 'openllmproxy.dev/fidelity-performance-budget/v1') throw new Error('Unknown evidence or budget schema');
  if (candidate.harness_sha256 !== budgets.baseline_harness_sha256) throw new Error('Benchmark harness/oracle differs; use a reviewed versioned extension, never weaken frozen workloads');
  if (candidate.contract_mode !== mode) throw new Error(`Expected ${mode} contract evidence, got ${candidate.contract_mode}`);
  if (mode === 'explicit' && (!candidate.route_contract || ['native', 'translated', 'rejected'].some((name) => !Object.keys(candidate.route_contract[name] ?? {}).length))) throw new Error('All explicit route contracts must be recorded');
  if (candidate.gomaxprocs !== budgets.gomaxprocs || candidate.repetitions !== budgets.repetitions) throw new Error('Measurement configuration differs from the frozen budget');
  for (const key of ['cpu', 'logical_cpus', 'architecture', 'cpu_quota']) {
    if (candidate.hardware[key] !== budgets.hardware[key]) throw new Error(`Hardware differs (${key}); results require separate qualification`);
  }
  const summary = summarize(candidate.runs, candidate.repetitions);
  if (Object.keys(budgets.workloads).length !== expectedNames.length || expectedNames.some((name) => !Object.hasOwn(budgets.workloads, name))) throw new Error('Frozen workload inventory changed');
  const failures = [];
  for (const name of expectedNames) {
    const budget = budgets.workloads[name];
    for (const [metric, maximum] of Object.entries(budget.maxima)) {
      if (!(summary[name].metrics[metric]?.median <= maximum)) failures.push(`${name}: ${metric} median ${summary[name].metrics[metric]?.median} > ${maximum}`);
    }
    for (const [metric, exact] of Object.entries(budget.exact)) {
      if (summary[name].metrics[metric]?.minimum !== exact || summary[name].metrics[metric]?.maximum !== exact) failures.push(`${name}: workload ${metric} changed`);
    }
    if (Math.min(...summary[name].iterations) < budget.minimum_samples) failures.push(`${name}: fewer samples than baseline`);
  }
  return failures;
}

function main() {
  const [operation, first, second, third] = process.argv.slice(2);
  if (operation === 'record' && first && !second) return record(first);
  if (operation === 'freeze' && first && second && !third) {
    const budgets = freezeBudgets(JSON.parse(readFileSync(first, 'utf8')));
    budgets.baseline_artifact_sha256 = digest(first);
    writeFileSync(second, JSON.stringify(budgets, null, 2) + '\n', { flag: 'wx' });
    return console.log(`Frozen replacement budgets in ${second}`);
  }
  if ((operation === 'compare' || operation === 'compare-explicit') && first && second && !third) {
    const failures = compareBudgets(JSON.parse(readFileSync(first, 'utf8')), JSON.parse(readFileSync(second, 'utf8')), operation === 'compare-explicit' ? 'explicit' : 'legacy');
    if (failures.length) throw new Error(failures.join('\n'));
    return console.log('All 22 workloads retain their outcomes and meet the frozen performance budgets.');
  }
  throw new Error('Usage: node scripts/fidelity-benchmark.mjs record OUTPUT.json | freeze BASELINE.json BUDGETS.json | compare CANDIDATE.json BUDGETS.json | compare-explicit CANDIDATE.json BUDGETS.json');
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try { main(); } catch (error) { console.error(error.message); process.exitCode = 1; }
}
