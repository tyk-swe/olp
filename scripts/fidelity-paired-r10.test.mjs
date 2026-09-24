import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { chmodSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { homedir } from 'node:os';
import { dirname, join } from 'node:path';
import { test } from 'node:test';
import * as r8 from './fidelity-paired-r8.mjs';
import { analyzePaired, analyzeSealedB8, binomialTailAtLeast, boundForGroup, cBoundForGroup, changedEntries,
  checkedSemanticReply, chooseProductCommit, evaluateCaptureHostInterval, hostInterval, makeManifest,
  requireExactProductRevision, requireMethodBuildInputLock, requireMethodGoLock,
  requirePreProductBuildInputHistory, requireTrackedGoLock, reservationHeader,
  resolvePairedProvenance, semanticGateOrder, SemanticFailure, sourceOrder, sourceSchedule,
  trackedBuildInputInventory, trackedGoInventory, validateOutputReservation, validateProductCommit,
  validateStrictProviderContracts, validateStrictRouteContracts, validateSubrun,
  verifyJournal } from './fidelity-paired-r10.mjs';

const manifest = makeManifest(process.cwd());
const frozenManifest = JSON.parse(readFileSync('docs/evidence/fidelity-performance/source-paired-v2/manifest-r10.json', 'utf8'));
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
  return { schema: 'openllmproxy.dev/fidelity-source-paired-r10', mode: 'paired', status: 'complete',
    manifest_sha256: hash(JSON.stringify(manifest)), B_only_sha256: manifest.reused_B8.artifact_sha256, blocks,
    hardware: B8.hardware, toolchain: B8.toolchain, go_build_environment: B8.go_build_environment,
    runtime_environment: B8.runtime_environment, conditions: manifest.conditions,
    diagnostics_before: diagnostics, diagnostics_after: diagnostics, clock_ticks_per_second: 100,
    host_stability_rule: manifest.host_stability,
    host_preflight: { samples: structuredClone(preflightSamples), intervals: structuredClone(preflightIntervals), passed: true, failures: [] },
    semantic_gate: semanticGateOrder(true).map(({ arm, name }) => ({ arm, name, reply: get(name) })),
    B: B8.B, C: { revision: 'synthetic-C10', binary_sha256: 'synthetic-C10-binary' },
    C_route_contract: structuredClone(manifest.C_route_contract), C_provider_contract: structuredClone(manifest.C_provider_contract) };
}
function paired() { const result = capture(); return result; }

// The old baseline is read from committed bytes and is never mutated by these tests.
test('r10 pins original B8, failed C9, exact profiles, same schedule and prospective bounds', () => {
  assert.deepEqual(manifest, frozenManifest);
  assert.equal(manifest.reused_B8.method_revision, '5ad7cda6f2864fc760b277f22cd47ce067c60f65');
  assert.equal(manifest.reused_B8.evidence_revision, '274e95a21cc78731bd32b64959cd083eeee0e227');
  assert.equal(manifest.reused_B8.artifact_sha256, 'f537f5a936eccdaaf123707689898c8fd88d197900c9fa351c53dc79a590678f');
  assert.equal(manifest.reused_B8.journal_sha256, 'b6941932994aaebe3a900de79f02abe051efdf9e670677b8405b373bb17858fb');
  assert.equal(manifest.previous_attempts.r8.artifact_sha256, '64f5a68119af5c00720c1e7eb740b78f84fc238199c3a94a3dad75e89ab052e6');
  assert.equal(manifest.previous_attempts.r8.journal_sha256, 'ddc2909c8443ab0230e2af1665b1216cf792ca430b527c62c7cbb670ec1845a5');
  assert.equal(manifest.previous_attempts.r8.completed_blocks, 0);
  assert.equal(manifest.previous_attempts.r9.artifact_sha256, 'db0fd64596800f08f9e237bf74dcc28fb50225b99735f14793082e2dd3e67d64');
  assert.equal(manifest.previous_attempts.r9.journal_sha256, '660b8a33823de6d3e496a5ad1d9ac289705aaa282333454f0aad42dbaa95a265');
  assert.deepEqual([manifest.previous_attempts.r9.completed_blocks, manifest.previous_attempts.r9.semantic_gate_passed_entries,
    manifest.previous_attempts.r9.B_controls_failed, manifest.previous_attempts.r9.paired_primaries_failed,
    manifest.previous_attempts.r9.hard_C_gateway_old_L_failed], [480, 44, 0, 8, 7]);
  assert.equal(manifest.C9_failed_evidence_head, '39ad40cb704812b656a1c3be61f019c100c79848');
  assert.deepEqual(manifest.abandoned_pre_data_drafts.map(({ commit, status }) => [commit, status]), [
    ['4875e86bfd669eb0a9161a2813ec8d2ad5a26311', 'abandoned before C10 build, timing or reservation'],
    ['a8f84804b1f9fad71a9cfbf0a44f36a86b2c3a3c', 'abandoned before C10 build, timing or reservation'],
    ['7ea650b75457c0d570d1f12d994917f8abb5a307', 'abandoned before C10 build, timing or reservation'],
    ['ab3856525fa8bb35f2c798631d8bc1901abf6018', 'abandoned before C10 build, timing or reservation'],
    ['95e3c3ed993ffac293f22aa0ed2a357552d80380', 'abandoned before C10 build, timing or reservation']
  ]);
  assert.deepEqual(manifest.required_product_paths, ['internal/connectors/defaults.go', 'internal/connectors/operations.go',
    'internal/connectors/profiles.go', 'internal/connectors/profiles_test.go']);
  assert.deepEqual(manifest.tracked_build_input_scope, { directory_prefixes: ['internal/', 'cmd/', 'openapi/', 'tests/', 'vendor/'],
    root_files: ['go.mod', 'go.sum'], root_glob: 'go.work*' });
  assert.equal(manifest.seed, r8Manifest.seed);
  assert.equal(manifest.schedule_sha256, r8Manifest.schedule_sha256);
  assert.deepEqual(manifest.blocks_per_group, r8Manifest.blocks_per_group);
  assert.deepEqual(manifest.comparisons, r8Manifest.comparisons);
  assert.deepEqual(manifest.added_latency, r8Manifest.added_latency);
  assert.deepEqual([boundForGroup('native_unary/c1').upper_statistic, cBoundForGroup('native_unary/c1').upper_statistic], [23, 25]);
  assert.deepEqual([boundForGroup('native_slow_stream_64/c1').upper_statistic, cBoundForGroup('native_slow_stream_64/c1').upper_statistic], [78, 82]);
  assert.deepEqual([cBoundForGroup('native_unary/c1').lower_statistic, cBoundForGroup('native_slow_stream_64/c1').lower_statistic], [8, 47]);
  assert.match(manifest.sequential_rule, /0\.0486130202.*0\.049664221.*0\.05/);
  assert.ok(Math.abs(binomialTailAtLeast(32, 25, 0.5) - 0.0010512007866) < 1e-12);
  assert.ok(Math.abs(binomialTailAtLeast(128, 82, 0.5) - 0.000931234262) < 1e-12);
  assert.equal(sourceSchedule().length, 480);
  assert.equal(hash(JSON.stringify(sourceSchedule())), manifest.schedule_sha256);
  assert.deepEqual(manifest.C_provider_contract, { native: { profile_id: 'compatible-chat', profile_revision: '1' },
    translated: { profile_id: 'anthropic-messages', profile_revision: '1' },
    rejected: { profile_id: 'anthropic-messages', profile_revision: '1' } });
});

test('failed C9 cannot be retrospectively re-scored as an R10 candidate', () => {
  const C9 = JSON.parse(readFileSync('docs/evidence/fidelity-performance/source-paired-v2/paired-r9.json', 'utf8'));
  assert.equal(C9.analysis.status, 'failed');
  assert.throws(() => analyzePaired(C9, B8, manifest), /capture or manifest identity changed/);
});

test('the complete reservation header binds every pre-timing field and resists updated-hash tampering', (t) => {
  const root = mkdtempSync(join(homedir(), 'olp-r10-header-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const journal = join(root, 'candidate.json.journal.jsonl');
  const candidate = { schema: manifest.schema, mode: 'paired', status: 'complete', started_at: 'before-timing',
    manifest_sha256: hash(JSON.stringify(manifest)), schedule_sha256: manifest.schedule_sha256,
    B_only_sha256: manifest.reused_B8.artifact_sha256, B: { revision: 'M8' }, C: { revision: 'P10' },
    provenance: { method_commit: 'M10', product_revision: 'P10' },
    C_route_contract: structuredClone(manifest.C_route_contract),
    C_provider_contract: structuredClone(manifest.C_provider_contract),
    hardware: { cpu: 'pinned' }, toolchain: 'go1.27.1', go_build_environment: 'pinned',
    runtime_environment: { GOMAXPROCS: '4' }, clock_ticks_per_second: 100,
    host_stability_rule: { preflight_seconds: 60 }, host_preflight: { passed: true, samples: [1, 2] },
    conditions: { network: 'local' }, diagnostics_before: { loadavg: 'quiet' },
    semantic_gate: [], blocks: [], completed_at: 'after-timing', diagnostics_after: { loadavg: 'measured' },
    analysis: { status: 'passed' } };
  const writeJournal = (header) => {
    writeFileSync(journal, [JSON.stringify({ header }), JSON.stringify({ semantic_gate: [] }),
      JSON.stringify({ completion: { status: 'complete', block_count: 0 } })].join('\n') + '\n');
    candidate.journal_sha256 = hash(readFileSync(journal));
  };
  writeJournal(reservationHeader(candidate));
  assert.equal(verifyJournal(candidate, journal), true);
  const mutations = [
    (v) => { v.host_preflight.samples[0] = 99; },
    (v) => { v.host_stability_rule.preflight_seconds = 5; },
    (v) => { v.conditions.network = 'different'; },
    (v) => { v.hardware.cpu = 'different'; },
    (v) => { v.toolchain = 'different'; },
    (v) => { v.go_build_environment = 'different'; },
    (v) => { v.runtime_environment.GOMAXPROCS = '8'; },
    (v) => { v.C_route_contract.native.fidelity.mode = 'legacy'; },
    (v) => { v.C_provider_contract.native.profile_revision = '2'; },
    (v) => { v.B_only_sha256 = 'changed'; },
    (v) => { v.schedule_sha256 = 'changed'; },
    (v) => { v.manifest_sha256 = 'changed'; },
    (v) => { v.clock_ticks_per_second = 99; },
    (v) => { v.started_at = 'after-timing'; },
    (v) => { v.diagnostics_before.loadavg = 'busy'; },
    (v) => { v.provenance.product_revision = 'other'; },
    (v) => { v.unregistered_pre_timing = 'injected'; }
  ];
  for (const mutate of mutations) {
    const altered = structuredClone(candidate);
    mutate(altered);
    assert.throws(() => verifyJournal(altered, journal), /captured pre-timing conditions/);
  }
  const changedHeader = structuredClone(reservationHeader(candidate));
  changedHeader.host_preflight.passed = false;
  writeJournal(changedHeader); // Even a final JSON with the updated journal hash cannot accept a changed header.
  assert.throws(() => verifyJournal(candidate, journal), /captured pre-timing conditions/);
});

test('P10 source lock includes compiled Go tests and rejects every later commit', () => {
  const allGo = trackedGoInventory(process.cwd(), manifest.method_base_revision);
  const benchmark = allGo.find((line) => line.endsWith('\tinternal/gateway/fidelity_paired_benchmark_test.go'));
  assert.ok(benchmark);
  assert.equal(requireTrackedGoLock(allGo, structuredClone(allGo)), true);
  const changedTest = allGo.map((line) => line === benchmark ? line.replace(/[0-9a-f]{40}(?=\t)/, '0'.repeat(40)) : line);
  assert.throws(() => requireTrackedGoLock(changedTest, allGo), /tracked Go source\/test blobs differ/);
  assert.equal(requireExactProductRevision('P10', 'P10'), true);
  assert.throws(() => requireExactProductRevision('P10-docs-descendant', 'P10'), /exact normal P10 product commit/);
});

test('real M10 ancestry locks all Go blobs to its direct parent, including the changed benchmark adapter', () => {
  const root = process.cwd();
  const base = manifest.method_base_revision;
  const method = execFileSync('git', ['log', '-1', '--format=%H', '--diff-filter=A', 'HEAD', '--', manifest.method_manifest_path],
    { encoding: 'utf8' }).trim();
  assert.match(method, /^[0-9a-f]{40}$/);
  assert.equal(execFileSync('git', ['rev-parse', `${method}^`], { encoding: 'utf8' }).trim(), base);
  const preOptimizationGo = trackedGoInventory(root, manifest.pre_optimization_product_revision);
  const baseGo = trackedGoInventory(root, base);
  const methodGo = trackedGoInventory(root, method);
  const baseInputs = trackedBuildInputInventory(root, base);
  const methodInputs = trackedBuildInputInventory(root, method);
  const adapterPath = '\tinternal/gateway/fidelity_paired_benchmark_test.go';
  const adapter = (inventory) => inventory.find((line) => line.endsWith(adapterPath));
  assert.ok(adapter(preOptimizationGo));
  assert.ok(adapter(baseGo));
  assert.notEqual(adapter(preOptimizationGo), adapter(baseGo));
  assert.deepEqual(methodGo, baseGo);
  assert.deepEqual(methodInputs, baseInputs);
  assert.equal(requireMethodGoLock(methodGo, baseGo), true);
  assert.equal(requireMethodBuildInputLock(methodInputs, baseInputs), true);
  assert.throws(() => requireMethodGoLock(methodGo, preOptimizationGo), /M10 must not change any tracked Go source or test file/);
  const alteredMethodGo = methodGo.map((line) => line.endsWith(adapterPath)
    ? line.replace(/[0-9a-f]{40}(?=\t)/, '0'.repeat(40)) : line);
  assert.notDeepEqual(alteredMethodGo, methodGo);
  assert.throws(() => requireMethodGoLock(alteredMethodGo, baseGo), /M10 must not change any tracked Go source or test file/);
  const alteredInputs = methodInputs.map((line) => line.endsWith(adapterPath)
    ? line.replace(/[0-9a-f]{40}(?=\t)/, '0'.repeat(40)) : line);
  assert.throws(() => requireMethodBuildInputLock(alteredInputs, baseInputs), /M10 must not change any tracked build input/);
});

test('the actual c3f service-method merge changes only docs/scripts, preserving all tracked build inputs', () => {
  const root = process.cwd();
  const base = manifest.method_base_revision;
  const service = 'c3f054c46df341e5888ac6bf8f7d9a7e5bf37967';
  const parents = execFileSync('git', ['rev-list', '--parents', '-n', '1', service], { encoding: 'utf8' }).trim().split(' ');
  assert.equal(parents[1], base);
  assert.equal(parents.length, 3);
  assert.deepEqual(trackedBuildInputInventory(root, service), trackedBuildInputInventory(root, base));
  const changed = execFileSync('git', ['diff', '--name-only', base, service], { encoding: 'utf8' }).trim().split('\n');
  assert.ok(changed.length > 0 && changed.every((path) => path.startsWith('docs/') || path.startsWith('scripts/')));
});

test('real Git side histories allow docs-only merges but reject reverted Go, fixture, module and mode changes', (t) => {
  const root = mkdtempSync(join(homedir(), 'olp-r10-pre-product-history-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const git = (...args) => execFileSync('git', args, { cwd: root, encoding: 'utf8' }).trim();
  const put = (path, value) => {
    const full = join(root, path);
    mkdirSync(dirname(full), { recursive: true });
    writeFileSync(full, value);
  };
  const commit = (message) => {
    git('add', '-A');
    git('commit', '-q', '-m', message);
    return git('rev-parse', 'HEAD');
  };
  const adapter = 'internal/gateway/paired_adapter_test.go';
  const quotedAdapter = 'internal/gateway/quoted\tadapter_test.go';
  const connector = 'internal/connectors/defaults.go';
  const fixture = 'tests/fixtures/embedded.json';
  const quotedFixture = 'tests/fixtures/quoted\tasset.json';
  const newlineFixture = 'tests/fixtures/newline\nasset.json';
  const module = 'go.mod';
  git('init', '-q', '-b', 'main');
  git('config', 'user.name', 'R10 History Fixture');
  git('config', 'user.email', 'r10-history@example.invalid');
  git('config', 'core.filemode', 'true');
  put(adapter, 'package gateway\n');
  put(quotedAdapter, 'package gateway\n');
  put(connector, 'package connectors\n');
  put(fixture, '{"ok":true}\n');
  put(quotedFixture, '{"ok":true}\n');
  put(newlineFixture, '{"ok":true}\n');
  put(module, 'module example.invalid/r10\n\ngo 1.27.1\n');
  put('go.sum', 'example.invalid/mod v1 h1:fixture\n');
  put('go.work', 'go 1.27.1\n');
  put('cmd/olp/main.go', 'package main\n');
  put('openapi/management.json', '{}\n');
  put('vendor/example.invalid/mod/mod.go', 'package mod\n');
  const base = commit('base');
  put('docs/method.md', 'M10\n');
  const method = commit('M10');
  put('docs/main.md', 'main docs\n');
  commit('main docs');
  git('switch', '-q', '-c', 'service', base);
  put('docs/service.md', 'service method\n');
  commit('service method docs');
  git('switch', '-q', 'main');
  git('merge', '--no-ff', '-q', '-m', 'merge service method', 'service');
  const docsParent = git('rev-parse', 'HEAD');
  assert.equal(requirePreProductBuildInputHistory(root, method, docsParent), true);
  const methodInputs = trackedBuildInputInventory(root, method);
  for (const path of [adapter, quotedAdapter, fixture, quotedFixture, newlineFixture, module, connector, 'go.sum', 'go.work', 'cmd/olp/main.go',
    'openapi/management.json', 'vendor/example.invalid/mod/mod.go']) {
    assert.ok(methodInputs.some((line) => line.endsWith(`\t${path}`)));
  }
  assert.ok(trackedGoInventory(root, method).some((line) => line.endsWith(`\t${quotedAdapter}`)));
  git('switch', '-q', '-c', 'valid-product');
  put(connector, 'package connectors\n// optimized\n');
  const validProduct = commit('connector P10');
  assert.deepEqual(chooseProductCommit([{ revision: validProduct, parents: [docsParent], changed: [connector] }]),
    { revision: validProduct, affected: [connector] });
  assert.equal(requirePreProductBuildInputHistory(root, method, git('rev-parse', `${validProduct}^`)), true);
  assert.deepEqual(validateProductCommit(root, docsParent, validProduct), [connector]);

  for (const [name, changedPath, content, original, modeOnly = false] of [
    ['go-test', adapter, 'package gateway\n// changed benchmark adapter\n', 'package gateway\n'],
    ['fixture', fixture, '{"ok":false}\n', '{"ok":true}\n'],
    ['quoted-fixture', quotedFixture, '{"ok":false}\n', '{"ok":true}\n'],
    ['newline-fixture', newlineFixture, '{"ok":false}\n', '{"ok":true}\n'],
    ['module', module, 'module example.invalid/changed\n\ngo 1.27.1\n', 'module example.invalid/r10\n\ngo 1.27.1\n'],
    ['fixture-mode', fixture, null, null, true]
  ]) {
    git('switch', '-q', '-c', `bad-main-${name}`, docsParent);
    git('switch', '-q', '-c', `bad-side-${name}`, method);
    if (modeOnly) chmodSync(join(root, changedPath), 0o755);
    else put(changedPath, content);
    const changed = commit(`change ${name}`);
    if (modeOnly) chmodSync(join(root, changedPath), 0o644);
    else put(changedPath, original);
    commit(`revert ${name}`);
    git('switch', '-q', `bad-main-${name}`);
    git('merge', '--no-ff', '-q', '-m', `merge reverted ${name} history`, `bad-side-${name}`);
    const badParent = git('rev-parse', 'HEAD');
    assert.deepEqual(trackedBuildInputInventory(root, method), trackedBuildInputInventory(root, badParent));
    put(connector, `package connectors\n// optimization after ${name}\n`);
    const badProduct = commit(`connector P10 after reverted ${name}`);
    assert.deepEqual(chooseProductCommit([{ revision: badProduct, parents: [badParent], changed: [connector] }]),
      { revision: badProduct, affected: [connector] });
    assert.throws(() => requirePreProductBuildInputHistory(root, method, git('rev-parse', `${badProduct}^`)),
      (error) => error.message.includes(changed) && /tracked build input blob or mode changed before P10/.test(error.message));
  }
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

test('C10 is the only reservable output and any existing journal blocks retry', (t) => {
  const root = mkdtempSync(join(homedir(), 'olp-r10-reservation-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const output = join(root, manifest.paired_evidence_path);
  mkdirSync(dirname(output), { recursive: true });
  assert.equal(validateOutputReservation('paired', output, root, manifest).output, output);
  assert.throws(() => validateOutputReservation('B-only', join(root, manifest.B_only_evidence_path), root, manifest), /r10 reserves only/);
  assert.throws(() => validateOutputReservation('paired', join(root, 'alternate.json'), root, manifest), /single frozen evidence path/);
  writeFileSync(join(root, manifest.paired_journal_path), '{"reserved":true}\n');
  assert.throws(() => validateOutputReservation('paired', output, root, manifest), /already reserves/);
  const CLI = spawnSync(process.execPath, ['scripts/fidelity-paired-r10.mjs', 'record-B', 'out', 'manifest', 'binary', 'root'], { encoding: 'utf8' });
  assert.equal(CLI.status, 1);
  assert.match(CLI.stderr, /Usage: fidelity-paired-r10/);
  const wrongManifest = spawnSync(process.execPath, ['scripts/fidelity-paired-r10.mjs', 'freeze-manifest', join(root, 'selected.json')],
    { encoding: 'utf8' });
  assert.equal(wrongManifest.status, 1);
  assert.match(wrongManifest.stderr, /one fixed output path/);
});

test('a pre-M10 connector side branch, merge-only change, or extra product edit cannot satisfy P10', () => {
  const valid = { revision: 'later-P10', parents: ['M10'], changed: ['internal/connectors/defaults.go', 'internal/connectors/profiles_test.go'] };
  assert.deepEqual(chooseProductCommit([valid]), { revision: 'later-P10', affected: ['internal/connectors/defaults.go'] });
  assert.throws(() => chooseProductCommit([]), /one later normal P10/); // An earlier side-branch commit is absent from M10..C10.
  assert.throws(() => chooseProductCommit([{ ...valid, parents: ['left', 'right'] }]), /one later normal P10/);
  assert.throws(() => chooseProductCommit([valid, { ...valid, revision: 'second' }]), /one later normal P10/);
  assert.throws(() => chooseProductCommit([{ ...valid, changed: [...valid.changed, 'internal/gateway/route.go'] }]),
    /outside the exact pinned connector fix set/);
  assert.throws(() => chooseProductCommit([{ ...valid, changed: [...valid.changed, 'docs/extra.md'] }]),
    /outside the exact pinned connector fix set/);
});

test('P10 accepts only real connector blob edits and rejects renamed, extra or chmod-only paths', (t) => {
  const root = mkdtempSync(join(homedir(), 'olp-r10-product-paths-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const git = (...args) => execFileSync('git', args, { cwd: root, encoding: 'utf8' }).trim();
  const put = (path, value) => {
    const full = join(root, path);
    mkdirSync(dirname(full), { recursive: true });
    writeFileSync(full, value);
  };
  const commit = (message) => {
    git('add', '-A');
    git('commit', '-q', '-m', message);
    return git('rev-parse', 'HEAD');
  };
  const connector = 'internal/connectors/defaults.go';
  const connectorTest = 'internal/connectors/profiles_test.go';
  const foreign = 'internal/gateway/foreign.go';
  git('init', '-q', '-b', 'main');
  git('config', 'user.name', 'R10 Product Fixture');
  git('config', 'user.email', 'r10-product@example.invalid');
  git('config', 'core.filemode', 'true');
  put(connector, 'package connectors\n');
  put(connectorTest, 'package connectors\n');
  put(foreign, 'package gateway\n');
  put('docs/setup.md', 'base\n');
  const base = commit('base');

  git('switch', '-q', '-c', 'valid', base);
  put(connector, 'package connectors\n// optimized\n');
  put(connectorTest, 'package connectors\n// changed test\n');
  const valid = commit('prepared connector fix');
  assert.deepEqual(changedEntries(root, base, valid), [
    { status: 'M', path: connector }, { status: 'M', path: connectorTest }
  ]);
  assert.deepEqual(validateProductCommit(root, base, valid), [connector, connectorTest]);

  git('switch', '-q', '-c', 'extra', base);
  put(connector, 'package connectors\n// optimized\n');
  put('docs/setup.md', 'unexpected\n');
  const extra = commit('connector plus unrelated path');
  assert.throws(() => chooseProductCommit([{ revision: extra, parents: [base],
    changed: changedEntries(root, base, extra).map(({ path }) => path) }]), /outside the exact pinned connector fix set/);
  assert.throws(() => validateProductCommit(root, base, extra), /only the exact four pinned connector fix paths/);

  git('switch', '-q', '-c', 'rename', base);
  git('mv', foreign, 'internal/connectors/profiles.go');
  const renamed = commit('rename outside path into connector set');
  const renamedEntries = changedEntries(root, base, renamed);
  assert.deepEqual(renamedEntries, [
    { status: 'A', path: 'internal/connectors/profiles.go' }, { status: 'D', path: foreign }
  ]);
  assert.throws(() => chooseProductCommit([{ revision: renamed, parents: [base],
    changed: renamedEntries.map(({ path }) => path) }]), /outside the exact pinned connector fix set/);
  assert.throws(() => validateProductCommit(root, base, renamed), /only the exact four pinned connector fix paths/);

  git('switch', '-q', '-c', 'mode', base);
  chmodSync(join(root, connector), 0o755);
  const modeOnly = commit('chmod-only connector');
  assert.deepEqual(changedEntries(root, base, modeOnly), [{ status: 'M', path: connector }]);
  assert.throws(() => validateProductCommit(root, base, modeOnly), /retain regular-file mode/);
});

test('real M10 ancestry cannot qualify C10 before a post-method P10 commit', () => {
  const revision = execFileSync('git', ['log', '-1', '--format=%H', '--diff-filter=A', 'HEAD', '--', manifest.method_manifest_path],
    { encoding: 'utf8' }).trim();
  assert.match(revision, /^[0-9a-f]{40}$/);
  const path = join(process.cwd(), manifest.B_only_evidence_path);
  assert.throws(() => resolvePairedProvenance(process.cwd(), path, revision, manifest),
    /R10 manifest and runner|M10 must be a strict ancestor|one later normal P10/);
  const wrongE = { ...manifest, reused_B8: { ...manifest.reused_B8, evidence_revision: manifest.pre_optimization_product_revision } };
  assert.throws(() => resolvePairedProvenance(process.cwd(), path, revision, wrongE), /single sealed E8/);
  const wrongC9 = { ...manifest, C9_failed_evidence_head: '0'.repeat(40) };
  assert.throws(() => resolvePairedProvenance(process.cwd(), path, revision, wrongC9), /failed-C9 evidence/);
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

test('synthetic paired capture passes with sealed B8 and exact C10 profile contract', () => {
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

test('C10 d25 rejects eight high ordinary blocks that old C9 d24 would accept', () => {
  const candidate = capture(), name = 'native_unary/c1/gateway', group = 'native_unary/c1';
  const { margin, old_B_median: B } = manifest.comparisons[name]['ns/op'];
  const blocks = candidate.blocks.filter((block) => block.group === group);
  for (let i = 0; i < 8; i++) for (const run of blocks[i].subruns.filter((entry) => entry.arm === 'C/gateway')) {
    run.reply = structuredClone(run.reply); run.reply.metrics['ns/op'] = B + margin + 1;
  }
  const result = analyzePaired(candidate, B8, manifest);
  assert.equal(result.status, 'failed');
  assert.equal(result.results[name]['ns/op'].upper, margin + 1);
  assert.equal(result.C_gateway_absolute_old_L.passed, true);
});

test('C10 slow/c1 d82 rejects 47 high blocks that old C9 d80 would accept', () => {
  const candidate = capture(), group = 'native_slow_stream_64/c1', name = `${group}/gateway`;
  const { margin, old_B_median: B } = manifest.comparisons[name]['ns/op'];
  const blocks = candidate.blocks.filter((block) => block.group === group);
  for (let i = 0; i < 47; i++) for (const run of blocks[i].subruns.filter((entry) => entry.arm === 'C/gateway')) {
    run.reply = structuredClone(run.reply); run.reply.metrics['ns/op'] = B + margin + 1;
  }
  const result = analyzePaired(candidate, B8, manifest);
  assert.equal(result.status, 'failed');
  assert.equal(result.results[name]['ns/op'].upper, margin + 1);
  assert.equal(result.C_gateway_absolute_old_L.passed, true);
});

test('C10 relay d47 lower control rejects signed cancellation while C9 d49 would pass', () => {
  const candidate = capture(), group = 'native_slow_stream_64/c1', name = `${group}/relay`;
  const { margin, old_B_median: B } = manifest.comparisons[name]['ns/op'];
  assert.ok(B > margin + 1);
  const blocks = candidate.blocks.filter((block) => block.group === group);
  for (let i = 0; i < 47; i++) for (const run of blocks[i].subruns.filter((entry) => entry.arm === 'C/relay')) {
    run.reply = structuredClone(run.reply); run.reply.metrics['ns/op'] = B - margin - 1;
  }
  const result = analyzePaired(candidate, B8, manifest);
  assert.equal(result.status, 'inconclusive');
  assert.equal(result.results[name]['ns/op'].lower, -margin - 1);
  assert.equal(result.results[name]['ns/op'].control_passed, false);
});

test('paired B sham keeps original r8 bounds independently of tightened C10 bound', () => {
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

test('one gateway C old-L excess fails even when tightened C10 paired primary passes', () => {
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
