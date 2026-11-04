import assert from 'node:assert/strict';
import test from 'node:test';
import { checkStructure, layerViolations, listStructureEntries } from './check-structure.mjs';

const directory = (path) => ({ isDirectory: true, path });
const file = (path) => ({ isDirectory: false, path });

test('layer folders, root buckets and layer files are rejected', () => {
  const entries = [
    directory('console/src/lib/api'),
    directory('console/src/lib/components'),
    directory('console/src/lib/features/providers/models'),
    directory('console/src/routes/(console)/models'),
    directory('plugins'),
    directory('sdk'),
    file('internal/cache/store.go'),
    file('internal/mcp/session_store_test.go'),
    file('internal/management/contract/types.gen.go'),
    directory('console/src/lib/features/access'),
    directory('internal/plugins')
  ];
  assert.deepEqual(layerViolations(entries), [
    'console/src/lib/api',
    'console/src/lib/components',
    'console/src/lib/features/providers/models',
    'console/src/routes/(console)/models',
    'internal/cache/store.go',
    'internal/mcp/session_store_test.go',
    'plugins',
    'sdk'
  ]);
});

test('capability folders and files pass', () => {
  const entries = [
    directory('console/src/lib/management'),
    directory('internal/gateway'),
    directory('plugin/abi'),
    file('internal/cache/cache.go'),
    file('management/management.json')
  ];
  assert.equal(checkStructure(entries), entries.length);
});

test('the repository contains no layer-first structure', () => {
  const entries = listStructureEntries();
  assert.equal(checkStructure(entries), entries.length);
});
