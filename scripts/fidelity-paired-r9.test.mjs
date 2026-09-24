import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { homedir } from 'node:os';
import { dirname, join } from 'node:path';
import { test } from 'node:test';
import * as r8 from './fidelity-paired-r8.mjs';
import { analyzePaired, analyzeSealedB8, boundForGroup, cBoundForGroup, checkedSemanticReply, evaluateCaptureHostInterval,
  hostInterval, makeManifest, resolvePairedProvenance, semanticGateOrder, SemanticFailure, sourceOrder, sourceSchedule,
  validateOutputReservation, validateStrictProviderContracts, validateStrictRouteContracts, validateSubrun } from './fidelity-paired-r9.mjs';

const manifest = makeManifest(process.cwd());
const r8Manifest = JSON.parse(readFileSync('docs/evidence/fidelity-performance/source-paired-v2/manifest-r8.json', 'utf8'));
const B8 = JSON.parse(readFileSync('docs/evidence/fidelity-performance/source-paired-v2/baseline-r8.json', 'utf8'));
const hash = (value) => createHash('sha256').update(value).digest('hex');
const diagnostics = { loadavg: '0.1 0.1 0.1 1/100 100', cpu_pressure: 'some avg10=0.00 total=0', memory_pressure: 'some avg10=0.00 total=0' };
const hostSnapshot = (seconds, busyTicks, processTicks = {}) => ({ monotonic_ns: String(seconds * 1e9), cpu_line: 'cpu synthetic', host_busy_ticks: busyTicks,
  process_ticks: processTicks, runner_user_us: 0, runner_system_us: 0, loadavg: '0.1 0.1 0.1 1/100 100', load_1m: 0.1,
  cpu_pressure: 'some avg10=0.00 avg60=0.00 total=0', cpu_some_avg10_percent: 0 });
const preflightSamples = Array.from({ length: 13 }, (_, i) => hostSnapshot(i * 5, i * 20));
const preflightIntervals = Array.from({ length: 12 }, (_, i) => hostInterval(preflightSamples[i], preflightSamples[i + 1], 100));
const stableInterval = hostInterval(hostSnapshot(0, 0, { 123: 0 }), hostSnapshot(1, 100, { 123: 80 }), 100);

function rawSamples(metrics, workload) {
  const at = (prefix, index) => index < 32 ? metrics[`${prefix}-p50-us`] : index < 61 ? metrics[`${prefix}-p95-us`] : metrics[`${prefix}-p99-us`];
  const events = workload === 'native_stream_256' ? 256 : workload === 'native_slow_stream_64' ? 64 : 0;
  const eventBytes = workload === 'native_stream_256' ? 128 : workload === 'native_slow_stream_64' ? 16384 : 0;
  return Array.from({ length: 64 }, (_, i) => ({ latency_us: at('latency', i), first_event_us: events ? at('first-event', i) : 0,
    max_inter_event_gap_us: events ? at('max-inter-event-gap', i) : 0, events, text_bytes: events * eventBytes }));
}
function reply(name) {
  const workload = name.split('/')[0], rejected = workload === 'rejected_extension';
  const metrics = Object.fromEntries(Object.entries(manifest.comparisons[name]).map(([key, value]) => [key, value.old_B_median]));
  Object.assign(metrics, { dispatches: rejected ? 0 : 64, succeeded: rejected ? 0 : 64, rejected: rejected ? 64 : 0,
    'request-bytes': manifest.exact[name]['request-bytes'], 'content-events/op': manifest.exact[name]['content-events/op'] });
  return { name, iterations: 64, metrics, samples: rawSamples(metrics, workload), runtime: { num_gc_delta: 1,
    pause_total_ns_delta: 100, total_alloc_bytes_delta: 1000, heap_alloc_after_bytes: 1000, goroutines_after: 10,
    provider_effect_wait_ns: 100 }, scheduler: { latency_bucket_seconds: ['0', '0.0001', '+Inf'], latency_counts_delta: [1, 0] } };
}
function capture() {
  const cache = new Map(), get = (name) => {
    if (!cache.has(name)) cache.set(name, reply(name));
    return cache.get(name);
  };
  const blocks = sourceSchedule().map((block) => ({ ...block, diagnostics_before: diagnostics, diagnostics_after: diagnostics,
    host_cpu_interval: stableInterval, host_stability_decision: evaluateCaptureHostInterval(stableInterval, false),
    subruns: sourceOrder(block.group, block.variant, false, block.outer).map((arm) => ({ arm,
      reply: get(`${block.group}/${arm.split('/')[1]}`) })) }));
  return { schema: 'openllmproxy.dev/fidelity-source-paired-r9', mode: 'paired', status: 'complete',
    manifest_sha256: hash(JSON.stringify(manifest)), B_only_sha256: manifest.reused_B8.artifact_sha256, blocks,
    hardware: B8.hardware, toolchain: B8.toolchain, go_build_environment: B8.go_build_environment,
    runtime_environment: B8.runtime_environment, conditions: manifest.conditions,
    diagnostics_before: diagnostics, diagnostics_after: diagnostics, clock_ticks_per_second: 100,
    host_stability_rule: manifest.host_stability,
    host_preflight: { samples: structuredClone(preflightSamples), intervals: structuredClone(preflightIntervals), passed: true, failures: [] },
    semantic_gate: semanticGateOrder(true).map(({ arm, name }) => ({ arm, name, reply: get(name) })),
    B: B8.B, C: { revision: 'synthetic-C9', binary_sha256: 'synthetic-C9-binary' },
    C_route_contract: structuredClone(manifest.C_route_contract), C_provider_contract: structuredClone(manifest.C_provider_contract) };
}
function paired() { const result = capture(); return result; }

// The old baseline is read from committed bytes and is never mutated by these tests.
test('r9 pins original B8, failed C8, exact profiles, same schedule and new C-only bounds', () => {
  assert.equal(manifest.reused_B8.method_revision, '5ad7cda6f2864fc760b277f22cd47ce067c60f65');
  assert.equal(manifest.reused_B8.evidence_revision, '274e95a21cc78731bd32b64959cd083eeee0e227');
  assert.equal(manifest.reused_B8.artifact_sha256, 'f537f5a936eccdaaf123707689898c8fd88d197900c9fa351c53dc79a590678f');
  assert.equal(manifest.reused_B8.journal_sha256, 'b6941932994aaebe3a900de79f02abe051efdf9e670677b8405b373bb17858fb');
  assert.equal(manifest.previous_attempts.r8.artifact_sha256, '64f5a68119af5c00720c1e7eb740b78f84fc238199c3a94a3dad75e89ab052e6');
  assert.equal(manifest.previous_attempts.r8.journal_sha256, 'ddc2909c8443ab0230e2af1665b1216cf792ca430b527c62c7cbb670ec1845a5');
  assert.equal(manifest.previous_attempts.r8.completed_blocks, 0);
  assert.equal(manifest.seed, r8Manifest.seed);
  assert.equal(manifest.schedule_sha256, r8Manifest.schedule_sha256);
  assert.deepEqual(manifest.blocks_per_group, r8Manifest.blocks_per_group);
  assert.deepEqual(manifest.comparisons, r8Manifest.comparisons);
  assert.deepEqual(manifest.added_latency, r8Manifest.added_latency);
  assert.deepEqual([boundForGroup('native_unary/c1').upper_statistic, cBoundForGroup('native_unary/c1').upper_statistic], [23, 24]);
  assert.deepEqual([boundForGroup('native_slow_stream_64/c1').upper_statistic, cBoundForGroup('native_slow_stream_64/c1').upper_statistic], [78, 80]);
  assert.deepEqual([cBoundForGroup('native_unary/c1').lower_statistic, cBoundForGroup('native_slow_stream_64/c1').lower_statistic], [9, 49]);
  assert.match(manifest.sequential_rule, /0\.0486130202.*0\.05/);
  assert.equal(sourceSchedule().length, 480);
  assert.equal(hash(JSON.stringify(sourceSchedule())), manifest.schedule_sha256);
  assert.deepEqual(manifest.C_provider_contract, { native: { profile_id: 'compatible-chat', profile_revision: '1' },
    translated: { profile_id: 'anthropic-messages', profile_revision: '1' },
    rejected: { profile_id: 'anthropic-messages', profile_revision: '1' } });
});

test('original r8 comparator alone accepts sealed B8; wrong method or bytes fail', () => {
  assert.equal(r8.analyzeBaseline(B8, r8Manifest).passed, true);
  assert.equal(analyzeSealedB8(B8, manifest, r8Manifest).signed_B_sham.controls, 254);
  const wrong = { ...B8, B: { ...B8.B, revision: 'wrong' } };
  assert.throws(() => analyzeSealedB8(wrong, manifest, r8Manifest), /sealed B8 JSON/);
  const wrongManifest = { ...r8Manifest, seed: 'selected-after-B8' };
  assert.throws(() => analyzeSealedB8(B8, manifest, wrongManifest), /sealed B8 JSON|original r8 manifest/);
});

test('exact provider overlay fails closed on omission, wrong profile, revision and extra fields', () => {
  assert.deepEqual(validateStrictProviderContracts(structuredClone(manifest.C_provider_contract)), manifest.C_provider_contract);
  assert.deepEqual(validateStrictRouteContracts(structuredClone(manifest.C_route_contract)), manifest.C_route_contract);
  for (const category of ['native', 'translated', 'rejected']) {
    for (const mutate of [
      (v) => { delete v[category]; },
      (v) => { v[category].profile_id = 'other'; },
      (v) => { v[category].profile_revision = '2'; },
      (v) => { v[category].extra = true; }
    ]) {
      const candidate = structuredClone(manifest.C_provider_contract);
      mutate(candidate);
      assert.throws(() => validateStrictProviderContracts(candidate), /exact versioned/);
    }
  }
  assert.throws(() => validateStrictProviderContracts(null), /exact versioned/);
  const candidate = capture();
  candidate.C_provider_contract = null;
  assert.throws(() => analyzePaired(candidate, B8, manifest), /exact versioned/);
});

test('C9 is the only reservable output and any existing journal blocks retry', (t) => {
  const root = mkdtempSync(join(homedir(), 'olp-r9-reservation-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const output = join(root, manifest.paired_evidence_path);
  mkdirSync(dirname(output), { recursive: true });
  assert.equal(validateOutputReservation('paired', output, root, manifest).output, output);
  assert.throws(() => validateOutputReservation('B-only', join(root, manifest.B_only_evidence_path), root, manifest), /r9 reserves only/);
  assert.throws(() => validateOutputReservation('paired', join(root, 'alternate.json'), root, manifest), /single frozen evidence path/);
  writeFileSync(join(root, manifest.paired_journal_path), '{"reserved":true}\n');
  assert.throws(() => validateOutputReservation('paired', output, root, manifest), /already reserves/);
  const CLI = spawnSync(process.execPath, ['scripts/fidelity-paired-r9.mjs', 'record-B', 'out', 'manifest', 'binary', 'root'], { encoding: 'utf8' });
  assert.equal(CLI.status, 1);
  assert.match(CLI.stderr, /Usage: fidelity-paired-r9/);
});

test('recorded C8-failure revision locks C9 to prior evidence and product', () => {
  const revision = manifest.C8_failed_evidence_head;
  const path = join(process.cwd(), manifest.B_only_evidence_path);
  const p = resolvePairedProvenance(process.cwd(), path, revision, manifest);
  assert.equal(p.evidence_commit, manifest.reused_B8.evidence_revision);
  assert.equal(p.product_revision, manifest.C_product_revision);
  assert.equal(p.B_only_sha256, manifest.reused_B8.artifact_sha256);
  const wrongE = { ...manifest, reused_B8: { ...manifest.reused_B8, evidence_revision: manifest.C_product_revision } };
  assert.throws(() => resolvePairedProvenance(process.cwd(), path, revision, wrongE), /single sealed E8/);
  const wrongC8 = { ...manifest, C8_failed_evidence_head: '0'.repeat(40) };
  assert.throws(() => resolvePairedProvenance(process.cwd(), path, revision, wrongC8), /failed-C8 evidence/);
});

test('all-22 gate and fixed schedule reject missing semantic or host observations', () => {
  const missing = capture();
  missing.semantic_gate.shift();
  assert.throws(() => analyzePaired(missing, B8, manifest), /all-22 semantic gate/);
  const noisy = capture();
  noisy.host_preflight.samples[0].cpu_some_avg10_percent = 5;
  noisy.host_preflight.intervals[0] = hostInterval(noisy.host_preflight.samples[0], noisy.host_preflight.samples[1], 100);
  assert.throws(() => analyzePaired(noisy, B8, manifest), /host preflight limit failed/);
  const short = capture();
  short.blocks.pop();
  assert.throws(() => analyzePaired(short, B8, manifest), /block schedule or count changed/);
});

test('synthetic paired capture passes with sealed B8 and exact C9 profile contract', () => {
  const result = analyzePaired(paired(), B8, manifest);
  assert.equal(result.status, 'passed');
  assert.equal(result.B_only.passed, true);
  assert.equal(result.paired_B_signed_sham.controls, 254);
  assert.equal(result.C_gateway_absolute_old_L.metrics, 120);
  assert.equal(result.C_gateway_absolute_old_L.passed, true);
  assert.equal(Object.values(result.results).flatMap(Object.keys).length, 224);
  assert.equal(Object.values(result.added_latency).flatMap(Object.keys).length, 30);
  assert.equal(result.count.blocks, 480);
});

test('C9 d24 rejects nine high ordinary blocks that old d23 would accept', () => {
  const candidate = capture(), name = 'native_unary/c1/gateway', group = 'native_unary/c1';
  const { margin, old_B_median: B } = manifest.comparisons[name]['ns/op'];
  const blocks = candidate.blocks.filter((block) => block.group === group);
  for (let i = 0; i < 9; i++) for (const run of blocks[i].subruns.filter((entry) => entry.arm === 'C/gateway')) {
    run.reply = structuredClone(run.reply); run.reply.metrics['ns/op'] = B + margin + 1;
  }
  const result = analyzePaired(candidate, B8, manifest);
  assert.equal(result.status, 'failed');
  assert.equal(result.results[name]['ns/op'].upper, margin + 1);
  assert.equal(result.C_gateway_absolute_old_L.passed, true);
});

test('C9 slow/c1 d80 rejects 49 high blocks that old d78 would accept', () => {
  const candidate = capture(), group = 'native_slow_stream_64/c1', name = `${group}/gateway`;
  const { margin, old_B_median: B } = manifest.comparisons[name]['ns/op'];
  const blocks = candidate.blocks.filter((block) => block.group === group);
  for (let i = 0; i < 49; i++) for (const run of blocks[i].subruns.filter((entry) => entry.arm === 'C/gateway')) {
    run.reply = structuredClone(run.reply); run.reply.metrics['ns/op'] = B + margin + 1;
  }
  const result = analyzePaired(candidate, B8, manifest);
  assert.equal(result.status, 'failed');
  assert.equal(result.results[name]['ns/op'].upper, margin + 1);
  assert.equal(result.C_gateway_absolute_old_L.passed, true);
});

test('C9 relay d49 lower control rejects signed cancellation while d51 would pass', () => {
  const candidate = capture(), group = 'native_slow_stream_64/c1', name = `${group}/relay`;
  const { margin, old_B_median: B } = manifest.comparisons[name]['ns/op'];
  assert.ok(B > margin + 1);
  const blocks = candidate.blocks.filter((block) => block.group === group);
  for (let i = 0; i < 49; i++) for (const run of blocks[i].subruns.filter((entry) => entry.arm === 'C/relay')) {
    run.reply = structuredClone(run.reply); run.reply.metrics['ns/op'] = B - margin - 1;
  }
  const result = analyzePaired(candidate, B8, manifest);
  assert.equal(result.status, 'inconclusive');
  assert.equal(result.results[name]['ns/op'].lower, -margin - 1);
  assert.equal(result.results[name]['ns/op'].control_passed, false);
});

test('paired B sham keeps original r8 bounds independently of tightened C9 bound', () => {
  const candidate = capture(), group = 'native_slow_stream_64/c1', name = `${group}/relay`;
  const { margin, old_B_median: B } = manifest.comparisons[name]['ns/op'];
  for (const block of candidate.blocks.filter((entry) => entry.group === group)) {
    const pair = block.subruns.filter((entry) => entry.arm === 'B/relay');
    pair[1].reply = structuredClone(pair[1].reply);
    pair[1].reply.metrics['ns/op'] = B + (block.variant === 'ABBA' ? 1 : -1) * (margin + 1);
  }
  const result = analyzePaired(candidate, B8, manifest);
  assert.equal(result.status, 'inconclusive');
  assert.equal(result.B_only.passed, true);
  assert.equal(result.paired_B_signed_sham.passed, false);
  assert.equal(result.paired_B_signed_sham.paths[name]['ns/op'].upper, margin + 1);
  assert.equal(result.C_gateway_absolute_old_L.passed, true);
});

test('one gateway C old-L excess fails even when tightened C9 paired primary passes', () => {
  const candidate = capture(), group = 'native_unary/c1', name = `${group}/gateway`;
  const L = manifest.comparisons[name]['ns/op'].old_limit;
  const blocks = candidate.blocks.filter((entry) => entry.group === group);
  for (let i = 0; i < blocks.length; i++) {
    const runs = blocks[i].subruns.filter((entry) => entry.arm === 'C/gateway');
    for (const run of runs) run.reply = structuredClone(run.reply);
    runs[0].reply.metrics['ns/op'] = L + 1;
    runs[1].reply.metrics['ns/op'] = i === 0 ? L + 1 : 1;
  }
  const result = analyzePaired(candidate, B8, manifest);
  assert.equal(result.status, 'failed');
  assert.equal(result.results[name]['ns/op'].passed, true);
  assert.equal(result.C_gateway_absolute_old_L.passed, false);
  assert.equal(result.C_gateway_absolute_old_L.metrics, 120);
});

test('structured semantic failure stays RED without candidate timing', () => {
  const name = 'native_unary/c1/gateway';
  const failure = { stage: 'benchmark_setup_or_sample_count', benchmark_iterations: 0, reply_iterations: 0, reply_samples: 0,
    dispatched: 0, completed: 0, expected_effects: 0 };
  assert.throws(() => checkedSemanticReply({ name, error: 'oracle, effect count or fixed sample count failed', failure }, 'C/gateway', name, manifest), (error) => {
    assert.ok(error instanceof SemanticFailure);
    assert.equal(error.detail.stage, 'benchmark_setup_or_sample_count');
    assert.equal(error.detail.adapter_failure.benchmark_iterations, 0);
    return true;
  });
  assert.equal(validateSubrun(reply(name), name, manifest), undefined);
});
