import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import {
  benchPattern,
  checkBenchstat,
  confirmRegressions,
  evaluate,
  flaggedPatterns,
  parseBenchstat
} from './check-benchstat.mjs';

// The fixtures are verbatim `benchstat` output at the version pinned by
// bench-gate.sh. `real-olp-*` are bench-gate.sh runs over BenchmarkSign in
// internal/plugins, unchanged and with its workload doubled. The other `real-*`
// come from `go test -bench` runs of toy modules, and the rest compare
// synthetic samples that were generated to hit one case each.
const fixture = (name) =>
  readFileSync(
    new URL(`./testdata/benchstat/${name}.txt`, import.meta.url),
    'utf8'
  );
const names = (findings) =>
  findings.map(
    ({ pkg, name, unit }) => `${pkg.split('/').at(-1)} ${name} ${unit}`
  );

test('significant regressions above the threshold fail, in time and allocations', () => {
  const { ok, result, summary } = checkBenchstat(fixture('mixed'));
  assert.equal(ok, false);
  assert.deepEqual(names(result.regressions), [
    'gateway Admission-8 time/op',
    'gateway Relay/64-8 allocs/op',
    'gateway ZeroAlloc-8 allocs/op'
  ]);
  assert.match(
    summary,
    /^FAIL: 3 significant regressions above 10% in time\/op or allocs\/op\.$/m
  );
  assert.match(
    summary,
    /Admission-8\s+time\/op\s+\+25\.00%\s+1\.210µ -> 1\.512µ\s+p=0\.000 n=10/
  );
});

test('the plugin benchmarks pass unchanged and fail when their workload doubles', () => {
  const unchanged = checkBenchstat(fixture('real-olp-unchanged'));
  assert.equal(unchanged.ok, true);
  assert.equal(unchanged.result.compared, 4);
  assert.equal(unchanged.result.regressions.length, 0);

  const doubled = checkBenchstat(fixture('real-olp-injected-slowdown'));
  assert.equal(doubled.ok, false);
  assert.equal(doubled.result.compared, 4);
  const sizes = [
    'interpreted/1KiB',
    'interpreted/16KiB',
    'compiled/1KiB',
    'compiled/16KiB'
  ];
  assert.deepEqual(names(doubled.result.regressions), [
    ...sizes.map((size) => `plugins Sign/${size}-8 time/op`),
    ...sizes.map((size) => `plugins Sign/${size}-8 allocs/op`)
  ]);
  assert.equal(doubled.result.advisories.length, 4);
  assert.match(
    doubled.summary,
    /Sign\/compiled\/16KiB-8\s+allocs\/op\s+\+100\.46%\s+109\.0 -> 218\.5/
  );
});

test('growth from a zero baseline is an unbounded regression and a drop to zero an improvement', () => {
  const { result } = checkBenchstat(fixture('mixed'));
  const zero = result.regressions.find(
    (finding) => finding.name === 'ZeroAlloc-8'
  );
  assert.equal(zero.percent, Infinity);
  assert.equal(zero.base, '0.000');
  assert.equal(zero.head, '1.000');
  assert.match(
    checkBenchstat(fixture('mixed')).summary,
    /ZeroAlloc-8\s+allocs\/op\s+from zero/
  );

  const dropped = fixture('mixed').replace(
    /(ZeroAlloc-8 +)0\.000( ± 0% +)1\.000/,
    '$11.000$20.000'
  );
  assert.notEqual(dropped, fixture('mixed'));
  const after = checkBenchstat(dropped).result;
  assert.ok(
    after.improvements.some(
      (finding) => finding.name === 'ZeroAlloc-8' && finding.percent === -100
    )
  );
  assert.equal(
    after.regressions.some(
      (finding) =>
        finding.name === 'ZeroAlloc-8' && finding.unit === 'allocs/op'
    ),
    false
  );
});

test('a significant slowdown within the threshold is tolerated and an improvement is reported', () => {
  const { result } = checkBenchstat(fixture('mixed'));
  assert.deepEqual(names(result.within), ['access Authenticate-8 time/op']);
  assert.deepEqual(names(result.improvements), ['gateway Faster-8 time/op']);
  assert.equal(result.improvements[0].percent, -33.33);
});

test('a significant change that rounds to zero is unsigned and no regression', () => {
  const sample = fixture('mixed').replace(
    '+25.00% (p=0.000 n=10)',
    '0.00% (p=0.037 n=10)'
  );
  const { result } = checkBenchstat(sample);
  assert.equal(
    result.regressions.some((finding) => finding.name === 'Admission-8'),
    false
  );
});

test('the threshold is exclusive and configurable', () => {
  const sample = fixture('mixed');
  const atThreshold = sample.replace(
    '+25.00% (p=0.000 n=10)',
    '+10.00% (p=0.000 n=10)'
  );
  assert.equal(
    checkBenchstat(atThreshold).result.regressions.some(
      (finding) => finding.name === 'Admission-8'
    ),
    false
  );
  const above = sample.replace(
    '+25.00% (p=0.000 n=10)',
    '+10.01% (p=0.000 n=10)'
  );
  assert.equal(
    checkBenchstat(above).result.regressions.some(
      (finding) => finding.name === 'Admission-8'
    ),
    true
  );
  const lenient = checkBenchstat(sample, { threshold: 30 }).result;
  assert.deepEqual(names(lenient.regressions), [
    'gateway ZeroAlloc-8 allocs/op'
  ]);
  assert.equal(
    lenient.within.some((finding) => finding.name === 'Admission-8'),
    true
  );
});

test('bytes per operation is reported without gating', () => {
  const { result, ok } = checkBenchstat(fixture('real-one-package'));
  assert.deepEqual(names(result.advisories), ['toy Join-8 B/op']);
  assert.equal(ok, false, 'the time regression in the same sample still fails');
  const timeOnlyIncrease = fixture('real-one-package').replace(
    '+519.52% (p=0.002 n=6)',
    '       ~ (p=0.310 n=6)'
  );
  // The sample also lacks a benchmark of the base, which fails apart from this.
  const after = checkBenchstat(timeOnlyIncrease, { allowRemoved: true });
  assert.equal(after.ok, true);
  assert.deepEqual(names(after.result.advisories), ['toy Join-8 B/op']);
});

test('noise rows pass, even when the geomean moves', () => {
  const noise = checkBenchstat(fixture('noise-and-new-package'));
  assert.equal(noise.ok, true);
  assert.equal(noise.result.compared, 2);
  assert.match(noise.summary, /^PASS: /m);
  // The geomean aggregates insignificant noise and is never a finding.
  assert.deepEqual(noise.result.geomeans, [
    {
      pkg: 'github.com/tyk-swe/olp/internal/gateway',
      percent: 0.41,
      comparable: true
    }
  ]);
  assert.equal(noise.result.regressions.length, 0);
});

test('a benchmark that exists only in head is reported, not compared', () => {
  const { ok, result, summary } = checkBenchstat(
    fixture('noise-and-new-package')
  );
  assert.equal(ok, true);
  assert.deepEqual(result.added, [
    { pkg: 'github.com/tyk-swe/olp/internal/estimate', name: 'Count-8' }
  ]);
  assert.match(
    summary,
    /Added in head, no baseline \(not compared\):\n {2}internal\/estimate {2}Count-8/
  );
});

test('benchmarks that lack a counterpart in a shared table are told apart by column', () => {
  const { ok, result } = checkBenchstat(fixture('mixed'));
  assert.equal(ok, false);
  assert.deepEqual(result.added, [
    { pkg: 'github.com/tyk-swe/olp/internal/gateway', name: 'OnlyHead-8' }
  ]);
  assert.deepEqual(result.removed, [
    { pkg: 'github.com/tyk-swe/olp/internal/gateway', name: 'OnlyBase-8' }
  ]);
  assert.equal(result.compared, 7);
  const added = checkBenchstat(fixture('real-one-package'));
  assert.deepEqual(added.result.added, [{ pkg: 'toy', name: 'OnlyHead-8' }]);
  assert.deepEqual(added.result.removed, [{ pkg: 'toy', name: 'OnlyBase-8' }]);
  assert.match(
    added.summary,
    /geomean time\/op toy: \+149\.25% \(benchmark sets differ, informational\)/
  );
});

test('no overlap between the base and head benchmark names fails: nothing was compared', () => {
  const { ok, result, summary } = checkBenchstat(fixture('all-new'));
  assert.equal(ok, false);
  assert.equal(result.compared, 0);
  assert.equal(result.added.length, 3);
  assert.match(summary, /0 benchmarks compared, 3 added, 0 removed/);
  assert.match(summary, /nothing to compare/);
  assert.match(summary, /^FAIL: no benchmark ran on both sides, so nothing was compared/m);
  // Allowing removals does not make an empty comparison say anything.
  assert.equal(checkBenchstat(fixture('all-new'), { allowRemoved: true }).ok, false);

  // Renaming every benchmark is the same situation, and a slowdown hidden by
  // the rename must not pass for a clean run.
  const renamed = `pkg: example.com/p
         │    base     │                head                │
         │   sec/op    │   sec/op     vs base               │
Old-8      101.4n ± 3%
New-8                     910.4n ± 3%
`.replace(/ +\n/g, '\n');
  const after = checkBenchstat(renamed);
  assert.equal(after.ok, false);
  assert.equal(after.result.compared, 0);
  assert.deepEqual(after.result.removed, [{ pkg: 'example.com/p', name: 'Old-8' }]);
  assert.deepEqual(after.result.added, [{ pkg: 'example.com/p', name: 'New-8' }]);
  assert.match(after.summary, /^FAIL: no benchmark ran on both sides/m);
  assert.match(after.summary, /^FAIL: 1 benchmark of the base is absent from the head/m);
});

test('a benchmark of the base that is absent from the head fails unless the removal is allowed', () => {
  const sample = `pkg: example.com/p
         │    base     │             head              │
         │   sec/op    │   sec/op     vs base          │
Same-8     101.4n ± 3%   101.4n ± 3%  ~ (p=0.500 n=10)
Gone-8     64.84n ± 12%
`.replace(/ +\n/g, '\n');
  const failing = checkBenchstat(sample);
  assert.equal(failing.ok, false);
  assert.equal(failing.result.compared, 1);
  assert.deepEqual(failing.result.removed, [{ pkg: 'example.com/p', name: 'Gone-8' }]);
  assert.match(failing.summary, /^FAIL: 1 benchmark of the base is absent from the head\./m);
  assert.match(failing.summary, /--allow-removed/);
  assert.doesNotMatch(failing.summary, /^PASS/m);
  const allowed = checkBenchstat(sample, { allowRemoved: true });
  assert.equal(allowed.ok, true);
  // It is still said, though it does not fail.
  assert.match(allowed.summary, /Absent from head \(not compared\):\n {2}example\.com\/p {2}Gone-8/);
  // A benchmark that is only new is not a removal.
  assert.equal(checkBenchstat(fixture('noise-and-new-package')).ok, true);

  // A base with nothing but a benchmark the head lost compared nothing at all.
  const baseOnly = `pkg: example.com/p
        │    base     │
        │   sec/op    │
Gone-8    101.4n ± 3%

        │    base     │
        │  allocs/op  │
Gone-8    2.000 ± 0%
`;
  const lost = checkBenchstat(baseOnly, { allowRemoved: true });
  assert.equal(lost.ok, false);
  assert.deepEqual(lost.result.removed, [{ pkg: 'example.com/p', name: 'Gone-8' }]);
  assert.equal(lost.result.added.length, 0);
});

test('packages that share benchmark names are kept apart', () => {
  const { result } = checkBenchstat(fixture('real-two-packages'));
  assert.deepEqual(
    names(result.regressions).filter((name) => name.includes('Same-8')),
    ['b Same-8 time/op']
  );
  assert.deepEqual(
    names(result.improvements).filter((name) => name.includes('Same-8')),
    ['a Same-8 time/op']
  );
  assert.equal(result.compared, 7);
});

test('sub-nanosecond times, large scales and throughput tables parse', () => {
  const { ok, result } = checkBenchstat(
    fixture('sub-nanosecond-and-throughput')
  );
  assert.equal(ok, false);
  assert.deepEqual(names(result.regressions), [
    'codec Noop-8 time/op',
    'codec Copy-8 time/op'
  ]);
  assert.equal(result.regressions[0].base, '0.4012n');
  // Throughput falling is a slowdown that time/op already reports, and the
  // B/s table, which prints larger as better, never gates.
  assert.equal(result.improvements.length, 0);
  assert.deepEqual(
    parseBenchstat(fixture('sub-nanosecond-and-throughput')).map(
      (table) => table.unit
    ),
    ['sec/op', 'B/op', 'allocs/op', 'B/s']
  );
});

test('a zero summary orders against any scale benchstat prints', () => {
  const table = (head) => `pkg: example.com/p
        │    base     │
        │   sec/op    │
Same-8    101.4n ± 3%

         │    base    │            head             │
         │ allocs/op  │ allocs/op   vs base         │
Grow-8     0.000 ± 0%   ${head} ± 0%  ? (p=0.000 n=10)
`;
  for (const head of ['1.000', '1.000k', '1.000M', '1.000G', '1.000T'])
    assert.deepEqual(names(checkBenchstat(table(head)).result.regressions), [
      'p Grow-8 allocs/op'
    ]);
  // Where a scale would decide the verdict, an unknown one is not guessed at.
  assert.throws(
    () => checkBenchstat(table('1.000P')),
    /Unrecognized benchstat value 1\.000P/
  );
});

test('custom metric tables are ignored', () => {
  const tables = parseBenchstat(fixture('real-two-packages'));
  assert.deepEqual(
    [...new Set(tables.map((table) => table.unit))],
    ['sec/op', 'B/op', 'allocs/op', 'widgets/op']
  );
  const { result } = checkBenchstat(fixture('real-two-packages'));
  assert.equal(
    result.regressions.some((finding) => finding.unit === 'widgets/op'),
    false
  );
});

test('inputs named old.txt and new.txt parse the same as base and head', () => {
  const tables = parseBenchstat(fixture('real-one-package'));
  const time = tables.find((table) => table.unit === 'sec/op');
  assert.equal(time.compared, true);
  assert.deepEqual(
    time.rows
      .filter((row) => !row.geomean)
      .map((row) => [row.name, Boolean(row.base), Boolean(row.head)]),
    [
      ['Join-8', true, true],
      ['Alloc-8', true, true],
      ['OnlyBase-8', true, false],
      ['OnlyHead-8', false, true]
    ]
  );
});

test('a single-input table takes its side from the input name', () => {
  const headOnly = `pkg: example.com/p
        │    head     │
        │   sec/op    │
New-8     101.4n ± 3%
`;
  assert.deepEqual(checkBenchstat(headOnly).result.added, [
    { pkg: 'example.com/p', name: 'New-8' }
  ]);
  assert.equal(
    checkBenchstat(headOnly.replace('head', 'new.txt')).result.added.length,
    1
  );
  assert.equal(
    checkBenchstat(headOnly.replace('head', 'old.txt')).result.removed.length,
    1
  );
});

test('too few samples cannot register a change and say so', () => {
  const { result, summary } = checkBenchstat(fixture('few-samples'));
  // A 50% slowdown at n=3 is below the significance floor, so every row is `~`.
  assert.equal(result.regressions.length, 0);
  assert.equal(result.fewSamples.length, 2);
  assert.match(summary, /too few samples/);
});

test('a run in which nothing could register a change is inconclusive and fails', () => {
  const { ok, result, summary } = checkBenchstat(fixture('few-samples'));
  assert.equal(result.inconclusive, true);
  assert.equal(ok, false);
  assert.match(summary, /^FAIL: inconclusive/m);
});

test('unequal sample counts can still be significant', () => {
  const { ok, result, summary } = checkBenchstat(fixture('unequal-samples'));
  assert.equal(ok, false);
  assert.equal(result.inconclusive, false);
  assert.deepEqual(names(result.regressions), ['gateway Admission-8 time/op']);
  assert.equal(result.regressions[0].baseN, 3);
  assert.equal(result.regressions[0].headN, 10);
  assert.match(summary, /p=0\.007 n=3\+10/);
  assert.equal(result.fewSamples.length, 0);
});

test('footnote markers between columns and after rows do not disturb parsing', () => {
  const tables = parseBenchstat(fixture('few-samples'));
  const time = tables.find((table) => table.unit === 'sec/op');
  const admission = time.rows.find((row) => row.name === 'Admission-8');
  assert.equal(admission.base.text, '1.203µ');
  assert.equal(admission.change.kind, 'noise');
  assert.equal(admission.change.p, '0.100');
  assert.deepEqual(
    time.rows.filter((row) => row.geomean),
    [{ geomean: true, percent: 22.47 }]
  );
});

test('unrecognized benchstat output is an error rather than a pass', () => {
  assert.throws(() => checkBenchstat(''), /No benchstat time\/op table/);
  assert.throws(
    () => checkBenchstat('goos: linux\nPASS\n'),
    /No benchstat time\/op table/
  );
  // The format benchstat printed before v2.
  const legacy =
    'name    old time/op  new time/op  delta\nSame-8  100ns ± 2%  300ns ± 2%  +200.00%  (p=0.000 n=10+10)\n';
  assert.throws(() => checkBenchstat(legacy), /No benchstat time\/op table/);
  const garbled = fixture('mixed').replace(
    'Admission-8   1.210µ ±  2%   1.512µ ±  2%  +25.00% (p=0.000 n=10)',
    'Admission-8   1.210µ ±  2%   1.512µ ±  2%  slower'
  );
  assert.notEqual(garbled, fixture('mixed'));
  assert.throws(
    () => checkBenchstat(garbled),
    /Unrecognized benchstat row: Admission-8/
  );
  const header = fixture('mixed').replace('vs base', 'versus ');
  assert.throws(
    () => checkBenchstat(header),
    /Unrecognized benchstat table header/
  );
});

function run(input, ...args) {
  return spawnSync(
    process.execPath,
    [new URL('./check-benchstat.mjs', import.meta.url).pathname, ...args],
    {
      input,
      encoding: 'utf8',
      env: { ...process.env, GITHUB_STEP_SUMMARY: '' }
    }
  );
}

test('the command exits 1 on a regression and 0 otherwise', () => {
  const failing = run(fixture('mixed'));
  assert.equal(failing.status, 1);
  assert.match(failing.stdout, /FAIL: 3 significant regressions/);
  const passing = run(fixture('noise-and-new-package'));
  assert.equal(passing.status, 0);
  assert.match(passing.stdout, /PASS: no significant regression/);
});

test('the command takes a file, a threshold, and rejects bad input with status 2', () => {
  const path = new URL('./testdata/benchstat/mixed.txt', import.meta.url)
    .pathname;
  assert.equal(run('', path).status, 1);
  assert.equal(
    run('', '--threshold', '30', path).stdout.includes('Regressions above 30%'),
    true
  );
  for (const args of [
    ['--threshold'],
    ['--threshold', 'fast'],
    ['--unknown'],
    [path, path]
  ])
    assert.equal(run('', ...args).status, 2, args.join(' '));
  const unrecognized = run('nothing to see');
  assert.equal(unrecognized.status, 2);
  assert.match(
    unrecognized.stderr,
    /check-benchstat: No benchstat time\/op table/
  );
});

// A benchstat comparison of one package whose rows are `[name, change]`, where
// a change is a percentage or `~` for noise, each at ten samples.
function comparison(rows, pkg = 'example.com/p') {
  const lines = [
    `pkg: ${pkg}`,
    '         │    base     │            head             │',
    '         │   sec/op    │   sec/op     vs base        │'
  ];
  for (const [name, change] of rows) {
    const verdict =
      change === '~'
        ? '~ (p=0.500 n=10)'
        : `${change > 0 ? '+' : ''}${change.toFixed(2)}% (p=0.002 n=10)`;
    lines.push(`${name}  100.0µ ± 2%  ${change === '~' ? '100.0µ' : `${(100 * (1 + change / 100)).toFixed(1)}µ`} ± 2%  ${verdict}`);
  }
  return `${lines.join('\n')}\n`;
}

test('a -bench pattern selects the benchmarks named, level by level', () => {
  assert.equal(
    benchPattern(['Sign/compiled/16KiB-8']),
    '^(?:BenchmarkSign)$/^(?:compiled)$/^(?:16KiB)$'
  );
  // Alternatives at each level, over the benchmarks of one package.
  assert.equal(
    benchPattern(['Sign/compiled/16KiB-8', 'Sign/interpreted/1KiB-8', 'Admission-8']),
    '^(?:BenchmarkSign|BenchmarkAdmission)$/^(?:compiled|interpreted)$/^(?:16KiB|1KiB)$'
  );
  assert.equal(benchPattern(['Admission-8']), '^(?:BenchmarkAdmission)$');
  // The GOMAXPROCS suffix is not part of the name go test matches, and
  // characters that mean something to a regexp are quoted.
  assert.equal(benchPattern(['Parse/a.b(c)+[d]-16']), '^(?:BenchmarkParse)$/^(?:a\\.b\\(c\\)\\+\\[d\\])$');
  // Every level is a regexp that matches its own name and no other one of the same length.
  for (const level of benchPattern(['Parse/a.b(c)+[d]-16', 'Parse/x|y-16']).split('/')) new RegExp(level);
  const [, second] = benchPattern(['Parse/a.b-2']).split('/');
  assert.ok(new RegExp(second).test('a.b'));
  assert.ok(!new RegExp(second).test('aXb'));
  assert.ok(!new RegExp(second).test('a.bc'));
});

test('the regressions to measure again are grouped by package', () => {
  const text = `${comparison([['Stable-8', '~'], ['Slow/a-8', 30], ['Slow/b-8', 25]], 'example.com/p')}\n${comparison([['Other-8', 40]], 'example.com/q')}`;
  const { result } = checkBenchstat(text);
  assert.deepEqual(flaggedPatterns(result), [
    'example.com/p\t^(?:BenchmarkSlow)$/^(?:a|b)$',
    'example.com/q\t^(?:BenchmarkOther)$'
  ]);
  assert.deepEqual(flaggedPatterns(checkBenchstat(comparison([['Stable-8', '~']])).result), []);
});

test('a regression counts only if measuring again reproduces it', () => {
  const first = comparison([['Stable-8', '~'], ['Chance-8', 20], ['Real-8', 30], ['Unseen-8', 15]]);
  const again = comparison([['Chance-8', '~'], ['Real-8', 28]]);
  const checked = checkBenchstat(first, { confirm: again });
  assert.equal(checked.ok, false);
  // The reproduced regression is reported as the second measurement saw it.
  assert.deepEqual(checked.result.regressions.map(({ name, percent }) => [name, percent]), [
    ['Real-8', 28],
    // The second measurement did not cover it, so it cannot clear it.
    ['Unseen-8', 15]
  ]);
  assert.deepEqual(checked.result.unconfirmed.map(({ name }) => name), ['Chance-8']);
  assert.match(checked.summary, /^FAIL: 2 significant regressions above 10%/m);
  assert.match(checked.summary, /Regressions above 10% \(reproduced when measured again\):/);
  assert.match(checked.summary, /did not reproduce when measured again \(taken for noise\):\n {2}example\.com\/p {2}Chance-8/);

  // When nothing reproduces the comparison passes, and says what it let go.
  const noise = checkBenchstat(comparison([['Chance-8', 20]]), { confirm: comparison([['Chance-8', '~']]) });
  assert.equal(noise.ok, true);
  assert.match(noise.summary, /^PASS: /m);
  assert.match(noise.summary, /did not reproduce/);

  // Measuring again changes nothing else about the verdict: a removal or an
  // empty comparison still fails, and a regression below the threshold or in
  // another unit is not made one.
  const removed = `${comparison([['Chance-8', 20]])}Gone-8  64.84n ± 12%\n`;
  assert.equal(checkBenchstat(removed, { confirm: comparison([['Chance-8', '~']]) }).ok, false);
  assert.equal(checkBenchstat(comparison([['A-8', 5]]), { confirm: comparison([['A-8', 5]]) }).ok, true);
  const reproduced = confirmRegressions(evaluate(parseBenchstat(first)), evaluate(parseBenchstat(again)));
  assert.equal(reproduced.compared, 4);
});

test('the command nominates regressions for measuring again, and judges them when it has been', () => {
  const dir = mkdtempSync(join(tmpdir(), 'check-benchstat-'));
  try {
    const flagged = join(dir, 'flagged.tsv');
    const first = comparison([['Stable-8', '~'], ['Chance-8', 20], ['Real/x-8', 30]]);
    const pending = run(first, '--flagged', flagged);
    assert.equal(pending.status, 3, pending.stdout);
    assert.match(pending.stdout, /^PENDING: 2 significant regressions above 10% in time\/op or allocs\/op, to be measured again/m);
    assert.doesNotMatch(pending.stdout, /^FAIL/m);
    assert.equal(readFileSync(flagged, 'utf8'), 'example.com/p\t^(?:BenchmarkChance|BenchmarkReal)$/^(?:x)$\n');

    // Nothing flagged is a pass with an empty list.
    const clean = run(comparison([['Stable-8', '~']]), '--flagged', flagged);
    assert.equal(clean.status, 0);
    assert.equal(readFileSync(flagged, 'utf8'), '');

    // A regression beside another failure is not left pending: it fails now.
    const removed = run(`${comparison([['Chance-8', 20]])}Gone-8  64.84n ± 12%\n`, '--flagged', flagged);
    assert.equal(removed.status, 1);
    assert.match(removed.stdout, /^FAIL: 1 significant regression/m);
    assert.match(removed.stdout, /^FAIL: 1 benchmark of the base is absent/m);

    // The second comparison settles it.
    const again = join(dir, 'again.txt');
    const firstFile = join(dir, 'first.txt');
    writeFileSync(firstFile, first);
    writeFileSync(again, comparison([['Chance-8', '~'], ['Real/x-8', 28]]));
    const settled = run('', '--confirm', again, firstFile);
    assert.equal(settled.status, 1);
    assert.match(settled.stdout, /^FAIL: 1 significant regression above 10%/m);
    writeFileSync(again, comparison([['Chance-8', '~'], ['Real/x-8', '~']]));
    const noise = run('', '--confirm', again, firstFile);
    assert.equal(noise.status, 0);
    assert.match(noise.stdout, /^PASS: /m);

    // The step summary carries the verdict only, never the pending first pass.
    const summary = join(dir, 'step-summary.md');
    const withSummary = (input, ...args) =>
      spawnSync(process.execPath, [new URL('./check-benchstat.mjs', import.meta.url).pathname, ...args], {
        input,
        encoding: 'utf8',
        env: { ...process.env, GITHUB_STEP_SUMMARY: summary }
      });
    assert.equal(withSummary(first, '--flagged', flagged).status, 3);
    assert.throws(() => readFileSync(summary, 'utf8'), /ENOENT/);
    assert.equal(withSummary('', '--confirm', again, firstFile).status, 0);
    assert.match(readFileSync(summary, 'utf8'), /PASS: /);

    // The two options are alternatives, and each needs its file.
    for (const args of [
      ['--flagged', flagged, '--confirm', again, firstFile],
      ['--flagged'],
      ['--confirm'],
      ['--confirm', join(dir, 'missing.txt'), firstFile]
    ])
      assert.equal(run('', ...args).status, 2, args.join(' '));
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('--allow-removed lets a removal pass, and the default does not', () => {
  const sample = `${comparison([['Same-8', '~']])}Gone-8  64.84n ± 12%\n`;
  assert.equal(run(sample).status, 1);
  assert.equal(run(sample, '--allow-removed').status, 0);
  assert.equal(run(fixture('all-new'), '--allow-removed').status, 1);
});
