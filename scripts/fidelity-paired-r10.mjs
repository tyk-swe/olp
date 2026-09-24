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
import * as r8 from './fidelity-paired-r8.mjs';
import * as r9 from './fidelity-paired-r9.mjs';

export const SCHEMA = 'openllmproxy.dev/fidelity-source-paired-r10';
export const SEED = 'oif-source-paired-r8-2026-09-23-sealed-7e89a37c29d6';
export const BLOCKS = 32;
export const SLOW_BLOCKS = 128;
export const SLOW_GROUP = 'native_slow_stream_64/c1';
export const SAMPLES = 64;
export const UPPER_INDEX = 22; // B sham d_(23), zero-based.
export const LOWER_INDEX = 9; // B sham d_(10), zero-based.
export const SLOW_UPPER_INDEX = 77; // B sham d_(78), zero-based.
export const SLOW_LOWER_INDEX = 50; // B sham d_(51), zero-based.
export const C_UPPER_INDEX = 24; // C primary d_(25), zero-based.
export const C_LOWER_INDEX = 7; // C relay d_(8), zero-based.
export const C_SLOW_UPPER_INDEX = 81; // C primary d_(82), zero-based.
export const C_SLOW_LOWER_INDEX = 46; // C relay d_(47), zero-based.
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
const strictProviderContracts = {
  native: { profile_id: 'compatible-chat', profile_revision: '1' },
  translated: { profile_id: 'anthropic-messages', profile_revision: '1' },
  rejected: { profile_id: 'anthropic-messages', profile_revision: '1' }
};
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
export function binomialTailAtLeast(n, minimum, probability) {
  invariant(Number.isSafeInteger(n) && Number.isSafeInteger(minimum) && minimum >= 0 && minimum <= n &&
    finite(probability) && probability >= 0 && probability <= 1, 'invalid reference-only power inputs');
  if (probability === 0) return minimum === 0 ? 1 : 0;
  if (probability === 1) return 1;
  let mass = (1 - probability) ** n;
  let tail = 0;
  for (let successes = 0; successes <= n; successes++) {
    if (successes >= minimum) tail += mass;
    mass *= (n - successes) / (successes + 1) * probability / (1 - probability);
  }
  return tail;
}
const groupPaths = (group) => group.startsWith('rejected_extension/') ? ['gateway'] : ['relay', 'gateway'];
const metricList = (group) => group.includes('stream_') ? [...metricKinds, ...streamMetrics] : metricKinds;
const pathName = (group, path) => `${group}/${path}`;
export const blockCount = (group) => group === SLOW_GROUP ? SLOW_BLOCKS : BLOCKS;
export const boundForGroup = (group) => group === SLOW_GROUP
  ? { blocks: SLOW_BLOCKS, upper_index: SLOW_UPPER_INDEX, lower_index: SLOW_LOWER_INDEX, upper_statistic: 78, lower_statistic: 51 }
  : { blocks: BLOCKS, upper_index: UPPER_INDEX, lower_index: LOWER_INDEX, upper_statistic: 23, lower_statistic: 10 };
export const cBoundForGroup = (group) => group === SLOW_GROUP
  ? { blocks: SLOW_BLOCKS, upper_index: C_SLOW_UPPER_INDEX, lower_index: C_SLOW_LOWER_INDEX, upper_statistic: 82, lower_statistic: 47 }
  : { blocks: BLOCKS, upper_index: C_UPPER_INDEX, lower_index: C_LOWER_INDEX, upper_statistic: 25, lower_statistic: 8 };
const oldFiles = {
  baseline: 'docs/evidence/fidelity-performance/v1/baseline.json',
  budgets: 'docs/evidence/fidelity-performance/v1/replacement-budgets.json',
  harness: 'internal/gateway/fidelity_benchmark_test.go',
  runner: 'scripts/fidelity-benchmark.mjs',
  pairedHarness: 'internal/gateway/fidelity_paired_benchmark_test.go',
  pairedRunner: 'scripts/fidelity-paired-r10.mjs',
  r5Manifest: 'docs/evidence/fidelity-performance/source-paired-v2/manifest-r5.json',
  r6Manifest: 'docs/evidence/fidelity-performance/source-paired-v2/manifest-r6.json',
  r6Artifact: 'docs/evidence/fidelity-performance/source-paired-v2/paired-r6.json',
  r6Journal: 'docs/evidence/fidelity-performance/source-paired-v2/paired-r6.json.journal.jsonl',
  r7Manifest: 'docs/evidence/fidelity-performance/source-paired-v2/manifest-r7.json',
  r7BArtifact: 'docs/evidence/fidelity-performance/source-paired-v2/baseline-r7.json',
  r7BJournal: 'docs/evidence/fidelity-performance/source-paired-v2/baseline-r7.json.journal.jsonl',
  r8Manifest: 'docs/evidence/fidelity-performance/source-paired-v2/manifest-r8.json',
  r8BArtifact: 'docs/evidence/fidelity-performance/source-paired-v2/baseline-r8.json',
  r8BJournal: 'docs/evidence/fidelity-performance/source-paired-v2/baseline-r8.json.journal.jsonl',
  r8CArtifact: 'docs/evidence/fidelity-performance/source-paired-v2/paired-r8.json',
  r8CJournal: 'docs/evidence/fidelity-performance/source-paired-v2/paired-r8.json.journal.jsonl',
  r9Manifest: 'docs/evidence/fidelity-performance/source-paired-v2/manifest-r9.json',
  r9CArtifact: 'docs/evidence/fidelity-performance/source-paired-v2/paired-r9.json',
  r9CJournal: 'docs/evidence/fidelity-performance/source-paired-v2/paired-r9.json.journal.jsonl'
};
const B_ONLY_EVIDENCE_PATH = 'docs/evidence/fidelity-performance/source-paired-v2/baseline-r8.json';
const B_ONLY_JOURNAL_PATH = `${B_ONLY_EVIDENCE_PATH}.journal.jsonl`;
const PAIRED_EVIDENCE_PATH = 'docs/evidence/fidelity-performance/source-paired-v2/paired-r10.json';
const PAIRED_JOURNAL_PATH = `${PAIRED_EVIDENCE_PATH}.journal.jsonl`;
const METHOD_MANIFEST_PATH = 'docs/evidence/fidelity-performance/source-paired-v2/manifest-r10.json';
const METHOD_BASE_REVISION = '670408ee58e36e677571006b25107f0a4730f8f2';
const CONNECTOR_HOT_PATHS = ['internal/connectors/defaults.go', 'internal/connectors/operations.go', 'internal/connectors/profiles.go'];
const CONNECTOR_FIX_PATHS = [...CONNECTOR_HOT_PATHS, 'internal/connectors/profiles_test.go'];
const BUILD_INPUT_PREFIXES = ['internal/', 'cmd/', 'openapi/', 'tests/', 'vendor/'];
const R5_INVALID_COMMIT = 'c104953720f7817d2367eb68f99cc2e812bdf91d';
const R5_INVALID_ARTIFACT_SHA256 = '68f7bb88ce70bcc4b179179749577faf085838c0fd2696c7e3b2f9ba3b239b06';
const R5_INVALID_JOURNAL_SHA256 = '5127ea75ec3688cbadf433eae8d021ce0d473b269f571fe03face683f7dd8295';
const R6_INVALID_ARTIFACT_SHA256 = '33f5efb6dc681b2be570e5e2352e53ea35f01d8adfbaff836a05083a8bf1f023';
const R6_INVALID_JOURNAL_SHA256 = '25f76f751721fa4252d4969e5c0583ea354a1deecd34eeb13e0953552c41bdde';
const R7_B_ARTIFACT_SHA256 = '73d4701f9684789296bf51d52e54a0fbe7fd759469cb7a36e9f8995fa2687b00';
const R7_B_JOURNAL_SHA256 = '3d10f864608e2b318fabaa83ff42037dd25d82940526d5abe38c6272ed0f5308';
const R8_MANIFEST_SHA256 = '385dcf24a6a767e16cb16c66b3f6feb20f22f4d0e50b96ed1e4dda80329fca78';
const R8_B_ARTIFACT_SHA256 = 'f537f5a936eccdaaf123707689898c8fd88d197900c9fa351c53dc79a590678f';
const R8_B_JOURNAL_SHA256 = 'b6941932994aaebe3a900de79f02abe051efdf9e670677b8405b373bb17858fb';
const R8_C_ARTIFACT_SHA256 = '64f5a68119af5c00720c1e7eb740b78f84fc238199c3a94a3dad75e89ab052e6';
const R8_C_JOURNAL_SHA256 = 'ddc2909c8443ab0230e2af1665b1216cf792ca430b527c62c7cbb670ec1845a5';
const R9_MANIFEST_SHA256 = '8595b5c66211cc41309581803d964db3f0f133c76a0007f382a8569adfbd6c40';
const R9_C_ARTIFACT_SHA256 = 'db0fd64596800f08f9e237bf74dcc28fb50225b99735f14793082e2dd3e67d64';
const R9_C_JOURNAL_SHA256 = '660b8a33823de6d3e496a5ad1d9ac289705aaa282333454f0aad42dbaa95a265';
const B_METHOD_REVISION = '5ad7cda6f2864fc760b277f22cd47ce067c60f65';
const B_EVIDENCE_REVISION = '274e95a21cc78731bd32b64959cd083eeee0e227';
const C8_FAILED_EVIDENCE_HEAD = '042cf96e5d3abc38c274611388f39095ea69791d';
const C9_FAILED_EVIDENCE_HEAD = '39ad40cb704812b656a1c3be61f019c100c79848';
const PRE_OPT_PRODUCT_REVISION = 'bc325da50c575803c771531dc4e3aab1ac203a53';
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
    const count = blockCount(group);
    const indices = Array.from({ length: count }, (_, index) => index)
      .sort((a, b) => rank(seed, `variant:${group}:${a}`).localeCompare(rank(seed, `variant:${group}:${b}`)));
    const variant = new Map(indices.map((index, ordinal) => [index, ordinal < count / 2 ? 'ABBA' : 'BAAB']));
    const outer = new Map();
    for (const treatment of ['ABBA', 'BAAB']) {
      const placed = indices.filter((index) => variant.get(index) === treatment)
        .sort((a, b) => rank(seed, `outer:${group}:${a}`).localeCompare(rank(seed, `outer:${group}:${b}`)));
      placed.forEach((index, ordinal) => outer.set(index, group.startsWith('rejected_extension/') ? 'gateway' : ordinal < count / 4 ? 'relay' : 'gateway'));
    }
    return Array.from({ length: count }, (_, index) => ({ group, index, variant: variant.get(index), outer: outer.get(index) }));
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
export function validateStrictProviderContracts(contracts) {
  invariant(isDeepStrictEqual(contracts, strictProviderContracts), 'C requires exact versioned native and Anthropic provider profiles');
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
    isDeepStrictEqual(Object.fromEntries(expectedNames.map((name) => [name, budgets.workloads[name].exact])), r5Manifest.exact), 'r8 changed an old workload, metric, margin or exact outcome');
  const r5Artifact = committedEvidence(root, R5_INVALID_COMMIT, 'docs/evidence/fidelity-performance/source-paired-v2/baseline.json');
  const r5Journal = committedEvidence(root, R5_INVALID_COMMIT, 'docs/evidence/fidelity-performance/source-paired-v2/baseline.json.journal.jsonl');
  invariant(hash(r5Artifact) === R5_INVALID_ARTIFACT_SHA256 && hash(r5Journal) === R5_INVALID_JOURNAL_SHA256, 'r5 invalid evidence hashes changed');
  const r5Observed = JSON.parse(r5Artifact.toString());
  invariant(r5Observed.status === 'invalid' && r5Observed.blocks.length === 151 && r5Observed.C === null, 'r5 invalid B-only outcome changed');
  const r6Manifest = json(full('r6Manifest'));
  invariant(digest(full('r6Manifest')) === 'ca611cb5ad4f7716039319d910f561e1f2bf72ff0754ee0d3ee79179ef0d563a', 'r6 frozen manifest changed');
  invariant(isDeepStrictEqual(comparisons, r6Manifest.comparisons) && isDeepStrictEqual(addedLatency, r6Manifest.added_latency) &&
    isDeepStrictEqual(Object.fromEntries(expectedNames.map((name) => [name, budgets.workloads[name].exact])), r6Manifest.exact), 'r8 changed r6 numeric margins, inventory or exact outcome');
  invariant(digest(full('r6Artifact')) === R6_INVALID_ARTIFACT_SHA256 && digest(full('r6Journal')) === R6_INVALID_JOURNAL_SHA256, 'r6 invalid evidence hashes changed');
  const r6Observed = json(full('r6Artifact'));
  invariant(r6Observed.status === 'invalid' && r6Observed.blocks.length === 0 &&
    r6Observed.C?.revision === PRE_OPT_PRODUCT_REVISION && r6Observed.journal_sha256 === R6_INVALID_JOURNAL_SHA256 &&
    r6Observed.error === 'C: oracle, effect count or fixed sample count failed ', 'r6 adverse first-C result changed');
  const r7Manifest = json(full('r7Manifest'));
  invariant(digest(full('r7Manifest')) === '5fcaae31ab22fb0f89c8cf43906d052e6d38b229efb5ec5a3f363933d33394aa', 'r7 frozen manifest changed');
  invariant(isDeepStrictEqual(comparisons, r7Manifest.comparisons) && isDeepStrictEqual(addedLatency, r7Manifest.added_latency) &&
    isDeepStrictEqual(Object.fromEntries(expectedNames.map((name) => [name, budgets.workloads[name].exact])), r7Manifest.exact), 'r8 changed r7 numeric margins, inventory or exact outcome');
  invariant(digest(full('r7BArtifact')) === R7_B_ARTIFACT_SHA256 && digest(full('r7BJournal')) === R7_B_JOURNAL_SHA256, 'r7 B evidence hashes changed');
  const r7B = json(full('r7BArtifact'));
  const r7Failure = 'native_slow_stream_64/c1/relay max-inter-event-gap-p99-us: B-only median 4317.099 > frozen 3544';
  invariant(r7B.status === 'complete' && r7B.mode === 'B-only' && r7B.blocks.length === 384 &&
    r7B.semantic_gate.length === 22 && r7B.C === null && r7B.analysis?.passed === false &&
    isDeepStrictEqual(r7B.analysis.failures, [r7Failure]) && r7B.journal_sha256 === R7_B_JOURNAL_SHA256, 'r7 inconclusive B-only outcome changed');
  const r8Manifest = json(full('r8Manifest'));
  invariant(digest(full('r8Manifest')) === R8_MANIFEST_SHA256 &&
    isDeepStrictEqual(r8.makeManifest(root), r8Manifest), 'original r8 method or manifest changed');
  invariant(isDeepStrictEqual(comparisons, r8Manifest.comparisons) && isDeepStrictEqual(addedLatency, r8Manifest.added_latency) &&
    isDeepStrictEqual(Object.fromEntries(expectedNames.map((name) => [name, budgets.workloads[name].exact])), r8Manifest.exact),
  'r10 changed old workload, metric, margin or exact outcome');
  invariant(digest(full('r8BArtifact')) === R8_B_ARTIFACT_SHA256 && digest(full('r8BJournal')) === R8_B_JOURNAL_SHA256, 'sealed B8 bytes changed');
  const B8 = json(full('r8BArtifact'));
  invariant(B8.schema === r8.SCHEMA && B8.status === 'complete' && B8.mode === 'B-only' &&
    B8.B?.revision === B_METHOD_REVISION && B8.C === null && B8.blocks.length === 480 &&
    B8.semantic_gate.length === 22 && B8.journal_sha256 === R8_B_JOURNAL_SHA256 &&
    B8.manifest_sha256 === hash(JSON.stringify(r8Manifest)), 'sealed B8 identity or count changed');
  invariant(r8.analyzeBaseline(B8, r8Manifest).passed && r8.verifyJournal(B8, full('r8BJournal')),
    'sealed B8 no longer passes original r8 comparator and journal');
  invariant(digest(full('r8CArtifact')) === R8_C_ARTIFACT_SHA256 && digest(full('r8CJournal')) === R8_C_JOURNAL_SHA256,
    'failed C8 bytes changed');
  const C8 = json(full('r8CArtifact'));
  invariant(C8.status === 'failed' && C8.blocks.length === 0 && C8.semantic_gate.length === 2 &&
    C8.C?.revision === '21c403b6ebfc5927250cd5e7dab840d77b3422b3' &&
    C8.C_provider_contract === null && C8.failure?.stage === 'benchmark_setup_or_sample_count' &&
    C8.failure?.name === 'native_unary/c1/gateway' && C8.journal_sha256 === R8_C_JOURNAL_SHA256,
  'failed C8 setup/gate outcome changed');
  const schedule = sourceSchedule();
  invariant(SEED === r8Manifest.seed && hash(JSON.stringify(schedule)) === r8Manifest.schedule_sha256 &&
    isDeepStrictEqual(Object.fromEntries(groups.map((group) => [group, blockCount(group)])), r8Manifest.blocks_per_group),
  'r10 changed the sealed B8 seed, order or block count');
  const r9Manifest = json(full('r9Manifest'));
  invariant(digest(full('r9Manifest')) === R9_MANIFEST_SHA256 &&
    isDeepStrictEqual(r9.makeManifest(root), r9Manifest), 'original r9 method or manifest changed');
  invariant(isDeepStrictEqual(comparisons, r9Manifest.comparisons) &&
    isDeepStrictEqual(addedLatency, r9Manifest.added_latency) &&
    isDeepStrictEqual(strictRouteContracts, r9Manifest.C_route_contract) &&
    isDeepStrictEqual(strictProviderContracts, r9Manifest.C_provider_contract),
  'r10 changed old numeric margins or strict route/provider profiles');
  invariant(digest(full('r9CArtifact')) === R9_C_ARTIFACT_SHA256 &&
    digest(full('r9CJournal')) === R9_C_JOURNAL_SHA256, 'failed C9 evidence bytes changed');
  const C9 = json(full('r9CArtifact'));
  invariant(C9.schema === r9.SCHEMA && C9.status === 'complete' && C9.blocks.length === 480 &&
    C9.semantic_gate.length === 44 && C9.C?.revision === '7445289f4adf3312ea64fbe7118caf6be62d4d5b' &&
    C9.provenance?.evidence_commit === B_EVIDENCE_REVISION &&
    C9.provenance?.product_revision === PRE_OPT_PRODUCT_REVISION &&
    C9.analysis?.status === 'failed' && C9.analysis?.controls.length === 0 &&
    C9.analysis?.primaries.length === 8 && C9.analysis?.paired_B_signed_sham.passed === true &&
    C9.analysis?.C_gateway_absolute_old_L.metrics === 120 &&
    C9.analysis?.C_gateway_absolute_old_L.failures.length === 7 &&
    isDeepStrictEqual(C9.C_route_contract, strictRouteContracts) &&
    isDeepStrictEqual(C9.C_provider_contract, strictProviderContracts) &&
    C9.journal_sha256 === R9_C_JOURNAL_SHA256,
  'failed C9 full-timing outcome or sealed method changed');
  return {
    schema: `${SCHEMA}-manifest`, method_version: 6, manifest_revision: 'post-C9-connector-optimization-one-shot-r10-nul-inventory-corrected-pre-data', attempt_id: 'source-paired-r10',
    supersedes_manifest_sha256: R9_MANIFEST_SHA256,
    abandoned_pre_data_drafts: [
      { commit: '4875e86bfd669eb0a9161a2813ec8d2ad5a26311',
        status: 'abandoned before C10 build, timing or reservation',
        correction: 'bind the complete journal reservation header and lock all tracked Go source/test inputs' },
      { commit: 'a8f84804b1f9fad71a9cfbf0a44f36a86b2c3a3c',
        status: 'abandoned before C10 build, timing or reservation',
        correction: 'compare M10 tracked Go source/test blobs with its pinned direct parent, not the earlier bc325 product revision' },
      { commit: '7ea650b75457c0d570d1f12d994917f8abb5a307',
        status: 'abandoned before C10 build, timing or reservation',
        correction: 'check every introduced pre-P10 commit, including merged side histories, so a tracked Go change and revert cannot escape the source lock' },
      { commit: 'ab3856525fa8bb35f2c798631d8bc1901abf6018',
        status: 'abandoned before C10 build, timing or reservation',
        correction: 'extend the full-history lock to tracked Go build inputs and constrain P10 to exact prepared connector paths with real production blob changes' },
      { commit: '95e3c3ed993ffac293f22aa0ed2a357552d80380',
        status: 'abandoned before C10 build, timing or reservation',
        correction: 'parse all tracked Git tree paths as NUL-delimited records so quoted unusual filenames cannot evade the build-input prefix lock' }
    ],
    previous_attempts: {
      r6: { artifact_path: oldFiles.r6Artifact, artifact_sha256: R6_INVALID_ARTIFACT_SHA256,
        journal_path: oldFiles.r6Journal, journal_sha256: R6_INVALID_JOURNAL_SHA256, completed_blocks: 0, status: 'invalid' },
      r7: { artifact_path: oldFiles.r7BArtifact, artifact_sha256: R7_B_ARTIFACT_SHA256,
        journal_path: oldFiles.r7BJournal, journal_sha256: R7_B_JOURNAL_SHA256,
        C_observations: 0, B_only_passed: false, failure: r7Failure },
      r8: { artifact_path: oldFiles.r8CArtifact, artifact_sha256: R8_C_ARTIFACT_SHA256,
        journal_path: oldFiles.r8CJournal, journal_sha256: R8_C_JOURNAL_SHA256,
        status: 'failed', completed_blocks: 0, semantic_gate_passed_entries: 2,
        failure: 'C/gateway native_unary/c1/gateway: explicit versioned provider profile omitted; diagnostic setup failure, no numeric result' },
      r9: { artifact_path: oldFiles.r9CArtifact, artifact_sha256: R9_C_ARTIFACT_SHA256,
        journal_path: oldFiles.r9CJournal, journal_sha256: R9_C_JOURNAL_SHA256,
        status: 'complete', completed_blocks: 480, semantic_gate_passed_entries: 44,
        B_controls_failed: 0, paired_primaries_failed: 8, hard_C_gateway_old_L_failed: 7, decision: 'failed' }
    },
    sequential_rule: 'R6 C failed before complete numeric blocks; R7 had no C; R8 C failed during semantic gate; C9 completed 480 blocks and failed eight paired primaries and seven hard C gateway old-L limits. R10 is one prospective write-once C opportunity reusing exactly sealed B8. Only C10 paired primary tightens to n32 d25 one-sided alpha 0.0010512007866 and slow/c1 n128 d82 alpha 0.000931234262, relay symmetric lower d8/d47; B8 and paired B signed sham retain r8 d23/d78 and lower d10/d51. Conservative per-metric prior alpha sum 0.0486130202 plus max new alpha 0.0010512008 = 0.049664221 < 0.05, counting unused numeric looks. No across-metric familywise claim, no r10 retry. Hard C gateway absolute L is unchanged and separate from the sign bound.',
    host_stability: hostStabilityRule,
    B_only_evidence_path: B_ONLY_EVIDENCE_PATH, B_only_journal_path: B_ONLY_JOURNAL_PATH,
    paired_evidence_path: PAIRED_EVIDENCE_PATH, paired_journal_path: PAIRED_JOURNAL_PATH,
    reused_B8: { manifest_path: oldFiles.r8Manifest, manifest_sha256: R8_MANIFEST_SHA256,
      artifact_path: oldFiles.r8BArtifact, artifact_sha256: R8_B_ARTIFACT_SHA256,
      journal_path: oldFiles.r8BJournal, journal_sha256: R8_B_JOURNAL_SHA256,
      method_revision: B_METHOD_REVISION, evidence_revision: B_EVIDENCE_REVISION,
      decision: 'pass all 254 signed B controls under original r8 manifest; no B10 capture or seed choice' },
    chronology: 'B stays at sealed M8 with exact untracked B8 reservations. E8 and failed-C9 evidence strictly precede corrected M10, a normal method commit directly after the pinned PR base; M10 tracked Go and build-input blobs/modes must equal that direct parent. Every commit newly introduced into P10 parent ancestry after M10, including merged side histories and merge results, must retain the complete M10 tracked build-input inventory. A later normal P10 may modify only the four pinned connector fix paths and must change a production connector blob, not only its mode. Timed C10 must be exactly P10, with all tracked Go source/test blobs and every other tree input unchanged; later descendants may only perform offline comparison against the recorded P10 revision.',
    C_route_contract: strictRouteContracts, C_provider_contract: strictProviderContracts,
    B_product_revision: B_PRODUCT, pre_optimization_product_revision: PRE_OPT_PRODUCT_REVISION,
    C8_failed_evidence_head: C8_FAILED_EVIDENCE_HEAD, C9_failed_evidence_head: C9_FAILED_EVIDENCE_HEAD,
    method_base_revision: METHOD_BASE_REVISION, method_manifest_path: METHOD_MANIFEST_PATH,
    required_product_paths: CONNECTOR_FIX_PATHS,
    tracked_build_input_scope: { directory_prefixes: BUILD_INPUT_PREFIXES, root_files: ['go.mod', 'go.sum'], root_glob: 'go.work*' },
    C_revision_rule: 'recorded C10 revision must equal the single later normal P10 product commit exactly',
    old_baseline_sha256: digest(full('baseline')), old_budgets_sha256: digest(full('budgets')),
    old_harness_sha256: digest(full('harness')), old_runner_sha256: digest(full('runner')),
    paired_harness_sha256: digest(full('pairedHarness')), paired_runner_sha256: digest(full('pairedRunner')),
    seed: SEED, blocks_per_group: r8Manifest.blocks_per_group, samples_per_subrun: SAMPLES,
    semantic_gate_order_sha256: { B_only: r8Manifest.semantic_gate_order_sha256.B_only,
      paired: hash(JSON.stringify(semanticGateOrder(true))) },
    order: r8Manifest.order, schedule_sha256: r8Manifest.schedule_sha256,
    bound: {
      B_signed_sham: { ordinary: r8Manifest.bound.ordinary, slow_c1: r8Manifest.bound.slow_c1,
        rule: r8Manifest.bound.B_sham },
      C_primary_ordinary: { blocks: 32, lower_order_statistic: 8, upper_order_statistic: 25,
        upper_coverage: 0.9989487992133945 },
      C_primary_slow_c1: { group: SLOW_GROUP, blocks: 128, lower_order_statistic: 47,
        upper_order_statistic: 82, upper_coverage: 0.9990687657378505 },
      primary: 'paired C-minus-B d_(25)<M at n32 and d_(82)<M at slow/c1 n128; relay lower d_(8)>-M or d_(47)>-M',
      B_sham: 'both sealed B8 and contemporary paired B use original r8 signed two-B d23/d78 and d10/d51 bounds',
      C_gateway_absolute: 'all 120 contemporary C gateway medians <= original v1 L; separate hard gate from sign CI',
      B_and_relay_absolute: 'old v1 absolute L for B and relay C is descriptive with all misses visible'
    },
    comparisons, added_latency: addedLatency,
    exact: Object.fromEntries(expectedNames.map((name) => [name, budgets.workloads[name].exact])),
    old_hardware: r8Manifest.old_hardware, old_toolchain: r8Manifest.old_toolchain,
    old_go_build_environment: r8Manifest.old_go_build_environment,
    old_runtime_environment: r8Manifest.old_runtime_environment,
    conditions: { ...r8Manifest.conditions,
      baseline: 'reuse complete B8 under its own frozen r8 manifest, exact M8 binary hash, seed, schedule and B sham; no B10',
      candidate_provider: 'exact explicit compatible-chat/1 for native and anthropic-messages/1 for translated/rejected',
      timing: 'same B8 480-block schedule and 64 requests/subrun; C10 paired capture one-shot after full fixed gate and host preflight' }
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
  invariant(mode === 'paired', 'r10 reserves only the fixed one-shot C10 paired output');
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

function gitBlobs(root, revision, paths, cache) {
  if (!cache.has(revision)) {
    const entries = new Map();
    for (const line of command('git', ['ls-tree', '-r', revision, '--', ...paths], root).split('\n').filter(Boolean)) {
      const tab = line.indexOf('\t');
      invariant(tab > 0, 'malformed git tree entry while checking immutable evidence');
      const [mode, type, oid] = line.slice(0, tab).split(/\s+/);
      invariant(mode && type === 'blob' && /^[0-9a-f]{40}$/.test(oid), 'non-blob immutable evidence path');
      entries.set(line.slice(tab + 1), oid);
    }
    cache.set(revision, entries);
  }
  return cache.get(revision);
}

export function reservationHeader(capture) {
  const { analysis, completed_at, diagnostics_after, journal_sha256, error, failure, blocks, semantic_gate, ...reserved } = capture;
  return { ...reserved, status: 'in_progress', semantic_gate: [] };
}

export function verifyJournal(capture, journalPath) {
  invariant(capture.journal_sha256 === digest(journalPath), 'capture does not bind its exact reservation journal bytes');
  const lines = readFileSync(journalPath, 'utf8').trimEnd().split('\n').map((line) => JSON.parse(line));
  invariant(lines.length === capture.blocks.length + 3, 'reservation journal gate/block count or terminal record changed');
  const header = lines[0]?.header;
  invariant(isDeepStrictEqual(header, reservationHeader(capture)),
    'reservation journal header differs from all captured pre-timing conditions');
  invariant(isDeepStrictEqual(lines[1]?.semantic_gate, capture.semantic_gate), 'reservation journal semantic gate differs from JSON artifact');
  for (let index = 0; index < capture.blocks.length; index++) {
    invariant(isDeepStrictEqual(lines[index + 2]?.block, capture.blocks[index]), `reservation journal block ${index} differs from JSON artifact`);
  }
  invariant(lines.at(-1)?.completion?.status === 'complete' &&
    lines.at(-1).completion.block_count === capture.blocks.length, 'reservation journal lacks a complete terminal record');
  return true;
}

function trackedTreeEntries(root, revision) {
  const result = spawnSync('git', ['ls-tree', '-r', '-z', revision], { cwd: root, maxBuffer: 128 << 20 });
  invariant(result.status === 0, `git ls-tree failed: ${result.stderr}`);
  const bytes = result.stdout;
  const entries = [];
  let start = 0;
  for (let end = 0; end < bytes.length; end++) {
    if (bytes[end] !== 0) continue;
    const record = bytes.subarray(start, end);
    const tab = record.indexOf(9);
    invariant(tab > 0, 'tracked tree entry is malformed');
    const path = record.subarray(tab + 1).toString('utf8');
    const decoded = record.toString('utf8');
    const identity = Buffer.from(decoded, 'utf8').equals(record) ? decoded : `raw:${record.toString('hex')}`;
    entries.push({ path, identity });
    start = end + 1;
  }
  invariant(start === bytes.length, 'tracked tree output lacks its final NUL separator');
  return entries;
}

export function trackedGoInventory(root, revision) {
  return trackedTreeEntries(root, revision).filter(({ path }) => path.endsWith('.go')).map(({ identity }) => identity);
}

export function trackedBuildInputInventory(root, revision) {
  return trackedTreeEntries(root, revision).filter(({ path }) => {
    return BUILD_INPUT_PREFIXES.some((prefix) => path.startsWith(prefix)) ||
      path === 'go.mod' || path === 'go.sum' || path.startsWith('go.work');
  }).map(({ identity }) => identity);
}

function productionGoInventory(root, revision) {
  return trackedGoInventory(root, revision).filter((line) =>
    !line.slice(line.indexOf('\t') + 1).endsWith('_test.go'));
}

export function requireTrackedGoLock(actual, expected) {
  invariant(isDeepStrictEqual(actual, expected), 'locked C10 tracked Go source/test blobs differ from P10');
  return true;
}

export function requireMethodGoLock(actual, expected) {
  invariant(isDeepStrictEqual(actual, expected), 'M10 must not change any tracked Go source or test file');
  return true;
}

export function requireMethodBuildInputLock(actual, expected) {
  invariant(isDeepStrictEqual(actual, expected), 'M10 must not change any tracked build input blob or mode');
  return true;
}

export function requirePreProductBuildInputHistory(root, methodCommit, productParent) {
  invariant(gitSucceeds(['merge-base', '--is-ancestor', methodCommit, productParent], root),
    'P10 parent must descend from M10');
  const methodInputs = trackedBuildInputInventory(root, methodCommit);
  // Rev-list includes commits merged from side branches as well as first-parent
  // commits. Checking every tree catches an input change later reverted before P10.
  const introduced = command('git', ['rev-list', productParent, '--not', methodCommit], root).split('\n').filter(Boolean);
  for (const revision of introduced) {
    invariant(isDeepStrictEqual(trackedBuildInputInventory(root, revision), methodInputs),
      `${revision}: tracked build input blob or mode changed before P10`);
  }
  return true;
}

export function requireExactProductRevision(CRevision, productCommit) {
  invariant(CRevision === productCommit, 'timed C10 revision must be the exact normal P10 product commit');
  return true;
}

export function chooseProductCommit(commits, requiredPaths = CONNECTOR_FIX_PATHS) {
  const candidates = [];
  for (const { revision, parents, changed } of commits) {
    if (parents.length !== 1) continue;
    const affected = changed.filter((path) => CONNECTOR_HOT_PATHS.includes(path));
    if (!affected.length) continue;
    invariant(changed.every((path) => requiredPaths.includes(path)),
      'P10 changed a path outside the exact pinned connector fix set');
    candidates.push({ revision, affected });
  }
  invariant(candidates.length === 1, 'C10 requires exactly one later normal P10 connector hot-path product commit after M10');
  return candidates[0];
}

export function changedEntries(root, parent, revision) {
  const result = spawnSync('git', ['diff-tree', '--no-commit-id', '--name-status', '--no-renames', '--ignore-submodules=none', '-r', '-z', parent, revision],
    { cwd: root, encoding: 'utf8', maxBuffer: 1 << 20 });
  invariant(result.status === 0, `git diff-tree failed: ${result.stderr}`);
  const fields = result.stdout.split('\0');
  invariant(fields.pop() === '' && fields.length % 2 === 0, 'P10 path diff is malformed');
  return Array.from({ length: fields.length / 2 }, (_, index) => ({ status: fields[2 * index], path: fields[2 * index + 1] }));
}

function treeBlob(root, revision, path) {
  const line = command('git', ['ls-tree', revision, '--', path], root);
  const match = /^(\d+) blob ([0-9a-f]{40})\t(.+)$/.exec(line);
  return match?.[3] === path ? { mode: match[1], oid: match[2] } : null;
}

export function validateProductCommit(root, parent, revision, allowedPaths = CONNECTOR_FIX_PATHS) {
  const entries = changedEntries(root, parent, revision);
  invariant(entries.length > 0 && entries.every(({ status, path }) => status === 'M' && allowedPaths.includes(path)),
    'P10 must modify only the exact four pinned connector fix paths');
  const blobs = entries.map(({ path }) => ({ path, before: treeBlob(root, parent, path), after: treeBlob(root, revision, path) }));
  invariant(blobs.every(({ before, after }) => before?.mode === '100644' && after?.mode === '100644'),
    'P10 connector fix paths must retain regular-file mode');
  invariant(blobs.some(({ path, before, after }) => CONNECTOR_HOT_PATHS.includes(path) && before.oid !== after.oid),
    'P10 must change a pinned production connector blob, not only file mode');
  return entries.map(({ path }) => path);
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
  const methodPaths = [manifest.method_manifest_path, oldFiles.pairedRunner];
  const priorCandidatePaths = [oldFiles.r9CArtifact, oldFiles.r9CJournal];
  const immutablePaths = [trackedPath, manifest.B_only_journal_path, ...priorCandidatePaths, ...methodPaths];
  const cache = new Map();
  const blobAt = (revision, path) => gitBlobs(root, revision, immutablePaths, cache).get(path) || null;
  invariant(blobAt(CRevision, trackedPath) === baselineBlob, 'locked C does not contain the exact B-only artifact blob');
  invariant(blobAt(CRevision, manifest.B_only_journal_path) === journalBlob, 'locked C does not contain the exact B-only journal blob');
  const graph = command('git', ['rev-list', '--parents', CRevision], root).split('\n').filter(Boolean)
    .map((line) => { const [revision, ...parents] = line.split(/\s+/); return { revision, parents }; });
  const creations = Object.fromEntries(immutablePaths.map((path) => [path, []]));
  for (const { revision, parents } of graph) {
    for (const path of immutablePaths) {
      const current = blobAt(revision, path);
      const before = parents.map((parent) => blobAt(parent, path));
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
  invariant(evidenceCommit === manifest.reused_B8.evidence_revision, 'C10 must reuse the single sealed E8 evidence commit');
  invariant(graph.find((item) => item.revision === evidenceCommit)?.parents.length === 1, 'B-only evidence must be created in a normal commit, not a merge');
  invariant(blobAt(evidenceCommit, trackedPath) === baselineBlob &&
    blobAt(evidenceCommit, manifest.B_only_journal_path) === journalBlob, 'evidence creation commit has different JSON or journal bytes');
  invariant(evidenceCommit !== CRevision && gitSucceeds(['merge-base', '--is-ancestor', evidenceCommit, CRevision], root), 'B-only evidence must be a strict ancestor of C');
  invariant(gitSucceeds(['merge-base', '--is-ancestor', manifest.C8_failed_evidence_head, CRevision], root), 'C10 must descend from retained failed-C8 evidence');
  invariant(gitSucceeds(['merge-base', '--is-ancestor', manifest.C9_failed_evidence_head, CRevision], root), 'C10 must descend from retained failed-C9 evidence');
  invariant(priorCandidatePaths.every((path) => creations[path].length === 1 &&
    creations[path][0] === manifest.C9_failed_evidence_head &&
    blobAt(CRevision, path) === command('git', ['hash-object', resolve(root, path)], root)),
  'C9 failed JSON and journal must originate unchanged in the pinned normal evidence commit');
  invariant(graph.find((item) => item.revision === manifest.C9_failed_evidence_head)?.parents.length === 1,
    'C9 failed evidence must be created in a normal commit');
  const manifestCreations = creations[manifest.method_manifest_path];
  const runnerCreations = creations[oldFiles.pairedRunner];
  invariant(manifestCreations.length === 1 && runnerCreations.length === 1 && manifestCreations[0] === runnerCreations[0],
    'R10 manifest and runner must be created together in one method commit');
  const methodCommit = manifestCreations[0];
  const methodParents = graph.find((item) => item.revision === methodCommit)?.parents;
  invariant(isDeepStrictEqual(methodParents, [manifest.method_base_revision]), 'M10 must be a normal direct-child commit of the pinned pre-method PR base');
  invariant(gitSucceeds(['merge-base', '--is-ancestor', evidenceCommit, methodCommit], root) &&
    gitSucceeds(['merge-base', '--is-ancestor', manifest.C9_failed_evidence_head, methodCommit], root),
  'E8 and failed-C9 evidence must strictly precede M10');
  invariant(methodCommit !== CRevision && gitSucceeds(['merge-base', '--is-ancestor', methodCommit, CRevision], root),
    'M10 must be a strict ancestor of locked C10');
  for (const path of methodPaths) {
    const currentBlob = command('git', ['hash-object', resolve(root, path)], root);
    invariant(blobAt(methodCommit, path) === currentBlob &&
      blobAt(CRevision, path) === currentBlob, `${path}: R10 method bytes changed after M10`);
  }
  requireMethodGoLock(trackedGoInventory(root, methodCommit),
    trackedGoInventory(root, manifest.method_base_revision));
  requireMethodBuildInputLock(trackedBuildInputInventory(root, methodCommit),
    trackedBuildInputInventory(root, manifest.method_base_revision));
  const onPath = command('git', ['rev-list', '--ancestry-path', `${methodCommit}..${CRevision}`], root).split('\n').filter(Boolean);
  const productChanges = onPath.map((revision) => {
    const parents = graph.find((entry) => entry.revision === revision)?.parents || [];
    const changed = parents.length === 1 ? changedEntries(root, parents[0], revision).map(({ path }) => path) : [];
    return { revision, parents, changed };
  });
  const product = chooseProductCommit(productChanges, manifest.required_product_paths);
  const productCommit = product.revision;
  requireExactProductRevision(CRevision, productCommit);
  const methodInventory = productionGoInventory(root, methodCommit);
  const productParent = graph.find((entry) => entry.revision === productCommit)?.parents[0];
  requirePreProductBuildInputHistory(root, methodCommit, productParent);
  const productChangedPaths = validateProductCommit(root, productParent, productCommit, manifest.required_product_paths);
  const productInventory = productionGoInventory(root, productCommit);
  invariant(!isDeepStrictEqual(productInventory, methodInventory), 'P10 did not change production Go blobs');
  invariant(isDeepStrictEqual(productionGoInventory(root, CRevision), productInventory), 'locked C10 production Go blobs differ from P10');
  const productAllGo = trackedGoInventory(root, productCommit);
  requireTrackedGoLock(trackedGoInventory(root, CRevision), productAllGo);
  return { B_only_path: trackedPath, B_only_sha256: baselineSHA256, B_only_blob_oid: baselineBlob,
    B_journal_path: manifest.B_only_journal_path, B_journal_sha256: journalSHA256, B_journal_blob_oid: journalBlob,
    evidence_commit: evidenceCommit, method_commit: methodCommit,
    product_revision: productCommit, product_changed_paths: productChangedPaths,
    product_go_inventory_sha256: hash(JSON.stringify(productInventory)),
    tracked_go_inventory_sha256: hash(JSON.stringify(productAllGo)),
    C_revision: CRevision };
}

export function verifyPairedProvenance(capture, baseline, manifest, root, baselinePath) {
  invariant(capture.B_only_sha256 === R8_B_ARTIFACT_SHA256 && capture.B_only_sha256 === digest(baselinePath) &&
    capture.B_only_sha256 === hash(JSON.stringify(baseline, null, 2) + '\n'), 'sealed B8 artifact bytes differ from capture');
  invariant(baseline.B?.revision === manifest.reused_B8.method_revision &&
    baseline.B?.binary_sha256 === capture.B?.binary_sha256, 'C10 changed sealed B8 method or binary');
  r8.verifyJournal(baseline, resolve(root, manifest.B_only_journal_path));
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
  invariant(unexpected.length === 0, `${B ? 'B8' : 'C10'} source tree is dirty: ${unexpected.join(', ')}`);
  invariant(digest(resolve(root, oldFiles.harness)) === manifest.old_harness_sha256, 'frozen v1 harness differs');
  invariant(digest(resolve(root, oldFiles.runner)) === manifest.old_runner_sha256, 'frozen v1 runner differs');
  invariant(digest(resolve(root, oldFiles.baseline)) === manifest.old_baseline_sha256, 'frozen v1 baseline differs');
  invariant(digest(resolve(root, oldFiles.budgets)) === manifest.old_budgets_sha256, 'frozen v1 budgets differ');
  invariant(digest(resolve(root, oldFiles.pairedHarness)) === manifest.paired_harness_sha256, 'paired Go adapter differs');
  invariant(command('git', ['merge-base', manifest.B_product_revision, 'HEAD'], root) === manifest.B_product_revision,
    `${B ? 'B8' : 'C10'} does not descend from historical product`);
  if (B) {
    invariant(revision === manifest.reused_B8.method_revision, 'B8 must remain at exact sealed M8 method revision');
    const reservations = [manifest.B_only_evidence_path, manifest.B_only_journal_path];
    invariant(status.split('\n').filter(Boolean).length === 2 && reservations.every((path) => status.split('\n').includes(`?? ${path}`)),
      'B8 must retain exactly its two untracked original reservations');
    const frozenR8 = json(resolve(root, oldFiles.r8Manifest));
    invariant(digest(resolve(root, oldFiles.r8Manifest)) === R8_MANIFEST_SHA256 &&
      isDeepStrictEqual(r8.makeManifest(root), frozenR8), 'B8 original r8 method changed');
    invariant(digest(resolve(root, 'scripts/fidelity-paired-r8.mjs')) === frozenR8.paired_runner_sha256 &&
      digest(resolve(root, oldFiles.pairedHarness)) === frozenR8.paired_harness_sha256,
    'B8 runner or adapter differs from original r8 method');
  } else {
    invariant(digest(resolve(root, oldFiles.pairedRunner)) === manifest.paired_runner_sha256, 'C10 runner differs');
    invariant(gitSucceeds(['merge-base', '--is-ancestor', manifest.C9_failed_evidence_head, revision], root),
      'C10 must descend from failed-C9 evidence commit');
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
  invariant(!baselineOnly, 'r10 is C-only and cannot record a new B baseline');
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
    validateStrictProviderContracts(capture.C_provider_contract);
    invariant(isDeepStrictEqual(capture.C_provider_contract, manifest.C_provider_contract), 'C provider profile overlay differs from frozen r10 manifest');
    invariant(capture.B_only_sha256 === R8_B_ARTIFACT_SHA256 && capture.B?.revision === B_METHOD_REVISION,
      'sealed B8 artifact or method identity missing');
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
export function signedBValues(blocks, path, metric) {
  return blocks.map((block) => {
    const pair = block.subruns.filter((run) => run.arm === `B/${path}`);
    invariant(pair.length === 2, `${block.group}: expected exactly two B/${path} observations`);
    const direction = block.variant === 'ABBA' ? 1 : -1;
    return direction * (pair[1].reply.metrics[metric] - pair[0].reply.metrics[metric]);
  });
}

export function signedAddedBValues(blocks, metric) {
  return blocks.map((block) => {
    const relay = block.subruns.filter((run) => run.arm === 'B/relay');
    const gateway = block.subruns.filter((run) => run.arm === 'B/gateway');
    invariant(relay.length === 2 && gateway.length === 2, `${block.group}: expected two B observations per added-latency path`);
    const direction = block.variant === 'ABBA' ? 1 : -1;
    const first = gateway[0].reply.metrics[metric] - relay[0].reply.metrics[metric];
    const second = gateway[1].reply.metrics[metric] - relay[1].reply.metrics[metric];
    return direction * (second - first);
  });
}

function shamResult(values, old, group) {
  const bound = boundForGroup(group);
  invariant(values.length === bound.blocks && values.every(finite), `${group}: incomplete signed two-B sham`);
  const sorted = [...values].sort((a, b) => a - b);
  const lower = sorted[bound.lower_index], upper = sorted[bound.upper_index];
  return { values, median: median(values), lower, upper, ...old,
    lower_passed: lower > -old.margin, upper_passed: upper < old.margin,
    passed: lower > -old.margin && upper < old.margin,
    method: 'signed_B2_minus_B1_ABBA_positive_BAAB_negative', control_scope: 'two_B_order_and_stability_only' };
}

export function analyzeSignedBSham(capture, manifest) {
  const paths = {}, addedLatency = {}, failures = [];
  for (const group of groups) {
    const blocks = capture.blocks.filter((block) => block.group === group);
    invariant(blocks.length === blockCount(group), `${group}: missing signed B sham blocks`);
    for (const path of groupPaths(group)) {
      const name = pathName(group, path);
      paths[name] = {};
      for (const [metric, old] of Object.entries(manifest.comparisons[name])) {
        const result = shamResult(signedBValues(blocks, path, metric), old, group);
        paths[name][metric] = result;
        if (!result.passed) failures.push(`${name} ${metric}: signed B sham [d_(${boundForGroup(group).lower_statistic})=${result.lower}, d_(${boundForGroup(group).upper_statistic})=${result.upper}] outside (-${old.margin}, ${old.margin})`);
      }
    }
    if (manifest.added_latency[group]) {
      addedLatency[group] = {};
      for (const [metric, old] of Object.entries(manifest.added_latency[group])) {
        const result = shamResult(signedAddedBValues(blocks, metric), old, group);
        addedLatency[group][metric] = result;
        if (!result.passed) failures.push(`${group} added ${metric}: signed B sham [d_(${boundForGroup(group).lower_statistic})=${result.lower}, d_(${boundForGroup(group).upper_statistic})=${result.upper}] outside (-${old.margin}, ${old.margin})`);
      }
    }
  }
  return { passed: failures.length === 0, failures, paths, added_latency: addedLatency,
    controls: 254, limitation: 'two B observations with ABBA/BAAB sign flip; narrow order/stability check, not a four-arm placebo' };
}

export function analyzeSealedB8(baseline, manifest, baselineManifest = json(oldFiles.r8Manifest)) {
  invariant(hash(JSON.stringify(baseline, null, 2) + '\n') === R8_B_ARTIFACT_SHA256 &&
    baseline.B?.revision === manifest.reused_B8.method_revision &&
    baseline.manifest_sha256 === hash(JSON.stringify(baselineManifest)),
  'sealed B8 JSON or original r8 manifest identity changed');
  invariant(hash(JSON.stringify(baselineManifest, null, 2) + '\n') === R8_MANIFEST_SHA256,
    'sealed B8 comparator must use the original r8 manifest');
  const result = r8.analyzeBaseline(baseline, baselineManifest);
  invariant(result.passed && result.signed_B_sham?.controls === 254,
    'sealed B8 failed its original r8 signed sham controls');
  return result;
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
function pairedResult(values, old, control, group) {
  const bound = cBoundForGroup(group);
  invariant(values.length === bound.blocks && values.every(finite), `${group}: incomplete paired statistic`);
  const sorted = [...values].sort((a, b) => a - b);
  const lower = sorted[bound.lower_index], upper = sorted[bound.upper_index];
  return { values, median: median(values), lower, upper, ...old, passed: upper < old.margin,
    ...(control ? { control_passed: lower > -old.margin && upper < old.margin } : {}) };
}
export function analyzePaired(capture, baseline, manifest) {
  validateCapture(capture, manifest, false);
  invariant(isDeepStrictEqual(capture.hardware, baseline.hardware) &&
    capture.toolchain === baseline.toolchain && capture.go_build_environment === baseline.go_build_environment &&
    isDeepStrictEqual(capture.runtime_environment, baseline.runtime_environment),
  'B8 and C10 measurement hardware, toolchain or runtime differ');
  invariant(capture.B?.revision === baseline.B?.revision && capture.B?.binary_sha256 === baseline.B?.binary_sha256, 'historical B method revision or binary changed');
  invariant(capture.B_only_sha256 === R8_B_ARTIFACT_SHA256 &&
    capture.B_only_sha256 === hash(JSON.stringify(baseline, null, 2) + '\n'), 'sealed B8 artifact reference changed');
  const BOnly = analyzeSealedB8(baseline, manifest);
  const pairedBSham = analyzeSignedBSham(capture, manifest);
  const controls = [...BOnly.failures.map((failure) => `B-only signed sham: ${failure}`),
    ...pairedBSham.failures.map((failure) => `paired B signed sham: ${failure}`)];
  const primaries = [], gatewayAbsoluteFailures = [];
  const BEnvelope = {}, results = {}, descriptiveAbsoluteC = {};
  const descriptiveBFailures = [], descriptiveCFailures = [];
  let gatewayAbsoluteMetricCount = 0;
  for (const group of groups) {
    const blocks = capture.blocks.filter((block) => block.group === group);
    invariant(blocks.length === blockCount(group), `${group}: missing paired blocks`);
    for (const path of groupPaths(group)) {
      const name = pathName(group, path), B = perName(capture, name, 'B'), C = perName(capture, name, 'C');
      invariant(B.length === 2 * blockCount(group) && C.length === 2 * blockCount(group), `${name}: missing B/C subruns`);
      results[name] = {};
      BEnvelope[name] = {};
      descriptiveAbsoluteC[name] = {};
      for (const [metric, old] of Object.entries(manifest.comparisons[name])) {
        const BMedian = median(B.map((run) => run.metrics[metric]));
        const CMedian = median(C.map((run) => run.metrics[metric]));
        BEnvelope[name][metric] = { B_median: BMedian, old_v1_limit: old.old_limit,
          within_old_v1_limit: BMedian <= old.old_limit, diagnostic_only: true };
        if (BMedian > old.old_limit) descriptiveBFailures.push(`${name} ${metric}: paired B median ${BMedian} > old v1 L ${old.old_limit}`);
        descriptiveAbsoluteC[name][metric] = { C_median: CMedian, old_v1_limit: old.old_limit,
          within_old_v1_limit: CMedian <= old.old_limit, diagnostic_only: path !== 'gateway', hard_candidate_gate: path === 'gateway' };
        if (CMedian > old.old_limit) {
          const failure = `${name} ${metric}: C median ${CMedian} > old v1 L ${old.old_limit}`;
          descriptiveCFailures.push(failure);
          if (path === 'gateway') gatewayAbsoluteFailures.push(failure);
        }
        if (path === 'gateway') gatewayAbsoluteMetricCount++;
        const result = pairedResult(pairedValues(blocks, path, metric), old, path === 'relay', group);
        result.B_median = BMedian;
        result.C_median = CMedian;
        if (metric === 'ns/op') {
          result.B_requests_per_second = 1e9 / result.B_median;
          result.C_requests_per_second = 1e9 / result.C_median;
        }
        results[name][metric] = result;
        if (!result.passed) primaries.push(`${name} ${metric}: paired d_(${cBoundForGroup(group).upper_statistic}) ${result.upper} >= frozen margin ${old.margin}`);
        if (path === 'relay' && !result.control_passed) controls.push(`${name} ${metric}: relay paired interval [d_(${cBoundForGroup(group).lower_statistic})=${result.lower}, d_(${cBoundForGroup(group).upper_statistic})=${result.upper}] outside (-${old.margin}, ${old.margin})`);
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
      const result = pairedResult(values, old, false, group);
      result.B_gateway_minus_relay_values = vectors.map(({ B }) => B);
      result.C_gateway_minus_relay_values = vectors.map(({ C }) => C);
      result.B_gateway_minus_relay_median = median(result.B_gateway_minus_relay_values);
      result.C_gateway_minus_relay_median = median(result.C_gateway_minus_relay_values);
      addedLatency[group][metric] = result;
      if (!result.passed) primaries.push(`${group} added ${metric}: paired d_(${cBoundForGroup(group).upper_statistic}) ${result.upper} >= frozen gateway margin ${old.margin}`);
    }
  }
  const timed = capture.blocks.flatMap((block) => block.subruns);
  const sumMetric = (arm, metric) => timed.filter((run) => run.arm.startsWith(`${arm}/`))
    .reduce((sum, run) => sum + run.reply.metrics[metric], 0);
  return { status: gatewayAbsoluteFailures.length ? 'failed' : controls.length ? 'inconclusive' : primaries.length ? 'failed' : 'passed',
    failures: [...primaries, ...gatewayAbsoluteFailures], controls, primaries,
    B_only: { passed: BOnly.passed, failures: BOnly.failures }, paired_B_signed_sham: pairedBSham,
    envelope: BEnvelope, results, added_latency: addedLatency,
    descriptive_absolute_B_vs_v1: { metrics: BEnvelope, within_old_limit: 224 - descriptiveBFailures.length,
      above_old_limit: descriptiveBFailures.length, above_old_limit_details: descriptiveBFailures, diagnostic_only: true },
    descriptive_absolute_C_vs_v1: { metrics: descriptiveAbsoluteC,
      within_old_limit: 224 - descriptiveCFailures.length, above_old_limit: descriptiveCFailures.length,
      above_old_limit_details: descriptiveCFailures, relay_diagnostic_only: true, gateway_hard_candidate_gate: true },
    C_gateway_absolute_old_L: { passed: gatewayAbsoluteFailures.length === 0, metrics: gatewayAbsoluteMetricCount,
      failures: gatewayAbsoluteFailures, separate_from_paired_sign_bound: true },
    count: { blocks: capture.blocks.length, source_primary_metrics: 224, added_latency_metrics: 30,
      B_successes: sumMetric('B', 'succeeded'), C_successes: sumMetric('C', 'succeeded'),
      B_dispatches: sumMetric('B', 'dispatches'), C_dispatches: sumMetric('C', 'dispatches'),
      B_rejections: sumMetric('B', 'rejected'), C_rejections: sumMetric('C', 'rejected'),
      B_semantic_gate_requests: 22 * SAMPLES, C_semantic_gate_requests: 22 * SAMPLES } };
}

async function record(mode, output, manifestPath, baselinePath, Bbinary, Broot, Cbinary, Croot) {
  invariant(mode === 'paired', 'r10 is C-only; B8 must be reused without another B capture');
  const manifest = json(manifestPath);
  validateManifest(manifest, Croot);
  const reservation = validateOutputReservation(mode, output, Croot, manifest);
  const frozenBaseline = json(baselinePath);
  const originalR8Manifest = json(resolve(Croot, oldFiles.r8Manifest));
  const baselineAnalysis = analyzeSealedB8(frozenBaseline, manifest, originalR8Manifest);
  invariant(baselineAnalysis.passed && digest(baselinePath) === R8_B_ARTIFACT_SHA256,
    'sealed B8 no longer passes its original r8 comparator');
  r8.verifyJournal(frozenBaseline, resolve(Croot, manifest.B_only_journal_path));
  {
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
    validateStrictProviderContracts(providerContract);
    invariant(isDeepStrictEqual(providerContract, manifest.C_provider_contract), 'C provider profiles differ from frozen r10 manifest');
  }
  const currentHardware = hardware();
  invariant(hardwareMatches(currentHardware, manifest.old_hardware), 'physical hardware differs from v1');
  const toolchain = command('go', ['version'], Broot);
  const goBuildEnvironment = command('go', ['env', 'GOFLAGS', 'GOAMD64', 'GOARCH', 'GOOS'], Broot);
  invariant(toolchain === manifest.old_toolchain && goBuildEnvironment === manifest.old_go_build_environment, 'toolchain/build environment differs from v1');
  const B = buildBinary(Bbinary, Broot, manifest, true, BReserved);
  invariant(B.revision === B_METHOD_REVISION && B.binary_sha256 === frozenBaseline.B.binary_sha256,
    'fresh B8 binary hash or method revision differs from sealed baseline');
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
  writeFileSync(reservation.journal, JSON.stringify({ header: reservationHeader(artifact) }) + '\n', { flag: 'wx' });
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
    const analysis = analyzePaired(artifact, frozenBaseline, manifest);
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
    invariant(resolve(args[0]) === resolve(process.cwd(), METHOD_MANIFEST_PATH), 'R10 manifest has one fixed output path');
    writeFileSync(args[0], JSON.stringify(makeManifest(process.cwd()), null, 2) + '\n', { flag: 'wx' });
    return console.log(`Frozen source r10 manifest: ${args[0]}`);
  }
  if (action === 'record-paired' && args.length === 7) return record('paired', ...args);
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
  throw new Error('Usage: fidelity-paired-r10.mjs freeze-manifest OUT | record-paired OUT MANIFEST B8_BASELINE B8_BINARY B8_METHOD_ROOT C10_BINARY C10_ROOT | compare-paired PAIRED_CAPTURE B8_CAPTURE MANIFEST C10_REPO B8_BASELINE');
}
if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try { await main(); } catch (error) { console.error(error.message); process.exitCode = 1; }
}
