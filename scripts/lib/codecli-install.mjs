// Resolve npm's latest channel on every invocation; only archives are cached.
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { createReadStream, existsSync } from 'node:fs';
import { mkdir, mkdtemp, rename, rm, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const registry = 'https://registry.npmjs.org';
const definitions = {
  codex: { package: '@openai/codex', platform: '@openai/codex-linux-', env: 'OLP_CODEX', output: (v) => `codex-cli ${v}` },
  'claude-code': { package: '@anthropic-ai/claude-code', platform: '@anthropic-ai/claude-code-linux-', env: 'OLP_CLAUDE_CODE', output: (v) => `${v} (Claude Code)` },
  opencode: { package: 'opencode-ai', platform: 'opencode-linux-', env: 'OLP_OPENCODE', output: (v) => v }
};
const exactVersion = /^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/;
const curlArgs = ['--fail', '--silent', '--show-error', '--location', '--proto', '=https', '--proto-redir', '=https', '--tlsv1.2', '--connect-timeout', '20', '--max-time', '300'];
const metadata = (url) => JSON.parse(execFileSync('curl', [...curlArgs, url], { encoding: 'utf8' }));
const download = (url, file) => execFileSync('curl', [...curlArgs, url, '-o', file]);
const quote = (value) => `'${value.replaceAll("'", "'\\''")}'`;

function distribution(document) {
  assert.match(document.dist?.tarball ?? '', /^https:\/\//, 'package archive must use HTTPS');
  assert.match(document.dist?.integrity ?? '', /^sha512-[A-Za-z0-9+/]{86}==$/, 'package must publish SHA-512 integrity');
  return { url: document.dist.tarball, integrity: document.dist.integrity };
}

export async function installClients({
  root, arch = process.arch, clients = Object.keys(definitions), readMetadata = metadata, downloadArchive = download
}) {
  assert.equal(process.platform, 'linux', 'Qualification installer supports Linux only');
  assert.ok(['x64', 'arm64'].includes(arch), 'Qualification installer supports amd64/arm64 only');
  for (const client of clients) assert.ok(definitions[client], `unknown client: ${client}`);
  const base = path.resolve(root, '.local/codecli');
  await mkdir(path.join(base, 'evidence'), { recursive: true });
  const evidenceDir = await mkdtemp(path.join(base, 'evidence/run-'));
  const envFile = path.join(evidenceDir, 'environment.sh');
  const evidenceFile = path.join(evidenceDir, 'resolution.json');
  const evidence = { channel: 'latest', platform: `linux-${arch}`, resolved_at: new Date().toISOString(), status: 'resolving', clients: {} };
  const save = () => writeFile(evidenceFile, JSON.stringify(evidence, null, 2) + '\n');
  const environment = { OLP_CODECLI_ENV: envFile };
  await save();
  try {
    // Resolve every requested package before running any binary, preserving
    // metadata for all clients if installation or compatibility checks fail.
    for (const client of clients) {
      const spec = definitions[client];
      const entry = evidence.clients[client] = { package: spec.package, metadata_url: `${registry}/${spec.package}/latest` };
      await save();
      const latest = await readMetadata(entry.metadata_url);
      assert.equal(latest.name, spec.package, 'unexpected publisher package');
      assert.match(latest.version, exactVersion, 'latest must resolve to an exact version');
      Object.assign(entry, { version: latest.version, release: distribution(latest) });
      await save();
      const dependency = spec.platform + arch;
      const reference = latest.optionalDependencies?.[dependency];
      assert.ok(reference, `latest ${client} has no ${dependency} package`);
      const alias = /^npm:(.+)@([^@]+)$/.exec(reference);
      const packageName = alias ? alias[1] : dependency;
      const packageVersion = alias ? alias[2] : reference;
      assert.equal(packageName, client === 'codex' ? spec.package : dependency, 'unexpected platform package');
      assert.equal(packageVersion, latest.version + (client === 'codex' ? `-linux-${arch}` : ''), 'platform version must match latest');
      entry.platform_package = { name: packageName, version: packageVersion, metadata_url: `${registry}/${packageName}/${packageVersion}` };
      await save();
      const platform = await readMetadata(entry.platform_package.metadata_url);
      assert.equal(platform.name, packageName, 'unexpected platform package metadata');
      assert.equal(platform.version, packageVersion, 'unexpected platform version metadata');
      Object.assign(entry.platform_package, distribution(platform));
      await save();
    }
    for (const client of clients) {
      const spec = definitions[client];
      const entry = evidence.clients[client];
      const cache = path.join(base, 'cache', client, entry.version, `linux-${arch}`);
      await mkdir(cache, { recursive: true });
      const archive = path.join(cache, 'package.tgz');
      if (!existsSync(archive)) {
        try {
          await downloadArchive(entry.platform_package.url, archive + '.part');
          await rename(archive + '.part', archive);
        } finally {
          await rm(archive + '.part', { force: true });
        }
      }
      const hash = createHash('sha512');
      for await (const chunk of createReadStream(archive)) hash.update(chunk);
      assert.equal(`sha512-${hash.digest('base64')}`, entry.platform_package.integrity, `${client} archive integrity mismatch; refusing to execute`);
      // Re-extract verified bytes, so a modified extracted binary is never reused.
      await rm(path.join(cache, 'package'), { recursive: true, force: true });
      execFileSync('tar', ['-xzf', archive, '-C', cache]);
      const relative = client === 'codex'
        ? `vendor/${arch === 'x64' ? 'x86_64' : 'aarch64'}-unknown-linux-musl/bin/codex`
        : client === 'claude-code' ? 'claude' : 'bin/opencode';
      entry.binary = path.join(cache, 'package', relative);
      const home = await mkdtemp(path.join(cache, 'version-home-'));
      try {
        entry.binary_version = execFileSync(entry.binary, ['--version'], {
          encoding: 'utf8', timeout: 30_000,
          env: { PATH: process.env.PATH, HOME: home, CODEX_HOME: home, XDG_CONFIG_HOME: home,
            XDG_DATA_HOME: home, XDG_CACHE_HOME: home, XDG_STATE_HOME: home,
            NO_COLOR: '1', DISABLE_AUTOUPDATER: '1', OPENCODE_DISABLE_AUTOUPDATE: '1' }
        }).trim();
        await save();
        assert.equal(entry.binary_version, spec.output(entry.version), `${client} binary does not match resolved version`);
        process.stderr.write(`${client}: ${entry.binary_version}\n`);
      } finally {
        await rm(home, { recursive: true, force: true });
      }
      environment[`${spec.env}_BINARY`] = entry.binary;
      environment[`${spec.env}_VERSION`] = entry.version;
    }
    evidence.status = 'ready';
    await save();
    await writeFile(envFile, Object.entries(environment).map(([key, value]) => `export ${key}=${quote(value)}\n`).join(''));
    return { envFile, environment, evidenceFile };
  } catch (error) {
    evidence.status = 'failed';
    evidence.error = error.message;
    await save();
    throw error;
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const client = process.argv[2];
    const result = await installClients({ root: fileURLToPath(new URL('../../', import.meta.url)), ...(client ? { clients: [client] } : {}) });
    console.log(client ? result.environment[`${definitions[client].env}_BINARY`] : result.envFile);
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
