import assert from 'node:assert/strict';
import test from 'node:test';
import { compareCoverage, parseCoverage } from './check-coverage.mjs';

const output = [
  'ok  \tgithub.com/tyk-swe/olp/internal/gateway\t2.357s\tcoverage: 42.2% of statements',
  '\tgithub.com/tyk-swe/olp/internal/billing\t\tcoverage: 0.0% of statements',
  '?   \tgithub.com/tyk-swe/olp/openapi\t[no test files]'
].join('\n');

test('parseCoverage reads package percentages and skips no-test packages', () => {
  assert.deepEqual(
    parseCoverage(output),
    new Map([
      ['github.com/tyk-swe/olp/internal/gateway', 42.2],
      ['github.com/tyk-swe/olp/internal/billing', 0]
    ])
  );
});

test('equal or rising coverage passes', () => {
  const base = parseCoverage(output);
  const head = new Map(base);
  head.set('github.com/tyk-swe/olp/internal/gateway', 43);
  assert.deepEqual(compareCoverage(base, head), []);
});

test('a drop fails and names the package and both numbers', () => {
  const base = parseCoverage(output);
  const head = new Map(base);
  head.set('github.com/tyk-swe/olp/internal/gateway', 40.1);
  const violations = compareCoverage(base, head);
  assert.equal(violations.length, 1);
  assert.match(violations[0], /internal\/gateway coverage dropped from 42\.2% to 40\.1%/);
});

test('a base package missing at head fails instead of looking uncovered', () => {
  const base = parseCoverage(output);
  const head = new Map([...base].slice(1));
  const violations = compareCoverage(base, head);
  assert.equal(violations.length, 1);
  assert.match(violations[0], /missing at head/);
});

test('a package new at head has no base to drop from and passes', () => {
  const base = parseCoverage(output);
  const head = new Map(base);
  head.set('github.com/tyk-swe/olp/internal/newpkg', 5);
  assert.deepEqual(compareCoverage(base, head), []);
});
