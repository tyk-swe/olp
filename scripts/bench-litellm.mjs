#!/usr/bin/env node
import { execFile, spawn } from 'node:child_process';
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { cpus, loadavg, totalmem, release } from 'node:os';
import { join } from 'node:path';
import { parseArgs, promisify } from 'node:util';
import { fileURLToPath } from 'node:url';

// Runs one benchmark scenario against LiteLLM, the way the scenario suite
// (tests/bench) runs it against OLP, and writes .local/bench/litellm/<scenario>.json
// in the shape of OLP's result so that bench-compare.mjs can read both. It is
// called by scripts/bench-compare.sh once LiteLLM is up (deploy/compose.bench.yaml),
// after the same scenario has run against OLP.
//
// The load is the one OLP's result records under `workload`: its rate, prompts,
// mock behavior and timeouts, so the two gateways cannot be given different
// loads by a script that drifted. The sequence is the suite's: the mock
// answers as the scenario says, a virtual key is made and tried, a
// direct-to-mock baseline is run with the cmd/loadgen binary, and then the same
// load through LiteLLM, while the CPU time and resident memory of its
// containers are read from their cgroups. Added latency is the difference of
// the two runs' percentiles.

const run = promisify(execFile);

// A budget no scenario can exhaust, as in tests/bench/scenarios_test.go.
const budget = 1_000_000;
// The pause between the baseline and the run, as in the suite.
const settleSeconds = 3;
// The model groups of deploy/litellm/*.yaml.
const groups = { route: 'bench-route', priced: 'bench-priced', failover: 'bench-failover' };

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// The CPUs in a list such as 0-3,6.
export function cpuSet(list) {
  const seen = new Set();
  for (const part of String(list).split(',')) {
    const match = /^(\d+)(?:-(\d+))?$/.exec(part.trim());
    const lo = Number(match?.[1]);
    const hi = Number(match?.[2] ?? match?.[1]);
    if (!match || hi < lo) throw new Error(`${JSON.stringify(list)} is not a CPU list such as 0-3,6`);
    for (let cpu = lo; cpu <= hi; cpu++) seen.add(cpu);
  }
  return seen;
}

// How many CPUs are in a list such as 0-3,6.
export function parseCpuList(list) {
  return cpuSet(list).size;
}

// The pairs of pinned processes that share a CPU, as sentences naming the CPUs,
// in the words the scenario suite uses. A reference run gives the gateway, the
// mock upstream and the load generator CPUs of their own, since one that
// competes with the gateway for a core bends what is measured. A process left
// unpinned shares nothing it can be told of.
export function sharedCpus({ gateway, mock, loadgen }) {
  const pins = [
    ['the gateway', gateway],
    ['the mock upstream', mock],
    ['the load generator', loadgen]
  ];
  const shared = [];
  pins.forEach(([nameA, listA], index) => {
    for (const [nameB, listB] of pins.slice(index + 1)) {
      if (!listA || !listB) continue;
      const other = cpuSet(listB);
      const both = [...cpuSet(listA)].filter((cpu) => other.has(cpu)).sort((a, b) => a - b);
      if (both.length) shared.push(`${nameA} and ${nameB} both run on CPU ${both.join(',')}`);
    }
  });
  return shared;
}

// The conditions the roadmap's targets are stated for: full scale, each process
// pinned to CPUs of its own, and load runs that delivered their schedule, with
// nothing else wrong with the run. `all` is true only when every one holds.
export function referenceConditions({ scale, gatewayCpus, mockCpus, loadgenCpus, baseline, gateway, problems = [] }) {
  const reference = {
    full_scale: scale === 1,
    gateway_pinned: Boolean(gatewayCpus),
    mock_pinned: Boolean(mockCpus),
    loadgen_pinned: Boolean(loadgenCpus),
    pins_disjoint: sharedCpus({ gateway: gatewayCpus, mock: mockCpus, loadgen: loadgenCpus }).length === 0,
    valid_runs: baseline.valid === true && gateway.valid === true && problems.length === 0
  };
  reference.all = Object.values(reference).every(Boolean);
  return reference;
}

// The LiteLLM model group a scenario calls.
export function modelGroup(workload) {
  if (workload.failover) return groups.failover;
  if (workload.cost_budget) return groups.priced;
  return groups.route;
}

// The scenario's virtual key: restricted to its model group, and with the
// budget OLP's key has when the scenario has one.
export function keyRequest(workload, scenario) {
  const body = {
    models: [modelGroup(workload)],
    key_alias: `bench-${scenario.toLowerCase()}-${Date.now()}`
  };
  if (workload.cost_budget) {
    body.max_budget = budget;
    body.budget_duration = '30d';
  }
  return body;
}

// The mock upstream's configuration, as tests/bench plan.spec: its behavior
// for every model, and with failing set the first model of a failover route
// answering 503.
export function mockSpec(workload, failing) {
  const models = Object.fromEntries(workload.upstream_models.map((m) => [m, {}]));
  if (failing) models[workload.upstream_models[0]] = { status: 503 };
  return { default: workload.mock_behavior, models };
}

// The cmd/loadgen arguments for a run of the workload against a base URL.
// MaxInFlight is as tests/bench plan.loadConfig sizes it: room for every
// request the schedule can have open and as much again waiting.
export function loadgenArgs(workload, { url, model, name, json, lateAfter = '5ms' }) {
  if (workload.slow_read_bytes_per_second || workload.drain_seconds)
    throw new Error('a held-open scenario (S6) has no LiteLLM counterpart');
  const seconds = (v) => `${v}s`;
  return [
    '-url', url,
    '-dialect', workload.dialect,
    '-model', model,
    '-rate', String(workload.rate_rps),
    '-duration', seconds(workload.duration_seconds),
    '-warmup', seconds(workload.warmup_seconds),
    '-stream-share', String(workload.stream_share),
    '-prompt-tokens', workload.prompt_tokens.join(','),
    '-max-tokens', String(workload.max_tokens),
    '-max-in-flight', String(workload.expected_concurrency * 2 + 1024),
    '-timeout', seconds(workload.timeout_seconds),
    '-late-after', lateAfter,
    '-name', name,
    '-json', json
  ];
}

// Resident memory of a cgroup from its memory.stat: anonymous memory plus
// mapped file pages, which is what a process's resident set holds, without the
// page cache a container's files leave behind.
export function residentBytes(memoryStat) {
  let anon = 0;
  let mapped = 0;
  for (const line of memoryStat.split('\n')) {
    const [key, value] = line.split(' ');
    if (key === 'anon') anon = Number(value);
    if (key === 'file_mapped') mapped = Number(value);
  }
  return anon + mapped;
}

// CPU seconds a cgroup has used, from its cpu.stat.
export function cpuSeconds(cpuStat) {
  const match = /^usage_usec (\d+)$/m.exec(cpuStat);
  if (!match) throw new Error('cpu.stat has no usage_usec');
  return Number(match[1]) / 1e6;
}

const delta = (base, run) =>
  base.count > 0 && run.count > 0
    ? { p50: run.p50_ms - base.p50_ms, p95: run.p95_ms - base.p95_ms, p99: run.p99_ms - base.p99_ms }
    : null;

// The gateway's added latency: its run less the baseline's, as loadgen.Compare
// and the suite's computeAdded take it, for all successful requests and for
// each mode, null for a mode the scenario does not exercise.
export function addedLatency(baseline, gateway) {
  const b = baseline.latency;
  const g = gateway.latency;
  return {
    all: delta(b.all, g.all),
    unary: delta(b.unary, g.unary),
    stream: delta(b.stream, g.stream)
  };
}

// The added time to first token of streams, or null with the reason.
export function ttftOverhead(baseline, gateway) {
  const d = delta(baseline.latency.ttft, gateway.latency.ttft);
  return d
    ? { value: d }
    : { value: null, reason: 'the scenario has no streaming requests, so there is no first token to time' };
}

// The requests the gateway answered successfully over the whole run, warmup
// included, which is the span its CPU time covers: one it failed or shed cost it
// little and says nothing of what a CPU carries, so a gateway that fails fast
// would look the more efficient for counting them.
export function handledRequests(report) {
  return report.requests.succeeded + report.requests.warmup_sent - report.requests.warmup_failed;
}

// Throughput, as the suite's computeThroughput: successful requests that
// finished per second of the measured period over the vCPUs the gateway may
// use, and its CPU time per request answered (warmup included) net of what it
// spends idle.
export function throughput({ report, vcpus, cpuSeconds: used, idleCores, wallSeconds }) {
  const t = {
    target_rps: report.rates.target_rps,
    offered_rps: report.rates.offered_rps,
    sustained_rps: report.rates.throughput_rps,
    vcpus,
    rps_per_vcpu: vcpus > 0 ? report.rates.throughput_rps / vcpus : 0,
    gateway_cpu_ms_per_request: 0,
    gateway_cpu_ms_per_request_gross: 0,
    requests_per_cpu_second: 0
  };
  const handled = handledRequests(report);
  if (handled > 0 && used > 0) {
    t.gateway_cpu_ms_per_request_gross = (used * 1000) / handled;
    const net = used - idleCores * wallSeconds;
    if (net > 0) {
      t.gateway_cpu_ms_per_request = (net * 1000) / handled;
      t.requests_per_cpu_second = handled / net;
    }
  }
  return t;
}

// The share of a run's requests that may fail before its latencies stop being
// fit for judging, as the suite's maxFailureRate: they are the latencies of the
// requests that succeeded, so a gateway that fails requests fast would look
// quicker than one that answers them.
export const maxFailureRate = 0.001;

// How far below zero an added latency may fall and still be taken for jitter: a
// millisecond or a tenth of the baseline's figure, whichever is larger, as the
// suite's negativeAllowance and bench-compare's baseline allowance.
export const negativeAllowance = (baseline) => Math.max(1, 0.1 * (baseline ?? 0));

// The population whose latencies stand for a workload: the mode it exercises,
// or every successful request when it mixes them.
export const populationOf = (workload) =>
  workload.stream_share === 0 ? 'unary' : workload.stream_share === 1 ? 'stream' : 'all';

// The premise of S4, which is that the first target of the failover route
// failed. LiteLLM keeps no per-attempt record this harness reads, so the
// evidence is the mock's own count of the errors it injected: none means the
// rule was not armed, or LiteLLM never tried the target first, and the run
// measured a healthy route that would be compared as a failover. It returns the
// problem, or null when the premise holds or the workload is not a failover.
export function failoverProblem(workload, stats) {
  if (!workload.failover || stats.injected_errors > 0) return null;
  return 'the mock upstream injected no error, so the first target never failed and the run measured no failover';
}

// What makes the run unfit for judging a target, and what only bends it, as the
// suite's judgeValidity.
export function validity({ workload, baseline, gateway, resources, problems = [], reference }) {
  const v = { valid: true, problems: [], warnings: [] };
  const problem = (text) => {
    v.valid = false;
    v.problems.push(text);
  };
  for (const [name, report] of [['baseline', baseline], ['gateway run', gateway]])
    if (!report.valid)
      problem(`the ${name} load did not deliver its schedule: ${(report.problems ?? []).join('; ')}`);
  for (const [name, report] of [['baseline', baseline], ['gateway run', gateway]]) {
    const rate = report.rates.error_rate;
    if (rate > maxFailureRate)
      problem(`${(100 * rate).toFixed(2)}% of the ${name}'s requests failed, above the ${(100 * maxFailureRate).toFixed(1)}% a run may lose: its latencies are the survivors' alone and say nothing of the gateway`);
  }
  // A gateway cannot answer faster than the upstream it calls, so a negative
  // difference is a baseline that was slower than the run, not a speed-up.
  if (workload) {
    const population = populationOf(workload);
    const added = addedLatency(baseline, gateway)[population];
    const base = baseline.latency[population];
    for (const [name, key, gates] of [['p50', 'p50', true], ['p95', 'p95', true], ['p99', 'p99', false]]) {
      if (!added || !(added[key] < -negativeAllowance(base[`${key}_ms`]))) continue;
      if (gates)
        problem(`the gateway's added latency is ${added[key].toFixed(2)} ms at ${name}, below zero by more than a baseline's jitter: the baseline was slower than the run it is subtracted from, and no target can be judged on that`);
      else
        v.warnings.push(`the gateway's added latency is ${added[key].toFixed(2)} ms at ${name}, below zero by more than a baseline's jitter, as a tail varies between sessions`);
    }
  }
  for (const text of problems) problem(text);
  for (const [name, use] of [['the mock upstream', resources?.mock_cpu]])
    if (use?.allowed_cpus > 0 && use.cores > 0.8 * use.allowed_cpus)
      v.warnings.push(`${name} kept ${use.cores.toFixed(2)} of its ${use.allowed_cpus} CPUs busy, so it may have limited the run`);
  const g = resources?.gateway_cpu;
  if (g?.allowed_cpus > 0 && g.cores > 0.95 * g.allowed_cpus)
    v.warnings.push(`LiteLLM kept ${g.cores.toFixed(2)} of its ${g.allowed_cpus} CPUs busy: it was at its limit`);
  if (!reference.pins_disjoint)
    v.warnings.push('two of the gateway, the mock upstream and the load generator were given a CPU in common, so they competed for it');
  if (!reference.all)
    v.warnings.push('this is not a reference run: full scale, pinned processes and valid runs are all needed');
  return v;
}

// The cgroups of LiteLLM's containers, which are read for their CPU time and
// resident memory.
class Containers {
  constructor(dirs) {
    this.dirs = dirs;
    const effective = readFileSync(join(dirs[0], 'cpuset.cpus.effective'), 'utf8').trim();
    this.cpus = effective;
    this.vcpus = parseCpuList(effective);
  }

  // Resolves the cgroup directory of each container through the host PID of
  // its main process.
  static async open(names) {
    const dirs = [];
    for (const name of names) {
      const { stdout } = await run('docker', ['inspect', '-f', '{{.State.Pid}}', name]);
      const pid = Number(stdout.trim());
      if (!pid) throw new Error(`container ${name} is not running`);
      const line = readFileSync(`/proc/${pid}/cgroup`, 'utf8').split('\n').find((l) => l.startsWith('0::'));
      if (!line) throw new Error(`container ${name} is not in a cgroup v2 hierarchy, which the benchmark reads CPU time and memory from`);
      dirs.push(`/sys/fs/cgroup${line.slice(3)}`);
    }
    return new Containers(dirs);
  }

  cpuSeconds() {
    return this.dirs.reduce((sum, d) => sum + cpuSeconds(readFileSync(join(d, 'cpu.stat'), 'utf8')), 0);
  }

  residentMiB() {
    return this.dirs.reduce((sum, d) => sum + residentBytes(readFileSync(join(d, 'memory.stat'), 'utf8')), 0) / 2 ** 20;
  }
}

// CPU seconds of a process, from its stat file: user and system time in clock
// ticks.
function processCpuSeconds(pid, ticks) {
  const stat = readFileSync(`/proc/${pid}/stat`, 'utf8');
  const fields = stat.slice(stat.lastIndexOf(')') + 2).split(' ');
  return (Number(fields[11]) + Number(fields[12])) / ticks;
}

function allowedCpus(pid) {
  const status = readFileSync(`/proc/${pid}/status`, 'utf8');
  const match = /^Cpus_allowed_list:\s*(\S+)/m.exec(status);
  return match ? parseCpuList(match[1]) : 0;
}

const children = new Set();
for (const signal of ['SIGINT', 'SIGTERM'])
  process.on(signal, () => {
    for (const child of children) child.kill('SIGKILL');
    process.exit(signal === 'SIGINT' ? 130 : 143);
  });

// Runs the load generator, pinned when CPUs are given, and returns its report.
async function loadgen(binary, args, key, pin, out) {
  const command = pin ? 'taskset' : binary;
  const argv = pin ? ['-c', pin, binary, ...args] : args;
  const child = spawn(command, argv, { env: { ...process.env, LOADGEN_API_KEY: key }, stdio: ['ignore', 'inherit', 'inherit'] });
  children.add(child);
  const code = await new Promise((resolve, reject) => {
    child.on('error', reject);
    child.on('close', resolve);
  });
  children.delete(child);
  if (code !== 0) throw new Error(`the load generator exited with status ${code}`);
  return JSON.parse(readFileSync(out, 'utf8'));
}

async function json(response) {
  const text = await response.text();
  try {
    return JSON.parse(text);
  } catch {
    return { raw: text.slice(0, 500) };
  }
}

// A request to LiteLLM in the scenario's dialect with a prompt of about
// `tokens` tokens: the vocabulary of one short word per token that the load
// generator uses.
function request(url, workload, key, tokens) {
  const content = 'the '.repeat(tokens) + 'ping';
  const body = { model: workload.model, max_tokens: 8, messages: [{ role: 'user', content }] };
  if (workload.dialect === 'anthropic')
    return [`${url}/v1/messages`, { 'x-api-key': key, 'anthropic-version': '2023-06-01', 'content-type': 'application/json' }, body];
  return [`${url}/v1/chat/completions`, { authorization: `Bearer ${key}`, 'content-type': 'application/json' }, body];
}

async function send(url, workload, key, tokens, timeout) {
  const [target, headers, body] = request(url, workload, key, tokens);
  return fetch(target, { method: 'POST', headers, body: JSON.stringify(body), signal: AbortSignal.timeout(timeout) });
}

// Polls LiteLLM with the key until it answers, since a proxy loads its models
// and keys a moment after it listens.
async function awaitServing(url, workload, key) {
  const deadline = Date.now() + 120_000;
  let last = '';
  for (;;) {
    try {
      const response = await send(url, workload, key, 0, 30_000);
      const text = await response.text();
      if (response.status === 200) return;
      last = `status ${response.status}: ${text.slice(0, 300)}`;
    } catch (error) {
      last = String(error);
    }
    if (Date.now() > deadline) throw new Error(`LiteLLM did not serve the key within two minutes; last answer ${last}`);
    await sleep(500);
  }
}

// After a run with failures, what LiteLLM answers to one request of the largest
// prompt, since the generator keeps only statuses.
async function sampleFailure(url, workload, key) {
  try {
    const response = await send(url, workload, key, Math.max(...workload.prompt_tokens), 60_000);
    return `status ${response.status}: ${(await response.text()).slice(0, 800)}`;
  } catch (error) {
    return String(error);
  }
}

// Waits for the key's spend to show in LiteLLM's database, which it writes in
// batches, and returns it. A budget key that accrued nothing means the scenario
// did no budget work, so the run is not comparable with OLP's.
async function awaitSpend(url, master, key, seconds) {
  const deadline = Date.now() + seconds * 1000;
  let spend = 0;
  for (;;) {
    const response = await fetch(`${url}/key/info?key=${encodeURIComponent(key)}`, {
      headers: { authorization: `Bearer ${master}` },
      signal: AbortSignal.timeout(30_000)
    });
    const body = await json(response);
    spend = Number(body.info?.spend ?? body.spend ?? 0);
    if (spend > 0 || Date.now() > deadline) return spend;
    await sleep(5000);
  }
}

function environment(args) {
  const model = readFileSync('/proc/cpuinfo', 'utf8').split('\n').find((l) => l.startsWith('model name'));
  return {
    os: process.platform,
    arch: process.arch === 'x64' ? 'amd64' : process.arch,
    kernel: release(),
    cpu_model: model ? model.split(':')[1].trim() : '',
    host_cpus: cpus().length,
    memory_gib: totalmem() / 2 ** 30,
    load_average_at_start: loadavg()[0],
    litellm_image: args.image,
    mock_cpus: args['mock-cpus'] ?? '',
    gateway_cpus: args['gateway-cpus'] ?? '',
    loadgen_cpus: args['loadgen-cpus'] ?? ''
  };
}

async function mock(url, method, path, body) {
  const response = await fetch(`${url}${path}`, {
    method,
    headers: { 'content-type': 'application/json' },
    body: body ? JSON.stringify(body) : undefined,
    signal: AbortSignal.timeout(10_000)
  });
  if (!response.ok) throw new Error(`${method} ${path} on the mock upstream: status ${response.status}`);
  return json(response);
}

async function main(argv) {
  const { values: args, positionals } = parseArgs({
    args: argv,
    allowPositionals: true,
    options: {
      'olp-result': { type: 'string' },
      out: { type: 'string' },
      'litellm-url': { type: 'string' },
      profile: { type: 'string' },
      containers: { type: 'string' },
      workers: { type: 'string' },
      image: { type: 'string', default: '' },
      'mock-url': { type: 'string' },
      'mock-pid': { type: 'string' },
      'loadgen-binary': { type: 'string' },
      'loadgen-cpus': { type: 'string' },
      'mock-cpus': { type: 'string' },
      'gateway-cpus': { type: 'string' },
      'late-after': { type: 'string', default: '5ms' }
    }
  });
  const [scenario] = positionals;
  for (const name of ['olp-result', 'out', 'litellm-url', 'profile', 'containers', 'mock-url', 'mock-pid', 'loadgen-binary'])
    if (!args[name]) throw new Error(`--${name} is required`);
  const master = process.env.LITELLM_MASTER_KEY;
  if (!master) throw new Error('LITELLM_MASTER_KEY is required');
  if (!/^S[1-5]$/.test(scenario ?? '')) throw new Error('name a scenario from S1 to S5; S6 has no LiteLLM counterpart');

  const olp = JSON.parse(readFileSync(args['olp-result'], 'utf8'));
  const workload = { ...olp.workload, model: modelGroup(olp.workload) };
  const scale = olp.scale;
  const mockPid = Number(args['mock-pid']);
  const ticks = Number((await run('getconf', ['CLK_TCK'])).stdout);
  const containers = await Containers.open(args.containers.split(','));
  const raw = join(args.out, 'raw');
  mkdirSync(raw, { recursive: true });
  const id = scenario.toLowerCase();
  const log = (text) => console.log(`[${scenario} litellm] ${text}`);

  // While the key is made and tried the upstream answers as the scenario
  // says, and a failover route's first target is not yet failing: LiteLLM
  // cools a deployment down for its failures, and none should be spent on
  // setup.
  await mock(args['mock-url'], 'PUT', '/_mock/config', mockSpec(workload, false));
  const created = await json(await fetch(`${args['litellm-url']}/key/generate`, {
    method: 'POST',
    headers: { authorization: `Bearer ${master}`, 'content-type': 'application/json' },
    body: JSON.stringify(keyRequest(workload, scenario)),
    signal: AbortSignal.timeout(60_000)
  }));
  if (!created.key) throw new Error(`LiteLLM did not make a key: ${JSON.stringify(created).slice(0, 300)}`);
  const key = created.key;
  await awaitServing(args['litellm-url'], workload, key);
  log(`serving ${workload.model} through a virtual key${workload.cost_budget ? ` with a budget of ${budget}` : ''}`);
  await mock(args['mock-url'], 'PUT', '/_mock/config', mockSpec(workload, workload.failover));
  await sleep(settleSeconds * 1000);

  const lateAfter = args['late-after'];
  await mock(args['mock-url'], 'POST', '/_mock/reset');
  const baseline = await loadgen(
    args['loadgen-binary'],
    loadgenArgs(workload, { url: args['mock-url'], model: workload.baseline_model, name: 'baseline', json: join(raw, `${id}-baseline.json`), lateAfter }),
    'bench-baseline', args['loadgen-cpus'], join(raw, `${id}-baseline.json`)
  );

  // LiteLLM is idle while the baseline runs, so this pause also measures what
  // it costs to do nothing.
  const idleFrom = { cpu: containers.cpuSeconds(), at: process.hrtime.bigint() };
  await sleep(settleSeconds * 1000);
  const idleTo = { cpu: containers.cpuSeconds(), at: process.hrtime.bigint() };
  const idleCores = (idleTo.cpu - idleFrom.cpu) / (Number(idleTo.at - idleFrom.at) / 1e9);

  await mock(args['mock-url'], 'POST', '/_mock/reset');
  const mockBefore = processCpuSeconds(mockPid, ticks);
  const before = { cpu: containers.cpuSeconds(), at: process.hrtime.bigint(), memory: containers.residentMiB() };
  let peak = before.memory;
  const sampler = setInterval(() => {
    try {
      peak = Math.max(peak, containers.residentMiB());
    } catch {
      // A container that has gone away is reported by the run's failures.
    }
  }, 250);
  let gateway;
  try {
    gateway = await loadgen(
      args['loadgen-binary'],
      loadgenArgs(workload, { url: args['litellm-url'], model: workload.model, name: 'gateway', json: join(raw, `${id}-gateway.json`), lateAfter }),
      key, args['loadgen-cpus'], join(raw, `${id}-gateway.json`)
    );
  } finally {
    clearInterval(sampler);
  }
  const after = { cpu: containers.cpuSeconds(), at: process.hrtime.bigint(), memory: containers.residentMiB() };
  const mockAfter = processCpuSeconds(mockPid, ticks);
  const stats = await mock(args['mock-url'], 'GET', '/_mock/stats');
  peak = Math.max(peak, after.memory);
  const wall = Number(after.at - before.at) / 1e9;
  const used = after.cpu - before.cpu;

  const problems = [];
  let sample;
  if (gateway.requests.failed > 0) {
    sample = await sampleFailure(args['litellm-url'], workload, key);
    log(`LiteLLM answered ${sample}`);
  }
  let keySpend = null;
  if (workload.cost_budget) {
    log('waiting for the key\'s spend to reach the database');
    keySpend = await awaitSpend(args['litellm-url'], master, key, 150);
    if (!(keySpend > 0))
      problems.push('the key\'s budget accrued no spend, so LiteLLM did no budget work and the run is not comparable with OLP\'s, whose key is charged for every request');
  }

  const premise = failoverProblem(workload, stats);
  if (premise) problems.push(premise);

  const reached = gateway.requests.sent + gateway.requests.warmup_sent;
  const reference = referenceConditions({
    scale,
    gatewayCpus: args['gateway-cpus'],
    mockCpus: args['mock-cpus'],
    loadgenCpus: args['loadgen-cpus'],
    baseline,
    gateway,
    problems
  });
  const resources = {
    rss_before_mib: before.memory,
    rss_peak_mib: peak,
    rss_after_mib: after.memory,
    memory_basis: 'the sum over LiteLLM\'s containers of the cgroup\'s anonymous memory and mapped file pages, sampled every 250 ms',
    wall_seconds: wall,
    gateway_idle_cores: idleCores,
    gateway_cpu: { seconds: used, cores: used / wall, allowed_cpus: containers.vcpus },
    mock_cpu: { seconds: mockAfter - mockBefore, cores: (mockAfter - mockBefore) / wall, allowed_cpus: allowedCpus(mockPid) }
  };
  const ttft = ttftOverhead(baseline, gateway);
  const result = {
    scenario,
    gateway: 'litellm',
    title: olp.title,
    why: olp.why,
    generated_at: new Date().toISOString(),
    scale,
    reference_conditions: reference,
    environment: environment(args),
    workload: olp.workload,
    litellm: {
      profile: args.profile,
      image: args.image,
      model_group: workload.model,
      workers: Number(args.workers) || null,
      containers: args.containers.split(','),
      cpus_allowed: containers.cpus,
      vcpus: containers.vcpus
    },
    baseline,
    gateway_run: gateway,
    added_latency_ms: addedLatency(baseline, gateway),
    ttft_overhead_ms: ttft.value,
    ttft_overhead_reason: ttft.reason,
    throughput: throughput({ report: gateway, vcpus: containers.vcpus, cpuSeconds: used, idleCores, wallSeconds: wall }),
    resources,
    error_rate: {
      baseline: baseline.rates.error_rate,
      gateway: gateway.rates.error_rate,
      gateway_success_rate: gateway.rates.success_rate,
      gateway_by_kind: gateway.errors.by_kind,
      gateway_status_codes: gateway.errors.status_codes
    },
    error_sample: sample,
    // LiteLLM keeps no per-attempt record this harness reads, so its attempts
    // are what the mock saw during the run: every upstream call, over the
    // requests sent to the gateway.
    attempts: {
      per_request: reached > 0 ? stats.requests / reached : 0,
      upstream_requests: stats.requests,
      injected_errors: stats.injected_errors,
      by_model: stats.models,
      // Which of the mock's endpoints LiteLLM called: for an Anthropic client
      // it calls the OpenAI Responses endpoint.
      by_dialect: stats.dialects
    },
    budget: workload.cost_budget ? { max_budget: budget, key_spend: keySpend } : undefined,
    validity: validity({ workload, baseline, gateway, resources, problems, reference })
  };
  writeFileSync(join(args.out, `${id}.json`), `${JSON.stringify(result, null, 2)}\n`);

  const added = result.added_latency_ms;
  const headline = workload.stream_share === 0 ? added.unary : workload.stream_share === 1 ? added.stream : added.all;
  log(`added latency ${headline ? `p50 ${headline.p50.toFixed(2)} p95 ${headline.p95.toFixed(2)} p99 ${headline.p99.toFixed(2)} ms` : 'not measured'}; ` +
    `${result.throughput.sustained_rps.toFixed(1)} rps over ${containers.vcpus} vCPU, ${result.throughput.requests_per_cpu_second.toFixed(1)} requests per CPU second; ` +
    `peak ${peak.toFixed(0)} MiB; success ${(100 * gateway.rates.success_rate).toFixed(2)}%; ${result.attempts.per_request.toFixed(4)} upstream calls per request`);
  for (const text of result.validity.problems) log(`PROBLEM ${text}`);
  for (const text of result.validity.warnings) log(`warning ${text}`);
  return problems.length ? 1 : 0;
}

if (process.argv[1] === fileURLToPath(import.meta.url))
  main(process.argv.slice(2)).then(
    (code) => process.exit(code),
    (error) => {
      console.error(`bench-litellm: ${error.message}`);
      process.exit(1);
    }
  );
