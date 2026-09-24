import assert from 'node:assert/strict';
import { test } from 'node:test';
import { execFileSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, renameSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { assertOnlyEvidenceChanges, changedPaths, postProductEvidencePrefix } from './fidelity-final-source-lock.mjs';

test('post-P10 C source permits evidence only and locks all tracked build inputs', () => {
  assert.deepEqual(assertOnlyEvidenceChanges([
    `${postProductEvidencePrefix}final-source-repeat-v1/product-lock.json`,
    `${postProductEvidencePrefix}source-paired-v2/paired-r10.json`,
    `${postProductEvidencePrefix}lifecycle-v3/strict-candidate.jsonl`
  ]).length, 3);
  for (const path of [
    'go.mod', 'go.sum', 'internal/connectors/profiles.go',
    'internal/database/migrations/0030_strict_video_sources.sql',
    'internal/limits/scripts/reserve_limits.lua', 'internal/usage/scripts/ack_delete.lua',
    'openapi/management.json', 'tests/integration/continuation_candidate_paired_v2_test.go',
    'tests/fixtures/fidelity/v1/anthropic-tool-next-request.json',
    'scripts/continuation-barrier-paired-v2-attempt3.mjs',
    'docs/qualification/fidelity/qualification-status-v1.md'
  ]) assert.throws(() => assertOnlyEvidenceChanges([path]), /changed after P10/);
});

test('identical rename into evidence still exposes and rejects the removed build input', () => {
  const root = mkdtempSync(join(tmpdir(), 'olp-product-lock-rename-'));
  const run = (...args) => execFileSync('git', ['-C', root, ...args], { encoding: 'utf8' }).trim();
  try {
    run('init', '-q');
    writeFileSync(join(root, 'go.mod'), 'module example.com/lock-fixture\ngo 1.27.1\n');
    run('add', 'go.mod');
    run('-c', 'user.name=Fixture', '-c', 'user.email=fixture@example.invalid', 'commit', '-qm', 'base');
    const before = run('rev-parse', 'HEAD');
    const destination = `${postProductEvidencePrefix}moved-go.mod`;
    mkdirSync(join(root, postProductEvidencePrefix), { recursive: true });
    renameSync(join(root, 'go.mod'), join(root, destination));
    run('add', '-A');
    run('-c', 'user.name=Fixture', '-c', 'user.email=fixture@example.invalid', 'commit', '-qm', 'move');
    const after = run('rev-parse', 'HEAD');
    assert.equal(run('-c', 'diff.renames=true', 'diff', '--name-only', before, after), destination);
    const changed = changedPaths(before, after, [], root);
    assert.deepEqual(changed.sort(), ['go.mod', destination].sort());
    assert.throws(() => assertOnlyEvidenceChanges(changed), /go\.mod/);
  } finally { rmSync(root, { recursive: true, force: true }); }
});
