import assert from 'node:assert/strict';
import { existsSync, readFileSync } from 'node:fs';
import test from 'node:test';
import { checkDocs, collectAnchors, findLiveProvenClaims, listScopedDocs } from './check-docs.mjs';

const reader =
  (files) =>
  (file) =>
    files.get(file);
const exists = (files) => (file) => files.has(file) || file.endsWith('/');

test('documentation anchors follow the rendered heading contract', () => {
  const anchors = collectAnchors(
    ['# Routes & revisions', '## Routes & revisions', '<a id="stable"></a>'].join('\n')
  );
  assert.deepEqual([...anchors], ['routes--revisions', 'routes--revisions-1', 'stable']);
});

test('broken files, anchors, index entries and em dashes are rejected', () => {
  const handbook = new Map([
    ['docs/README.md', '[Guide](guide.md#present)\n[Missing](missing.md)'],
    ['docs/guide.md', '# Present\n\n[Bad anchor](#absent)\n\nThis sentence uses — punctuation.'],
    ['docs/unindexed.md', '# Unindexed']
  ]);
  let message = '';
  try {
    checkDocs([...handbook.keys()], reader(handbook), exists(handbook));
  } catch (error) {
    message = error.message;
  }
  assert.match(message, /broken local link missing\.md/);
  assert.match(message, /broken local anchor #absent/);
  assert.match(message, /contains an em dash/);
  assert.match(message, /missing handbook link for unindexed\.md/);
});

test('live-proven claims are rejected in guides and console copy', () => {
  assert.deepEqual(findLiveProvenClaims('Codex bands are live-proven only'), [
    'live-proven only',
    'are live-proven'
  ]);
  assert.deepEqual(findLiveProvenClaims('live proven ordering for every adapter'), [
    'live proven ordering'
  ]);
  assert.deepEqual(findLiveProvenClaims('A mechanism counts as live proven only when a lane runs green'), []);
  assert.deepEqual(findLiveProvenClaims('Codex bands, fixture-proven only'), []);
  const userFacing = [
    'docs/use/code-mode.md',
    'console/src/lib/features/code-mode/presentation.ts',
    'console/src/lib/features/code-mode/RouteEditor.svelte',
    'console/src/lib/features/code-mode/RouteRevisions.svelte'
  ];
  for (const file of userFacing) {
    assert.deepEqual(findLiveProvenClaims(readFileSync(file, 'utf8')), [], file);
  }
});

test('the scoped repository documentation passes the documentation contract', () => {
  const files = listScopedDocs();
  assert.equal(
    checkDocs(
      files,
      (file) => (existsSync(file) ? readFileSync(file, 'utf8') : undefined),
      (file) => existsSync(file)
    ),
    files.length
  );
});
