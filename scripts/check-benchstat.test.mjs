import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { checkBenchstat, flaggedPatterns } from './check-benchstat.mjs';

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

test('--allow-removed lets a removal pass, and the default does not', () => {
  const sample = `${comparison([['Same-8', '~']])}Gone-8  64.84n ± 12%\n`;
  assert.equal(run(sample).status, 1);
  assert.equal(run(sample, '--allow-removed').status, 0);
  assert.equal(run(fixture('all-new'), '--allow-removed').status, 1);
});

test('the rerun selector measures each flagged mixed-depth benchmark once', () => {
  const rows = [
    ['Meter/gpt-4/code/50000-4', 20],
    ['Estimate/openai-cl100k-4', 20],
    ['WalkAgainstLegacy/small/walk+o200k-4', 20],
    ['Direct-4', 20],
    ['Tree/a/b-4', 20],
    ['Tree/a/b/c-4', 20]
  ];
  const result = checkBenchstat(comparison(rows)).result;
  const patterns = flaggedPatterns(result);
  const dir = mkdtempSync(join(tmpdir(), 'bench-selection-'));
  try {
    const path = join(dir, 'selection_test.go');
    writeFileSync(path, `package selection

import "testing"

func selected(t *testing.T) {
  t.Run("selected", func(t *testing.T) {})
}

func TestBenchmarkDirect(t *testing.T) { selected(t) }
func TestBenchmarkEstimate(t *testing.T) { t.Run("openai-cl100k", selected) }
func TestBenchmarkWalkAgainstLegacy(t *testing.T) {
  t.Run("small", func(t *testing.T) { t.Run("walk+o200k", selected) })
}
func TestBenchmarkMeter(t *testing.T) {
  t.Run("gpt-4", func(t *testing.T) {
    t.Run("code", func(t *testing.T) { t.Run("50000", selected) })
  })
}
func TestBenchmarkTree(t *testing.T) {
  t.Run("a/b", selected)
  t.Run("a", func(t *testing.T) { t.Run("b/c", selected) })
}
`);
    const selected = patterns.flatMap((entry) => {
      const [pkg, pattern] = entry.split('\t');
      assert.equal(pkg, 'example.com/p');
      const selection = spawnSync('go', [
        'test', '-v', '-count=1', '-run', pattern.replaceAll('Benchmark', 'TestBenchmark'), path
      ], { encoding: 'utf8' });
      assert.equal(selection.status, 0, selection.stderr);
      return selection.stdout
        .split('\n')
        .filter((line) => /^=== RUN\s+TestBenchmark.*\/selected$/.test(line))
        .map((line) => line.replace(/^=== RUN\s+TestBenchmark/, '').replace(/\/selected$/, ''));
    }).toSorted();
    assert.deepEqual(selected, rows.map(([name]) => name.replace(/-\d+$/, '')).toSorted());
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('a missing rerun remains a failure and is not reported as reproduced', () => {
  const first = comparison([
    ['Remeasured-8', 30],
    ['Missing-8', 20],
    ['Noise-8', 40]
  ]);
  const second = comparison([
    ['Remeasured-8', 25],
    ['Noise-8', '~']
  ]);
  const { ok, result, summary } = checkBenchstat(first, { confirm: second });
  assert.equal(ok, false);
  assert.deepEqual(names(result.regressions), ['p Remeasured-8 time/op', 'p Missing-8 time/op']);
  assert.deepEqual(names(result.unconfirmed), ['p Noise-8 time/op']);
  assert.match(summary, /Regressions above 10% \(reproduced when measured again\):\n  example\.com\/p  Remeasured-8/);
  assert.match(summary, /Regressions above 10% not measured again \(retained as failures\):\n  example\.com\/p  Missing-8/);
  assert.match(summary, /Regressions above 10% that did not reproduce when measured again \(taken for noise\):\n  example\.com\/p  Noise-8/);
});
