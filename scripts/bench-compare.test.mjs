import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import {
  baselineDrift,
  belowZero,
  buildReport,
  compareScenario,
  enforcementFailures,
  incomparable,
  metrics,
  mode,
  published,
  renderMarkdown,
  renderText
} from './bench-compare.mjs';

const summary = (p50, p95, p99) => ({ count: 100, p50_ms: p50, p95_ms: p95, p99_ms: p99 });
const triple = (p50, p95, p99) => ({ p50, p95, p99 });
const load = {
  dialect: 'openai',
  rate: 1000,
  duration_seconds: 60,
  warmup_seconds: 10,
  stream_share: 0,
  prompt_tokens: [0],
  max_tokens: 16,
  max_in_flight: 1026,
  timeout_seconds: 30
};

// A full-scale, valid, pinned result of either gateway, which share the shape
// the comparison reads. `added` is the added latency of the scenario's mode.
function result({ id = 'S1', added = triple(0.5, 1.5, 4), rpc = 2000, vcpus = 2, share = 0, rest = {} } = {}) {
  const mode = share === 0 ? 'unary' : share === 1 ? 'stream' : 'all';
  return {
    scenario: id,
    title: `title of ${id}`,
    scale: 1,
    workload: { stream_share: share },
    environment: { olp_version: 'olp 0.1.0', cpu_model: 'Test CPU', host_cpus: 8, memory_gib: 16, kernel: '7.0', gateway_cpus: '0-1', mock_cpus: '2', loadgen_cpus: '3-4' },
    litellm: { image: 'ghcr.io/berriai/litellm:v1.103.2@sha256:abc' },
    baseline: { latency: { [mode]: summary(20, 21, 22) } },
    gateway_run: { config: { ...load, stream_share: share } },
    added_latency_ms: { all: null, unary: null, stream: null, [mode]: added },
    ttft_overhead_ms: share === 0 ? null : triple(1, 2, 3),
    throughput: {
      sustained_rps: 1000,
      vcpus,
      rps_per_vcpu: 1000 / vcpus,
      gateway_cpu_ms_per_request: 1000 / rpc,
      requests_per_cpu_second: rpc
    },
    resources: { rss_peak_mib: 300 },
    error_rate: { gateway: 0, gateway_success_rate: 1 },
    attempts: { per_request: 1 },
    validity: { valid: true, problems: [] },
    reference_conditions: { all: true },
    targets: [],
    ...rest
  };
}

const find = (scenario, id) => scenario.targets.find((t) => t.id === id);

test('the added latency of the mode a scenario exercises stands for it', () => {
  assert.deepEqual(metrics(result({ share: 0 })).added, triple(0.5, 1.5, 4));
  assert.deepEqual(metrics(result({ share: 1, added: triple(7, 8, 9) })).added, triple(7, 8, 9));
  assert.deepEqual(metrics(result({ share: 0.5, added: triple(1, 2, 3) })).added, triple(1, 2, 3));
  assert.equal(metrics(null), null);
});

// The result's own population keys are written out here, since a fixture that
// computes them as the code does cannot see the code get them wrong.
test('a scenario reads the baseline of the population it exercises', () => {
  assert.equal(mode({ workload: { stream_share: 0 } }), 'unary');
  assert.equal(mode({ workload: { stream_share: 1 } }), 'stream');
  assert.equal(mode({ workload: { stream_share: 0.5 } }), 'all');
  for (const [share, key] of [[0, 'unary'], [1, 'stream'], [0.5, 'all']]) {
    const side = (p95) => ({ ...result({ share }), baseline: { latency: { [key]: summary(20, p95, 22) } } });
    assert.deepEqual(metrics(side(21)).baseline, triple(20, 21, 22), `${key} baseline`);
    // A baseline that differs by 5 ms at p95 is found, and fails an enforcing run.
    const drift = baselineDrift(side(21), side(26));
    assert.deepEqual(drift?.gating, ['p95'], `${key} drift`);
    assert.equal(baselineDrift(side(21), side(21.5)).gating.length, 0);
    // A baseline kept under another population's key is not the scenario's.
    const other = { ...side(21), baseline: { latency: { [key === 'stream' ? 'unary' : 'stream']: summary(20, 21, 22) } } };
    assert.equal(baselineDrift(other, side(21)), null, `${key} read from the wrong population`);
  }
});

test('a CPU figure that was not measured is missing, never zero', () => {
  assert.equal(metrics(result({ rpc: 0 })).requestsPerCpuSecond, null);
  assert.equal(metrics(result({ rpc: 1500 })).requestsPerCpuSecond, 1500);
});

test('the latency target needs OLP lower at p50, p95 and p99', () => {
  const met = compareScenario('S1', result({ added: triple(0.5, 1.5, 4) }), result({ added: triple(3, 9, 20) }));
  assert.equal(find(met, 'added-latency-below-litellm').status, 'met');
  // Equal is not lower, and one percentile above is a miss.
  for (const [litellm, worse] of [
    [triple(3, 1.5, 20), 'p95'],
    [triple(3, 9, 4), 'p99'],
    [triple(0.4, 9, 20), 'p50']
  ]) {
    const t = find(compareScenario('S1', result({ added: triple(0.5, 1.5, 4) }), result({ added: litellm })), 'added-latency-below-litellm');
    assert.equal(t.status, 'missed');
    assert.match(t.reason, new RegExp(`not lower at ${worse}`));
  }
});

test('a negative added latency is noise, and beats nothing', () => {
  // Added latency below zero by more than a baseline's jitter (a millisecond or
  // a tenth of its figure, here 2.1 ms at p95) cannot be faster than the
  // upstream, so the comparison is not judged on it, whichever gateway has it.
  assert.deepEqual(belowZero(metrics(result({ added: triple(0.5, 1.5, 4) }))), []);
  assert.deepEqual(belowZero(metrics(result({ added: triple(-0.9, -2, -2.3) }))), ['p99']);
  assert.deepEqual(belowZero(metrics(result({ added: triple(-2.2, -3, 4) }))), ['p50', 'p95']);
  assert.deepEqual(belowZero(null), []);
  assert.deepEqual(belowZero({ added: null }), []);
  for (const [olp, litellm, who] of [
    [triple(0.5, 1.5, -5), triple(3, 9, 20), 'OLP'],
    [triple(0.5, 1.5, 4), triple(3, -6, 20), 'LiteLLM']
  ]) {
    const t = find(compareScenario('S1', result({ added: olp }), result({ added: litellm })), 'added-latency-below-litellm');
    assert.equal(t.status, 'not_checked', `${who} has a negative figure`);
    assert.match(t.reason, new RegExp(`${who}'s added latency is below zero at p\\d+`));
  }
  // It counts against an enforcing run, as any target left unchecked does.
  const noisy = good();
  noisy.olp.S1.added_latency_ms.unary = triple(0.5, 1.5, -5);
  assert.ok(buildReport({ ...noisy, ids: ['S1'], enforce: true }).failures.some((f) => /added-latency-below-litellm was not checked/.test(f)));
});

test('requests per CPU second settle what sustained RPS per vCPU cannot', () => {
  // Both gateways hold the 1,000 requests per second offered, so they tie on
  // sustained requests per allotted vCPU.
  const tie = compareScenario('S2', result({ id: 'S2', rpc: 3000 }), result({ id: 'S2', rpc: 800 }));
  assert.equal(metrics(result()).rpsPerVcpu, metrics(result()).rpsPerVcpu);
  assert.equal(find(tie, 'rps-per-vcpu-above-litellm').status, 'met');
  assert.match(find(tie, 'rps-per-vcpu-above-litellm').reason, /OLP 3000\.0, LiteLLM 800\.0/);
  assert.equal(find(compareScenario('S2', result({ id: 'S2', rpc: 800 }), result({ id: 'S2', rpc: 800 })), 'rps-per-vcpu-above-litellm').status, 'missed');
  assert.equal(find(compareScenario('S2', result({ id: 'S2', rpc: 700 }), result({ id: 'S2', rpc: 800 })), 'rps-per-vcpu-above-litellm').status, 'missed');
  const unmeasured = compareScenario('S2', result({ id: 'S2', rpc: 0 }), result({ id: 'S2', rpc: 800 }));
  assert.equal(find(unmeasured, 'rps-per-vcpu-above-litellm').status, 'not_checked');
  assert.match(find(unmeasured, 'rps-per-vcpu-above-litellm').reason, /too little CPU/);
});

test('a comparison is judged only at full scale, on valid runs, the same vCPUs and the same load', () => {
  const full = result();
  assert.deepEqual(incomparable(full, result()), []);
  assert.match(incomparable({ ...full, scale: 0.02 }, result())[0], /scale 0\.02 and 1,/);
  assert.match(incomparable(full, { ...result(), scale: 0.5 })[0], /scale 1 and 0\.5,/);
  // Runs at the same reduced scale name it once.
  assert.deepEqual(incomparable({ ...full, scale: 0.02 }, { ...result(), scale: 0.02 }), ['the runs were at scale 0.02, and the targets are stated at full rates']);
  assert.match(incomparable(full, result({ rest: { validity: { valid: false, problems: ['3 requests were dropped'] } } }))[0], /LiteLLM run was not valid: 3 requests were dropped/);
  assert.match(incomparable(result({ rest: { validity: { valid: false, problems: ['late'] } } }), result())[0], /OLP run was not valid: late/);
  assert.match(incomparable(full, result({ vcpus: 4 }))[0], /2 and 4 vCPUs/);
  const other = result();
  other.gateway_run.config.rate = 500;
  assert.match(incomparable(full, other)[0], /did not apply the same load/);
  assert.deepEqual(incomparable(full, null), ['there is no LiteLLM result']);
  assert.deepEqual(incomparable(null, full), ['there is no OLP result']);
  // Whatever the reason, the target is unchecked and says why.
  const t = find(compareScenario('S1', { ...full, scale: 0.02 }, result()), 'added-latency-below-litellm');
  assert.equal(t.status, 'not_checked');
  assert.match(t.reason, /scale 0\.02/);
});

test('the targets OLP\'s own result judges are copied, not recomputed', () => {
  const own = [
    { id: 's1-2vcpu-added-p95', description: 'p95', target: '<= 2 ms', status: 'met', measured: 1.1 },
    { id: 's1-2vcpu-added-p99', description: 'p99', target: '<= 5 ms', status: 'missed', measured: 6.2, reason: 'too slow' },
    { id: 'request-metadata-lost', description: 'metadata', target: '0', status: 'not_checked', reason: 'valkey was down' },
    { id: 'added-latency-below-litellm', status: 'needs_comparison' }
  ];
  const s = compareScenario('S1', result({ rest: { targets: own } }), result({ added: triple(3, 9, 20) }));
  assert.equal(find(s, 's1-2vcpu-added-p95').status, 'met');
  assert.equal(find(s, 's1-2vcpu-added-p95').olp, 1.1);
  assert.equal(find(s, 's1-2vcpu-added-p99').status, 'missed');
  assert.equal(find(s, 's1-2vcpu-added-p99').reason, 'too slow');
  assert.equal(find(s, 'request-metadata-lost').status, 'not_checked');
  // OLP's own comparison placeholder is replaced by the real one.
  assert.equal(s.targets.filter((t) => t.id === 'added-latency-below-litellm').length, 1);
  assert.equal(find(s, 'added-latency-below-litellm').status, 'met');
  // S4 and S5 have no such targets, and S2 only the metadata one.
  assert.deepEqual(compareScenario('S4', result({ id: 'S4', rest: { targets: own } }), result({ id: 'S4' })).targets.map((t) => t.id), ['added-latency-below-litellm', 'rps-per-vcpu-above-litellm']);
  const s2 = compareScenario('S2', result({ id: 'S2', rest: { targets: own } }), result({ id: 'S2' }));
  assert.deepEqual(s2.targets.map((t) => t.id), ['added-latency-below-litellm', 'rps-per-vcpu-above-litellm', 'request-metadata-lost']);
  // A missing copy is reported, not skipped.
  assert.equal(find(compareScenario('S1', result(), result()), 's1-2vcpu-added-p95').status, 'not_checked');
});

test('S3 is judged against the published profile and the same profile measured here', () => {
  const s3 = (olp, litellm) => find(compareScenario('S3', olp, litellm), 's3-fewer-vcpu-than-litellm');
  const olp = (over) => result({ id: 'S3', share: 0.5, vcpus: 8, added: triple(3, 9, 20), ...over });
  const lite = (over) => result({ id: 'S3', share: 0.5, vcpus: 8, added: triple(40, 120, 400), ...over });

  const met = s3(olp(), lite());
  assert.equal(met.status, 'met');
  assert.match(met.reason, /OLP used 8 vCPU against the profile's 132 requested \(33 pods of 4 workers\)/);
  assert.match(met.reason, /against the profile's published 54\.029 ms/);
  assert.match(met.reason, /measured on the same hardware at 120\.00 ms, answering 100\.00% of requests/);
  assert.equal(met.litellm.vcpus, 8);

  // As many vCPU as the profile, or a p95 above its published one, is a miss.
  assert.equal(s3(olp({ vcpus: 132 }), lite({ vcpus: 132 })).status, 'missed');
  assert.equal(s3(olp({ added: triple(3, 55, 70) }), lite({ added: triple(40, 120, 400) })).status, 'missed');
  // So is a p95 above what the profile measured on this hardware, though below
  // the published one.
  assert.equal(s3(olp({ added: triple(3, 30, 50) }), lite({ added: triple(10, 20, 80) })).status, 'missed');

  // Without a LiteLLM run that can be compared with, the published figures
  // stand alone and the note says the cross-check was not made.
  const alone = s3(olp(), lite({ rest: { validity: { valid: false, problems: ['dropped'] } } }));
  assert.equal(alone.status, 'met');
  assert.match(alone.reason, /cross-check was not made: the LiteLLM run was not valid: dropped/);
  const none = s3(olp(), null);
  assert.equal(none.status, 'met');
  assert.match(none.reason, /cross-check was not made: there is no LiteLLM result/);

  // OLP's own run must be fit to judge, whatever LiteLLM did.
  assert.equal(s3({ ...olp(), scale: 0.02 }, lite()).status, 'not_checked');
  assert.equal(s3(olp({ rest: { validity: { valid: false, problems: ['x'] } } }), lite()).status, 'not_checked');
  assert.equal(s3(null, lite()).status, 'not_checked');
});

test('baselines that disagree beyond noise are reported', () => {
  const a = result();
  const near = result();
  near.baseline.latency.unary = summary(20.5, 21.9, 22.8);
  assert.deepEqual(baselineDrift(a, near).exceeds, []);
  const far = result();
  far.baseline.latency.unary = summary(20, 25, 22);
  const drift = baselineDrift(a, far);
  assert.deepEqual(drift.exceeds, ['p95']);
  assert.deepEqual(drift.gating, ['p95']);
  // The tail is reported, and does not gate.
  const tail = result();
  tail.baseline.latency.unary = summary(20, 21, 30);
  assert.deepEqual(baselineDrift(a, tail).exceeds, ['p99']);
  assert.deepEqual(baselineDrift(a, tail).gating, []);
  assert.equal(drift.p95, 4);
  assert.equal(baselineDrift(a, null), null);
  // The allowance is a millisecond or a tenth, whichever is larger.
  const big = result();
  big.baseline.latency.unary = summary(200, 210, 220);
  const bigger = result();
  bigger.baseline.latency.unary = summary(221, 232, 243);
  assert.deepEqual(baselineDrift(big, bigger).exceeds, ['p50', 'p95', 'p99']);
  assert.deepEqual(baselineDrift(big, bigger).gating, ['p50', 'p95']);
  bigger.baseline.latency.unary = summary(219.9, 229.9, 239.9);
  assert.deepEqual(baselineDrift(big, bigger).exceeds, []);
});

const good = () => ({
  olp: { S1: result({ rest: { targets: [{ id: 's1-2vcpu-added-p95', status: 'met', measured: 1 }, { id: 's1-2vcpu-added-p99', status: 'met', measured: 2 }, { id: 'request-metadata-lost', status: 'met', measured: 0 }] } }) },
  litellm: { S1: result({ added: triple(3, 9, 20), rpc: 500 }) }
});

test('an enforcing report fails on a miss, an unchecked target, a run that is not a reference and drifting baselines', () => {
  assert.deepEqual(buildReport({ ...good(), ids: ['S1'], enforce: true }).failures, []);

  const slow = good();
  slow.litellm.S1 = result({ added: triple(0.1, 9, 20), rpc: 500 });
  const missed = buildReport({ ...slow, ids: ['S1'], enforce: true }).failures;
  assert.equal(missed.length, 1);
  assert.match(missed[0], /^S1: target added-latency-below-litellm missed: .*not lower at p50/);

  const smoke = good();
  smoke.olp.S1.scale = 0.02;
  smoke.litellm.S1.scale = 0.02;
  const unchecked = buildReport({ ...smoke, ids: ['S1'], enforce: true }).failures;
  assert.equal(unchecked.filter((f) => /was not checked/.test(f)).length, 2);

  const unpinned = good();
  unpinned.litellm.S1.reference_conditions = { all: false };
  assert.match(buildReport({ ...unpinned, ids: ['S1'], enforce: true }).failures[0], /the LiteLLM run was not a reference run/);

  const drift = good();
  drift.litellm.S1.baseline.latency.unary = summary(30, 31, 32);
  assert.match(buildReport({ ...drift, ids: ['S1'], enforce: true }).failures[0], /baselines differ at p50, p95,/);
  // A tail that differs alone is noted and fails nothing.
  const tail = good();
  tail.litellm.S1.baseline.latency.unary = summary(20, 21, 40);
  const tailed = buildReport({ ...tail, ids: ['S1'], enforce: true });
  assert.deepEqual(tailed.failures, []);
  assert.ok(tailed.notes.some((n) => /baselines of the two sessions differ at p99/.test(n)));

  // Without enforcement the same report fails nothing.
  assert.deepEqual(buildReport({ ...slow, ids: ['S1'] }).failures, []);
  assert.deepEqual(enforcementFailures({ scenarios: [] }), []);
});

test('the report names what it compared, what it could not, and notes a LiteLLM that dropped requests', () => {
  const g = good();
  g.litellm.S1 = result({ added: triple(3, 9, 20), rest: { error_rate: { gateway: 0.2, gateway_success_rate: 0.8 }, validity: { valid: false, problems: ['5 requests were dropped'] } } });
  const report = buildReport({ ...g, ids: ['S1', 'S2'], enforce: false });
  assert.deepEqual(report.scenarios.map((s) => s.id), ['S1']);
  assert.equal(report.olp_version, 'olp 0.1.0');
  assert.equal(report.litellm_image, 'ghcr.io/berriai/litellm:v1.103.2@sha256:abc');
  assert.deepEqual(report.published_litellm_profile, published);
  assert.ok(report.notes.some((n) => n === 'S1 LiteLLM: 5 requests were dropped'));
  assert.ok(report.notes.some((n) => /LiteLLM answered 80\.00% of the scheduled requests/.test(n)));
  // OLP is held to the same: a run that failed requests says so beside LiteLLM's.
  const lossy = good();
  lossy.olp.S1.error_rate = { gateway: 0.1, gateway_success_rate: 0.9 };
  assert.ok(buildReport({ ...lossy, ids: ['S1'] }).notes.some((n) => /^S1: OLP answered 90\.00% of the scheduled requests/.test(n)));
  assert.equal(report.summary.not_checked + report.summary.met + report.summary.missed, report.targets.length);
  assert.ok(report.targets.some((t) => t.status === 'not_checked'));
});

test('the text and markdown tables show both gateways, dash what is missing and escape pipes', () => {
  const g = good();
  g.litellm.S1.ttft_overhead_ms = null;
  const report = buildReport({ ...g, ids: ['S1', 'S4'], enforce: false });
  const text = renderText(report);
  const [header, , olp, litellm] = text.split('\n');
  assert.match(header, /^scenario\s+gateway\s+p50 ms\s+p95 ms\s+p99 ms/);
  assert.match(olp, /^S1\s+OLP\s+0\.50\s+1\.50\s+4\.00\s+-\s+500\.0\s+2000\.0\s+0\.50\s+300\s+0\.000\s+yes$/);
  assert.match(litellm, /^S1\s+LiteLLM\s+3\.00\s+9\.00\s+20\.00/);
  const missing = compareScenario('S5', result({ id: 'S5' }), null);
  const rows = renderText({ ...report, scenarios: [missing], targets: missing.targets, notes: [], failures: [], summary: { met: 0, missed: 0, not_checked: 2 } });
  assert.match(rows, /S5\s+LiteLLM\s+-\s+-.*no result$/m);
  const md = renderMarkdown({ ...report, notes: ['a | b'], failures: [] });
  assert.match(md, /^# OLP and LiteLLM on the same scenarios/);
  assert.match(md, /\| S1 \| OLP \| 0\.50 \|/);
  assert.match(md, /- a \| b/);
  assert.match(md, /\| Pins \| gateway 0-1, mock 2, load generator 3-4 \|/);
  assert.match(md, /Test CPU, 8 CPUs/);
  assert.ok(!/\| a \| b \|/.test(md.split('## Targets')[1].split('## Notes')[0]));
});

test('the command writes compare.json and compare.md, and exits non-zero on a miss only when enforcing', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bench-compare-'));
  try {
    const script = new URL('./bench-compare.mjs', import.meta.url).pathname;
    const cli = (...args) => spawnSync('node', [script, '--olp', dir, '--scenarios', 'S1', ...args], { encoding: 'utf8' });
    assert.equal(cli().status, 2);
    mkdirSync(join(dir, 'litellm'));
    const g = good();
    g.litellm.S1 = result({ added: triple(0.1, 9, 20), rpc: 500 });
    writeFileSync(join(dir, 's1.json'), JSON.stringify(g.olp.S1));
    writeFileSync(join(dir, 'litellm', 's1.json'), JSON.stringify(g.litellm.S1));

    const lenient = cli();
    assert.equal(lenient.status, 0, lenient.stderr);
    assert.match(lenient.stdout, /^S1\s+OLP\s+0\.50/m);
    assert.match(lenient.stdout, /added-latency-below-litellm\s+missed/);
    const written = JSON.parse(readFileSync(join(dir, 'compare.json'), 'utf8'));
    assert.equal(written.scenarios[0].targets.find((t) => t.id === 'added-latency-below-litellm').status, 'missed');
    assert.match(readFileSync(join(dir, 'compare.md'), 'utf8'), /^# OLP and LiteLLM/);

    const strict = cli('--enforce');
    assert.equal(strict.status, 1);
    assert.match(strict.stdout, /FAIL S1: target added-latency-below-litellm missed/);

    writeFileSync(join(dir, 'litellm', 's1.json'), JSON.stringify(good().litellm.S1));
    assert.equal(cli('--enforce').status, 0);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

// What the shell script rejects before it starts anything.
test('the script refuses bad settings before it touches Docker', () => {
  const script = new URL('./bench-compare.sh', import.meta.url).pathname;
  const run = (env, args = []) =>
    spawnSync('bash', [script, ...args], { encoding: 'utf8', env: { PATH: process.env.PATH, HOME: process.env.HOME, ...env } });

  const s6 = run({ BENCH_SCENARIOS: 'S1,S6' });
  assert.equal(s6.status, 2);
  assert.match(s6.stderr, /S6 has no LiteLLM counterpart/);
  assert.match(run({ BENCH_SCENARIOS: 'S7' }).stderr, /scenarios from S1 to S5, got 'S7'/);
  assert.match(run({ BENCH_SCENARIOS: 'S1;rm' }).stderr, /got 'S1;rm'/);
  assert.match(run({ BENCH_SERVICES: 'cloud' }).stderr, /BENCH_SERVICES must be compose or external/);
  assert.match(run({ OLP_BENCH_ENFORCE: 'maybe' }).stderr, /OLP_BENCH_ENFORCE must be 0 or 1/);
  for (const env of [{ BENCH_SCENARIOS: 'S7' }, { BENCH_SERVICES: 'cloud' }, { OLP_BENCH_ENFORCE: 'maybe' }])
    assert.equal(run(env).status, 2);

  // Enforcement needs full scale and a pin for each process, and says so before
  // anything is started.
  const smoke = run({ OLP_BENCH_ENFORCE: '1', OLP_BENCH_SCALE: '0.02' });
  assert.equal(smoke.status, 2);
  assert.match(smoke.stderr, /judges targets stated at full rates/);
  const pins = { OLP_BENCH_GATEWAY_CPUS: '0-1', OLP_BENCH_MOCK_CPUS: '2', OLP_BENCH_LOADGEN_CPUS: '3' };
  for (const missing of Object.keys(pins)) {
    const rest = { ...pins, OLP_BENCH_ENFORCE: '1' };
    delete rest[missing];
    assert.match(run(rest).stderr, new RegExp(`OLP_BENCH_ENFORCE=1 needs ${missing}`));
  }
  // The pins must be apart: a mock or a generator on the gateway's CPU competes
  // with it. The shared CPUs are named, in order.
  const apart = (over) => run({ ...pins, ...over, OLP_BENCH_ENFORCE: '1' });
  assert.match(apart({ OLP_BENCH_MOCK_CPUS: '1' }).stderr, /the gateway and the mock upstream both run on CPU 1\n/);
  assert.match(apart({ OLP_BENCH_LOADGEN_CPUS: '0-1' }).stderr, /the gateway and the load generator both run on CPU 0,1\n/);
  assert.match(apart({ OLP_BENCH_GATEWAY_CPUS: '0-12', OLP_BENCH_MOCK_CPUS: '9-11', OLP_BENCH_LOADGEN_CPUS: '20' }).stderr, /the gateway and the mock upstream both run on CPU 9,10,11\n/);
  const crowded = apart({ OLP_BENCH_MOCK_CPUS: '3', OLP_BENCH_GATEWAY_CPUS: '3,4' });
  assert.equal(crowded.status, 2);
  assert.match(crowded.stderr, /the gateway and the mock upstream both run on CPU 3\n/);
  assert.match(crowded.stderr, /the mock upstream and the load generator both run on CPU 3\n/);
  assert.match(crowded.stderr, /each have CPUs of their own/);
  assert.match(run({ OLP_BENCH_DURATION: 'a minute' }).stderr, /must be durations in whole seconds/);

  const stray = run({}, ['extra']);
  assert.equal(stray.status, 2);
  assert.match(stray.stderr, /usage: scripts\/bench-compare\.sh/);
  const help = run({}, ['--help']);
  assert.equal(help.status, 0);
  assert.match(help.stderr, /OLP_BENCH_GATEWAY_CPUS/);
  assert.match(help.stderr, /S6 has no LiteLLM counterpart/);
});

// The properties of deploy/compose.bench.yaml that the comparison's fairness and
// safety rest on, read from its text, since there is no YAML parser to hand.
test('the LiteLLM compose file pins its image, shares the host network and stays on loopback', () => {
  const text = readFileSync(new URL('../deploy/compose.bench.yaml', import.meta.url), 'utf8');
  assert.match(text, /x-litellm-image: &litellm-image ghcr\.io\/berriai\/litellm:v\d+\.\d+\.\d+@sha256:[0-9a-f]{64}\n/);
  assert.equal(text.match(/^\s+image:/gm)?.length, 1, 'every container uses the one pinned image');
  assert.doesNotMatch(text, /^[^#\n]*(:latest|main-stable)/m, 'no moving tag');
  assert.doesNotMatch(text, /^\s+ports:/m, 'nothing is published through Docker, which would add a hop');
  const services = text.slice(text.indexOf('\nservices:'), text.indexOf('\nvolumes:'));
  const names = [...services.matchAll(/^  ([a-z-]+):\n/gm)].map((m) => m[1]);
  assert.deepEqual(names, ['litellm', 'litellm-ht', 'litellm-ht-collector', 'litellm-ht-metrics']);
  // Each shares the container settings: the host network and the gateway's CPUs.
  assert.equal(services.match(/<<: \*litellm-container/g)?.length, names.length);
  assert.match(text, /network_mode: host/);
  assert.match(text, /cpuset: \$\{LITELLM_CPUSET:\?/);
  assert.match(text, /nofile:\n\s+soft: 524288\n\s+hard: 524288/);
  // Everything that listens does so on loopback only.
  assert.equal(services.match(/--host=127\.0\.0\.1/g)?.length, 3);
  assert.match(text, /LITELLM_PGBOUNCER_PORT/);
  // No response caching.
  for (const file of ['production', 'high-throughput'])
    assert.doesNotMatch(readFileSync(new URL(`../deploy/litellm/${file}.yaml`, import.meta.url), 'utf8'), /^\s*cache:/m);
});
