import assert from 'node:assert/strict';
import { mkdirSync, rmSync, writeFileSync } from 'node:fs';
import test from 'node:test';
import { formatInventory, summarizeInventory, walk } from './test-baseline.mjs';

const files = [
  'internal/gateway/gateway.go',
  'internal/gateway/gateway_test.go',
  'internal/gateway/bench_test.go',
  'internal/secrets/store.go',
  'internal/secrets/key.go',
  'sdk/client.go',
  'sdk/client_test.go',
  'console/src/lib/x.ts',
  'console/src/lib/x.test.ts',
  'console/src/lib/y.test.tsx',
  'console/src/lib/features/big/a.ts',
  'console/src/lib/features/big/b.svelte',
  'console/src/lib/features/big/types.d.ts',
  'console/tests/a.spec.ts',
  'tests/integration/main_test.go',
  'scripts/foo.test.mjs',
  'scripts/z.test.mts',
  'cmd/olp/main.go',
  'main.go',
  'README.md'
];

test('counts test files per area and totals them', () => {
  const summary = summarizeInventory(files);
  assert.equal(summary.goTests, 4);
  assert.equal(summary.jsTests, 5);
  assert.equal(summary.byArea.get('internal').goTests, 2);
  assert.equal(summary.byArea.get('console').jsTests, 3);
});

test('lists Go packages with source but no sibling test file', () => {
  const summary = summarizeInventory(files);
  assert.deepEqual(summary.unmappedGoPackages, [
    { dir: '.', sourceFiles: 1 },
    { dir: 'cmd/olp', sourceFiles: 1 },
    { dir: 'internal/secrets', sourceFiles: 2 }
  ]);
  assert.equal(summary.goPackages, 5);
  assert.equal(summary.mappedGoPackages, 2);
});

test('a sibling test file removes the package from the unmapped list', () => {
  const summary = summarizeInventory([...files, 'internal/secrets/store_test.go']);
  assert.deepEqual(
    summary.unmappedGoPackages.map((p) => p.dir),
    ['.', 'cmd/olp']
  );
});

test('lists console source directories with no sibling test, largest first', () => {
  const summary = summarizeInventory(files);
  assert.deepEqual(summary.unmappedJsDirs, [
    { dir: 'console/src/lib/features/big', sourceFiles: 2 }
  ]);
  assert.equal(summary.jsDirs, 2);
  assert.equal(summary.mappedJsDirs, 1);
});

test('report carries totals, per-area rows and unmapped rows in any input order', () => {
  const report = formatInventory(summarizeInventory(files));
  assert.match(report, /Go test files: 4\. JS\/TS test files: 5\./);
  assert.match(report, /\| internal \| 2 \| 0 \|/);
  assert.match(report, /\| console \| 0 \| 3 \|/);
  assert.match(report, /Go packages with source: 5\. With a sibling test file: 2\./);
  assert.match(report, /\| internal\/secrets \| 2 \|/);
  assert.match(report, /Console source directories: 2\. With a sibling test file: 1\./);
  assert.match(report, /\| console\/src\/lib\/features\/big \| 2 \|/);
  assert.equal(formatInventory(summarizeInventory([...files].reverse())), report);
});

test('walk skips dependency and build output directories', () => {
  const root = '.local/test-baseline-fixture';
  rmSync(root, { recursive: true, force: true });
  mkdirSync(`${root}/node_modules/pkg`, { recursive: true });
  mkdirSync(`${root}/src`, { recursive: true });
  writeFileSync(`${root}/node_modules/pkg/index.js`, '');
  writeFileSync(`${root}/src/keep.go`, '');
  try {
    assert.deepEqual(walk(root), [`${root}/src/keep.go`]);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
