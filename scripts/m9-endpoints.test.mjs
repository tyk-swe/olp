import assert from 'node:assert/strict';
import { readFileSync, existsSync } from 'node:fs';
import { test } from 'node:test';

const root = new URL('../', import.meta.url);
const ledger = JSON.parse(readFileSync(new URL('tests/clients/m9-endpoints.json', root)));

test('M9 endpoint ledger pins the installed SDK contract and uniquely identifies tuples', () => {
  const module = readFileSync(new URL('tests/clients/go.mod', root), 'utf8');
  for (const sdk of Object.values(ledger.sdk_locks)) {
    assert.ok(module.includes(`${sdk.module} v${sdk.version}`), `${sdk.module} pin drifted`);
  }
  const tuples = new Set();
  const slices = new Set();
  for (const row of ledger.endpoints) {
    slices.add(row.slice);
    const key = [row.method, row.path, row.source_dialect, row.provider, row.profile, row.mode].join('|');
    assert.ok(!tuples.has(key), `duplicate tuple ${key}`);
    tuples.add(key);
    assert.ok(Object.hasOwn(ledger.status_contract, row.status));
    assert.ok(row.path.startsWith('/'));
    assert.ok(row.reference_revision);
    assert.ok(row.operation && row.profile_revision && row.dialect_revision);
    assert.ok(['http', 'sse', 'websocket', 'bedrock-bidi'].includes(row.transport));
    if (row.status === 'implemented') {
      assert.ok(row.evidence.length > 0);
      assert.ok(row.supported_fields.length > 0);
      for (const path of row.evidence) assert.ok(existsSync(new URL(path, root)), path);
      assert.equal(row.live_qualification, 'required');
    }
    if (row.status === 'excluded') assert.ok(row.expected_refusal?.reason);
  }
  for (let n = 1; n <= 10; n++) assert.ok(slices.has(`M9.${n}`));
});
