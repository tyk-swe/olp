import { readFileSync } from 'node:fs';

const version = JSON.parse(readFileSync('package.json', 'utf8')).version;
if (!/^3\.\d+\.\d+$/.test(version ?? '')) throw new Error('Expected a stable 3.x workspace version');
const chart = readFileSync('deploy/helm/Chart.yaml', 'utf8');
for (const key of ['version', 'appVersion']) {
  const actual = new RegExp(`^${key}:\\s*["']?([^"'\\s]+)`, 'm').exec(chart)?.[1];
  if (actual !== version) throw new Error(`Chart ${key} does not match workspace ${version}`);
}
for (const file of ['package.json', 'console/package.json', 'tests/sdk-smoke/package.json']) {
  if (JSON.parse(readFileSync(file, 'utf8')).version !== version)
    throw new Error(`${file} version does not match workspace ${version}`);
}
for (const [file, pattern] of [
  ['deploy/Dockerfile', /^ARG OLP_VERSION=(.+)$/m],
  ['deploy/compose.yaml', /ghcr\.io\/tyk-swe\/olp:([^}\s]+)/]
]) {
  if (pattern.exec(readFileSync(file, 'utf8'))?.[1] !== version)
    throw new Error(`${file} default image version does not match workspace ${version}`);
}
const tag = process.argv[2];
if (tag && tag !== `v${version}`) throw new Error(`Tag must equal v${version}`);
console.log(`Release version: ${version}`);
