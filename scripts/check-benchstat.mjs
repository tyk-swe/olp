#!/usr/bin/env node
import { appendFileSync, readFileSync, writeFileSync } from 'node:fs';

// Reads the text output of `benchstat base=old.txt head=new.txt` and fails on a
// statistically significant regression above the threshold in time or
// allocations per operation. benchstat prints a delta only when its
// significance test passes and `~` otherwise, so a printed delta is the
// significance signal. Bytes per operation is reported but never gates.
//
// The gate also fails when it has compared nothing, or when a benchmark of the
// base is missing from the head: a check that compares no benchmark passes
// whatever the change did, and a renamed or deleted benchmark is one whose
// regression would go unseen. A removal that is meant is allowed with
// --allow-removed.
//
// A single benchmark can show a significant change by chance, and the more
// benchmarks the gate compares the likelier one does. With --flagged the first
// comparison only nominates regressions, writing for each package the -bench
// pattern that selects them; the caller measures those again with more samples
// and passes that comparison as --confirm, and only a regression that shows in
// both fails.
const units = new Map([
  ['sec/op', { label: 'time/op', gated: true }],
  ['allocs/op', { label: 'allocs/op', gated: true }],
  ['B/op', { label: 'B/op', gated: false }]
]);

// benchstat's default test is a two-sided Mann-Whitney U test at alpha 0.05.
// Its smallest possible p-value is 2 / C(base + head, base), so a run with too
// few samples on both sides can never report a change, however large.
const alpha = 0.05;
function detectable(base, head) {
  let combinations = 1;
  for (let index = 1; index <= Math.min(base, head); index++)
    combinations = (combinations * (base + head - index + 1)) / index;
  return 2 / combinations < alpha;
}

const scales = new Map([
  ['', 1],
  ['n', 1e-9],
  ['µ', 1e-6],
  ['μ', 1e-6],
  ['m', 1e-3],
  ['k', 1e3],
  ['M', 1e6],
  ['G', 1e9],
  ['T', 1e12],
  ['Ki', 2 ** 10],
  ['Mi', 2 ** 20],
  ['Gi', 2 ** 30],
  ['Ti', 2 ** 40]
]);

const marks = '⁰¹²³⁴⁵⁶⁷⁸⁹';
const footnote = new RegExp(`^[${marks}]+\\s`);
const cell = /(\d[^\s±]*)\s*±\s*(\S+)/g;
const change =
  /^[\s⁰¹²³⁴⁵⁶⁷⁸⁹]*(?:([+-])(\d+(?:\.\d+)?|Inf)%|(~)|(\?))\s+\(p=([\d.]+)\s+n=(\d+)(?:\+(\d+))?\)/;

const shortPackage = (pkg) => pkg.replace(/^github\.com\/[^/]+\/[^/]+\//, '');
const signed = (percent) => `${percent > 0 ? '+' : ''}${percent.toFixed(2)}%`;

// Values are read only to order a zero summary against another, and only in
// the units that gate, so an unfamiliar scale elsewhere cannot fail the run.
// benchstat's scaler emits exactly the prefixes above.
function parseValue(text) {
  const match = /^(\d+(?:\.\d+)?)(\D*)$/.exec(text);
  const scale = scales.get(match?.[2]);
  if (scale === undefined)
    throw new Error(`Unrecognized benchstat value ${text}`);
  return Number(match[1]) * scale;
}

// A table is headed by a row naming its inputs and a row naming its unit. A
// table of two inputs adds the comparison column; one with a single input
// covers benchmarks that exist on only one side.
function openTable(pkg, inputs, header) {
  const names = inputs
    .split('│')
    .map((part) => part.trim())
    .filter(Boolean);
  const [unit, comparison] = header
    .split('│')
    .map((part) => part.trim())
    .filter(Boolean);
  const compared = names.length === 2;
  const shape = compared
    ? comparison?.replace(/\s+/g, ' ') === `${unit} vs base`
    : names.length === 1;
  if (!shape) throw new Error(`Unrecognized benchstat table header: ${header}`);
  return {
    pkg,
    unit,
    compared,
    // A single input is named by the caller, `base` or `head`.
    side: /^(base|old)/i.test(names[0]) ? 'base' : 'head',
    bars: [...inputs].flatMap((char, index) => (char === '│' ? [index] : [])),
    rows: []
  };
}

function parseRow(line, table) {
  const [, name, rest] = /^(\S+)\s*(.*)$/.exec(line);
  if (name === 'geomean') {
    const percent = /([+-]\d+(?:\.\d+)?)%/.exec(rest);
    return { geomean: true, percent: percent ? Number(percent[1]) : null };
  }
  const bad = () => new Error(`Unrecognized benchstat row: ${line}`);
  const cells = [...rest.matchAll(cell)];
  if (cells.length === 0 || cells.length > 2) throw bad();
  const values = cells.map(([, text]) => ({ text }));
  const last = cells.at(-1);
  const trailing = rest.slice(last.index + last[0].length);
  if (cells.length === 1) {
    if (trailing.replace(new RegExp(`[${marks}\\s]`, 'g'), '') !== '')
      throw bad();
    // A table of two inputs pads the side without a result, so the column
    // holding the value says which input produced it.
    const column = line.length - rest.length + cells[0].index;
    const head = table.compared
      ? column > table.bars[1]
      : table.side === 'head';
    return { name, [head ? 'head' : 'base']: values[0] };
  }
  const match = table.compared ? change.exec(trailing) : null;
  if (match === null) throw bad();
  const [, sign, magnitude, noise, unknown, p, baseN, headN] = match;
  let kind = 'change';
  let percent;
  if (noise) kind = 'noise';
  else if (unknown) kind = 'unknown';
  else
    percent =
      (sign === '-' ? -1 : 1) *
      (magnitude === 'Inf' ? Infinity : Number(magnitude));
  return {
    name,
    base: values[0],
    head: values[1],
    change: {
      kind,
      percent,
      p,
      baseN: Number(baseN),
      headN: Number(headN ?? baseN)
    }
  };
}

// Returns one table per package and unit, in input order.
export function parseBenchstat(text) {
  const tables = [];
  let pkg = '';
  let inputs = null;
  let table = null;
  for (const line of text.split(/\r?\n/)) {
    if (line.trim() === '') {
      inputs = table = null;
    } else if (line.trimStart().startsWith('│')) {
      if (inputs === null) {
        inputs = line;
      } else {
        table = openTable(pkg, inputs, line);
        tables.push(table);
        inputs = null;
      }
    } else if (table === null) {
      inputs = null;
      pkg = /^pkg:\s*(.+?)\s*$/.exec(line)?.[1] ?? pkg;
    } else if (!footnote.test(line)) {
      table.rows.push(parseRow(line, table));
    }
  }
  return tables;
}

// A regression is a gated change above the threshold. A smaller significant
// slowdown is tolerated, and reported as within the budget.
export function evaluate(tables, { threshold = 10, allowRemoved = false } = {}) {
  const result = {
    threshold,
    compared: 0,
    // The benchmarks that had a result on both sides, in any unit, by key.
    measured: new Set(),
    regressions: [],
    within: [],
    improvements: [],
    advisories: [],
    added: [],
    removed: [],
    geomeans: [],
    fewSamples: []
  };
  for (const table of tables) {
    const unit = units.get(table.unit);
    if (!unit) continue;
    const pairs = table.rows.filter((row) => row.base && row.head);
    if (unit.label === 'time/op') {
      result.compared += pairs.length;
      const single = table.rows.filter(
        (row) => !row.geomean && !(row.base && row.head)
      );
      for (const row of single)
        result[row.head ? 'added' : 'removed'].push({
          pkg: table.pkg,
          name: row.name
        });
      for (const row of table.rows.filter(
        (row) => row.geomean && row.percent !== null
      ))
        result.geomeans.push({
          pkg: table.pkg,
          percent: row.percent,
          comparable: single.length === 0
        });
    }
    for (const row of pairs) {
      result.measured.add(keyOf({ pkg: table.pkg, name: row.name, unit: unit.label }));
      const { kind, p, baseN, headN } = row.change;
      let { percent } = row.change;
      // benchstat cannot divide by a zero summary but still tested the samples,
      // and growing from zero is an unbounded regression.
      if (kind === 'unknown')
        percent =
          parseValue(row.head.text) > parseValue(row.base.text)
            ? Infinity
            : -100;
      const finding = {
        pkg: table.pkg,
        name: row.name,
        unit: unit.label,
        base: row.base.text,
        head: row.head.text,
        percent,
        p,
        baseN,
        headN
      };
      if (unit.label === 'time/op' && !detectable(baseN, headN))
        result.fewSamples.push(finding);
      if (kind === 'noise' || percent === 0) continue;
      if (!unit.gated) {
        if (percent > threshold) result.advisories.push(finding);
      } else if (percent > threshold) {
        result.regressions.push(finding);
      } else if (percent > 0) {
        result.within.push(finding);
      } else {
        result.improvements.push(finding);
      }
    }
  }
  // A run where no benchmark could register a change has checked nothing.
  result.inconclusive =
    result.compared > 0 && result.fewSamples.length === result.compared;
  // What fails the gate besides a regression.
  result.failures = [];
  if (result.compared === 0)
    result.failures.push(
      'FAIL: no benchmark ran on both sides, so nothing was compared and the gate has said nothing about the change.'
    );
  if (result.inconclusive)
    result.failures.push(
      'FAIL: inconclusive, collect more samples per benchmark.'
    );
  if (result.removed.length > 0 && !allowRemoved)
    result.failures.push(
      `FAIL: ${result.removed.length} ${result.removed.length === 1 ? 'benchmark' : 'benchmarks'} of the base ` +
        `${result.removed.length === 1 ? 'is' : 'are'} absent from the head. A renamed or deleted benchmark is not compared, ` +
        'so a regression in it would go unseen; pass --allow-removed (BENCH_ALLOW_REMOVED=1) when the removal is intended.'
    );
  return result;
}

const keyOf = ({ pkg, name, unit }) => `${pkg}\t${name}\t${unit}`;

// The regressions of a first comparison that a second one of the same
// benchmarks, with more samples, reproduces. A benchmark that the second did
// not measure cannot be cleared by it and stays a regression: the gate fails
// safe when its own bookkeeping is wrong.
export function confirmRegressions(first, second) {
  const reproduced = new Map(
    second.regressions.map((finding) => [keyOf(finding), finding])
  );
  const confirmed = [];
  const unconfirmed = [];
  for (const finding of first.regressions) {
    const key = keyOf(finding);
    if (reproduced.has(key)) confirmed.push(reproduced.get(key));
    else if (!second.measured.has(key)) confirmed.push(finding);
    else unconfirmed.push(finding);
  }
  return { ...first, regressions: confirmed, unconfirmed };
}

// The -bench regexp that selects the named benchmarks, as benchstat prints them
// (without the Benchmark prefix and with the GOMAXPROCS suffix). go test splits
// the pattern at slashes and matches each part against one level of the name,
// so the parts are the alternatives of that level, which may select more than
// was asked for and never less.
export function benchPattern(names) {
  const levels = [];
  for (const full of names) {
    full
      .replace(/-\d+$/, '')
      .split('/')
      .forEach((part, depth) => {
        (levels[depth] ??= new Set()).add(depth === 0 ? `Benchmark${part}` : part);
      });
  }
  return levels
    .map((alternatives) => `^(?:${[...alternatives].map(quoteRegexp).join('|')})$`)
    .join('/');
}

const quoteRegexp = (text) => text.replace(/[\\.+*?()|[\]{}^$]/g, '\\$&');

// The regressions to measure again, as lines of a package's import path and the
// -bench pattern that selects its flagged benchmarks, separated by a tab.
export function flaggedPatterns(result) {
  const byPackage = new Map();
  for (const { pkg, name } of result.regressions) {
    if (!byPackage.has(pkg)) byPackage.set(pkg, new Set());
    byPackage.get(pkg).add(name);
  }
  return [...byPackage].map(([pkg, names]) => `${pkg}\t${benchPattern([...names])}`);
}

function section(title, findings) {
  if (findings.length === 0) return [];
  const rows = findings.map((finding) => [
    shortPackage(finding.pkg),
    finding.name,
    finding.unit,
    Number.isFinite(finding.percent) ? signed(finding.percent) : 'from zero',
    `${finding.base} -> ${finding.head}`,
    `p=${finding.p} n=${finding.baseN === finding.headN ? finding.baseN : `${finding.baseN}+${finding.headN}`}`
  ]);
  const widths = rows[0].map((_, column) =>
    Math.max(...rows.map((row) => row[column].length))
  );
  const table = rows.map(
    (row) =>
      `  ${row
        .map((text, column) => text.padEnd(widths[column]))
        .join('  ')
        .trimEnd()}`
  );
  return ['', `${title}:`, ...table];
}

function unpaired(title, rows) {
  if (rows.length === 0) return [];
  return [
    '',
    `${title}:`,
    ...rows.map((row) => `  ${shortPackage(row.pkg)}  ${row.name}`)
  ];
}

export function summarize(result, { pending = false } = {}) {
  const { threshold, regressions } = result;
  const lines = [
    `benchstat gate: ${result.compared} benchmarks compared, ${result.added.length} added, ` +
      `${result.removed.length} removed, threshold ${threshold}%`
  ];
  if (result.compared === 0)
    lines.push(
      'No benchmark ran on both sides, so there is nothing to compare.'
    );
  const reproduced = result.unconfirmed !== undefined;
  lines.push(
    ...section(
      `Regressions above ${threshold}%${reproduced ? ' (reproduced when measured again)' : ''}`,
      regressions
    ),
    ...(reproduced
      ? section(
          `Regressions above ${threshold}% that did not reproduce when measured again (taken for noise)`,
          result.unconfirmed
        )
      : []),
    ...section(
      `Significant slowdowns within ${threshold}% (tolerated)`,
      result.within
    ),
    ...section('Improvements', result.improvements),
    ...section(
      `B/op growth above ${threshold}% (not gated)`,
      result.advisories
    ),
    ...unpaired('Added in head, no baseline (not compared)', result.added),
    ...unpaired('Absent from head (not compared)', result.removed)
  );
  if (result.fewSamples.length > 0)
    lines.push(
      '',
      `Warning: ${result.fewSamples.length} benchmarks have too few samples for benchstat to detect ` +
        'any change, so they cannot fail the gate.'
    );
  for (const geomean of result.geomeans) {
    const note = geomean.comparable
      ? ''
      : ' (benchmark sets differ, informational)';
    lines.push(
      `geomean time/op ${shortPackage(geomean.pkg) || 'overall'}: ${signed(geomean.percent)}${note}`
    );
  }
  const count = regressions.length;
  const verdicts = [];
  if (count > 0) {
    const what = `${count} significant ${count === 1 ? 'regression' : 'regressions'} above ${threshold}% in time/op or allocs/op`;
    verdicts.push(
      pending
        ? `PENDING: ${what}, to be measured again to see whether they reproduce.`
        : `FAIL: ${what}.`
    );
  }
  verdicts.push(...result.failures);
  lines.push(
    '',
    ...(verdicts.length > 0
      ? verdicts
      : ['PASS: no significant regression above the threshold.'])
  );
  return lines.join('\n');
}

// Judges the benchstat output of a comparison. With `confirm`, the output of a
// second comparison of the benchmarks the first flagged, only a regression that
// shows in both counts.
export function checkBenchstat(text, options = {}) {
  const evaluated = (input) => {
    const tables = parseBenchstat(input);
    if (!tables.some((table) => table.unit === 'sec/op'))
      throw new Error('No benchstat time/op table in the input');
    return evaluate(tables, options);
  };
  let result = evaluated(text);
  if (options.confirm !== undefined)
    result = confirmRegressions(result, evaluated(options.confirm));
  const ok = result.regressions.length === 0 && result.failures.length === 0;
  return { result, summary: summarize(result, options), ok };
}

// The exit status of a first comparison that found regressions and nothing
// else wrong, which are to be measured again before they fail the gate.
const pendingStatus = 3;

function usage() {
  console.error(
    'Usage: check-benchstat.mjs [--threshold percent] [--allow-removed] [--flagged file | --confirm benchstat-output-file] [benchstat-output-file]'
  );
  process.exit(2);
}

if (import.meta.main) {
  const args = process.argv.slice(2);
  const options = { threshold: 10, allowRemoved: false };
  let flagged;
  const takeValue = () => {
    const value = args.splice(0, 2)[1];
    if (value === undefined || value.startsWith('--')) usage();
    return value;
  };
  while (args[0]?.startsWith('--')) {
    if (args[0] === '--threshold') {
      options.threshold = Number(takeValue());
      if (!(options.threshold >= 0)) usage();
    } else if (args[0] === '--allow-removed') {
      options.allowRemoved = true;
      args.shift();
    } else if (args[0] === '--flagged') flagged = takeValue();
    else if (args[0] === '--confirm') {
      try {
        options.confirm = readFileSync(takeValue(), 'utf8');
      } catch (error) {
        console.error(`check-benchstat: ${error.message}`);
        process.exit(2);
      }
    } else usage();
  }
  if (args.length > 1 || args[0]?.startsWith('-')) usage();
  if (flagged !== undefined && options.confirm !== undefined) usage();
  let report;
  try {
    report = checkBenchstat(readFileSync(args[0] ?? 0, 'utf8'), options);
    if (flagged !== undefined) {
      const patterns = flaggedPatterns(report.result);
      writeFileSync(flagged, patterns.map((line) => `${line}\n`).join(''));
      // Regressions alone are not a verdict yet.
      if (patterns.length > 0 && report.result.failures.length === 0) {
        console.log(summarize(report.result, { pending: true }));
        process.exit(pendingStatus);
      }
    }
  } catch (error) {
    console.error(`check-benchstat: ${error.message}`);
    process.exit(2);
  }
  console.log(report.summary);
  if (process.env.GITHUB_STEP_SUMMARY)
    appendFileSync(
      process.env.GITHUB_STEP_SUMMARY,
      `\`\`\`\n${report.summary}\n\`\`\`\n`
    );
  process.exit(report.ok ? 0 : 1);
}
