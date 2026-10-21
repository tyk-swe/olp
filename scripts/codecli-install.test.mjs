import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { chmod, copyFile, mkdir, mkdtemp, readFile, readdir, rm, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { installClients } from './lib/codecli-install.mjs';

const registry = 'https://registry.npmjs.org';
const specs = {
  codex: { name: '@openai/codex', dependency: '@openai/codex-linux-', env: 'OLP_CODEX', output: (v) => `codex-cli ${v}` },
  'claude-code': { name: '@anthropic-ai/claude-code', dependency: '@anthropic-ai/claude-code-linux-', env: 'OLP_CLAUDE_CODE', output: (v) => `${v} (Claude Code)` },
  opencode: { name: 'opencode-ai', dependency: 'opencode-linux-', env: 'OLP_OPENCODE', output: (v) => v }
};

async function fixture(t) {
  const root = await mkdtemp(path.join(os.tmpdir(), "codecli-'"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const responses = new Map();
  const archives = new Map();
  const lookups = [];
  const downloads = [];
  async function release(client, version, arch) {
    const spec = specs[client];
    const source = await mkdtemp(path.join(root, 'archive-'));
    const relative = client === 'codex'
      ? `vendor/${arch === 'x64' ? 'x86_64' : 'aarch64'}-unknown-linux-musl/bin/codex`
      : client === 'claude-code' ? 'claude' : 'bin/opencode';
    const binary = path.join(source, 'package', relative);
    await mkdir(path.dirname(binary), { recursive: true });
    await writeFile(binary, `#!/bin/sh\n[ -z "\u0024{ANTHROPIC_API_KEY:-}" ] || exit 9\nprintf '%s\\n' '${spec.output(version)}'\n`);
    await chmod(binary, 0o755);
    const archive = path.join(source, 'package.tgz');
    execFileSync('tar', ['-czf', archive, '-C', source, 'package']);
    const integrity = `sha512-${createHash('sha512').update(await readFile(archive)).digest('base64')}`;
    const packageName = client === 'codex' ? spec.name : spec.dependency + arch;
    const packageVersion = version + (client === 'codex' ? `-linux-${arch}` : '');
    const dist = { tarball: `${registry}/${packageName}/-/fixture-${packageVersion}.tgz`, integrity };
    const latest = responses.get(`${registry}/${spec.name}/latest`);
    responses.set(`${registry}/${spec.name}/latest`, {
      name: spec.name, version,
      dist: { tarball: `${registry}/${spec.name}/-/wrapper-${version}.tgz`, integrity: `sha512-${createHash('sha512').update('wrapper').digest('base64')}` },
      optionalDependencies: { ...(latest?.version === version ? latest.optionalDependencies : {}),
        [spec.dependency + arch]: client === 'codex' ? `npm:${packageName}@${packageVersion}` : packageVersion }
    });
    responses.set(`${registry}/${packageName}/${packageVersion}`, { name: packageName, version: packageVersion, dist });
    archives.set(dist.tarball, archive);
  }
  const options = { root, readMetadata: async (url) => {
    lookups.push(url);
    assert.ok(responses.has(url), `unexpected metadata lookup: ${url}`);
    return structuredClone(responses.get(url));
  }, downloadArchive: async (url, file) => {
    downloads.push(url);
    assert.ok(archives.has(url), `unexpected archive download: ${url}`);
    await copyFile(archives.get(url), file);
  } };
  async function evidence() {
    const dir = path.join(root, '.local/codecli/evidence');
    return Promise.all((await readdir(dir)).map(async (name) => JSON.parse(await readFile(path.join(dir, name, 'resolution.json')))));
  }
  return { root, responses, archives, lookups, downloads, release, options, evidence };
}

test('checksum failure preserves resolved evidence and never extracts an unverified archive', async (t) => {
  const f = await fixture(t);
  await f.release('codex', '7.8.9', 'x64');
  const archive = [...f.archives.values()][0];
  await writeFile(archive, 'corrupt bytes');
  await assert.rejects(installClients({ ...f.options, arch: 'x64', clients: ['codex'] }), /integrity mismatch/);
  const [evidence] = await f.evidence();
  assert.equal(evidence.status, 'failed');
  assert.match(evidence.clients.codex.platform_package.integrity, /^sha512-/);
  await assert.rejects(readFile(path.join(f.root, '.local/codecli/cache/codex/7.8.9/linux-x64/package/vendor/x86_64-unknown-linux-musl/bin/codex')), /ENOENT/);
});

test('metadata failure fails a new run even when the previous release is cached', async (t) => {
  const f = await fixture(t);
  await f.release('codex', '7.8.9', 'x64');
  await installClients({ ...f.options, arch: 'x64', clients: ['codex'] });
  await assert.rejects(installClients({ ...f.options, arch: 'x64', clients: ['codex'], readMetadata: async () => { throw new Error('registry unavailable'); } }), /registry unavailable/);
  assert.equal(f.downloads.length, 1);
  assert.ok((await f.evidence()).some((item) => item.status === 'failed' && item.error === 'registry unavailable'));
});

test('missing platforms, mismatched releases and missing SHA-512 fail closed', async (t) => {
  const f = await fixture(t);
  await f.release('opencode', '7.8.9', 'x64');
  const latest = f.responses.get(`${registry}/opencode-ai/latest`);
  const options = { ...f.options, arch: 'x64', clients: ['opencode'] };
  await assert.rejects(installClients({ ...options, arch: 'arm64' }), /has no opencode-linux-arm64/);
  latest.optionalDependencies['opencode-linux-x64'] = '7.8.8';
  await assert.rejects(installClients(options), /platform version must match latest/);
  latest.optionalDependencies['opencode-linux-x64'] = '7.8.9';
  delete f.responses.get(`${registry}/opencode-linux-x64/7.8.9`).dist.integrity;
  await assert.rejects(installClients(options), /SHA-512 integrity/);
  assert.equal(f.downloads.length, 0);
});

test('qualification reuses the shared environment without resolving again', async (t) => {
  const f = await fixture(t);
  for (const client of Object.keys(specs)) await f.release(client, '7.8.9', 'x64');
  const resolved = await installClients({ ...f.options, arch: 'x64' });
  const bin = path.join(f.root, 'bin');
  await mkdir(bin);
  await writeFile(path.join(bin, 'node'), '#!/bin/sh\nexit 99\n');
  await writeFile(path.join(bin, 'go'), '#!/bin/sh\nprintf "%s\\n" "$OLP_CODEX_BINARY" "$OLP_CLAUDE_CODE_BINARY" "$OLP_OPENCODE_BINARY" "$OLP_CODEX_VERSION"\n');
  for (const command of ['node', 'go']) await chmod(path.join(bin, command), 0o755);
  const script = fileURLToPath(new URL('./code-mode-qualification.sh', import.meta.url));
  const output = execFileSync('bash', [script, 'cli'], {
    encoding: 'utf8', env: { ...process.env, OLP_CODECLI_ENV: resolved.envFile, PATH: `${bin}:${process.env.PATH}` }
  });
  assert.deepEqual(output.trim().split('\n'), [resolved.environment.OLP_CODEX_BINARY, resolved.environment.OLP_CLAUDE_CODE_BINARY, resolved.environment.OLP_OPENCODE_BINARY, '7.8.9']);
});
