import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { readCompatibilityArtifact, validateCompatibilityMatrix } from './compatibility-matrix.mjs';

const inventory = readFileSync('tests/fixtures/fidelity/v1/inventory.json');
const matrix = JSON.parse(readFileSync('docs/qualification/fidelity/compatibility-matrix-v1.json', 'utf8'));
const copy = () => structuredClone(matrix);
const validate = (value, readArtifact) => validateCompatibilityMatrix(inventory, value, readArtifact);

test('all 47 frozen rows and additive operation/client rows have scoped evidence', () => {
  const result = validate(matrix);
  assert.equal(result.frozen, 47);
  assert.equal(result.additive, 52);
  assert.deepEqual(result.statuses, {
    native: 36, qualified: 21, incompatible: 2, unavailable: 1, unknown: 39
  });
  assert.ok(matrix.rows.every((row) => row.empirical_quality === 'unknown'));
  assert.ok(matrix.additions.every((row) => row.empirical_quality === 'unknown'));
});

test('a missing, duplicate, extra or stale frozen row cannot disappear into additive coverage', () => {
  const dropped = copy();
  dropped.rows.pop();
  assert.throws(() => validate(dropped), /Dropped frozen row/);
  const duplicate = copy();
  duplicate.rows.push(structuredClone(duplicate.rows[0]));
  assert.throws(() => validate(duplicate), /Duplicate matrix row/);
  const extra = copy();
  extra.rows.push({ ...extra.rows[0], id: 'unreviewed-frozen-row' });
  assert.throws(() => validate(extra), /Unknown frozen row/);
  const stale = copy();
  stale.rows[0].frozen_row_sha256 = '0'.repeat(64);
  assert.throws(() => validate(stale), /Stale frozen row/);
});

test('the frozen v1 bytes stay pinned independently of the matrix metadata', () => {
  const modified = Buffer.from(inventory.toString('utf8').replace(
    'openai-openai-chat-unary', 'openai-openai-chat-other'
  ));
  assert.throws(() => validateCompatibilityMatrix(modified, matrix), /Frozen v1 inventory changed/);
  const forged = copy();
  forged.frozen_inventory.sha256 = '0'.repeat(64);
  assert.throws(() => validate(forged), /Frozen denominator/);
});

test('additions cannot replace or duplicate a frozen tuple', () => {
  const duplicate = copy();
  duplicate.additions[0].id = duplicate.rows[0].id;
  assert.throws(() => validate(duplicate), /Duplicate additive row/);
  const sameTuple = copy();
  const frozen = JSON.parse(inventory).rows[0];
  Object.assign(sameTuple.additions[0], {
    operation: frozen.operation, profile: frozen.profile, dialect: frozen.dialect,
    mode: frozen.mode, client: frozen.client
  });
  assert.throws(() => validate(sameTuple), /duplicates existing tuple/);
});

test('missing, renamed or altered execution evidence fails validation', () => {
  const missingRef = copy();
  missingRef.rows[0].evidence = ['missing-execution'];
  assert.throws(() => validate(missingRef), /Missing evidence reference/);
  const missingSource = copy();
  missingSource.evidence['native-count-classification'].source =
    'tests/integration/removed_test.go#TestStrictClassificationAndNativeCountPublic';
  assert.throws(() => validate(missingSource), /Missing evidence source/);
  const renamedTest = copy();
  renamedTest.evidence['native-count-classification'].source =
    'tests/integration/strict_operations_test.go#TestRemovedQualification';
  assert.throws(() => validate(renamedTest), /Missing Go test symbol/);
  const alteredTest = copy();
  alteredTest.evidence['native-count-classification'].source_sha256 = '0'.repeat(64);
  assert.throws(() => validate(alteredTest), /Stale test evidence/);
  const alteredReceipt = copy();
  alteredReceipt.evidence['native-count-classification'].receipt_sha256 = '0'.repeat(64);
  assert.throws(() => validate(alteredReceipt), /Stale execution receipt/);
  const staleRun = copy();
  staleRun.evidence['strict-profile-control'].run_revision =
    '79168e7083ede1290ac92451cab4283dce213c72';
  assert.throws(() => validate(staleRun), /Run revision does not match test source/);
  const missingArtifact = copy();
  const readArtifact = (path, revision) => path === 'docs/qualification/fidelity/compatibility-matrix-v1.md'
    ? null : readCompatibilityArtifact(path, revision);
  assert.throws(() => validate(missingArtifact, readArtifact), /Missing evidence source/);
  const absentRevision = copy();
  absentRevision.assessed_revision = '0'.repeat(40);
  assert.throws(() => validate(absentRevision), /Missing evidence source/);
});

test('positive and incompatibility statuses require integrated executed proof', () => {
  const promoted = copy();
  promoted.rows[21].status = 'qualified';
  assert.throws(() => validate(promoted), /Unsupported positive qualification/);
  const partial = copy();
  const row = partial.additions.find((item) => item.id === 'voyage-rerank-native');
  row.status = 'native';
  assert.throws(() => validate(partial), /Unsupported positive qualification/);
  const incompatible = copy();
  incompatible.rows[0].status = 'incompatible';
  incompatible.rows[0].evidence_state = 'integrated-executed';
  assert.throws(() => validate(incompatible), /Incompatibility needs executed zero-dispatch evidence/);
  const inventedRefusal = copy();
  inventedRefusal.evidence['gemini-live-resumption-refusal'].outcome = 'positive';
  assert.throws(() => validate(inventedRefusal), /Incompatibility needs executed zero-dispatch evidence/);
});
