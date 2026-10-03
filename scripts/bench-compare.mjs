#!/usr/bin/env node
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { parseArgs } from 'node:util';
import { fileURLToPath } from 'node:url';
import { formatTable, headline, number } from './bench-summary.mjs';

// Compares the results of the gateway benchmark for OLP (.local/bench/s1.json
// to s5.json, written by the scenario suite) with those of LiteLLM
// (.local/bench/litellm/s1.json to s5.json, written by bench-litellm.mjs) and
// judges the roadmap's targets that compare them
// (docs/roadmap/m01-measured-advantage.md#targets). scripts/bench-compare.sh
// runs both sides and then this.
//
// A target is judged only when both runs can vouch for it: full scale, valid
// load runs, the same vCPUs and the same load. Otherwise it is not_checked,
// with the reason, as the scenario suite does for the targets it judges alone.
// Nothing here is estimated; a figure a run did not produce is a dash.

// What LiteLLM publishes about its high-throughput profile, quoted from
// https://docs.litellm.ai/docs/benchmarks and never measured here: 33 gateway
// pods of four workers, which requested 132 vCPU, sustained 3,000 requests per
// second of 50K to 100K-token prompts at these latencies, which come from the
// gateway's own metrics. A single host cannot reproduce 33 pods, so S3's
// comparison with the profile on the same hardware is made beside it.
export const published = Object.freeze({
  source: 'https://docs.litellm.ai/docs/benchmarks',
  pods: 33,
  workersPerPod: 4,
  vcpus: 132,
  requestsPerSecond: 3000,
  latencyMs: Object.freeze({ p50: 30.581, p95: 54.029, p99: 91.645 })
});

export const scenarios = ['S1', 'S2', 'S3', 'S4', 'S5'];

// The targets that OLP's own result already judges, which are copied rather
// than computed again.
const ownTargets = {
  S1: ['s1-2vcpu-added-p95', 's1-2vcpu-added-p99'],
  S3: ['s3-success-rate']
};
const metadataTarget = 'request-metadata-lost';

const percentiles = (p) => (p ? { p50: p.p50, p95: p.p95, p99: p.p99 } : null);
const finite = (v) => typeof v === 'number' && Number.isFinite(v);

// The mode of a scenario, which says which added latency stands for it.
export function mode(result) {
  const share = result.workload?.stream_share;
  if (share === 0) return 'unary';
  if (share === 1) return 'stream';
  return 'all';
}

// The latency of the direct-to-mock baseline in the population that stands for
// the scenario, in the shape of a percentile triple.
function baselineOf(result) {
  const s = result.baseline?.latency?.[mode(result)];
  return s?.count > 0 ? { p50: s.p50_ms, p95: s.p95_ms, p99: s.p99_ms } : null;
}

// The figures of a result that the comparison reads, from either gateway's
// file, which share these field names.
export function metrics(result) {
  if (!result) return null;
  const t = result.throughput ?? {};
  const perCpu = t.requests_per_cpu_second;
  return {
    mode: mode(result),
    added: percentiles(headline(result)),
    ttft: percentiles(result.ttft_overhead_ms),
    baseline: baselineOf(result),
    sustainedRps: t.sustained_rps ?? null,
    vcpus: t.vcpus ?? null,
    rpsPerVcpu: t.rps_per_vcpu ?? null,
    // What one fully used CPU carries: the requests handled over the CPU time
    // spent on them, net of what the gateway spends idle. At an open-loop rate
    // that two gateways both hold, sustained requests per allotted vCPU ties,
    // so this is the figure that tells them apart.
    requestsPerCpuSecond: finite(perCpu) && perCpu > 0 ? perCpu : null,
    cpuMsPerRequest: t.gateway_cpu_ms_per_request ?? null,
    peakMemoryMiB: result.resources?.rss_peak_mib ?? null,
    errorRate: result.error_rate?.gateway ?? null,
    successRate: result.error_rate?.gateway_success_rate ?? null,
    attemptsPerRequest: result.attempts?.per_request ?? null,
    valid: result.validity?.valid === true,
    problems: result.validity?.problems ?? [],
    reference: result.reference_conditions?.all === true
  };
}

// The loadgen settings that make two runs the same load.
function loadOf(result) {
  const c = result?.gateway_run?.config;
  return c
    ? JSON.stringify([
        c.dialect,
        c.rate,
        c.duration_seconds,
        c.warmup_seconds,
        c.stream_share,
        c.prompt_tokens,
        c.max_tokens,
        c.max_in_flight,
        c.timeout_seconds
      ])
    : null;
}

// How far LiteLLM's direct-to-mock baseline lies from OLP's, which should be
// nowhere: the same load against the same mock on the same CPUs. A
// difference means the sessions were not alike, and the added latencies are
// not strictly comparable. The allowance is a millisecond or a tenth of OLP's
// baseline, whichever is larger. A baseline's p99 varies from session to
// session even on pinned CPUs, so it is noted and the other two decide whether
// an enforcing run fails.
export function baselineDrift(olp, litellm) {
  const a = baselineOf(olp ?? {});
  const b = baselineOf(litellm ?? {});
  if (!a || !b) return null;
  const exceeds = ['p50', 'p95', 'p99'].filter(
    (p) => Math.abs(b[p] - a[p]) > Math.max(1, 0.1 * a[p])
  );
  return {
    p50: b.p50 - a.p50,
    p95: b.p95 - a.p95,
    p99: b.p99 - a.p99,
    exceeds,
    gating: exceeds.filter((p) => p !== 'p99')
  };
}

// What keeps OLP's own run from being judged against a target stated for
// full-rate runs, as a list of reasons, which is empty when nothing does.
function olpShortfall(olp) {
  const reasons = [];
  if (!olp) return ['there is no OLP result'];
  if (olp.scale !== 1)
    reasons.push(`the OLP run was at scale ${olp.scale}, and the targets are stated at full rates`);
  if (olp.validity?.valid !== true)
    reasons.push(
      `the OLP run was not valid: ${(olp.validity?.problems ?? ['it recorded no validity']).join('; ')}`
    );
  return reasons;
}

// Reasons two results cannot be judged against a target that compares them,
// or an empty list: full-rate runs that are valid, on the same vCPUs, under the
// same load.
export function incomparable(olp, litellm) {
  const reasons = [];
  if (!olp) reasons.push('there is no OLP result');
  if (!litellm) reasons.push('there is no LiteLLM result');
  if (reasons.length) return reasons;
  const scales = [...new Set([olp.scale, litellm.scale])];
  if (scales.some((scale) => scale !== 1))
    reasons.push(
      `the runs were at scale ${scales.join(' and ')}, and the targets are stated at full rates`
    );
  for (const [name, r] of [
    ['OLP', olp],
    ['LiteLLM', litellm]
  ])
    if (r.validity?.valid !== true)
      reasons.push(
        `the ${name} run was not valid: ${(r.validity?.problems ?? ['it recorded no validity']).join('; ')}`
      );
  const a = olp.throughput?.vcpus;
  const b = litellm.throughput?.vcpus;
  if (a !== b) reasons.push(`the gateways had ${a} and ${b} vCPUs`);
  if (loadOf(olp) !== loadOf(litellm))
    reasons.push('the two runs did not apply the same load');
  return reasons;
}

function target(id, scenario, description, wanted, fields) {
  return {
    id,
    scenario,
    description,
    target: wanted,
    status: 'not_checked',
    olp: null,
    litellm: null,
    reason: '',
    ...fields
  };
}

const ms = (p) => (p ? `${number(p.p50, 2)} / ${number(p.p95, 2)} / ${number(p.p99, 2)}` : '-');

// The percentiles at which a gateway's added latency is below zero by more than
// a baseline's jitter, a millisecond or a tenth of the baseline's figure. A
// gateway cannot answer faster than the upstream it calls, so such a figure is
// the baseline's noise, and a gateway that has it would beat any other by it.
export function belowZero(m) {
  if (!m?.added) return [];
  return ['p50', 'p95', 'p99'].filter(
    (p) => m.added[p] < -Math.max(1, 0.1 * (m.baseline?.[p] ?? 0))
  );
}

// Added latency lower than LiteLLM's at p50, p95 and p99.
function latencyTarget(scenario, olp, litellm, why) {
  const t = target(
    'added-latency-below-litellm',
    scenario,
    "OLP's added latency is lower than LiteLLM's at p50, p95 and p99",
    'OLP < LiteLLM at each',
    { olp: olp?.added ?? null, litellm: litellm?.added ?? null }
  );
  if (why.length) return { ...t, reason: why.join('; ') };
  if (!olp.added || !litellm.added)
    return { ...t, reason: 'a run recorded no successful request of the scenario' };
  const negative = [['OLP', olp], ['LiteLLM', litellm]]
    .map(([name, m]) => [name, belowZero(m)])
    .filter(([, at]) => at.length);
  if (negative.length)
    return {
      ...t,
      reason: `${negative.map(([name, at]) => `${name}'s added latency is below zero at ${at.join(', ')}`).join('; ')}, by more than a baseline's jitter: its baseline was slower than the run it is subtracted from, so the figure is noise and no comparison can rest on it`
    };
  const worse = ['p50', 'p95', 'p99'].filter((p) => !(olp.added[p] < litellm.added[p]));
  return {
    ...t,
    status: worse.length ? 'missed' : 'met',
    reason: `OLP ${ms(olp.added)} ms, LiteLLM ${ms(litellm.added)} ms at p50 / p95 / p99${worse.length ? `; not lower at ${worse.join(', ')}` : ''}`
  };
}

// More requests per CPU second than LiteLLM.
function throughputTarget(scenario, olp, litellm, why) {
  const t = target(
    'rps-per-vcpu-above-litellm',
    scenario,
    'OLP sustains more RPS per vCPU than LiteLLM',
    'OLP > LiteLLM',
    {
      olp: olp?.requestsPerCpuSecond ?? null,
      litellm: litellm?.requestsPerCpuSecond ?? null
    }
  );
  if (why.length) return { ...t, reason: why.join('; ') };
  if (olp.requestsPerCpuSecond === null || litellm.requestsPerCpuSecond === null)
    return {
      ...t,
      reason:
        'a run spent too little CPU above its idle level to say what one CPU carries'
    };
  const met = olp.requestsPerCpuSecond > litellm.requestsPerCpuSecond;
  return {
    ...t,
    status: met ? 'met' : 'missed',
    reason: `requests per CPU second: OLP ${number(olp.requestsPerCpuSecond, 1)}, LiteLLM ${number(litellm.requestsPerCpuSecond, 1)}; sustained per allotted vCPU: OLP ${number(olp.rpsPerVcpu, 1)}, LiteLLM ${number(litellm.rpsPerVcpu, 1)}`
  };
}

// S3: fewer vCPU than LiteLLM's high-throughput profile at equal or better p95.
// The profile's own figures are published, for 33 pods; the same profile run
// here on the hardware OLP had is the cross-check, which must hold too when it
// can be made.
function profileTarget(olp, litellm, olpWhy, pairWhy) {
  const t = target(
    's3-fewer-vcpu-than-litellm',
    'S3',
    "S3: fewer total vCPU than LiteLLM's high-throughput profile at equal or better p95",
    `< ${published.vcpus} vCPU with added p95 <= ${published.latencyMs.p95} ms, and no worse than the profile measured here`,
    {
      olp: olp ? { vcpus: olp.vcpus, p95: olp.added?.p95 ?? null } : null,
      litellm: litellm
        ? { vcpus: litellm.vcpus, p95: litellm.added?.p95 ?? null, successRate: litellm.successRate }
        : null
    }
  );
  const reasons = [...olpWhy];
  if (olp && olp.vcpus === null) reasons.push('the OLP run did not record its vCPUs');
  if (olp && !olp.added) reasons.push('the OLP run recorded no successful request');
  if (reasons.length) return { ...t, reason: reasons.join('; ') };
  const fewer = olp.vcpus < published.vcpus;
  const belowPublished = olp.added.p95 <= published.latencyMs.p95;
  const crossed = pairWhy.length === 0 && litellm.added !== null;
  const belowMeasured = crossed ? olp.added.p95 <= litellm.added.p95 : true;
  const parts = [
    `OLP used ${olp.vcpus} vCPU against the profile's ${published.vcpus} requested (${published.pods} pods of ${published.workersPerPod} workers)`,
    `added p95 ${number(olp.added.p95, 2)} ms against the profile's published ${published.latencyMs.p95} ms`,
    crossed
      ? `LiteLLM's profile measured on the same hardware at ${number(litellm.added.p95, 2)} ms, answering ${number(100 * litellm.successRate, 2)}% of requests`
      : `the same-hardware cross-check was not made: ${pairWhy.join('; ') || 'LiteLLM recorded no successful request'}`
  ];
  return {
    ...t,
    status: fewer && belowPublished && belowMeasured ? 'met' : 'missed',
    reason: parts.join('; ')
  };
}

// One scenario's side-by-side figures and the targets that depend on them.
export function compareScenario(id, olpResult, litellmResult) {
  const olp = metrics(olpResult);
  const litellm = metrics(litellmResult);
  const why = incomparable(olpResult, litellmResult);
  const olpWhy = olpShortfall(olpResult);
  const targets = [];
  targets.push(latencyTarget(id, olp, litellm, why));
  targets.push(throughputTarget(id, olp, litellm, why));
  if (id === 'S3')
    targets.push(
      profileTarget(olp, litellm, olpWhy, olpWhy.length ? [] : why)
    );
  // The targets the scenario suite judges on OLP alone.
  for (const wanted of [...(ownTargets[id] ?? []), ...(['S1', 'S2', 'S3'].includes(id) ? [metadataTarget] : [])]) {
    const found = olpResult?.targets?.find((x) => x.id === wanted);
    targets.push(
      found
        ? {
            id: found.id,
            scenario: id,
            description: found.description,
            target: found.target,
            status: found.status,
            olp: found.measured ?? null,
            litellm: null,
            reason: found.reason ?? ''
          }
        : target(wanted, id, wanted, '', { reason: 'there is no OLP result' })
    );
  }
  return {
    id,
    title: olpResult?.title ?? litellmResult?.title ?? id,
    olp,
    litellm,
    litellm_profile: litellmResult?.litellm?.profile ?? null,
    baseline_drift_ms: baselineDrift(olpResult, litellmResult),
    comparable: why.length === 0,
    not_comparable_because: why,
    targets
  };
}

// What makes an enforcing run fail: a target that is missed or was not
// checked, a run that is not a reference run, and baselines that disagree.
export function enforcementFailures(report) {
  const failures = [];
  for (const s of report.scenarios) {
    for (const t of s.targets) {
      if (t.status === 'missed')
        failures.push(`${s.id}: target ${t.id} missed: ${t.reason}`);
      else if (t.status === 'not_checked')
        failures.push(`${s.id}: target ${t.id} was not checked: ${t.reason}`);
    }
    for (const [name, m] of [
      ['OLP', s.olp],
      ['LiteLLM', s.litellm]
    ])
      if (m && !m.reference)
        failures.push(
          `${s.id}: the ${name} run was not a reference run: full scale, all three processes pinned and valid load runs are all needed`
        );
    if (s.baseline_drift_ms?.gating.length)
      failures.push(
        `${s.id}: the two sessions' direct-to-mock baselines differ at ${s.baseline_drift_ms.gating.join(', ')}, so the machine or its pinning was not the same`
      );
  }
  return failures;
}

// Everything worth saying that is not a target.
function notes(report) {
  const out = [];
  for (const s of report.scenarios) {
    if (!s.comparable)
      out.push(`${s.id}: the targets that compare the two were not judged: ${s.not_comparable_because.join('; ')}`);
    for (const [name, m] of [
      ['OLP', s.olp],
      ['LiteLLM', s.litellm]
    ])
      for (const p of m?.problems ?? []) out.push(`${s.id} ${name}: ${p}`);
    if (s.baseline_drift_ms?.exceeds.length)
      out.push(
        `${s.id}: the baselines of the two sessions differ at ${s.baseline_drift_ms.exceeds.join(', ')} (LiteLLM less OLP: ${ms(s.baseline_drift_ms)} ms at p50 / p95 / p99)`
      );
    for (const [name, m] of [
      ['OLP', s.olp],
      ['LiteLLM', s.litellm]
    ])
      if (finite(m?.successRate) && m.successRate < 1)
        out.push(
          `${s.id}: ${name} answered ${number(100 * m.successRate, 2)}% of the scheduled requests successfully`
        );
  }
  if (!report.scenarios.every((s) => s.olp?.reference && s.litellm?.reference))
    out.push(
      'Not a reference run: reference numbers need full scale, the gateway, mock and load generator pinned to their own CPUs, and valid load runs on both sides.'
    );
  return out;
}

// The comparison of every scenario that has a result on either side.
export function buildReport({ olp, litellm, ids = scenarios, enforce = false }) {
  const compared = ids
    .filter((id) => olp[id] || litellm[id])
    .map((id) => compareScenario(id, olp[id], litellm[id]));
  const any = Object.values({ ...olp, ...litellm })[0];
  const report = {
    generated_at: new Date().toISOString(),
    scale: any?.scale ?? null,
    enforce,
    olp_version: Object.values(olp)[0]?.environment?.olp_version ?? null,
    litellm_image: Object.values(litellm)[0]?.litellm?.image ?? null,
    published_litellm_profile: published,
    environment: any?.environment ?? null,
    scenarios: compared
  };
  report.targets = compared.flatMap((s) => s.targets);
  report.summary = {
    met: report.targets.filter((t) => t.status === 'met').length,
    missed: report.targets.filter((t) => t.status === 'missed').length,
    not_checked: report.targets.filter((t) => t.status === 'not_checked').length
  };
  report.notes = notes(report);
  report.failures = enforce ? enforcementFailures(report) : [];
  return report;
}

const columns = [
  ['scenario', (s) => s.id],
  ['gateway', (_, who) => who],
  ['p50 ms', (_, __, m) => number(m.added?.p50, 2)],
  ['p95 ms', (_, __, m) => number(m.added?.p95, 2)],
  ['p99 ms', (_, __, m) => number(m.added?.p99, 2)],
  ['ttft p95 ms', (_, __, m) => number(m.ttft?.p95, 2)],
  ['rps/vCPU', (_, __, m) => number(m.rpsPerVcpu, 1)],
  ['req/cpu-s', (_, __, m) => number(m.requestsPerCpuSecond, 1)],
  ['cpu ms/req', (_, __, m) => number(m.cpuMsPerRequest, 2)],
  ['peak MiB', (_, __, m) => number(m.peakMemoryMiB, 0)],
  ['errors %', (_, __, m) => number(100 * (m.errorRate ?? NaN), 3)],
  ['valid', (_, __, m) => (m.valid ? 'yes' : 'NO')]
];

function resultRows(report) {
  return report.scenarios.flatMap((s) =>
    [
      ['OLP', s.olp],
      ['LiteLLM', s.litellm]
    ].map(([who, m]) =>
      m
        ? columns.map(([, cell]) => cell(s, who, m))
        : columns.map(([name]) => (name === 'scenario' ? s.id : name === 'gateway' ? who : name === 'valid' ? 'no result' : '-'))
    )
  );
}

const targetColumns = [
  'scenario',
  'target',
  'status',
  'detail'
];

function targetRows(report) {
  return report.targets.map((t) => [t.scenario, t.id, t.status, t.reason || '-']);
}

// The detail of a target is cut for the terminal; compare.json and compare.md
// keep all of it.
const brief = (text) => (text.length > 110 ? `${text.slice(0, 107)}...` : text);

export function renderText(report) {
  const head = columns.map(([name]) => name);
  const targets = targetRows(report).map(([a, b, c, detail]) => [a, b, c, brief(detail)]);
  const out = [
    formatTable(head, resultRows(report)),
    '',
    formatTable(targetColumns, targets),
    ''
  ];
  const { met, missed, not_checked: unchecked } = report.summary;
  out.push(`targets: met ${met}, missed ${missed}, not checked ${unchecked}`);
  for (const note of report.notes) out.push(`  ${note}`);
  for (const failure of report.failures) out.push(`  FAIL ${failure}`);
  return out.join('\n');
}

const cell = (text) => String(text).replaceAll('|', '\\|').replaceAll('\n', ' ');
function markdownTable(headers, rows) {
  const line = (cells) => `| ${cells.map(cell).join(' | ')} |`;
  return [line(headers), line(headers.map(() => '---')), ...rows.map(line)].join('\n');
}

export function renderMarkdown(report) {
  const e = report.environment ?? {};
  const out = [
    '# OLP and LiteLLM on the same scenarios',
    '',
    `Generated ${report.generated_at} at scale ${report.scale}. Added latency is the gateway's less the direct-to-mock baseline's, in milliseconds, for the population the scenario exercises; req/cpu-s is requests handled per CPU second spent above idle.`,
    '',
    '| | |',
    '| --- | --- |',
    `| OLP | ${cell(report.olp_version ?? '-')} |`,
    `| LiteLLM | ${cell(report.litellm_image ?? '-')} |`,
    `| Host | ${cell(e.cpu_model ?? '-')}, ${e.host_cpus ?? '-'} CPUs, ${number(e.memory_gib, 1)} GiB, kernel ${cell(e.kernel ?? '-')} |`,
    `| Pins | gateway ${cell(e.gateway_cpus || 'none')}, mock ${cell(e.mock_cpus || 'none')}, load generator ${cell(e.loadgen_cpus || 'none')} |`,
    '',
    '## Results',
    '',
    markdownTable(columns.map(([name]) => name), resultRows(report)),
    '',
    '## Targets',
    '',
    markdownTable(targetColumns, targetRows(report)),
    '',
    `Targets: met ${report.summary.met}, missed ${report.summary.missed}, not checked ${report.summary.not_checked}.`
  ];
  if (report.notes.length) out.push('', '## Notes', '', ...report.notes.map((n) => `- ${n}`));
  if (report.failures.length)
    out.push('', '## Failures', '', ...report.failures.map((f) => `- ${f}`));
  return `${out.join('\n')}\n`;
}

function read(dir, id) {
  const file = join(dir, `${id.toLowerCase()}.json`);
  return existsSync(file) ? JSON.parse(readFileSync(file, 'utf8')) : null;
}

function main(args) {
  const { values } = parseArgs({
    args,
    options: {
      olp: { type: 'string', default: '.local/bench' },
      litellm: { type: 'string' },
      out: { type: 'string' },
      scenarios: { type: 'string' },
      enforce: { type: 'boolean', default: false }
    },
    allowPositionals: false
  });
  const litellmDir = values.litellm ?? join(values.olp, 'litellm');
  const outDir = values.out ?? values.olp;
  const ids = values.scenarios ? values.scenarios.split(',') : scenarios;
  const olp = {};
  const litellm = {};
  for (const id of ids) {
    olp[id] = read(values.olp, id);
    litellm[id] = read(litellmDir, id);
    if (!olp[id]) delete olp[id];
    if (!litellm[id]) delete litellm[id];
  }
  if (!Object.keys(olp).length && !Object.keys(litellm).length) {
    console.error(`no results for ${ids.join(', ')} under ${values.olp} or ${litellmDir}`);
    return 2;
  }
  const report = buildReport({ olp, litellm, ids, enforce: values.enforce });
  mkdirSync(outDir, { recursive: true });
  writeFileSync(join(outDir, 'compare.json'), `${JSON.stringify(report, null, 2)}\n`);
  writeFileSync(join(outDir, 'compare.md'), renderMarkdown(report));
  console.log(`\nOLP and LiteLLM, results in ${outDir}`);
  console.log(renderText(report));
  console.log(`\nWrote ${join(outDir, 'compare.json')} and ${join(outDir, 'compare.md')}`);
  return report.failures.length ? 1 : 0;
}

if (process.argv[1] === fileURLToPath(import.meta.url))
  process.exitCode = main(process.argv.slice(2));
