import { readFileSync } from 'node:fs';

const json = (text) => JSON.parse(text);

// Every copy of the release version, read from the file that carries it.
export const versionCopies = [
  ['console/package.json', (text) => json(text).version],
  ['tests/sdk-smoke/package.json', (text) => json(text).version],
  ['tests/sdk-smoke-python/pyproject.toml', (text) => /^version = "([^"]+)"$/m.exec(text)?.[1]],
  [
    'tests/sdk-smoke-python/uv.lock',
    (text) => /^name = "openllmproxy-sdk-smoke-python"\nversion = "([^"]+)"$/m.exec(text)?.[1]
  ],
  ['deploy/helm/Chart.yaml', (text) => /^version:\s*["']?([^"'\s]+)/m.exec(text)?.[1]],
  ['deploy/helm/Chart.yaml', (text) => /^appVersion:\s*["']?([^"'\s]+)/m.exec(text)?.[1]],
  ['deploy/helm/Chart.yaml', (text) => /image: ghcr\.io\/tyk-swe\/olp:(\S+)/.exec(text)?.[1]],
  ['deploy/Dockerfile', (text) => /^ARG OLP_VERSION=(.+)$/m.exec(text)?.[1]],
  ['deploy/compose.yaml', (text) => /ghcr\.io\/tyk-swe\/olp:([^}\s]+)/.exec(text)?.[1]],
  ['openapi/management.json', (text) => json(text).info?.version]
];

export function checkReleaseVersion(read, tag) {
  const version = json(read('package.json')).version;
  if (!/^0\.\d+\.\d+$/.test(version ?? '')) throw new Error('Expected a 0.x.y workspace version');
  for (const [file, extract] of versionCopies) {
    const actual = extract(read(file));
    if (actual !== version)
      throw new Error(`${file} version ${actual} does not match workspace ${version}`);
  }
  if (tag && tag !== `v${version}`) throw new Error(`Tag must equal v${version}`);
  return version;
}

if (import.meta.main) {
  const version = checkReleaseVersion((file) => readFileSync(file, 'utf8'), process.argv[2]);
  console.log(`Release version: ${version}`);
}
