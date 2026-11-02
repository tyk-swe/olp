import assert from 'node:assert/strict';
import test from 'node:test';
import { checkCoupling, formatReport, measureCoupling } from './check-coupling.mjs';

const file = (path, text) => ({ path, text });

test('shared tables count distinct packages, not mentions', () => {
  const measured = measureCoupling([
    file('internal/access/a.go', 'SELECT * FROM olp.users JOIN olp.users u2 ON true'),
    file('internal/access/b.go', 'SELECT * FROM olp.users'),
    file('internal/usage/c.go', 'SELECT * FROM olp.users'),
    file('internal/limits/d.go', 'SELECT * FROM olp.users JOIN olp.routes ON true')
  ]);
  assert.equal(measured.tablePackagePairs, 4);
  assert.equal(measured.sharedTables, 1);
});

test('env reads count outside config, the composition root, and mains only', () => {
  const measured = measureCoupling([
    file('internal/config/config.go', 'package config\nvar _ = os.Getenv("X")'),
    file('internal/process/run.go', 'package process\nvar _ = NewSender(policy, os.Getenv)'),
    file('internal/catalog/cmd/drift/main.go', 'package main\nvar _ = os.Getenv("X")'),
    file('internal/usage/u.go', 'package usage\nvar _ = os.Getenv("X")\nvar _, _ = os.LookupEnv("Y")')
  ]);
  assert.equal(measured.envOutsideConfig, 2);
});

test('passing os.Getenv as a value still counts as an env read', () => {
  const measured = measureCoupling([
    file('internal/export/s.go', 'package export\nvar _ = NewSender(policy, os.Getenv)')
  ]);
  assert.equal(measured.envOutsideConfig, 1);
});

test('time and package vars ignore tests, generated code, and locals', () => {
  const measured = measureCoupling([
    file(
      'internal/gateway/g.go',
      [
        'package gateway',
        'var registry = map[string]int{}',
        'var (',
        '\ta = time.Now()',
        '\tb = 2',
        ')',
        'func f() {',
        '\tvar local = time.Now()',
        '\t_ = local',
        '}'
      ].join('\n')
    ),
    file('internal/gateway/g_test.go', 'package gateway\nvar _ = time.Now()'),
    file('internal/gateway/types.gen.go', 'package gateway\nvar _ = time.Now()'),
    file('sdk/x/x.go', 'package x\nvar _ = time.Now()')
  ]);
  assert.equal(measured.timeNowCalls, 2);
  assert.equal(measured.packageVars, 3);
});

test('sentinel error vars are identity values, not mutable globals', () => {
  const measured = measureCoupling([
    file(
      'internal/batches/b.go',
      [
        'package batches',
        'var ErrTooLarge = errors.New("batch too large")',
        'var (',
        '\tErrA = errors.New("a")',
        '\tregistry = map[string]int{}',
        ')'
      ].join('\n')
    )
  ]);
  assert.equal(measured.packageVars, 1);
});

test('counts at or below baseline pass, above baseline fail by name', () => {
  const current = {
    tablePackagePairs: 10,
    sharedTables: 2,
    envOutsideConfig: 1,
    timeNowCalls: 5,
    packageVars: 7
  };
  assert.deepEqual(checkCoupling(current, { ...current, packageVars: 9 }), []);
  const violations = checkCoupling(current, { ...current, timeNowCalls: 4 });
  assert.equal(violations.length, 1);
  assert.match(violations[0], /timeNowCalls: 5 exceeds baseline 4/);
});

test('a metric missing from the baseline fails closed', () => {
  const current = {
    tablePackagePairs: 0,
    sharedTables: 0,
    envOutsideConfig: 0,
    timeNowCalls: 0,
    packageVars: 0
  };
  const violations = checkCoupling(current, { tablePackagePairs: 0 });
  assert.equal(violations.length, 4);
  assert.match(violations[0], /missing/);
});

test('the report shows each metric against its baseline delta', () => {
  const report = formatReport(
    {
      tablePackagePairs: 8,
      sharedTables: 2,
      envOutsideConfig: 0,
      timeNowCalls: 3,
      packageVars: 4
    },
    {
      tablePackagePairs: 10,
      sharedTables: 2,
      envOutsideConfig: 1,
      timeNowCalls: 3,
      packageVars: 6
    }
  );
  assert.match(report, /tablePackagePairs: 8 \(baseline 10, delta -2\)/);
  assert.match(report, /envOutsideConfig: 0 \(baseline 1, delta -1\)/);
});
