import { readFileSync } from 'node:fs';

const cargo = readFileSync('Cargo.toml', 'utf8').split(/^\[/m)[1];
const version = /^version\s*=\s*"([^"]+)"/m.exec(cargo)?.[1];
if (!/^3\.\d+\.\d+$/.test(version ?? '')) throw new Error('Expected a stable 3.x Cargo version');
const chart = readFileSync('deploy/helm/Chart.yaml', 'utf8');
for (const key of ['version', 'appVersion']) {
  const actual = new RegExp(`^${key}:\\s*["']?([^"'\\s]+)`, 'm').exec(chart)?.[1];
  if (actual !== version) throw new Error(`Chart ${key} does not match Cargo ${version}`);
}
for (const file of ['package.json', 'console/package.json']) {
  if (JSON.parse(readFileSync(file, 'utf8')).version !== version)
    throw new Error(`${file} version does not match Cargo ${version}`);
}
const tag = process.argv[2];
if (tag && tag !== `v${version}`) throw new Error(`Tag must equal v${version}`);
console.log(`Release version: ${version}`);
