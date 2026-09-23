#!/usr/bin/env node
import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { existsSync, readFileSync } from 'node:fs';

export const FROZEN_V1_SHA256 = 'c13728040b091bf511f01733f38039cb0717dc5c8f1b8fae9e71c3ce0bf90ffd';
const FROZEN_ROWS = 47;
const STATUSES = new Set(['native', 'qualified', 'incompatible', 'unavailable', 'unknown']);
const EVIDENCE_STATES = new Set(['integrated-executed', 'integrated-partial', 'isolated-executed', 'not-executed']);
const POSITIVE = new Set(['native', 'qualified']);

const sha256 = (bytes) => createHash('sha256').update(bytes).digest('hex');
const fail = (message) => { throw new Error(message); };
const isDigest = (value) => /^[a-f0-9]{64}$/.test(value ?? '');
const isRepoPath = (value) => typeof value === 'string' && value !== '' &&
  !value.startsWith('/') && !value.split('/').includes('..') && !value.includes('\\');

export function readCompatibilityArtifact(path, revision) {
  if (revision) return execFileSync('git', ['show', `${revision}:${path}`],
    { stdio: ['ignore', 'pipe', 'ignore'] });
  return existsSync(path) ? readFileSync(path) : null;
}

function artifact(path, readArtifact, revision) {
  if (!isRepoPath(path)) fail('Invalid evidence path: ' + path);
  try {
    return readArtifact(path, revision);
  } catch {
    return null;
  }
}

function verifyEvidence(id, item, readArtifact, assessedRevision) {
  if (!item || !['test', 'assessment', 'isolated-receipt'].includes(item.kind))
    fail('Invalid evidence kind: ' + id);
  if (!['integrated', 'isolated'].includes(item.integration) ||
    !['passed', 'partial', 'not-executed'].includes(item.execution))
    fail('Invalid evidence execution: ' + id);
  if (item.integration === 'isolated' && item.kind !== 'isolated-receipt')
    fail('Isolated evidence needs a portable receipt: ' + id);
  if (item.integration === 'isolated' && !/^[a-f0-9]{40}$/.test(item.revision ?? ''))
    fail('Isolated evidence needs a pinned revision: ' + id);
  for (const field of item.kind === 'test' ? ['source', 'receipt'] : ['source']) {
    const [path, symbol] = (item[field] ?? '').split('#');
    // Test hashes describe the assessed source snapshot. Later commits may
    // legitimately edit the same file without rewriting this historical row.
    const bytes = artifact(path, readArtifact, item.kind === 'test' ? assessedRevision : undefined);
    if (bytes == null) fail('Missing evidence ' + field + ': ' + id + ' (' + path + ')');
    if (item.kind === 'test' && field === 'source') {
      if (!isDigest(item.source_sha256) || sha256(bytes) !== item.source_sha256)
        fail('Stale test evidence: ' + id);
      if (path.endsWith('_test.go')) {
        if (!/^Test[A-Za-z0-9_]+$/.test(symbol ?? '') ||
          !new RegExp('^func ' + symbol + '\\(', 'm').test(bytes.toString('utf8')))
          fail('Missing Go test symbol: ' + id);
      } else if (symbol) {
        fail('Unexpected test symbol: ' + id);
      }
    } else if (symbol) {
      fail('Unexpected evidence fragment: ' + id);
    }
    if (item.kind === 'test' && field === 'receipt' &&
      (!isDigest(item.receipt_sha256) || sha256(bytes) !== item.receipt_sha256))
      fail('Stale execution receipt: ' + id);
  }
  if (item.kind === 'test' && (item.integration !== 'integrated' || item.execution !== 'passed' ||
    !/^[a-f0-9]{40}$/.test(item.run_revision ?? '')))
    fail('Test evidence must identify an executed integrated check: ' + id);
  if (item.kind !== 'test' && (item.source_sha256 || item.receipt_sha256))
    fail('Only tests pin test/receipt hashes: ' + id);
}

function verifyRow(row, evidence, frozen) {
  if (!row || typeof row.id !== 'string' || !row.id || !STATUSES.has(row.status) ||
    !EVIDENCE_STATES.has(row.evidence_state) || !row.constraints ||
    row.empirical_quality !== 'unknown' || !Array.isArray(row.evidence) || !row.evidence.length)
    fail('Incomplete compatibility row: ' + (row?.id ?? '<missing>'));
  if (new Set(row.evidence).size !== row.evidence.length)
    fail('Duplicate evidence in row: ' + row.id);
  const refs = row.evidence.map((id) => evidence[id] ?? fail('Missing evidence reference: ' + row.id + '/' + id));
  const integratedPass = refs.some((ref) => ref.kind === 'test' &&
    ref.integration === 'integrated' && ref.execution === 'passed');
  const isolatedPass = refs.some((ref) => ref.kind === 'isolated-receipt' &&
    ref.integration === 'isolated' && ref.execution === 'passed');
  if (POSITIVE.has(row.status) && (row.evidence_state !== 'integrated-executed' || !integratedPass))
    fail('Unsupported positive qualification: ' + row.id);
  if (row.status === 'incompatible' &&
    (row.evidence_state !== 'integrated-executed' || !refs.some((ref) =>
      ref.kind === 'test' && ref.outcome === 'zero-dispatch-refusal')))
    fail('Incompatibility needs executed zero-dispatch evidence: ' + row.id);
  if (row.evidence_state === 'integrated-executed' && !integratedPass)
    fail('Integrated execution has no test: ' + row.id);
  if (row.evidence_state === 'isolated-executed' && !isolatedPass)
    fail('Isolated execution has no receipt: ' + row.id);
  if (row.evidence_state === 'not-executed' && (integratedPass || isolatedPass))
    fail('Execution state hides a passing test: ' + row.id);
  if (frozen) {
    if (!isDigest(row.frozen_row_sha256) ||
      sha256(JSON.stringify(frozen)) !== row.frozen_row_sha256)
      fail('Stale frozen row: ' + row.id);
  } else {
    for (const field of ['operation', 'profile', 'dialect', 'mode', 'client'])
      if (typeof row[field] !== 'string' || !row[field]) fail('Incomplete additive row: ' + row.id);
    if (!Array.isArray(row.features) || !row.features.length ||
      row.features.some((feature) => typeof feature !== 'string' || !feature))
      fail('Missing additive features: ' + row.id);
  }
}

export function validateCompatibilityMatrix(inventoryBytes, matrix, readArtifact = readCompatibilityArtifact) {
  if (sha256(inventoryBytes) !== FROZEN_V1_SHA256)
    fail('Frozen v1 inventory changed; create a new version');
  const inventory = JSON.parse(inventoryBytes);
  if (inventory.version !== 1 || inventory.rows?.length !== FROZEN_ROWS ||
    matrix.version !== 1 || matrix.frozen_inventory?.sha256 !== FROZEN_V1_SHA256 ||
    matrix.frozen_inventory?.row_count !== FROZEN_ROWS ||
    matrix.frozen_inventory?.path !== 'tests/fixtures/fidelity/v1/inventory.json' ||
    matrix.frozen_inventory?.source_revision !== inventory.source_revision)
    fail('Frozen denominator or matrix version changed');
  const frozen = new Map();
  for (const row of inventory.rows) {
    if (!row.id || frozen.has(row.id)) fail('Duplicate frozen inventory ID: ' + row.id);
    frozen.set(row.id, row);
  }
  if (!matrix.assessed_revision || !/^[a-f0-9]{40}$/.test(matrix.assessed_revision))
    fail('Matrix needs a pinned source revision');
  if (!matrix.evidence || typeof matrix.evidence !== 'object' ||
    !Array.isArray(matrix.rows) || !Array.isArray(matrix.additions))
    fail('Matrix lacks evidence or row lists');
  for (const [id, item] of Object.entries(matrix.evidence))
    verifyEvidence(id, item, readArtifact, matrix.assessed_revision);
  const seen = new Set();
  for (const row of matrix.rows) {
    if (seen.has(row.id)) fail('Duplicate matrix row: ' + row.id);
    seen.add(row.id);
    const reference = frozen.get(row.id);
    if (!reference) fail('Unknown frozen row: ' + row.id);
    verifyRow(row, matrix.evidence, reference);
  }
  for (const id of frozen.keys()) if (!seen.has(id)) fail('Dropped frozen row: ' + id);
  const tuples = new Set([...frozen.values()].map((row) =>
    [row.operation, row.profile, row.dialect, row.mode, row.client].join('\u0000')));
  for (const row of matrix.additions) {
    if (seen.has(row.id)) fail('Duplicate additive row: ' + row.id);
    seen.add(row.id);
    verifyRow(row, matrix.evidence, null);
    const tuple = [row.operation, row.profile, row.dialect, row.mode, row.client].join('\u0000');
    if (tuples.has(tuple)) fail('Additive row duplicates existing tuple: ' + row.id);
    tuples.add(tuple);
  }
  return { frozen: frozen.size, additive: matrix.additions.length,
    statuses: Object.fromEntries([...STATUSES].map((status) =>
      [status, [...matrix.rows, ...matrix.additions].filter((row) => row.status === status).length])) };
}

if (import.meta.main) {
  const result = validateCompatibilityMatrix(
    readFileSync('tests/fixtures/fidelity/v1/inventory.json'),
    JSON.parse(readFileSync('docs/qualification/fidelity/compatibility-matrix-v1.json', 'utf8'))
  );
  console.log('Validated compatibility matrix:', JSON.stringify(result));
}
