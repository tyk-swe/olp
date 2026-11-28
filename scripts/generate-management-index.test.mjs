import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

const generator = join(import.meta.dirname, 'generate-management-index.mjs');
const text = (body) => `${JSON.stringify(body, null, 2)}\n`;
const reference = (target) => ({ content: { 'application/json': { schema: { $ref: target } } } });
const operation = (operationId, tag, target) => ({
  operationId,
  ...(tag ? { tags: [tag] } : {}),
  responses: target ? { 200: reference(target) } : {}
});

function combine(t, files) {
  const directory = mkdtempSync(join(tmpdir(), 'olp-contract-'));
  t.after(() => rmSync(directory, { recursive: true }));
  for (const [file, content] of Object.entries(files)) writeFileSync(join(directory, file), content);
  return spawnSync(process.execPath, [generator, directory, join(directory, 'index.out')], { encoding: 'utf8' });
}

const valid = {
  'shared.json': text({ components: { schemas: { Problem: { type: 'object' } } } }),
  'document.json': text({ openapi: '3.1.0', paths: { '/untagged': { get: operation('untagged') } } }),
  'cache.json': text({
    paths: { '/cache': { get: operation('get_cache', 'Cache', './shared.json#/components/schemas/Problem') } },
    components: { schemas: { Entry: { type: 'object' } } }
  })
};

test('fragments that each own their entries are accepted', (t) => {
  const result = combine(t, valid);
  assert.equal(result.status, 0, result.stderr);
});

test('a path, component, operation, webhook or document field defined twice is refused', (t) => {
  const result = combine(t, {
    ...valid,
    'usage.json': text({
      openapi: '3.1.0',
      paths: {
        '/cache': { delete: operation('purge_cache', 'usage') },
        '/usage': { get: operation('get_cache', 'usage') }
      },
      webhooks: { 'usage.ready': { post: operation('usage_ready', 'usage') } },
      components: { schemas: { Problem: { type: 'object' } } }
    }),
    'users.json': text({ webhooks: { 'usage.ready': { post: operation('users_ready', 'users') } } })
  });
  assert.equal(result.status, 1);
  assert.deepEqual(result.stderr.trim().split('\n').sort(), [
    'components schemas Problem is defined in shared.json and usage.json',
    'openapi is defined in document.json and usage.json',
    'operationId get_cache is used by GET /cache in cache.json and GET /usage in usage.json',
    'paths /cache is defined in cache.json and usage.json',
    'webhooks usage.ready is defined in usage.json and users.json'
  ]);
});

test('a key repeated inside one fragment is refused instead of silently dropped', (t) => {
  const result = combine(t, {
    ...valid,
    'usage.json': '{\n  "paths": {\n    "/usage": {},\n    "/usage": {}\n  }\n}\n'
  });
  assert.equal(result.status, 1);
  assert.match(result.stderr, /^usage\.json is not canonical two-space JSON/);
});

test('an operation outside its tag and a reference to anything but a reachable component are refused', (t) => {
  const result = combine(t, {
    ...valid,
    'usage.json': text({
      paths: {
        '/usage': { get: operation('get_usage', 'cache', './cache.json#/components/schemas/Entry') },
        '/usage/field': { get: operation('get_field', 'usage', './shared.json#/components/schemas/Problem/type') }
      }
    })
  });
  assert.equal(result.status, 1);
  assert.match(result.stderr, /GET \/usage in usage\.json is tagged cache, so it belongs in cache\.json/);
  assert.match(result.stderr, /usage\.json references \.\/cache\.json#\/components\/schemas\/Entry/);
  assert.match(result.stderr, /usage\.json references \.\/shared\.json#\/components\/schemas\/Problem\/type/);
});
