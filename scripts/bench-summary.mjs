#!/usr/bin/env node
import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

// Prints one row per scenario from the result files that `make bench` writes
// (.local/bench/s1.json to s6.json). Every figure is read from the result; a
// scenario that does not produce one, such as added latency for S6, whose
// streams never finish, shows a dash rather than zero.
const columns = [
  ['scenario', (r) => r.scenario],
  ['p50 ms', (r) => number(headline(r)?.p50, 2)],
  ['p95 ms', (r) => number(headline(r)?.p95, 2)],
  ['p99 ms', (r) => number(headline(r)?.p99, 2)],
  ['ttft p95 ms', (r) => number(r.ttft_overhead_ms?.p95, 2)],
  ['rps/vCPU', (r) => (r.slow_readers ? '-' : number(r.throughput?.rps_per_vcpu, 1))],
  ['cpu ms/req', (r) => number(r.throughput?.gateway_cpu_ms_per_request, 2)],
  ['peak RSS MiB', (r) => number(r.resources?.rss_peak_mib, 0)],
  ['errors %', (r) => number(100 * (r.error_rate?.gateway ?? NaN), 3)],
  [
    'metadata %',
    (r) =>
      number(
        100 * (r.request_metadata_completeness?.completeness ?? NaN),
        3
      )
  ],
  ['targets', targets],
  ['valid', (r) => (r.validity?.valid ? 'yes' : 'NO')],
  ['reference', (r) => (r.reference_conditions?.all ? 'yes' : 'no')]
];

// The added latency that stands for the scenario: the mode it exercises, or
// every successful request when it mixes them.
export function headline(result) {
  const { stream_share: share } = result.workload ?? {};
  const added = result.added_latency_ms ?? {};
  if (share === 0) return added.unary;
  if (share === 1) return added.stream;
  return added.all;
}

export function number(value, digits) {
  return typeof value === 'number' && Number.isFinite(value)
    ? value.toFixed(digits)
    : '-';
}

// How the targets stood: "met 2, missed 1, comparison 2".
function targets(result) {
  const counts = new Map();
  for (const { status } of result.targets ?? [])
    counts.set(status, (counts.get(status) ?? 0) + 1);
  const label = { needs_comparison: 'comparison', not_checked: 'unchecked' };
  return (
    [...counts].map(([status, n]) => `${label[status] ?? status} ${n}`).join(', ') ||
    '-'
  );
}

// Cells laid out in padded columns under a rule.
export function formatTable(headers, rows) {
  const widths = headers.map((name, i) =>
    Math.max(name.length, ...rows.map((row) => row[i].length))
  );
  const line = (cells) =>
    cells.map((cell, i) => cell.padEnd(widths[i])).join('  ').trimEnd();
  return [
    line(headers),
    line(widths.map((w) => '-'.repeat(w))),
    ...rows.map(line)
  ].join('\n');
}

export function render(results) {
  return formatTable(
    columns.map(([name]) => name),
    results.map((r) => columns.map(([, cell]) => cell(r)))
  );
}

// Everything that makes a run less than a reference run, once per scenario.
export function caveats(results) {
  const notes = [];
  for (const r of results) {
    for (const problem of r.validity?.problems ?? [])
      notes.push(`${r.scenario}: ${problem}`);
    for (const t of r.targets ?? [])
      if (t.status === 'missed')
        notes.push(`${r.scenario}: target ${t.id} missed`);
  }
  if (results.some((r) => !r.reference_conditions?.all))
    notes.push(
      'Not a reference run: reference numbers need full scale, the gateway, mock and load generator pinned to their own CPUs, and valid runs.'
    );
  return notes;
}

function main(args) {
  const [dir = '.local/bench', ...wanted] = args;
  const ids = wanted.length ? wanted : ['S1', 'S2', 'S3', 'S4', 'S5', 'S6'];
  const results = [];
  for (const id of ids) {
    const file = join(dir, `${id.toLowerCase()}.json`);
    if (!existsSync(file)) {
      console.log(`${id}: no result at ${file}`);
      continue;
    }
    results.push(JSON.parse(readFileSync(file, 'utf8')));
  }
  if (!results.length) return 1;
  console.log(`\nResults in ${dir}`);
  console.log(render(results));
  for (const note of caveats(results)) console.log(`  ${note}`);
  return 0;
}

if (process.argv[1] === fileURLToPath(import.meta.url))
  process.exitCode = main(process.argv.slice(2));
