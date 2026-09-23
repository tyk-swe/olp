import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { test } from 'node:test';
import { analyzeBaseline, analyzePaired, analyzeSignedBSham, binomialTailAtLeast, blockCount, boundForGroup, signedBValues, signedAddedBValues, evaluateCaptureHostInterval, evaluateHostPreflight, failureDisposition, hostInterval, makeManifest, resolvePairedProvenance, semanticGateOrder, checkedSemanticReply, SemanticFailure, sourceOrder, sourceSchedule, validateOutputReservation, validateStrictRouteContracts, validateSubrun, validateUntrackedBReservation, verifyPairedProvenance } from './fidelity-paired-r8.mjs';

const manifest = makeManifest(process.cwd());
const digest = (value) => createHash('sha256').update(value).digest('hex');
const diagnostics = { loadavg: '0.1 0.1 0.1 1/100 100', cpu_pressure: 'some avg10=0.00 total=0', memory_pressure: 'some avg10=0.00 total=0' };
const metadata = { revision: 'B-measurement-only', binary_sha256: 'same-B-binary' };
const hostSnapshot = (seconds, busyTicks, processTicks = {}) => ({
  monotonic_ns: String(seconds * 1e9), cpu_line: 'cpu synthetic', host_busy_ticks: busyTicks,
  process_ticks: processTicks, runner_user_us: 0, runner_system_us: 0,
  loadavg: '0.1 0.1 0.1 1/100 100', load_1m: 0.1,
  cpu_pressure: 'some avg10=0.00 avg60=0.00 total=0', cpu_some_avg10_percent: 0
});
const preflightSamples = Array.from({ length: 13 }, (_, index) => hostSnapshot(index * 5, index * 20));
const preflightIntervals = Array.from({ length: 12 }, (_, index) => hostInterval(preflightSamples[index], preflightSamples[index + 1], 100));
const stableBlockInterval = hostInterval(hostSnapshot(0, 0, { 123: 0 }), hostSnapshot(1, 100, { 123: 80 }), 100);

function syntheticJournal(capture) {
  const header = { schema: capture.schema, mode: capture.mode, status: 'in_progress',
    manifest_sha256: capture.manifest_sha256, B: capture.B, C: capture.C, provenance: capture.provenance };
  return JSON.stringify({ header }) + '\n' + JSON.stringify({ semantic_gate: capture.semantic_gate || [] }) + '\n' + JSON.stringify({ completion: { status: 'complete', block_count: capture.blocks.length } }) + '\n';
}

function gitFixture(t) {
  const root = mkdtempSync(join(tmpdir(), 'olp-source-v2-history-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const git = (...args) => execFileSync('git', args, { cwd: root, encoding: 'utf8', env: {
    ...process.env, GIT_AUTHOR_NAME: 'Benchmark Test', GIT_AUTHOR_EMAIL: 'benchmark@example.test',
    GIT_COMMITTER_NAME: 'Benchmark Test', GIT_COMMITTER_EMAIL: 'benchmark@example.test'
  } }).trim();
  git('init', '-q', '--initial-branch=main');
  git('config', 'commit.gpgsign', 'false');
  const put = (path, content) => { const full = join(root, path); mkdirSync(dirname(full), { recursive: true }); writeFileSync(full, content); return full; };
  const commit = (message) => { git('add', '.'); git('commit', '-q', '-m', message); return git('rev-parse', 'HEAD'); };
  const source = 'internal/gateway/dispatch.go';
  put(source, 'package gateway\nfunc dispatch() int { return 1 }\n');
  const initial = commit('Initial product');
  const baselinePath = join(root, manifest.B_only_evidence_path);
  const journalPath = join(root, manifest.B_only_journal_path);
  const baseline = { schema: 'openllmproxy.dev/fidelity-source-paired-r8', mode: 'B-only', status: 'complete',
    manifest_sha256: digest(JSON.stringify(manifest)), B: { revision: initial, binary_sha256: 'same-B-binary' }, C: null, provenance: null, semantic_gate: [], blocks: [] };
  const journal = syntheticJournal(baseline);
  baseline.journal_sha256 = digest(journal);
  const addBaseline = () => {
    put(manifest.B_only_evidence_path, JSON.stringify(baseline, null, 2) + '\n');
    put(manifest.B_only_journal_path, journal);
    return commit('Record B-only JSON and journal together');
  };
  return { root, git, put, commit, source, initial, manifest: { ...manifest, C_product_revision: initial }, baselinePath, journalPath, journal, baseline, addBaseline };
}

function rawSamples(metrics, stream) {
  const at = (prefix, index) => index < 32 ? metrics[`${prefix}-p50-us`] : index < 61 ? metrics[`${prefix}-p95-us`] : metrics[`${prefix}-p99-us`];
  const events = stream === 'native_stream_256' ? 256 : stream === 'native_slow_stream_64' ? 64 : 0;
  const eventBytes = stream === 'native_stream_256' ? 128 : stream === 'native_slow_stream_64' ? 16384 : 0;
  return Array.from({ length: 64 }, (_, index) => ({ latency_us: at('latency', index),
    first_event_us: events ? at('first-event', index) : 0,
    max_inter_event_gap_us: events ? at('max-inter-event-gap', index) : 0,
    events, text_bytes: events * eventBytes }));
}
function reply(name) {
  const workload = name.split('/')[0];
  const rejected = workload === 'rejected_extension';
  const metrics = Object.fromEntries(Object.entries(manifest.comparisons[name]).map(([metric, old]) => [metric, old.old_B_median]));
  Object.assign(metrics, { dispatches: rejected ? 0 : 64, succeeded: rejected ? 0 : 64, rejected: rejected ? 64 : 0,
    'request-bytes': manifest.exact[name]['request-bytes'], 'content-events/op': manifest.exact[name]['content-events/op'] });
  return { name, iterations: 64, metrics, samples: rawSamples(metrics, workload), runtime: {
    num_gc_delta: 1, pause_total_ns_delta: 100, total_alloc_bytes_delta: 1000,
    heap_alloc_after_bytes: 1000, goroutines_after: 10, provider_effect_wait_ns: 100 },
    scheduler: { latency_bucket_seconds: ['0', '0.0001', '+Inf'], latency_counts_delta: [1, 0] } };
}
function capture(baselineOnly) {
  const cache = new Map();
  const get = (name) => {
    if (!cache.has(name)) cache.set(name, reply(name));
    return cache.get(name);
  };
  const blocks = sourceSchedule().map((block) => ({ ...block, diagnostics_before: diagnostics, diagnostics_after: diagnostics,
    host_cpu_interval: stableBlockInterval, host_stability_decision: evaluateCaptureHostInterval(stableBlockInterval, false),
    subruns: sourceOrder(block.group, block.variant, baselineOnly, block.outer).map((arm) => ({ arm, reply: get(`${block.group}/${arm.split('/')[1]}`) })) }));
  return { schema: 'openllmproxy.dev/fidelity-source-paired-r8', mode: baselineOnly ? 'B-only' : 'paired', status: 'complete',
    manifest_sha256: digest(JSON.stringify(manifest)), B_only_sha256: null, blocks,
    hardware: manifest.old_hardware, toolchain: manifest.old_toolchain, go_build_environment: manifest.old_go_build_environment,
    runtime_environment: manifest.old_runtime_environment, conditions: manifest.conditions, diagnostics_before: diagnostics, diagnostics_after: diagnostics,
    clock_ticks_per_second: 100, host_stability_rule: manifest.host_stability,
    host_preflight: { samples: preflightSamples, intervals: preflightIntervals, passed: true, failures: [] },
    semantic_gate: semanticGateOrder(!baselineOnly).map(({ arm, name }) => ({ arm, name, reply: get(name) })),
    B: metadata, C: baselineOnly ? null : { revision: 'locked-C', binary_sha256: 'C-binary' },
    C_route_contract: baselineOnly ? null : structuredClone(manifest.C_route_contract) };
}

test('manifest derives exactly 224 path and 30 added-latency margins from immutable B data', () => {
  assert.equal(Object.keys(manifest.comparisons).length, 22);
  assert.equal(Object.values(manifest.comparisons).flatMap(Object.keys).length, 224);
  assert.equal(Object.values(manifest.added_latency).flatMap(Object.keys).length, 30);
  assert.equal(manifest.B_product_revision, '8e52f775df815c3a1a25d75064c3ffd540fb07f5');
  assert.equal(manifest.manifest_revision, 'signed-sham-one-shot-r8');
  assert.equal(manifest.B_only_evidence_path, 'docs/evidence/fidelity-performance/source-paired-v2/baseline-r8.json');
  assert.equal(manifest.B_only_journal_path, 'docs/evidence/fidelity-performance/source-paired-v2/baseline-r8.json.journal.jsonl');
  assert.equal(manifest.paired_evidence_path, 'docs/evidence/fidelity-performance/source-paired-v2/paired-r8.json');
  assert.equal(manifest.previous_invalid_attempts.r5.artifact_sha256, '68f7bb88ce70bcc4b179179749577faf085838c0fd2696c7e3b2f9ba3b239b06');
  assert.equal(manifest.previous_invalid_attempts.r5.journal_sha256, '5127ea75ec3688cbadf433eae8d021ce0d473b269f571fe03face683f7dd8295');
  assert.equal(manifest.previous_invalid_attempts.r6.artifact_sha256, '33f5efb6dc681b2be570e5e2352e53ea35f01d8adfbaff836a05083a8bf1f023');
  assert.equal(manifest.previous_invalid_attempts.r6.journal_sha256, '25f76f751721fa4252d4969e5c0583ea354a1deecd34eeb13e0953552c41bdde');
  assert.equal(manifest.previous_invalid_attempts.r6.completed_blocks, 0);
  assert.equal(manifest.previous_invalid_attempts.r7.artifact_sha256, '73d4701f9684789296bf51d52e54a0fbe7fd759469cb7a36e9f8995fa2687b00');
  assert.equal(manifest.previous_invalid_attempts.r7.journal_sha256, '3d10f864608e2b318fabaa83ff42037dd25d82940526d5abe38c6272ed0f5308');
  assert.equal(manifest.previous_invalid_attempts.r7.C_observations, 0);
  assert.equal(manifest.previous_invalid_attempts.r7.B_only_passed, false);
  assert.equal(manifest.C_product_revision, 'bc325da50c575803c771531dc4e3aab1ac203a53');
  assert.deepEqual([manifest.bound.ordinary.lower_order_statistic, manifest.bound.ordinary.upper_order_statistic], [10, 23]);
  assert.deepEqual([manifest.bound.slow_c1.lower_order_statistic, manifest.bound.slow_c1.upper_order_statistic], [51, 78]);
  assert.equal(manifest.bound.slow_c1.upper_coverage, 0.9916646328651513);
  assert.match(manifest.sequential_rule, /0\.0250512299.*0\.0451128369.*0\.05/);
  assert.equal(manifest.power_estimate.B7_successes, 22);
  assert.equal(manifest.power_estimate.R8_blocks, 128);
  assert.ok(Math.abs(manifest.power_estimate.illustrative_one_side_probability - 0.9757054433) < 1e-8);
  assert.equal(binomialTailAtLeast(128, 78, 22 / 32), manifest.power_estimate.illustrative_one_side_probability);
  assert.deepEqual(manifest.C_route_contract, { native: { fidelity: { mode: 'strict' } }, translated: { fidelity: { mode: 'strict' } }, rejected: { fidelity: { mode: 'strict' } } });
  for (const metrics of Object.values(manifest.comparisons)) for (const value of Object.values(metrics)) {
    assert.equal(value.margin, value.old_limit - value.old_B_median);
    assert.ok(value.margin > 0);
  }
});

test('fixed all-22 semantic gate starts with the adverse r6 C relay case and records every B/C name', () => {
  const B = semanticGateOrder(false), paired = semanticGateOrder(true);
  assert.equal(B.length, 22);
  assert.equal(paired.length, 44);
  assert.deepEqual(paired[0], { arm: 'C/relay', name: 'native_unary/c1/relay' });
  assert.deepEqual(paired[1], { arm: 'B/relay', name: 'native_unary/c1/relay' });
  assert.equal(new Set(B.map((entry) => entry.name)).size, 22);
  assert.equal(new Set(paired.map((entry) => `${entry.arm}:${entry.name}`)).size, 44);
  assert.equal(digest(JSON.stringify(B)), manifest.semantic_gate_order_sha256.B_only);
  assert.equal(digest(JSON.stringify(paired)), manifest.semantic_gate_order_sha256.paired);
  const baseline = capture(true), candidate = capture(false);
  candidate.B_only_sha256 = digest(JSON.stringify(baseline, null, 2) + '\n');
  candidate.semantic_gate.shift();
  assert.throws(() => analyzePaired(candidate, baseline, manifest), /complete all-22 semantic gate/);
});

test('signed two-B sham flips ABBA/BAAB order for paths and added latency', () => {
  const makeBlock = (variant) => ({ group: 'native_slow_stream_64/c1', variant, subruns: [
    { arm: 'B/relay', reply: { metrics: { 'ns/op': 10, 'latency-p50-us': 1 } } },
    { arm: 'B/relay', reply: { metrics: { 'ns/op': 15, 'latency-p50-us': 6 } } },
    { arm: 'B/gateway', reply: { metrics: { 'latency-p50-us': 5 } } },
    { arm: 'B/gateway', reply: { metrics: { 'latency-p50-us': 15 } } }
  ] });
  assert.deepEqual(signedBValues([makeBlock('ABBA'), makeBlock('BAAB')], 'relay', 'ns/op'), [5, -5]);
  assert.deepEqual(signedAddedBValues([makeBlock('ABBA'), makeBlock('BAAB')], 'latency-p50-us'), [5, -5]);
});

test('B-only old-L miss is descriptive while signed sham remains formal', () => {
  const baseline = capture(true);
  const name = 'native_unary/c1/gateway', limit = manifest.comparisons[name]['ns/op'].old_limit;
  for (const block of baseline.blocks.filter((item) => item.group === 'native_unary/c1')) {
    for (const run of block.subruns.filter((item) => item.arm === 'B/gateway')) {
      run.reply = structuredClone(run.reply);
      run.reply.metrics['ns/op'] = limit + 1;
    }
  }
  const result = analyzeBaseline(baseline, manifest);
  assert.equal(result.passed, true);
  assert.equal(result.descriptive_absolute_B_vs_v1.metrics[name]['ns/op'].within_old_limit, false);
  assert.equal(result.signed_B_sham.passed, true);
});

test('n128 d78 B-only sham is formal and rejects 51 signed high blocks', () => {
  const baseline = capture(true), group = 'native_slow_stream_64/c1', name = `${group}/relay`;
  const { margin, old_B_median: B } = manifest.comparisons[name]['ns/op'];
  assert.ok(B > margin + 1);
  const blocks = baseline.blocks.filter((item) => item.group === group);
  for (let index = 0; index < 51; index++) {
    const block = blocks[index], pair = block.subruns.filter((item) => item.arm === 'B/relay');
    pair[1].reply = structuredClone(pair[1].reply);
    pair[1].reply.metrics['ns/op'] = B + (block.variant === 'ABBA' ? 1 : -1) * (margin + 1);
  }
  const result = analyzeBaseline(baseline, manifest);
  assert.equal(result.passed, false);
  const sham = result.signed_B_sham.paths[name]['ns/op'];
  assert.equal(sham.upper, margin + 1);
  assert.equal([...sham.values].sort((a, b) => a - b)[76], 0); // d_(77) still passes.
});

test('paired B sham is formal even when C-minus-B and C gateway old-L pass', () => {
  const baseline = capture(true), candidate = capture(false);
  candidate.B_only_sha256 = digest(JSON.stringify(baseline, null, 2) + '\n');
  const group = 'native_slow_stream_64/c1', name = `${group}/relay`;
  const { margin, old_B_median: B } = manifest.comparisons[name]['ns/op'];
  for (const block of candidate.blocks.filter((item) => item.group === group)) {
    const pair = block.subruns.filter((item) => item.arm === 'B/relay');
    pair[1].reply = structuredClone(pair[1].reply);
    pair[1].reply.metrics['ns/op'] = B + (block.variant === 'ABBA' ? 1 : -1) * (margin + 1);
  }
  const result = analyzePaired(candidate, baseline, manifest);
  assert.equal(result.status, 'inconclusive');
  assert.equal(result.B_only.passed, true);
  assert.equal(result.paired_B_signed_sham.passed, false);
  assert.equal(result.C_gateway_absolute_old_L.passed, true);
  assert.equal(result.results[name]['ns/op'].passed, true);
});

test('added-latency signed B sham can fail while both B path shams pass', () => {
  const baseline = capture(true), group = 'native_slow_stream_64/c1', metric = 'latency-p50-us';
  const margin = manifest.added_latency[group][metric].margin;
  const blocks = baseline.blocks.filter((item) => item.group === group);
  for (let index = 0; index < 51; index++) {
    const block = blocks[index], sign = block.variant === 'ABBA' ? 1 : -1;
    const relay = block.subruns.filter((item) => item.arm === 'B/relay');
    const gateway = block.subruns.filter((item) => item.arm === 'B/gateway');
    relay[1].reply = structuredClone(relay[1].reply);
    gateway[1].reply = structuredClone(gateway[1].reply);
    relay[1].reply.metrics[metric] -= sign * 0.6 * margin;
    gateway[1].reply.metrics[metric] += sign * 0.6 * margin;
  }
  const sham = analyzeSignedBSham(baseline, manifest);
  assert.equal(sham.paths[`${group}/relay`][metric].passed, true);
  assert.equal(sham.paths[`${group}/gateway`][metric].passed, true);
  assert.equal(sham.added_latency[group][metric].passed, false);
  assert.equal(sham.passed, false);
});

test('forced adapter failure is RED with preserved stage, status, counters and redacted diagnostics', () => {
  const name = 'native_unary/c1/relay';
  const failure = { stage: 'provider_effects', dispatched: 64, completed: 63, expected_effects: 64,
    benchmark_iterations: 64, reply_iterations: 0, reply_samples: 0, http_status: 502 };
  assert.throws(() => checkedSemanticReply({ name, error: 'oracle, effect count or fixed sample count failed', failure }, 'C/relay', name, manifest,
    { stdout: 'benchmark-provider-key', stderr: 'benchmark-client-key' }), (error) => {
    assert.ok(error instanceof SemanticFailure);
    assert.equal(error.detail.stage, 'provider_effects');
    assert.equal(error.detail.adapter_failure.completed, 63);
    assert.equal(error.detail.adapter_failure.http_status, 502);
    assert.equal(error.detail.stdout_tail, '<REDACTED>');
    assert.equal(error.detail.stderr_tail, '<REDACTED>');
    return true;
  });
  assert.equal(checkedSemanticReply(reply(name), 'C/relay', name, manifest).iterations, 64);
  assert.equal(failureDisposition(new SemanticFailure('C/relay', name, { stage: 'provider_effects' }), true), 'failed');
  assert.equal(failureDisposition(new SemanticFailure('B/relay', name, { stage: 'provider_effects' }), false), 'invalid');
  assert.equal(failureDisposition(new Error('host stability'), true), 'invalid');
  const forgedComplete = reply(name);
  forgedComplete.error = 'hidden failure';
  assert.throws(() => validateSubrun(forgedComplete, name, manifest), /adapter reported a semantic failure/);
});

test('r7 d23 rejects a candidate that old d22 would accept', () => {
  const baseline = capture(true), candidate = capture(false);
  candidate.B_only_sha256 = digest(JSON.stringify(baseline, null, 2) + '\n');
  const name = 'native_unary/c1/gateway';
  const { margin, old_B_median: B } = manifest.comparisons[name]['ns/op'];
  const blocks = candidate.blocks.filter((entry) => entry.group === 'native_unary/c1');
  for (let index = 22; index < blocks.length; index++) {
    for (const run of blocks[index].subruns.filter((entry) => entry.arm === 'C/gateway')) {
      run.reply = structuredClone(run.reply);
      run.reply.metrics['ns/op'] = B + margin + 1;
    }
  }
  const result = analyzePaired(candidate, baseline, manifest);
  assert.equal(result.status, 'failed');
  assert.equal(result.results[name]['ns/op'].upper, margin + 1);
  assert.equal(result.results[name]['ns/op'].passed, false);
});

test('r8 slow/c1 d51 rejects a relay lower control that d52 would accept', () => {
  const baseline = capture(true), candidate = capture(false);
  candidate.B_only_sha256 = digest(JSON.stringify(baseline, null, 2) + '\n');
  const name = 'native_slow_stream_64/c1/relay';
  const { margin, old_B_median: B } = manifest.comparisons[name]['ns/op'];
  assert.ok(B > margin + 1);
  const blocks = candidate.blocks.filter((entry) => entry.group === 'native_slow_stream_64/c1');
  for (let index = 0; index < 51; index++) {
    for (const run of blocks[index].subruns.filter((entry) => entry.arm === 'C/relay')) {
      run.reply = structuredClone(run.reply);
      run.reply.metrics['ns/op'] = B - margin - 1;
    }
  }
  const result = analyzePaired(candidate, baseline, manifest);
  assert.equal(result.status, 'inconclusive');
  assert.equal(result.results[name]['ns/op'].lower, -margin - 1);
  assert.equal(result.results[name]['ns/op'].control_passed, false);
});

test('one C gateway old-L excess fails even when paired C-minus-B passes', () => {
  const baseline = capture(true), candidate = capture(false);
  candidate.B_only_sha256 = digest(JSON.stringify(baseline, null, 2) + '\n');
  const name = 'native_unary/c1/gateway';
  const L = manifest.comparisons[name]['ns/op'].old_limit;
  const blocks = candidate.blocks.filter((entry) => entry.group === 'native_unary/c1');
  for (let index = 0; index < blocks.length; index++) {
    const runs = blocks[index].subruns.filter((entry) => entry.arm === 'C/gateway');
    for (const run of runs) run.reply = structuredClone(run.reply);
    runs[0].reply.metrics['ns/op'] = L + 1;
    runs[1].reply.metrics['ns/op'] = index === 0 ? L + 1 : 1;
  }
  const result = analyzePaired(candidate, baseline, manifest);
  assert.equal(result.status, 'failed');
  assert.equal(result.results[name]['ns/op'].passed, true);
  assert.equal(result.descriptive_absolute_C_vs_v1.metrics[name]['ns/op'].within_old_v1_limit, false);
  assert.ok(result.descriptive_absolute_C_vs_v1.above_old_limit >= 1);
  assert.equal(result.descriptive_absolute_C_vs_v1.metrics[name]['ns/op'].hard_candidate_gate, true);
  assert.equal(result.C_gateway_absolute_old_L.passed, false);
});

test('r8 host preflight and per-block external CPU rules fail without trimming', () => {
  assert.deepEqual(evaluateHostPreflight(preflightSamples, preflightIntervals), { passed: true, failures: [] });
  const loaded = structuredClone(preflightSamples);
  loaded[6].load_1m = 2;
  assert.equal(evaluateHostPreflight(loaded, preflightIntervals).passed, false);
  const pressured = structuredClone(preflightSamples);
  pressured[5].cpu_some_avg10_percent = 5;
  assert.equal(evaluateHostPreflight(pressured, preflightIntervals).passed, false);
  const noisy = structuredClone(preflightIntervals);
  noisy[0].external_busy_cores = 1;
  assert.equal(evaluateHostPreflight(preflightSamples, noisy).passed, false);
  assert.equal(evaluateCaptureHostInterval({ external_busy_cores: 2 }, false).invalid, true);
  assert.equal(evaluateCaptureHostInterval({ external_busy_cores: 0.75 }, false).invalid, false);
  assert.equal(evaluateCaptureHostInterval({ external_busy_cores: 0.75 }, true).invalid, true);
  assert.equal(evaluateCaptureHostInterval({ external_busy_cores: 0.74 }, true).invalid, false);
  assert.throws(() => hostInterval(hostSnapshot(0, 0, { 1: 0 }), hostSnapshot(1, 100), 100), /process identity changed/);
  assert.throws(() => hostInterval(hostSnapshot(0, 100), hostSnapshot(1, 99), 100), /regressed host CPU counters/);
});

test('fresh E8 evidence precedes locked C8 without inventing a product change', (t) => {
  const fixture = gitFixture(t);
  const evidence = fixture.addBaseline();
  fixture.put('internal/gateway/measure_test.go', 'package gateway\nfunc TestMeasure() {}\n');
  const C = fixture.commit('Add measurement-only adapter');
  const provenance = resolvePairedProvenance(fixture.root, fixture.baselinePath, C, fixture.manifest);
  assert.equal(provenance.evidence_commit, evidence);
  assert.equal(provenance.product_revision, fixture.initial);
  assert.equal(provenance.B_journal_sha256, digest(fixture.journal));
  fixture.put('docs/after.md', 'Later qualification note\n');
  const docs = fixture.commit('Add later docs');
  assert.equal(resolvePairedProvenance(fixture.root, fixture.baselinePath, docs, fixture.manifest).product_revision, fixture.initial);
  const captured = { schema: fixture.baseline.schema, mode: 'paired', status: 'complete', manifest_sha256: fixture.baseline.manifest_sha256,
    B: fixture.baseline.B, C: { revision: C }, provenance, semantic_gate: [], blocks: [],
    B_only_sha256: digest(JSON.stringify(fixture.baseline, null, 2) + '\n') };
  const pairedJournal = syntheticJournal(captured);
  fixture.put(manifest.paired_journal_path, pairedJournal);
  captured.journal_sha256 = digest(pairedJournal);
  assert.deepEqual(verifyPairedProvenance(captured, fixture.baseline, fixture.manifest, fixture.root, fixture.baselinePath), provenance);
  captured.B = { ...captured.B, revision: evidence };
  const wrongBJournal = syntheticJournal(captured);
  fixture.put(manifest.paired_journal_path, wrongBJournal);
  captured.journal_sha256 = digest(wrongBJournal);
  assert.throws(() => verifyPairedProvenance(captured, fixture.baseline, fixture.manifest, fixture.root, fixture.baselinePath), /measured historical method revision/);
});

test('locked C8 rejects any production Go blob drift, including unrelated packages', (t) => {
  const fixture = gitFixture(t);
  fixture.addBaseline();
  fixture.put(fixture.source, 'package gateway\nfunc dispatch() int { return 2 }\n');
  const changed = fixture.commit('Change gateway product');
  assert.throws(() => resolvePairedProvenance(fixture.root, fixture.baselinePath, changed, fixture.manifest), /production Go blobs differ/);
  fixture.put(fixture.source, 'package gateway\nfunc dispatch() int { return 1 }\n');
  fixture.put('internal/other/new.go', 'package other\n');
  const added = fixture.commit('Restore gateway but add other product');
  assert.throws(() => resolvePairedProvenance(fixture.root, fixture.baselinePath, added, fixture.manifest), /production Go blobs differ/);
});

test('same commit cannot create E8 and lock C8', (t) => {
  const fixture = gitFixture(t);
  const same = fixture.addBaseline();
  assert.throws(() => resolvePairedProvenance(fixture.root, fixture.baselinePath, same, fixture.manifest), /strict ancestor/);
});

test('missing, separately committed, or later edited E8 journal fails', (t) => {
  const missing = gitFixture(t);
  missing.put(manifest.B_only_evidence_path, JSON.stringify(missing.baseline, null, 2) + '\n');
  const missingC = missing.commit('Record JSON only');
  assert.throws(() => resolvePairedProvenance(missing.root, missing.baselinePath, missingC, missing.manifest), /journal is missing/);

  const separate = gitFixture(t);
  separate.put(manifest.B_only_evidence_path, JSON.stringify(separate.baseline, null, 2) + '\n');
  separate.commit('Record JSON first');
  separate.put(manifest.B_only_journal_path, separate.journal);
  separate.commit('Record journal later');
  separate.put('docs/after.md', 'Measurement note\n');
  const separateC = separate.commit('Lock C');
  assert.throws(() => resolvePairedProvenance(separate.root, separate.baselinePath, separateC, separate.manifest), /one shared creation commit/);

  const edited = gitFixture(t);
  edited.addBaseline();
  edited.put(manifest.B_only_journal_path, '{"reservation":"changed"}\n');
  edited.put('docs/after.md', 'Measurement note\n');
  const editedC = edited.commit('Edit journal');
  assert.throws(() => resolvePairedProvenance(edited.root, edited.baselinePath, editedC, edited.manifest), /edited in C ancestry/);
});

test('no-ff E8 merge carries unchanged evidence and permits method-only C', (t) => {
  const fixture = gitFixture(t);
  fixture.git('checkout', '-q', '-b', 'evidence');
  const evidence = fixture.addBaseline();
  fixture.git('checkout', '-q', 'main');
  fixture.put('docs/main.md', 'Parallel method note\n');
  fixture.commit('Parallel docs');
  fixture.git('merge', '-q', '--no-ff', '-m', 'Merge E8', 'evidence');
  fixture.put('internal/gateway/measure_test.go', 'package gateway\nfunc TestMeasure() {}\n');
  const C = fixture.commit('Lock method-only C');
  const result = resolvePairedProvenance(fixture.root, fixture.baselinePath, C, fixture.manifest);
  assert.equal(result.evidence_commit, evidence);
  assert.equal(result.product_revision, fixture.initial);
});

test('fixed r8 output paths reject alternate attempts and existing journals', (t) => {
  const fixture = gitFixture(t);
  const B = join(fixture.root, manifest.B_only_evidence_path);
  const C = join(fixture.root, manifest.paired_evidence_path);
  assert.equal(validateOutputReservation('B-only', B, fixture.root, manifest).output, B);
  assert.equal(validateOutputReservation('paired', C, fixture.root, manifest).output, C);
  assert.throws(() => validateOutputReservation('B-only', join(fixture.root, 'retry.json'), fixture.root, manifest), /single frozen evidence path/);
  assert.throws(() => validateOutputReservation('paired', join(fixture.root, 'retry-paired.json'), fixture.root, manifest), /single frozen evidence path/);
  fixture.put(manifest.B_only_journal_path, fixture.journal);
  assert.throws(() => validateOutputReservation('B-only', B, fixture.root, manifest), /already reserves this attempt/);
});

test('paired B8 keeps exact reserved files untracked at measured M8', (t) => {
  const fixture = gitFixture(t);
  fixture.put(manifest.B_only_evidence_path, JSON.stringify(fixture.baseline, null, 2) + '\n');
  fixture.put(manifest.B_only_journal_path, fixture.journal);
  assert.equal(validateUntrackedBReservation(fixture.root, manifest), true);
  fixture.commit('Evidence committed in B checkout');
  assert.throws(() => validateUntrackedBReservation(fixture.root, manifest), /untracked reservation/);
});

test('legacy, transformed, omitted and ambiguous C fidelity contracts fail closed', () => {
  assert.deepEqual(validateStrictRouteContracts(structuredClone(manifest.C_route_contract)), manifest.C_route_contract);
  for (const category of ['native', 'translated', 'rejected']) {
    for (const fidelity of [{ mode: 'legacy' }, { mode: 'transformed' }, {}, null, { mode: 'strict', extra: true }]) {
      const contracts = structuredClone(manifest.C_route_contract);
      contracts[category].fidelity = fidelity;
      assert.throws(() => validateStrictRouteContracts(contracts), /exact explicit strict fidelity/);
    }
    const omitted = structuredClone(manifest.C_route_contract);
    delete omitted[category];
    assert.throws(() => validateStrictRouteContracts(omitted), /exact explicit strict fidelity/);
    const implicit = structuredClone(manifest.C_route_contract);
    delete implicit[category].fidelity;
    assert.throws(() => validateStrictRouteContracts(implicit), /exact explicit strict fidelity/);
  }
  assert.throws(() => validateStrictRouteContracts(null), /exact explicit strict fidelity/);
  const baseline = capture(true), paired = capture(false);
  paired.B_only_sha256 = digest(JSON.stringify(baseline, null, 2) + '\n');
  paired.C_route_contract.native.fidelity.mode = 'legacy';
  assert.throws(() => analyzePaired(paired, baseline, manifest), /exact explicit strict fidelity/);
});

test('fixed randomized schedule balances two orders per path and keeps four-arm palindromes', () => {
  const schedule = sourceSchedule();
  assert.equal(schedule.length, 480);
  assert.equal(digest(JSON.stringify(schedule)), manifest.schedule_sha256);
  assert.equal(new Set(schedule.map((block) => `${block.group}:${block.index}`)).size, 480);
  for (const group of new Set(schedule.map((block) => block.group))) {
    const blocks = schedule.filter((block) => block.group === group);
    const count = blockCount(group);
    assert.equal(blocks.length, count);
    assert.equal(manifest.blocks_per_group[group], count);
    assert.equal(blocks.filter((block) => block.variant === 'ABBA').length, count / 2);
    assert.equal(blocks.filter((block) => block.variant === 'BAAB').length, count / 2);
    if (!group.startsWith('rejected_extension/')) {
      for (const variant of ['ABBA', 'BAAB']) {
        assert.equal(blocks.filter((block) => block.variant === variant && block.outer === 'relay').length, count / 4);
        assert.equal(blocks.filter((block) => block.variant === variant && block.outer === 'gateway').length, count / 4);
      }
    }
    for (const block of blocks) {
      const arms = sourceOrder(group, block.variant, false, block.outer);
      assert.deepEqual(arms, [...arms].reverse());
      for (const path of group.startsWith('rejected_extension/') ? ['gateway'] : ['relay', 'gateway']) {
        assert.equal(arms.filter((arm) => arm === `B/${path}`).length, 2);
        assert.equal(arms.filter((arm) => arm === `C/${path}`).length, 2);
        const treatmentOrder = arms.filter((arm) => arm.endsWith(`/${path}`)).map((arm) => arm[0]).join('');
        const expected = path === 'relay' || group.startsWith('rejected_extension/')
          ? block.variant === 'ABBA' ? 'BCCB' : 'CBBC'
          : block.variant === 'ABBA' ? 'CBBC' : 'BCCB';
        assert.equal(treatmentOrder, expected, `${group} ${path} ${block.variant}`);
      }
      assert.deepEqual(sourceOrder(group, block.variant, true, block.outer), arms.filter((arm) => arm.startsWith('B/')));
      if (!group.startsWith('rejected_extension/')) {
        const opposite = sourceOrder(group, block.variant === 'ABBA' ? 'BAAB' : 'ABBA', false, block.outer);
        assert.deepEqual(opposite, arms.map((arm) => `${arm[0] === 'B' ? 'C' : 'B'}${arm.slice(1)}`));
      }
    }
  }
});

test('complete synthetic B-only and paired captures pass every predeclared metric and control', () => {
  const baseline = capture(true), paired = capture(false);
  paired.B_only_sha256 = digest(JSON.stringify(baseline, null, 2) + '\n');
  const B = analyzeBaseline(baseline, manifest);
  assert.equal(B.passed, true);
  assert.deepEqual(B.count, { blocks: 480, B_subruns: 1792, B_requests: 114688,
    B_semantic_gate_requests: 1408, B_dispatches: 106496, B_rejections: 8192 });
  assert.equal(B.signed_B_sham.controls, 254);
  assert.equal(B.signed_B_sham.paths['native_slow_stream_64/c1/relay']['max-inter-event-gap-p99-us'].values.length, 128);
  assert.equal(B.absolute_within_block_variability['native_unary/c1/relay']['ns/op'].values.length, 32);
  assert.equal(B.absolute_within_block_variability['native_unary/c1/relay']['ns/op'].upper, 0);
  const C = analyzePaired(paired, baseline, manifest);
  assert.equal(C.status, 'passed');
  assert.equal(Object.values(C.results).flatMap(Object.keys).length, 224);
  assert.equal(Object.values(C.added_latency).flatMap(Object.keys).length, 30);
  assert.equal(C.results['native_unary/c1/relay']['ns/op'].control_passed, true);
  assert.equal(C.paired_B_signed_sham.controls, 254);
  assert.equal(C.C_gateway_absolute_old_L.passed, true);
  const added = C.added_latency['native_unary/c1']['latency-p99-us'];
  assert.equal(added.B_gateway_minus_relay_values.length, 32);
  assert.equal(added.C_gateway_minus_relay_values.length, 32);
  assert.deepEqual(added.values, Array(32).fill(0));
});

test('missing blocks, arm order, counts, asset bytes, events and per-request gap statistic invalidate the study', () => {
  const mutations = [
    (study) => { study.blocks.pop(); },
    (study) => { [study.blocks[0].subruns[0], study.blocks[0].subruns[1]] = [study.blocks[0].subruns[1], study.blocks[0].subruns[0]]; },
    (study) => { study.blocks[0].subruns[0].reply = structuredClone(study.blocks[0].subruns[0].reply); study.blocks[0].subruns[0].reply.metrics.dispatches = 0; },
    (study) => { const block = study.blocks.find((entry) => entry.group.startsWith('native_asset_png/')); block.subruns[0].reply = structuredClone(block.subruns[0].reply); block.subruns[0].reply.metrics['request-bytes']--; },
    (study) => { const block = study.blocks.find((entry) => entry.group.startsWith('native_stream_256/')); block.subruns[0].reply = structuredClone(block.subruns[0].reply); block.subruns[0].reply.samples[0].events--; },
    (study) => { const block = study.blocks.find((entry) => entry.group.startsWith('native_slow_stream_64/')); block.subruns[0].reply = structuredClone(block.subruns[0].reply); block.subruns[0].reply.metrics['max-inter-event-gap-p99-us'] = 1; }
  ];
  const baseline = capture(true);
  for (const mutate of mutations) {
    const study = capture(false);
    study.B_only_sha256 = digest(JSON.stringify(baseline, null, 2) + '\n');
    mutate(study);
    assert.throws(() => analyzePaired(study, baseline, manifest));
  }
});

test('upper order statistic touching margin fails without a favorable equality rule', () => {
  const baseline = capture(true), paired = capture(false);
  paired.B_only_sha256 = digest(JSON.stringify(baseline, null, 2) + '\n');
  const name = 'native_unary/c1/gateway';
  const { margin, old_B_median: B } = manifest.comparisons[name]['ns/op'];
  for (const block of paired.blocks.filter((entry) => entry.group === 'native_unary/c1')) {
    for (const subrun of block.subruns.filter((entry) => entry.arm === 'C/gateway')) {
      subrun.reply = structuredClone(subrun.reply);
      subrun.reply.metrics['ns/op'] = B + margin;
    }
  }
  const result = analyzePaired(paired, baseline, manifest);
  assert.equal(result.status, 'failed');
  assert.equal(result.results[name]['ns/op'].upper, margin);
  assert.equal(result.results[name]['ns/op'].passed, false);
});

test('gateway-minus-relay added latency can fail while both absolute paths and relay control pass', () => {
  const baseline = capture(true), paired = capture(false);
  paired.B_only_sha256 = digest(JSON.stringify(baseline, null, 2) + '\n');
  const group = 'native_slow_stream_64/c1';
  const gatewayMargin = manifest.added_latency[group]['latency-p50-us'].margin;
  for (const block of paired.blocks.filter((entry) => entry.group === group)) {
    for (const subrun of block.subruns.filter((entry) => entry.arm.startsWith('C/'))) {
      subrun.reply = structuredClone(subrun.reply);
      const shift = subrun.arm === 'C/gateway' ? 0.7 * gatewayMargin : -0.4 * gatewayMargin;
      for (const p of [50, 95, 99]) subrun.reply.metrics[`latency-p${p}-us`] += shift;
      for (const sample of subrun.reply.samples) sample.latency_us += shift;
    }
  }
  const result = analyzePaired(paired, baseline, manifest);
  assert.equal(result.status, 'failed');
  assert.equal(result.results[`${group}/gateway`]['latency-p50-us'].passed, true);
  assert.equal(result.results[`${group}/relay`]['latency-p50-us'].control_passed, true);
  assert.equal(result.added_latency[group]['latency-p50-us'].passed, false);
  assert.ok(result.added_latency[group]['latency-p50-us'].upper > gatewayMargin);
});

test('relay negative control rejects signed cancellation and marks the whole study inconclusive', () => {
  const baseline = capture(true), paired = capture(false);
  paired.B_only_sha256 = digest(JSON.stringify(baseline, null, 2) + '\n');
  const name = 'native_slow_stream_64/c1/relay';
  const { margin, old_B_median: B } = manifest.comparisons[name]['ns/op'];
  assert.ok(B > margin * 1.1, 'fixture permits a positive low arm');
  const blocks = paired.blocks.filter((entry) => entry.group === 'native_slow_stream_64/c1');
  for (let index = 0; index < blocks.length; index++) {
    for (const subrun of blocks[index].subruns.filter((entry) => entry.arm === 'C/relay')) {
      subrun.reply = structuredClone(subrun.reply);
      subrun.reply.metrics['ns/op'] = B + (index < 64 ? -1.1 : 1.1) * margin;
    }
  }
  const result = analyzePaired(paired, baseline, manifest);
  assert.equal(result.status, 'inconclusive');
  assert.equal(result.results[name]['ns/op'].control_passed, false);
  assert.ok(result.results[name]['ns/op'].lower < -margin);
  assert.ok(result.results[name]['ns/op'].upper > margin);
});

test('contemporaneous B old-L miss stays descriptive when signed sham and C gates pass', () => {
  const baseline = capture(true), paired = capture(false);
  paired.B_only_sha256 = digest(JSON.stringify(baseline, null, 2) + '\n');
  const name = 'native_unary/c1/gateway';
  const limit = manifest.comparisons[name]['ns/op'].old_limit;
  for (const block of paired.blocks.filter((entry) => entry.group === 'native_unary/c1')) {
    for (const subrun of block.subruns.filter((entry) => entry.arm === 'B/gateway')) {
      subrun.reply = structuredClone(subrun.reply);
      subrun.reply.metrics['ns/op'] = limit + 1;
    }
  }
  const result = analyzePaired(paired, baseline, manifest);
  assert.equal(result.status, 'passed');
  assert.equal(result.descriptive_absolute_B_vs_v1.metrics[name]['ns/op'].within_old_v1_limit, false);
  assert.ok(result.descriptive_absolute_B_vs_v1.above_old_limit >= 1);
  assert.equal(result.paired_B_signed_sham.passed, true);
});

test('missing raw timing or runtime observations cannot be silently summarized', () => {
  const name = 'native_stream_256/c1/relay';
  const sample = reply(name);
  sample.samples.pop();
  assert.throws(() => validateSubrun(sample, name, manifest), /missing fixed samples/);
  const other = reply(name);
  delete other.runtime.num_gc_delta;
  assert.throws(() => validateSubrun(other, name, manifest), /runtime\/GC\/effect-wait diagnostics missing/);
});
