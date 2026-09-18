import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { reconcileEvidence } from './release-inventory.mjs';

const read = (path) => JSON.parse(readFileSync(path, 'utf8'));
const frozen = read('docs/roadmap/evidence/reference-inventory.json');
const manifest = read('docs/roadmap/evidence/release-behaviors.json');

test('every frozen scope has explicit evidence and accounting points to persistence tests', () => {
  const result = reconcileEvidence(frozen, manifest);
  assert.equal(result.referenceSuites.size, frozen.suites.length);
  const accounting = result.referenceSuites.get('tests/persistence/operations_postgres/attempt_accounting.rs');
  assert.ok(accounting.flatMap((item) => item.tests).includes(
    'tests/integration/accounting_test.go#TestAccountingPersistsPricedAttemptsAndIsReplaySafe'
  ));
});

test('new frozen suites cannot inherit coverage from a filename pattern', () => {
  const changed = structuredClone(frozen);
  changed.suites.push({ path: 'tests/persistence/operations_postgres/new_behavior.rs' });
  assert.throws(() => reconcileEvidence(changed, manifest), /Unmapped frozen referenceSuites/);
});

test('stale suite mappings are rejected', () => {
  const changed = structuredClone(manifest);
  changed.behaviors[0].referenceSuites.push('tests/removed.rs');
  assert.throws(() => reconcileEvidence(frozen, changed), /Unknown frozen referenceSuites/);
});

test('an existing file cannot hide a removed or renamed successor test', () => {
  const changed = structuredClone(manifest);
  changed.behaviors.find((item) => item.id === 'accounting').tests[0] =
    'tests/integration/accounting_test.go#TestRemovedAccountingScenario';
  assert.throws(() => reconcileEvidence(frozen, changed), /Missing test symbol/);
});

test('behavioral Go evidence must name the test and preserve its file', () => {
  for (const [reference, error] of [
    ['tests/integration/accounting_test.go', /Name the successor Go test/],
    ['tests/integration/removed_test.go#TestAccounting', /Missing evidence/]
  ]) {
    const changed = structuredClone(manifest);
    changed.behaviors.find((item) => item.id === 'accounting').tests[0] = reference;
    assert.throws(() => reconcileEvidence(frozen, changed), error);
  }
});

test('management, inference and CLI scopes cannot lose their evidence', () => {
  for (const field of ['managementOperations', 'inferenceOperations', 'commands']) {
    const changed = structuredClone(manifest);
    for (const behavior of changed.behaviors) delete behavior[field];
    assert.throws(() => reconcileEvidence(frozen, changed), new RegExp('Unmapped frozen ' + field));
  }
});

test('retiring harness code requires an explicit reason', () => {
  const changed = structuredClone(manifest);
  delete changed.behaviors.find((item) => item.disposition === 'replaced harness').reason;
  assert.throws(() => reconcileEvidence(frozen, changed), /Harness retirement needs a reason/);
});
