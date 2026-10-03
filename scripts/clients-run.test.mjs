import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { existsSync, mkdirSync, mkdtempSync, realpathSync, rmSync, symlinkSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import test from 'node:test';

// tests/clients/run.sh gives every suite a scratch directory under TMPDIR. A
// suite compares the paths it is handed with the ones its clients report, which
// are canonical, so a TMPDIR that is a symlink, as it is on macOS and under some
// CI runners, or that holds a `..`, must not make a suite fail for it.
const toolchain = ['go', 'jq', 'curl', 'timeout'].filter((command) => spawnSync('sh', ['-c', `command -v ${command}`]).status !== 0);
const skip = toolchain.length > 0
  ? `the client suite needs ${toolchain.join(', ')}`
  : !existsSync('tests/clients/node_modules') && "the client packages are not installed; run 'pnpm install --frozen-lockfile'";

test('the client suites run under a TMPDIR that is not a canonical path', { skip, timeout: 180_000 }, () => {
  const root = realpathSync(mkdtempSync(path.join(tmpdir(), 'clients-run-')));
  try {
    mkdirSync(path.join(root, 'real'));
    mkdirSync(path.join(root, 'other'));
    symlinkSync(path.join(root, 'real'), path.join(root, 'link'));
    // A symlink, and a parent reference through another directory.
    const noncanonical = path.join(root, 'other', '..', 'link');
    assert.notEqual(realpathSync(noncanonical), noncanonical);
    const run = spawnSync('tests/clients/run.sh', {
      env: { ...process.env, CLIENTS: 'harness', TMPDIR: noncanonical },
      encoding: 'utf8',
    });
    assert.equal(run.status, 0, `run.sh exited ${run.status}\n${run.stdout}\n${run.stderr}`);
    assert.match(run.stdout + run.stderr, /harness\s+passed/);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
