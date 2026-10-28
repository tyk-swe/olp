import assert from 'node:assert/strict';
import test from 'node:test';
import { assessMutation } from './check-mutation.mjs';

const report = (overrides = {}) => ({
  test_efficacy: 71.43,
  mutations_coverage: 41.18,
  mutants_total: 7,
  mutants_killed: 5,
  mutants_lived: 2,
  mutants_not_covered: 10,
  mutants_not_viable: 0,
  ...overrides
});
const thresholds = { efficacy: 70, mcover: 40 };

test('a report at the pilot wire values passes both thresholds', () => {
  assert.deepEqual(assessMutation(report(), thresholds), []);
});

test('values exactly at the thresholds pass, just below fails', () => {
  assert.deepEqual(
    assessMutation(report({ test_efficacy: 70, mutations_coverage: 40 }), thresholds),
    []
  );
  assert.equal(
    assessMutation(report({ test_efficacy: 69.99, mutations_coverage: 39.99 }), thresholds)
      .length,
    2
  );
});

test('only non-viable mutants means nothing killable and passes', () => {
  const nonViable = report({
    test_efficacy: 0,
    mutations_coverage: 0,
    mutants_total: 4,
    mutants_killed: 0,
    mutants_lived: 0,
    mutants_not_covered: 0,
    mutants_not_viable: 4
  });
  assert.deepEqual(assessMutation(nonViable, thresholds), []);
});

test('efficacy below the threshold fails and names both numbers', () => {
  const violations = assessMutation(report({ test_efficacy: 50 }), thresholds);
  assert.equal(violations.length, 1);
  assert.match(violations[0], /efficacy 50\.00% is below 70%/);
});

test('vacuous efficacy with almost nothing covered fails on mutant coverage', () => {
  const violations = assessMutation(
    report({
      test_efficacy: 100,
      mutations_coverage: 1.09,
      mutants_total: 1,
      mutants_killed: 1,
      mutants_lived: 0,
      mutants_not_covered: 91
    }),
    thresholds
  );
  assert.equal(violations.length, 1);
  assert.match(violations[0], /mutant coverage 1\.09% is below 40%/);
});

test('a package with no mutants at all has nothing to kill and passes', () => {
  const empty = report({
    test_efficacy: 0,
    mutations_coverage: 0,
    mutants_total: 0,
    mutants_killed: 0,
    mutants_lived: 0,
    mutants_not_covered: 0
  });
  assert.deepEqual(assessMutation(empty, thresholds), []);
});

test('uncovered mutants with nothing killed fail both thresholds', () => {
  const violations = assessMutation(
    report({
      test_efficacy: 0,
      mutations_coverage: 0,
      mutants_total: 0,
      mutants_killed: 0,
      mutants_lived: 0,
      mutants_not_covered: 12
    }),
    thresholds
  );
  assert.equal(violations.length, 2);
});

test('a report missing its summary fields is rejected, not passed', () => {
  assert.throws(() => assessMutation({}, thresholds), /missing/);
});

test('non-numeric thresholds are rejected instead of passing everything', () => {
  assert.throws(
    () => assessMutation(report({ test_efficacy: 0 }), { efficacy: NaN, mcover: 40 }),
    /finite/
  );
});
