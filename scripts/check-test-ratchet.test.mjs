import assert from 'node:assert/strict';
import test from 'node:test';
import { checkRatchet, parseDiff, wholeFileChanges } from './check-test-ratchet.mjs';

const change = (path, addedLines, status = 'M') => ({ path, status, addedLines });

test('a clean behavior test addition passes', () => {
  const violations = checkRatchet([
    change('internal/gateway/gateway_test.go', [
      'func TestRefusesOversizedBody(t *testing.T) {',
      '  if got := admit(huge); got != refused { t.Fatal(got) }'
    ]),
    change('console/src/lib/x.test.ts', ["expect(parse('1g')).toBe(1);"])
  ]);
  assert.deepEqual(violations, []);
});

test('focused and skipped tests are rejected with the rule named', () => {
  const violations = checkRatchet([
    change('console/src/lib/x.test.ts', ['it.only("works", () => {});']),
    change('internal/gateway/gateway_test.go', ['  t.Skip("later")']),
    change('console/tests/a.spec.ts', ['test.skip("journey", () => {});'])
  ]);
  assert.equal(violations.length, 3);
  assert.match(violations[0], /focused test/);
  assert.match(violations[1], /skipped test/);
  assert.match(violations[2], /skipped test/);
});

test('the checker fixture file is exempt, other script tests are not', () => {
  assert.deepEqual(
    checkRatchet([change('scripts/check-test-ratchet.test.mjs', ['it.only("x")'])]),
    []
  );
  assert.equal(
    checkRatchet([change('scripts/other.test.mjs', ['it.only("x")'])]).length,
    1
  );
});

test('snapshot assertions are rejected', () => {
  const violations = checkRatchet([
    change('console/src/lib/x.test.ts', ['expect(tree).toMatchSnapshot();'])
  ]);
  assert.equal(violations.length, 1);
  assert.match(violations[0], /snapshot/);
});

test('describe.only and inline snapshots are rejected too', () => {
  const violations = checkRatchet([
    change('console/src/lib/x.test.ts', [
      'describe.only("suite", () => {});',
      'expect(tree).toMatchInlineSnapshot();'
    ])
  ]);
  assert.equal(violations.length, 2);
});

test('skip and focus synonyms are rejected too', () => {
  const violations = checkRatchet([
    change('internal/gateway/gateway_test.go', [
      '  t.Skipf("later %s", reason)',
      '  b.Skip("benchmark later")',
      '  tb.SkipNow()'
    ]),
    change('console/src/lib/x.test.ts', [
      'it.skipIf(cond)("works", () => {});',
      'test.todo("later");',
      'xit("works", () => {});',
      'describe.concurrent.only("suite", () => {});'
    ]),
    change('console/tests/a.spec.ts', ['test.fixme("broken", () => {});'])
  ]);
  assert.equal(violations.length, 8);
  assert.match(violations[6], /focused test/);
});

test('file and error snapshot matchers are rejected too', () => {
  const violations = checkRatchet([
    change('console/src/lib/x.test.ts', [
      'expect(tree).toMatchFileSnapshot(path);',
      'expect(fn).toThrowErrorMatchingSnapshot();'
    ])
  ]);
  assert.equal(violations.length, 2);
});

test('touching a test file checks its whole current content', () => {
  const touched = wholeFileChanges(
    [change('internal/x/x_test.go', ['+ harmless added line'])],
    () => 'existing line\n  t.Skip("pre-existing")\n'
  );
  assert.equal(checkRatchet(touched).length, 1);
  const deleted = wholeFileChanges([change('internal/x/x_test.go', [], 'D')], () => {
    throw new Error('deleted files are not read');
  });
  assert.deepEqual(checkRatchet(deleted), []);
});

test('a newly added golden file is rejected, an edited one is not', () => {
  assert.equal(
    checkRatchet([change('internal/x/testdata/out.golden', ['{}'], 'A')]).length,
    1
  );
  assert.equal(
    checkRatchet([change('internal/x/testdata/out.golden.json', ['{}'], 'A')]).length,
    1
  );
  assert.deepEqual(
    checkRatchet([change('internal/x/testdata/out.golden', ['{}'], 'M')]),
    []
  );
});

test('banned patterns outside test files and on removed lines are ignored', () => {
  const diff = [
    'diff --git a/internal/x/x.go b/internal/x/x.go',
    '--- a/internal/x/x.go',
    '+++ b/internal/x/x.go',
    '@@ -1 +1 @@',
    '+// t.Skip( mentioned in a source comment is not a test skip',
    'diff --git a/internal/x/x_test.go b/internal/x/x_test.go',
    '--- a/internal/x/x_test.go',
    '+++ b/internal/x/x_test.go',
    '@@ -1 +1 @@',
    '-  t.Skip("removed skip")',
    '+  runCase(t)'
  ].join('\n');
  assert.deepEqual(checkRatchet(parseDiff(diff, new Map([['internal/x/x_test.go', 'M']]))), []);
});

test('parseDiff collects added lines per file', () => {
  const diff = [
    'diff --git a/a/x_test.go b/a/x_test.go',
    'new file mode 100644',
    '--- /dev/null',
    '+++ b/a/x_test.go',
    '@@ -0,0 +1 @@',
    '+  t.SkipNow()'
  ].join('\n');
  const violations = checkRatchet(parseDiff(diff, new Map([['a/x_test.go', 'A']])));
  assert.equal(violations.length, 1);
  assert.match(violations[0], /a\/x_test\.go/);
});
