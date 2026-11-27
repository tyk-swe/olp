import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, statSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import test from 'node:test';

const repoRoot = resolve(import.meta.dirname, '..');

test('restore preflight rejects dbname query parameters before touching the database', (t) => {
  const scratch = mkdtempSync(join(tmpdir(), 'olp-restore-test-'));
  t.after(() => rmSync(scratch, { recursive: true, force: true }));
  const missingDump = join(scratch, 'missing.dump');

  const cases = [
    ['path', 'postgresql://127.0.0.1/replacement?sslmode=require', 'Backup and manifest are required.'],
    ['query', 'postgresql://127.0.0.1/?dbname=replacement&sslmode=require', 'Set the restore destination database in the URL path'],
    ['path_and_query', 'postgresql://127.0.0.1/replacement?dbname=replacement', 'Set the restore destination database in the URL path'],
    ['encoded_query', 'postgresql://127.0.0.1/?%64bname=replacement', 'Set the restore destination database in the URL path'],
    ['leading_space', 'postgresql://127.0.0.1/replacement? dbname=replacement', 'Set the restore destination database in the URL path'],
    ['trailing_space', 'postgresql://127.0.0.1/replacement?dbname =replacement', 'Set the restore destination database in the URL path'],
    ['surrounding_spaces', 'postgresql://127.0.0.1/replacement?sslmode=require& dbname =replacement', 'Set the restore destination database in the URL path'],
    ['duplicate_query', 'postgresql://127.0.0.1/?dbname=&dbname=replacement', 'Set the restore destination database in the URL path']
  ];

  for (const [name, address, want] of cases) {
    const result = spawnSync('bash', ['scripts/restore.sh', missingDump], {
      cwd: repoRoot,
      env: {
        PATH: process.env.PATH,
        OLP_RESTORE_DATABASE_URL: address,
        OLP_RESTORE_VALKEY_ISOLATED: 'true'
      },
      encoding: 'utf8'
    });
    assert.notEqual(result.status, 0, `${name} unexpectedly succeeded`);
    assert.match(`${result.stdout}${result.stderr}`, new RegExp(want), name);
  }
});

test('secrets helper provisions private files idempotently and respects env overrides', (t) => {
  const scratch = mkdtempSync(join(tmpdir(), 'olp-secrets-test-'));
  t.after(() => rmSync(scratch, { recursive: true, force: true }));
  const dir = join(scratch, 'secrets');

  const run = (extraEnv = {}) =>
    execFileSync(
      'bash',
      ['-euo', 'pipefail', '-c', 'source scripts/secrets.sh "$1"; printf "%s\\n%s\\n%s\\n%s\\n" "$OLP_AUTH_HMAC_KEY_FILE" "$OLP_BOOTSTRAP_TOKEN_FILE" "$OLP_MASTER_KEY_FILE" "${olp_secrets_dir:-unset}"', '_', dir],
      { cwd: repoRoot, env: { PATH: process.env.PATH, ...extraEnv }, encoding: 'utf8' }
    ).trim().split('\n');

  const first = run();
  assert.deepEqual(first, [
    join(dir, 'auth.key'),
    join(dir, 'bootstrap.token'),
    join(dir, 'master.json'),
    'unset'
  ]);
  assert.equal(statSync(dir).mode & 0o777, 0o700);
  for (const name of ['auth.key', 'bootstrap.token', 'master.json']) {
    assert.equal(statSync(join(dir, name)).mode & 0o777, 0o600);
  }
  const initialAuth = readFileSync(join(dir, 'auth.key'), 'utf8');

  run();
  assert.equal(readFileSync(join(dir, 'auth.key'), 'utf8'), initialAuth);

  const overridden = run({ OLP_AUTH_HMAC_KEY_FILE: '/custom/auth.key' });
  assert.equal(overridden[0], '/custom/auth.key');
});
