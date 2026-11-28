import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, statSync, symlinkSync, writeFileSync } from 'node:fs';
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
  const paths = first.slice(0, 3);
  for (const path of paths) {
    assert.equal(statSync(path).mode & 0o777, 0o600);
  }
  const initialContents = paths.map((path) => readFileSync(path));

  run();
  for (const [index, path] of paths.entries()) {
    assert.ok(readFileSync(path).equals(initialContents[index]), `${path} was overwritten`);
  }

  const overridden = run({ OLP_AUTH_HMAC_KEY_FILE: '/custom/auth.key' });
  assert.equal(overridden[0], '/custom/auth.key');
});

test('sourcing the secrets helper rejects non-regular files without setting secret paths', (t) => {
  const scratch = mkdtempSync(join(tmpdir(), 'olp-secrets-test-'));
  t.after(() => rmSync(scratch, { recursive: true, force: true }));
  const target = join(scratch, 'regular-file');
  writeFileSync(target, 'test fixture');

  const cases = [
    ['dangling symlink', 'auth.key', (path) => symlinkSync(join(scratch, 'missing'), path)],
    ['symlink to a file', 'master.json', (path) => symlinkSync(target, path)],
    ['directory', 'bootstrap.token', (path) => mkdirSync(path)]
  ];

  for (const [name, filename, create] of cases) {
    const dir = join(scratch, name);
    mkdirSync(dir);
    const path = join(dir, filename);
    create(path);

    const result = spawnSync('bash', ['-c', `
      source scripts/secrets.sh "$1"
      status=$?
      printf '%s\\n' "\${OLP_AUTH_HMAC_KEY_FILE-unset}" "\${OLP_BOOTSTRAP_TOKEN_FILE-unset}" "\${OLP_MASTER_KEY_FILE-unset}"
      exit "$status"
    `, '_', dir], { cwd: repoRoot, env: { PATH: process.env.PATH }, encoding: 'utf8' });

    assert.ok(result.status > 0, `${name} did not return a nonzero status`);
    assert.equal(result.stdout, 'unset\nunset\nunset\n', name);
    assert.ok(result.stderr.includes(path), result.stderr);
    assert.match(result.stderr, /expected a regular file/, name);
  }
});
