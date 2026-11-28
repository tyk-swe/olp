import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { copyFileSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import test from 'node:test';

const script = path.join(import.meta.dirname, 'check-migrations.sh');
const directory = 'internal/platform/database/migrations';

function repository(t, released) {
  const root = mkdtempSync(path.join(tmpdir(), 'olp-migrations-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const env = { ...process.env, GIT_CONFIG_GLOBAL: '/dev/null', GIT_CONFIG_NOSYSTEM: '1' };
  const git = (...args) => {
    const result = spawnSync(
      'git',
      ['-c', 'user.name=test', '-c', 'user.email=test@example.invalid', ...args],
      {
        cwd: root,
        env,
        encoding: 'utf8'
      }
    );
    assert.equal(result.status, 0, result.stderr);
  };
  const add = (name) => writeFileSync(path.join(root, directory, name), 'SELECT 1;\n');

  mkdirSync(path.join(root, directory), { recursive: true });
  mkdirSync(path.join(root, 'scripts'));
  copyFileSync(script, path.join(root, 'scripts', 'check-migrations.sh'));
  released.forEach(add);
  git('init', '--quiet');
  git('add', '.');
  git('commit', '--quiet', '--message', 'released');

  return {
    add,
    remove: (name) => rmSync(path.join(root, directory, name)),
    check: (base = 'HEAD') =>
      spawnSync('bash', ['scripts/check-migrations.sh'], {
        cwd: root,
        env: { ...env, MIGRATIONS_BASE: base },
        encoding: 'utf8'
      })
  };
}

test('migrations numbered above the released ones are accepted', (t) => {
  const repo = repository(t, ['0001_schema.sql', '0002_prices.sql']);
  assert.equal(repo.check().status, 0);
  repo.add('0003_routes.sql');
  repo.add('0005_keys.sql');
  const result = repo.check();
  assert.equal(result.status, 0, result.stderr);
});

test('a number taken twice is rejected', (t) => {
  const repo = repository(t, ['0001_schema.sql']);
  repo.add('0002_routes.sql');
  repo.add('0002_keys.sql');
  const result = repo.check();
  assert.equal(result.status, 1);
  assert.match(result.stderr, /more than one migration is numbered 0002/);
});

test('a new migration must be numbered above the base even when the tree has no duplicate', (t) => {
  const repo = repository(t, ['0001_schema.sql', '0003_prices.sql']);
  repo.remove('0003_prices.sql');
  repo.add('0002_routes.sql');
  repo.add('0003_keys.sql');
  const result = repo.check();
  assert.equal(result.status, 1);
  assert.match(result.stderr, /0002_routes\.sql must be numbered above 0003/);
  assert.match(result.stderr, /0003_keys\.sql must be numbered above 0003/);
});

test('names that would sort out of order are rejected', (t) => {
  const repo = repository(t, ['0001_schema.sql']);
  repo.add('10_routes.sql');
  const result = repo.check();
  assert.equal(result.status, 1);
  assert.match(result.stderr, /10_routes\.sql is not named NNNN_name\.sql/);
});

test('a base that cannot be read fails the check', (t) => {
  const repo = repository(t, ['0001_schema.sql']);
  const result = repo.check('origin/absent');
  assert.equal(result.status, 1);
  assert.match(result.stderr, /cannot read migrations on origin\/absent/);
});
