import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { copyFileSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import test from 'node:test';

const script = path.join(import.meta.dirname, 'check-tables.sh');
const schema = 'internal/platform/database/migrations/0001_schema.sql';
const go = (sql) => `package fixture\n\nconst query = \`${sql}\`\n`;
const identifier = `package fixture

var target = pgx.Identifier{
	"olp",
	"secrets",
}
`;

function check(t, files = {}, baseline = []) {
  const root = mkdtempSync(path.join(tmpdir(), 'olp-tables-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const tree = {
    [schema]: 'CREATE TABLE olp.accounts (id uuid);\nCREATE TABLE olp.secrets (id uuid);\n',
    'internal/billing/tables.txt': 'accounts\n',
    'internal/platform/tables.txt': 'secrets\n',
    'scripts/check-tables.baseline': baseline.map((line) => `${line}\n`).join(''),
    ...files
  };
  for (const [file, content] of Object.entries(tree)) {
    mkdirSync(path.join(root, path.dirname(file)), { recursive: true });
    writeFileSync(path.join(root, file), content);
  }
  copyFileSync(script, path.join(root, 'scripts', 'check-tables.sh'));
  return spawnSync('bash', ['scripts/check-tables.sh'], { cwd: root, encoding: 'utf8' });
}

test('owner writes, foreign reads and writes from tests are accepted', (t) => {
  const foreignWrite = go('DELETE FROM olp.secrets');
  const result = check(t, {
    'internal/billing/imports/store.go': go(
      'INSERT INTO olp.accounts (id) SELECT id FROM olp.secrets FOR UPDATE ON CONFLICT DO UPDATE SET id = excluded.id'
    ),
    'internal/platform/secrets/store.go': foreignWrite,
    'internal/billing/store_test.go': foreignWrite,
    'tests/integration/seed.go': foreignWrite
  });
  assert.equal(result.status, 0, result.stderr);
});

test('every way of writing a table of another module is rejected', (t) => {
  const writes = {
    'internal/mcp/insert.go': go('INSERT INTO olp.secrets (id) VALUES ($1)'),
    'internal/mcp/update.go': go('update only olp.secrets set id = $1'),
    'internal/mcp/delete.go': go('DELETE FROM olp.secrets WHERE id = $1'),
    'internal/mcp/merge.go': go('MERGE INTO olp.secrets USING olp.accounts ON true'),
    'internal/mcp/truncate.go': go('TRUNCATE TABLE olp.accounts, olp.secrets'),
    'internal/mcp/copy.go': go('COPY olp.secrets (id) FROM STDIN'),
    'internal/mcp/wrapped.go': go('INSERT INTO\n\t\t"olp"."secrets" (id) VALUES ($1)'),
    'internal/mcp/rows.go': identifier,
    'cmd/olp/repair.go': go('DELETE FROM olp.secrets')
  };
  const result = check(t, writes);
  assert.equal(result.status, 1);
  for (const file of Object.keys(writes)) {
    assert.ok(
      result.stderr.includes(`${file} writes olp.secrets, which internal/platform owns`),
      `${file} was not rejected\n${result.stderr}`
    );
  }
});

test('the baseline allows only the modules and tables it lists', (t) => {
  const known = { 'internal/mcp/store.go': go('DELETE FROM olp.secrets') };
  const accepted = check(t, known, ['internal/mcp secrets 1']);
  assert.equal(accepted.status, 0, accepted.stderr);

  const result = check(
    t,
    {
      ...known,
      'internal/mcp/ledger.go': go('DELETE FROM olp.accounts'),
      'internal/usage/retention.go': go('DELETE FROM olp.secrets')
    },
    ['internal/mcp secrets 1']
  );
  assert.equal(result.status, 1);
  assert.match(result.stderr, /internal\/mcp\/ledger\.go writes olp\.accounts/);
  assert.match(result.stderr, /internal\/usage\/retention\.go writes olp\.secrets/);
  assert.doesNotMatch(result.stderr, /internal\/mcp\/store\.go|allowed/);
});

test('another write of a listed table by the same module is rejected', (t) => {
  const result = check(
    t,
    {
      'internal/mcp/store.go': go('DELETE FROM olp.secrets'),
      'internal/mcp/retire.go': go('UPDATE olp.secrets SET id = $1')
    },
    ['internal/mcp secrets 1']
  );
  assert.equal(result.status, 1);
  assert.match(result.stderr, /internal\/mcp\/retire\.go writes olp\.secrets/);
  assert.match(result.stderr, /internal\/mcp has 2 such writes, 1 allowed/);
});

test('a baseline line that allows more than the code writes is rejected', (t) => {
  const result = check(t, { 'internal/mcp/store.go': go('DELETE FROM olp.secrets') }, [
    'internal/mcp secrets 3',
    'internal/usage secrets 1'
  ]);
  assert.equal(result.status, 1);
  assert.match(
    result.stderr,
    /stale line "internal\/mcp secrets 3", change it to "internal\/mcp secrets 1"/
  );
  assert.match(result.stderr, /stale line "internal\/usage secrets 1", delete it/);
});

test('each table needs exactly one owner and each entry needs a table', (t) => {
  const result = check(t, {
    [schema]: 'CREATE TABLE olp.accounts (id uuid);\nCREATE TABLE olp.orphans (id uuid);\n',
    'internal/mcp/tables.txt': 'accounts\nmissing\n',
    'internal/platform/tables.txt': ''
  });
  assert.equal(result.status, 1);
  assert.match(result.stderr, /olp\.orphans has no owner/);
  assert.match(
    result.stderr,
    /olp\.accounts has more than one owner: internal\/billing internal\/mcp/
  );
  assert.match(result.stderr, /internal\/mcp\/tables\.txt names missing/);
});

test('later migrations that drop and rename tables are followed', (t) => {
  const result = check(t, {
    'internal/platform/database/migrations/0002_vault.sql': [
      'DROP TABLE IF EXISTS olp.accounts CASCADE;',
      'ALTER TABLE olp.secrets\n  RENAME TO vault;',
      'ALTER TABLE olp.vault RENAME COLUMN id TO key_id;'
    ].join('\n'),
    'internal/billing/tables.txt': '',
    'internal/platform/tables.txt': 'vault\n'
  });
  assert.equal(result.status, 0, result.stderr);
});
