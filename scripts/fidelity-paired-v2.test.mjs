import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { test } from 'node:test';
import { analyzeBaseline, analyzePaired, makeManifest, resolvePairedProvenance, sourceOrder, sourceSchedule, validateStrictRouteContracts, validateSubrun, verifyPairedProvenance } from './fidelity-paired-v2.mjs';

const manifest = makeManifest(process.cwd());
const digest = (value) => createHash('sha256').update(value).digest('hex');
const diagnostics = { loadavg: '0.1 0.1 0.1 1/100 100', cpu_pressure: 'some avg10=0.00 total=0', memory_pressure: 'some avg10=0.00 total=0' };
const metadata = { revision: 'B-measurement-only', binary_sha256: 'same-B-binary' };

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
  const baseline = { baseline: true };
  const addBaseline = () => { put(manifest.B_only_evidence_path, JSON.stringify(baseline, null, 2) + '\n'); return commit('Record B-only evidence'); };
  return { root, git, put, commit, source, initial, baselinePath, baseline, addBaseline };
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
    heap_alloc_after_bytes: 1000, goroutines_after: 10 },
    scheduler: { latency_bucket_seconds: ['0', '0.0001', '+Inf'], latency_counts_delta: [1, 0] } };
}
function capture(baselineOnly) {
  const cache = new Map();
  const get = (name) => {
    if (!cache.has(name)) cache.set(name, reply(name));
    return cache.get(name);
  };
  const blocks = sourceSchedule().map((block) => ({ ...block, diagnostics_before: diagnostics, diagnostics_after: diagnostics,
    subruns: sourceOrder(block.group, block.variant, baselineOnly, block.outer).map((arm) => ({ arm, reply: get(`${block.group}/${arm.split('/')[1]}`) })) }));
  return { schema: 'openllmproxy.dev/fidelity-source-paired-v2', mode: baselineOnly ? 'B-only' : 'paired', status: 'complete',
    manifest_sha256: digest(JSON.stringify(manifest)), B_only_sha256: null, blocks,
    hardware: manifest.old_hardware, toolchain: manifest.old_toolchain, go_build_environment: manifest.old_go_build_environment,
    runtime_environment: manifest.old_runtime_environment, conditions: manifest.conditions, diagnostics_before: diagnostics, diagnostics_after: diagnostics,
    B: metadata, C: baselineOnly ? null : { revision: 'locked-C', binary_sha256: 'C-binary' },
    C_route_contract: baselineOnly ? null : structuredClone(manifest.C_route_contract) };
}

test('manifest derives exactly 224 path and 30 added-latency margins from immutable B data', () => {
  assert.equal(Object.keys(manifest.comparisons).length, 22);
  assert.equal(Object.values(manifest.comparisons).flatMap(Object.keys).length, 224);
  assert.equal(Object.values(manifest.added_latency).flatMap(Object.keys).length, 30);
  assert.equal(manifest.B_product_revision, '8e52f775df815c3a1a25d75064c3ffd540fb07f5');
  assert.equal(manifest.manifest_revision, 'post-baseline-product-r3');
  assert.equal(manifest.B_only_evidence_path, 'docs/evidence/fidelity-performance/source-paired-v2/baseline.json');
  assert.deepEqual(manifest.C_route_contract, { native: { fidelity: { mode: 'strict' } }, translated: { fidelity: { mode: 'strict' } }, rejected: { fidelity: { mode: 'strict' } } });
  for (const metrics of Object.values(manifest.comparisons)) for (const value of Object.values(metrics)) {
    assert.equal(value.margin, value.old_limit - value.old_B_median);
    assert.ok(value.margin > 0);
  }
});

test('exact B-only blob precedes a real production change and stays verifiable after a docs descendant', (t) => {
  const fixture = gitFixture(t);
  const evidence = fixture.addBaseline();
  fixture.put(fixture.source, 'package gateway\nfunc dispatch() int { return 2 }\n');
  const product = fixture.commit('Improve dispatch');
  const provenance = resolvePairedProvenance(fixture.root, fixture.baselinePath, product, manifest);
  assert.equal(provenance.evidence_commit, evidence);
  assert.equal(provenance.product_commit, product);
  assert.deepEqual(provenance.production_paths, [fixture.source]);
  fixture.put('docs/after.md', 'Later qualification note\n');
  const docs = fixture.commit('Add later docs');
  assert.equal(resolvePairedProvenance(fixture.root, fixture.baselinePath, docs, manifest).product_commit, product);
  const capture = { C: { revision: product }, B_only_sha256: digest(JSON.stringify(fixture.baseline, null, 2) + '\n'), provenance };
  assert.deepEqual(verifyPairedProvenance(capture, fixture.baseline, manifest, fixture.root, fixture.baselinePath), provenance);
  capture.provenance = { ...provenance, evidence_commit: fixture.initial };
  assert.throws(() => verifyPairedProvenance(capture, fixture.baseline, manifest, fixture.root, fixture.baselinePath), /chronology/);
  fixture.put(manifest.B_only_evidence_path, '{"tampered":true}\n');
  assert.throws(() => resolvePairedProvenance(fixture.root, fixture.baselinePath, product, manifest), /exact B-only artifact blob/);
});

test('same commit for baseline and production change cannot qualify', (t) => {
  const fixture = gitFixture(t);
  fixture.put(manifest.B_only_evidence_path, JSON.stringify(fixture.baseline, null, 2) + '\n');
  fixture.put(fixture.source, 'package gateway\nfunc dispatch() int { return 2 }\n');
  const same = fixture.commit('Bundle evidence and candidate');
  assert.throws(() => resolvePairedProvenance(fixture.root, fixture.baselinePath, same, manifest), /strict ancestor/);
});

test('a pre-evidence product side branch hidden behind a no-ff merge cannot qualify', (t) => {
  const fixture = gitFixture(t);
  fixture.git('checkout', '-q', '-b', 'product');
  fixture.put(fixture.source, 'package gateway\nfunc dispatch() int { return 2 }\n');
  fixture.commit('Product on side branch before evidence');
  fixture.git('checkout', '-q', 'main');
  fixture.addBaseline();
  fixture.git('merge', '-q', '--no-ff', '-m', 'Merge pre-evidence product', 'product');
  const merged = fixture.git('rev-parse', 'HEAD');
  assert.throws(() => resolvePairedProvenance(fixture.root, fixture.baselinePath, merged, manifest), /no affected production Go commit follows/);
});

test('test-only and docs-only descendants do not satisfy the product chronology', (t) => {
  const fixture = gitFixture(t);
  fixture.addBaseline();
  fixture.put('internal/gateway/dispatch_test.go', 'package gateway\nfunc TestDispatch() {}\n');
  fixture.commit('Add only a test');
  fixture.put('docs/after.md', 'Only docs\n');
  const docs = fixture.commit('Add only docs');
  assert.throws(() => resolvePairedProvenance(fixture.root, fixture.baselinePath, docs, manifest), /no affected production Go commit follows/);
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
  assert.equal(schedule.length, 384);
  assert.equal(digest(JSON.stringify(schedule)), manifest.schedule_sha256);
  assert.equal(new Set(schedule.map((block) => `${block.group}:${block.index}`)).size, 384);
  for (const group of new Set(schedule.map((block) => block.group))) {
    const blocks = schedule.filter((block) => block.group === group);
    assert.equal(blocks.filter((block) => block.variant === 'ABBA').length, 16);
    assert.equal(blocks.filter((block) => block.variant === 'BAAB').length, 16);
    if (!group.startsWith('rejected_extension/')) {
      for (const variant of ['ABBA', 'BAAB']) {
        assert.equal(blocks.filter((block) => block.variant === variant && block.outer === 'relay').length, 8);
        assert.equal(blocks.filter((block) => block.variant === variant && block.outer === 'gateway').length, 8);
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
  assert.deepEqual(B.count, { blocks: 384, B_subruns: 1408, B_requests: 90112 });
  assert.equal(B.absolute_within_block_variability['native_unary/c1/relay']['ns/op'].values.length, 32);
  assert.equal(B.absolute_within_block_variability['native_unary/c1/relay']['ns/op'].upper, 0);
  const C = analyzePaired(paired, baseline, manifest);
  assert.equal(C.status, 'passed');
  assert.equal(Object.values(C.results).flatMap(Object.keys).length, 224);
  assert.equal(Object.values(C.added_latency).flatMap(Object.keys).length, 30);
  assert.equal(C.results['native_unary/c1/relay']['ns/op'].control_passed, true);
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
      subrun.reply.metrics['ns/op'] = B + (index < 16 ? -1.1 : 1.1) * margin;
    }
  }
  const result = analyzePaired(paired, baseline, manifest);
  assert.equal(result.status, 'inconclusive');
  assert.equal(result.results[name]['ns/op'].control_passed, false);
  assert.ok(result.results[name]['ns/op'].lower < -margin);
  assert.ok(result.results[name]['ns/op'].upper > margin);
});

test('contemporaneous B envelope failure is inconclusive even if C appears faster', () => {
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
  assert.equal(result.status, 'inconclusive');
  assert.equal(result.envelope[name]['ns/op'].passed, false);
});

test('missing raw timing or runtime observations cannot be silently summarized', () => {
  const name = 'native_stream_256/c1/relay';
  const sample = reply(name);
  sample.samples.pop();
  assert.throws(() => validateSubrun(sample, name, manifest), /missing fixed samples/);
  const other = reply(name);
  delete other.runtime.num_gc_delta;
  assert.throws(() => validateSubrun(other, name, manifest), /runtime\/GC diagnostics missing/);
});
