import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { copyFileSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { test } from 'node:test';

test('the development backend connects to the ports published by Compose', (t) => {
  const root = mkdtempSync(join(tmpdir(), 'olp-dev-test-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const write = (path, content, mode) => {
    const target = join(root, path);
    mkdirSync(dirname(target), { recursive: true });
    writeFileSync(target, content, { mode });
  };
  const postgres = '127.0.0.1:55432';
  const valkey = '127.0.0.1:63811';
  const cache = '127.0.0.1:63812';
  write('bin/go', '#!/usr/bin/env bash\necho bash\n', 0o755);
  write('bin/docker', `#!/usr/bin/env bash
case "$*" in
  'compose version'|compose*' up '*) exit 0 ;;
  compose*' port postgres 5432') echo '${postgres}' ;;
  compose*' port valkey 6379') echo '${valkey}' ;;
  compose*' port cache 6379') echo '${cache}' ;;
  *) echo "Unexpected Docker command: $*" >&2; exit 1 ;;
esac
`, 0o755);
  write('scripts/secrets.sh', 'export OLP_BOOTSTRAP_TOKEN_FILE=unused\n');
  write('console/src/lib/management/schema.d.ts', '');
  write('console/src/lib/management/requirements.ts', '');
  write('console/node_modules/.bin/vite', '#!/usr/bin/env bash\nexec sleep 10\n', 0o755);
  write('.local/bin/air', `#!/usr/bin/env bash
printf 'backend=%s\\n' "$OLP_DATABASE_URL" "$OLP_VALKEY_URL" "$OLP_CACHE_VALKEY_URL"
`, 0o755);
  copyFileSync(new URL('./dev.sh', import.meta.url), join(root, 'scripts/dev.sh'));

  const output = execFileSync('bash', ['scripts/dev.sh'], {
    cwd: root,
    env: { PATH: `${join(root, 'bin')}:${process.env.PATH}` },
    encoding: 'utf8',
    timeout: 5000,
  });
  assert.deepEqual(
    Array.from(output.matchAll(/^backend=(.+)$/gm), ([, url]) => new URL(url).host),
    [postgres, valkey, cache],
  );
});
