import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import {
  addedLatency,
  cpuSeconds,
  cpuSet,
  failoverProblem,
  handledRequests,
  keyRequest,
  loadgenArgs,
  maxFailureRate,
  mockSpec,
  modelGroup,
  parseCpuList,
  populationOf,
  referenceConditions,
  residentBytes,
  sharedCpus,
  throughput,
  ttftOverhead,
  validity
} from './bench-litellm.mjs';

// The workloads of the scenario suite's plans (tests/bench/scenarios_test.go),
// as its result files record them.
const s1 = {
  dialect: 'openai',
  rate_rps: 1000,
  stream_share: 0,
  prompt_tokens: [0],
  max_tokens: 16,
  mock_behavior: { ttft_ms: 20, interval_ms: 0, output_tokens: 16 },
  upstream_models: ['bench-chat'],
  baseline_model: 'bench-chat',
  duration_seconds: 60,
  warmup_seconds: 10,
  timeout_seconds: 30,
  expected_concurrency: 20
};
const s3 = {
  ...s1,
  rate_rps: 3000,
  stream_share: 0.5,
  prompt_tokens: [50000, 75000, 100000],
  upstream_models: ['gpt-4o-bench'],
  baseline_model: 'gpt-4o-bench',
  cost_budget: true
};
const s4 = { ...s1, upstream_models: ['bench-primary', 'bench-secondary'], baseline_model: 'bench-secondary', failover: true };
const s5 = { ...s1, dialect: 'anthropic', stream_share: 1, surface_path: '/anthropic', model_surfaces: ['openai', 'anthropic'] };

test('each scenario calls the model group of deploy/litellm that matches it', () => {
  assert.equal(modelGroup(s1), 'bench-route');
  assert.equal(modelGroup(s3), 'bench-priced');
  assert.equal(modelGroup(s4), 'bench-failover');
  assert.equal(modelGroup(s5), 'bench-route');
  // The groups exist in both configurations, which keep one model list.
  for (const file of ['production', 'high-throughput']) {
    const config = readFileSync(new URL(`../deploy/litellm/${file}.yaml`, import.meta.url), 'utf8');
    for (const group of ['bench-route', 'bench-priced', 'bench-failover'])
      assert.match(config, new RegExp(`model_name: ${group}\\n`), `${file} has ${group}`);
  }
});

test('the two LiteLLM configurations keep an identical model list', () => {
  const list = (file) => {
    const config = readFileSync(new URL(`../deploy/litellm/${file}.yaml`, import.meta.url), 'utf8');
    const start = config.indexOf('model_list:');
    return config.slice(start, config.indexOf('\nrouter_settings:'));
  };
  assert.ok(list('production').length > 400);
  assert.equal(list('production'), list('high-throughput'));
});

test('only a scenario with a cost budget gives its key one', () => {
  const plain = keyRequest(s1, 'S1');
  assert.deepEqual(plain.models, ['bench-route']);
  assert.equal(plain.max_budget, undefined);
  assert.match(plain.key_alias, /^bench-s1-\d+$/);
  const budgeted = keyRequest(s3, 'S3');
  assert.deepEqual(budgeted.models, ['bench-priced']);
  assert.equal(budgeted.max_budget, 1_000_000);
  assert.equal(budgeted.budget_duration, '30d');
});

test('the mock is configured as the scenario suite configures it', () => {
  assert.deepEqual(mockSpec(s1, false), { default: s1.mock_behavior, models: { 'bench-chat': {} } });
  // S3's upstream model is named as an OpenAI model, which OLP counts exactly.
  assert.deepEqual(mockSpec(s3, false).models, { 'gpt-4o-bench': {} });
  // Only a failover route's first model fails, and only when armed.
  assert.deepEqual(mockSpec(s4, false).models, { 'bench-primary': {}, 'bench-secondary': {} });
  assert.deepEqual(mockSpec(s4, true).models, { 'bench-primary': { status: 503 }, 'bench-secondary': {} });
});

test('the load generator is given the recorded load and nothing else', () => {
  const args = loadgenArgs(s3, { url: 'http://127.0.0.1:4000', model: 'bench-priced', name: 'gateway', json: '/tmp/s3.json' });
  const flag = (name) => args[args.indexOf(name) + 1];
  assert.equal(flag('-url'), 'http://127.0.0.1:4000');
  assert.equal(flag('-dialect'), 'openai');
  assert.equal(flag('-model'), 'bench-priced');
  assert.equal(flag('-rate'), '3000');
  assert.equal(flag('-duration'), '60s');
  assert.equal(flag('-warmup'), '10s');
  assert.equal(flag('-stream-share'), '0.5');
  assert.equal(flag('-prompt-tokens'), '50000,75000,100000');
  assert.equal(flag('-max-tokens'), '16');
  // Room for every request the schedule can have open and as much again.
  assert.equal(flag('-max-in-flight'), String(20 * 2 + 1024));
  assert.equal(flag('-timeout'), '30s');
  assert.equal(flag('-late-after'), '5ms');
  assert.equal(flag('-json'), '/tmp/s3.json');
  assert.ok(!args.includes('-api-key'), 'the key travels in the environment, not the process list');
  assert.equal(loadgenArgs(s5, { url: 'u', model: 'm', name: 'n', json: 'j', lateAfter: '2ms' })[1], 'u');
  assert.equal(loadgenArgs(s5, { url: 'u', model: 'm', name: 'n', json: 'j', lateAfter: '2ms' }).at(-5), '2ms');
  assert.equal(loadgenArgs(s5, { url: 'u', model: 'm', name: 'n', json: 'j' })[3], 'anthropic');
  // A held-open scenario has no counterpart.
  assert.throws(() => loadgenArgs({ ...s1, slow_read_bytes_per_second: 8192 }, { url: 'u', model: 'm', name: 'n', json: 'j' }), /S6/);
});

test('every flag the runner passes is one cmd/loadgen declares', () => {
  const source = readFileSync(new URL('../tests/bench/cmd/loadgen/main.go', import.meta.url), 'utf8');
  const declared = new Set([...source.matchAll(/fs\.\w+\((?:&\w+(?:\.\w+)*, )?"([a-z-]+)"/g)].map((m) => m[1]));
  const args = loadgenArgs(s3, { url: 'u', model: 'm', name: 'n', json: 'j' });
  for (const flag of args.filter((a) => a.startsWith('-')))
    assert.ok(declared.has(flag.slice(1)), `cmd/loadgen has no -${flag.slice(1)} flag`);
});

test('CPU lists give the CPUs they name, and processes that share one are found', () => {
  assert.deepEqual([...cpuSet('0-2,5')], [0, 1, 2, 5]);
  assert.deepEqual(sharedCpus({ gateway: '0-1', mock: '2-3', loadgen: '4-7' }), []);
  // Adjacent lists share nothing, and a process left unpinned shares nothing.
  assert.deepEqual(sharedCpus({ gateway: '0-1', mock: '2' }), []);
  assert.deepEqual(sharedCpus({ gateway: '0-7' }), []);
  assert.deepEqual(sharedCpus({}), []);
  assert.deepEqual(sharedCpus({ gateway: '0-3', mock: '3-4', loadgen: '6' }), ['the gateway and the mock upstream both run on CPU 3']);
  assert.deepEqual(sharedCpus({ gateway: '0', mock: '2-5', loadgen: '4,5,9' }), ['the mock upstream and the load generator both run on CPU 4,5']);
  assert.deepEqual(sharedCpus({ gateway: '0-1', mock: '1-2', loadgen: '0,2' }), [
    'the gateway and the mock upstream both run on CPU 1',
    'the gateway and the load generator both run on CPU 0',
    'the mock upstream and the load generator both run on CPU 2'
  ]);
});

test('CPU lists count like taskset reads them', () => {
  assert.equal(parseCpuList('0-3,6'), 5);
  assert.equal(parseCpuList('2'), 1);
  assert.equal(parseCpuList('4-5'), 2);
  assert.equal(parseCpuList('0-1,1-2'), 3);
  for (const bad of ['', 'a', '3-1', '-2', '1-']) assert.throws(() => parseCpuList(bad), /CPU list/);
});

test('resident memory is anonymous memory plus mapped file pages, not the page cache', () => {
  const stat = 'anon 1572519936\nfile 300384256\nkernel 30384128\nshmem 0\nfile_mapped 43331584\nfile_dirty 0\n';
  assert.equal(residentBytes(stat), 1572519936 + 43331584);
  assert.equal(residentBytes(''), 0);
});

test('CPU time is read from usage_usec', () => {
  assert.equal(cpuSeconds('usage_usec 63184223\nuser_usec 57964499\nsystem_usec 5219723\n'), 63.184223);
  assert.throws(() => cpuSeconds('user_usec 1\n'), /usage_usec/);
});

const summary = (count, p50, p95, p99) => ({ count, p50_ms: p50, p95_ms: p95, p99_ms: p99 });
const none = summary(0, 0, 0, 0);
const report = (over = {}) => ({
  valid: true,
  problems: [],
  rates: { target_rps: 1000, offered_rps: 999, throughput_rps: 998, success_rate: 1, error_rate: 0 },
  requests: { sent: 60000, succeeded: 60000, warmup_sent: 10000, warmup_failed: 0, failed: 0 },
  latency: { all: none, unary: none, stream: none, ttft: none },
  ...over
});

test('added latency is the run less the baseline, for the modes both have', () => {
  const baseline = report({ latency: { all: summary(10, 21, 22, 25), unary: summary(10, 21, 22, 25), stream: none, ttft: none } });
  const gateway = report({ latency: { all: summary(10, 24, 28, 40), unary: summary(10, 24, 28, 40), stream: none, ttft: none } });
  assert.deepEqual(addedLatency(baseline, gateway), { all: { p50: 3, p95: 6, p99: 15 }, unary: { p50: 3, p95: 6, p99: 15 }, stream: null });
  // A gateway that is faster than the baseline reads negative, which a noisy
  // machine does, rather than being clamped.
  assert.equal(addedLatency(gateway, baseline).all.p50, -3);
});

test('time to first token overhead is missing, with the reason, for a scenario that does not stream', () => {
  assert.match(ttftOverhead(report(), report()).reason, /no streaming requests/);
  const base = report({ latency: { ttft: summary(5, 20, 21, 22) } });
  const run = report({ latency: { ttft: summary(5, 22, 25, 30) } });
  assert.deepEqual(ttftOverhead(base, run), { value: { p50: 2, p95: 4, p99: 8 } });
});

test('only requests answered successfully count toward what a CPU carries', () => {
  const failing = report({ requests: { sent: 60000, succeeded: 36000, warmup_sent: 10000, warmup_failed: 2000, failed: 24000 } });
  assert.equal(handledRequests(failing), 44000);
  assert.equal(handledRequests(report()), 70000);
  const t = throughput({ report: failing, vcpus: 2, cpuSeconds: 44, idleCores: 0, wallSeconds: 80 });
  assert.equal(t.requests_per_cpu_second, 1000);
  assert.equal(t.gateway_cpu_ms_per_request, 1);
});

test('throughput nets out what the gateway spends idle, and says nothing when it spent too little', () => {
  const t = throughput({ report: report(), vcpus: 2, cpuSeconds: 100, idleCores: 0.5, wallSeconds: 80 });
  assert.equal(t.sustained_rps, 998);
  assert.equal(t.rps_per_vcpu, 499);
  assert.equal(t.gateway_cpu_ms_per_request_gross, (100 * 1000) / 70000);
  // 100 CPU seconds less 0.5 idle cores for 80 seconds is 60, over 70,000 requests.
  assert.equal(t.gateway_cpu_ms_per_request, (60 * 1000) / 70000);
  assert.equal(t.requests_per_cpu_second, 70000 / 60);
  const idle = throughput({ report: report(), vcpus: 2, cpuSeconds: 30, idleCores: 0.5, wallSeconds: 80 });
  assert.equal(idle.requests_per_cpu_second, 0);
  assert.equal(idle.gateway_cpu_ms_per_request, 0);
  assert.ok(idle.gateway_cpu_ms_per_request_gross > 0);
});

// What makes a run a reference run is each condition on its own: full scale,
// each process pinned, the pins apart, and load runs that delivered.
test('a reference run is full scale, pinned apart and valid, and nothing less is', () => {
  const pins = { gatewayCpus: '0-1', mockCpus: '2-3', loadgenCpus: '4-7' };
  const full = { scale: 1, ...pins, baseline: report(), gateway: report() };
  assert.deepEqual(referenceConditions(full), {
    full_scale: true,
    gateway_pinned: true,
    mock_pinned: true,
    loadgen_pinned: true,
    pins_disjoint: true,
    valid_runs: true,
    all: true
  });
  const without = (over, field) => {
    const r = referenceConditions({ ...full, ...over });
    assert.equal(r.all, false);
    assert.equal(r[field], false, field);
    return r;
  };
  without({ scale: 0.02 }, 'full_scale');
  without({ gatewayCpus: undefined }, 'gateway_pinned');
  without({ mockCpus: '' }, 'mock_pinned');
  without({ loadgenCpus: undefined }, 'loadgen_pinned');
  // Two processes on one CPU, and all on the same.
  without({ mockCpus: '1' }, 'pins_disjoint');
  without({ gatewayCpus: '0-1', mockCpus: '0-1', loadgenCpus: '0-1' }, 'pins_disjoint');
  without({ baseline: report({ valid: false }) }, 'valid_runs');
  without({ gateway: report({ valid: false }) }, 'valid_runs');
  // What the runner itself found wrong, such as a budget that accrued nothing.
  without({ problems: ['no spend'] }, 'valid_runs');
  assert.equal(referenceConditions({ ...full, gatewayCpus: undefined }).pins_disjoint, true);
});

const reference = { full_scale: true, pins_disjoint: true, all: true };

test('a run is valid only if both load runs delivered their schedule and nothing else went wrong', () => {
  const ok = validity({ baseline: report(), gateway: report(), resources: {}, reference });
  assert.deepEqual(ok, { valid: true, problems: [], warnings: [] });
  const late = validity({ baseline: report(), gateway: report({ valid: false, problems: ['12 requests were sent late'] }), resources: {}, reference });
  assert.equal(late.valid, false);
  assert.match(late.problems[0], /the gateway run load did not deliver its schedule: 12 requests were sent late/);
  const budget = validity({ baseline: report(), gateway: report(), resources: {}, problems: ['no spend'], reference });
  assert.deepEqual(budget.problems, ['no spend']);
});

const unary = { stream_share: 0 };
const latencies = (all) => ({ all, unary: all, stream: none, ttft: none });

test('a run that fails requests, or whose baseline did, is not valid', () => {
  assert.equal(maxFailureRate, 0.001);
  // A tenth of a percent is allowed, on either run, and a little more is not.
  const rates = (error_rate) => ({ ...report().rates, error_rate });
  for (const [rate, valid] of [[0, true], [0.001, true], [0.0011, false], [0.99, false]]) {
    const v = validity({ workload: unary, baseline: report(), gateway: report({ rates: rates(rate) }), resources: {}, reference });
    assert.equal(v.valid, valid, `${rate} of the run's requests failed`);
    if (!valid) assert.match(v.problems[0], /of the gateway run's requests failed.*survivors' alone/);
  }
  const base = validity({ workload: unary, baseline: report({ rates: rates(0.5) }), gateway: report(), resources: {}, reference });
  assert.equal(base.valid, false);
  assert.match(base.problems[0], /50\.00% of the baseline's requests failed/);
});

test('a negative added latency beyond a baseline\'s jitter is not valid, and a negative tail only a warning', () => {
  const p = (p50, p95, p99) => summary(100, p50, p95, p99);
  const check = (gateway) =>
    validity({ workload: unary, baseline: report({ latency: latencies(p(20, 21, 22)) }), gateway: report({ latency: latencies(gateway) }), resources: {}, reference });
  assert.deepEqual(check(p(21, 23, 24)), { valid: true, problems: [], warnings: [] });
  // A baseline p95 of 21 ms allows 2.1 ms of jitter.
  assert.equal(check(p(21, 19.5, 24)).valid, true);
  const p95 = check(p(21, 18, 24));
  assert.equal(p95.valid, false);
  assert.match(p95.problems[0], /-3\.00 ms at p95, below zero by more than a baseline's jitter/);
  const p50 = check(p(10, 23, 24));
  assert.equal(p50.valid, false);
  assert.match(p50.problems[0], /-10\.00 ms at p50/);
  const tail = check(p(21, 23, 10));
  assert.equal(tail.valid, true);
  assert.match(tail.warnings[0], /-12\.00 ms at p99/);
  // A streaming scenario is judged on its stream latencies.
  const stream = validity({
    workload: { stream_share: 1 },
    baseline: report({ latency: { all: none, unary: none, stream: p(100, 120, 130), ttft: none } }),
    gateway: report({ latency: { all: none, unary: none, stream: p(85, 121, 131), ttft: none } }),
    resources: {},
    reference
  });
  assert.equal(stream.valid, false);
  assert.match(stream.problems[0], /-15\.00 ms at p50/);
  assert.deepEqual([populationOf({ stream_share: 0 }), populationOf({ stream_share: 1 }), populationOf({ stream_share: 0.5 })], ['unary', 'stream', 'all']);
});

test('a busy mock or a LiteLLM at its limit is a warning, and a smoke run says it is not a reference run', () => {
  const resources = { mock_cpu: { cores: 0.9, allowed_cpus: 1 }, gateway_cpu: { cores: 1.99, allowed_cpus: 2 } };
  const v = validity({ baseline: report(), gateway: report(), resources, reference: { pins_disjoint: true, all: false } });
  assert.equal(v.valid, true);
  assert.equal(v.warnings.length, 3);
  assert.match(v.warnings[0], /the mock upstream kept 0\.90 of its 1 CPUs busy/);
  assert.match(v.warnings[1], /LiteLLM kept 1\.99 of its 2 CPUs busy/);
  assert.match(v.warnings[2], /not a reference run/);
  // Processes that share a CPU compete for it, which is said apart from the rest.
  const shared = validity({ baseline: report(), gateway: report(), resources: {}, reference: { pins_disjoint: false, all: false } });
  assert.equal(shared.valid, true);
  assert.match(shared.warnings[0], /given a CPU in common/);
});

test('a failover run is valid only if the mock injected errors, so the first target failed', () => {
  assert.match(failoverProblem(s4, { injected_errors: 0 }), /injected no error.*measured no failover/);
  assert.equal(failoverProblem(s4, { injected_errors: 5 }), null);
  // A scenario that is not a failover is not held to it.
  assert.equal(failoverProblem(s1, { injected_errors: 0 }), null);
  // It reaches the result's validity as a problem, as the budget's does.
  const v = validity({ baseline: report(), gateway: report(), resources: {}, problems: [failoverProblem(s4, { injected_errors: 0 })], reference });
  assert.equal(v.valid, false);
  assert.match(v.problems[0], /measured no failover/);
});

test('the runner refuses to start without what it needs, and S6', () => {
  const script = new URL('./bench-litellm.mjs', import.meta.url).pathname;
  const run = (args, env = {}) => spawnSync('node', [script, ...args], { encoding: 'utf8', env: { PATH: process.env.PATH, ...env } });
  assert.match(run(['S1']).stderr, /--olp-result is required/);
  const all = ['--olp-result', 'x', '--out', 'x', '--litellm-url', 'x', '--profile', 'x', '--containers', 'x', '--mock-url', 'x', '--mock-pid', '1', '--loadgen-binary', 'x'];
  assert.match(run(['S1', ...all]).stderr, /LITELLM_MASTER_KEY is required/);
  const s6 = run(['S6', ...all], { LITELLM_MASTER_KEY: 'sk-x' });
  assert.equal(s6.status, 1);
  assert.match(s6.stderr, /S6 has no LiteLLM counterpart/);
});
