import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { copyFileSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { test } from 'node:test';

const published = { postgres: '127.0.0.1:55432', valkey: '127.0.0.1:63811', cache: '127.0.0.1:63812' };

function startStack(t, { linkedWorktree = false } = {}) {
  const root = mkdtempSync(join(tmpdir(), 'olp-dev-test-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const write = (path, content, mode) => {
    const target = join(root, path);
    mkdirSync(dirname(target), { recursive: true });
    writeFileSync(target, content, { mode });
  };
  if (linkedWorktree) write('.git', 'gitdir: elsewhere\n');
  write('bin/go', '#!/usr/bin/env bash\necho bash\n', 0o755);
  write(
    'bin/docker',
    `#!/usr/bin/env bash
case "$*" in
  'compose version'|compose*' up '*) exit 0 ;;
  compose*' port postgres 5432') echo '${published.postgres}' ;;
  compose*' port valkey 6379') echo '${published.valkey}' ;;
  compose*' port cache 6379') echo '${published.cache}' ;;
  *) echo "Unexpected Docker command: $*" >&2; exit 1 ;;
esac
`,
    0o755
  );
  write('scripts/secrets.sh', 'export OLP_BOOTSTRAP_TOKEN_FILE=unused\n');
  write('console/src/lib/management/schema.d.ts', '');
  write('console/src/lib/management/requirements.ts', '');
  write('console/src/lib/management/enums.ts', '');
  write(
    'console/node_modules/.bin/vite',
    `#!/usr/bin/env bash
echo "$*" > ../vite.tmp
mv ../vite.tmp ../vite.args
exec sleep 10
`,
    0o755
  );
  write(
    '.local/bin/air',
    `#!/usr/bin/env bash
until [[ -f vite.args ]]; do sleep 0.01; done
env
printf 'VITE_ARGS=%s\\n' "$(<vite.args)"
`,
    0o755
  );
  for (const script of ['dev.sh', 'dev-env.sh']) {
    copyFileSync(new URL(`./${script}`, import.meta.url), join(root, 'scripts', script));
  }

  const output = execFileSync('bash', ['scripts/dev.sh'], {
    cwd: root,
    env: { PATH: `${join(root, 'bin')}:${process.env.PATH}` },
    encoding: 'utf8',
    timeout: 5000
  });
  const backend = Object.fromEntries(
    Array.from(output.matchAll(/^([A-Z_]+)=(.*)$/gm), ([, name, value]) => [name, value])
  );
  const consolePort = /--port (\d+)/.exec(backend.VITE_ARGS)?.[1];
  return { backend, consolePort };
}

const portOf = (address) => address.split(':').at(-1);

test('the development backend connects to the ports published by Compose', (t) => {
  const { backend } = startStack(t);
  assert.deepEqual(
    [backend.OLP_DATABASE_URL, backend.OLP_VALKEY_URL, backend.OLP_CACHE_VALKEY_URL].map(
      (url) => new URL(url).host
    ),
    [published.postgres, published.valkey, published.cache]
  );
});

test('the primary checkout keeps the documented compose project and ports', (t) => {
  const { backend, consolePort } = startStack(t);
  assert.equal(backend.COMPOSE_PROJECT_NAME, undefined);
  assert.equal(consolePort, '5173');
  assert.equal(backend.OLP_PUBLIC_ORIGIN, 'http://127.0.0.1:5173');
  assert.equal(backend.OLP_LISTEN_ADDR, '127.0.0.1:8082');
  assert.equal(backend.OLP_OBSERVABILITY_LISTEN_ADDR, '127.0.0.1:9092');
});

test('each linked worktree runs its own compose project on its own ports', (t) => {
  const stacks = [startStack(t, { linkedWorktree: true }), startStack(t, { linkedWorktree: true })];
  for (const { backend, consolePort } of stacks) {
    assert.deepEqual(
      [backend.OLP_POSTGRES_PORT, backend.OLP_VALKEY_PORT, backend.OLP_CACHE_VALKEY_PORT],
      ['0', '0', '0']
    );
    assert.equal(portOf(backend.OLP_PUBLIC_ORIGIN), consolePort);
    assert.equal(portOf(backend.OLP_DEV_API_ORIGIN), portOf(backend.OLP_LISTEN_ADDR));
    const ports = [
      consolePort,
      portOf(backend.OLP_LISTEN_ADDR),
      portOf(backend.OLP_OBSERVABILITY_LISTEN_ADDR)
    ];
    assert.equal(new Set([...ports, '5173', '8082', '9092']).size, 6);
  }
  assert.notEqual(stacks[0].backend.COMPOSE_PROJECT_NAME, stacks[1].backend.COMPOSE_PROJECT_NAME);
});
