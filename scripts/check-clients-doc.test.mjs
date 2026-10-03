import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

// docs/clients.md names the client releases the qualification suite pins, so
// its tables must equal the manifests that pin them.
const doc = readFileSync('docs/clients.md', 'utf8');
const manifest = JSON.parse(readFileSync('tests/clients/package.json', 'utf8'));
const goMod = readFileSync('tests/clients/go.mod', 'utf8');

// The pinned releases are the rows of the tables under "Pinned releases", such
// as: | Client | `package` | 1.2.3 | Qualification |
const section = doc.match(/^## Pinned releases\n([\s\S]*?)(?=^## )/m)?.[1] ?? '';
const rows = [...section.matchAll(/^\|[^|\n]*\|\s*`([^`\n]+)`\s*\|\s*(\S+)\s*\|/gm)].map(([, name, version]) => [name, version]);
const documented = new Map(rows);

test('every pinned release is documented once', () => {
  assert.ok(rows.length > 0, 'docs/clients.md has a "Pinned releases" section with tables');
  assert.equal(documented.size, rows.length, 'docs/clients.md lists a package twice');
});

test('the pinned npm packages in docs/clients.md equal tests/clients/package.json', () => {
  const pinned = manifest.devDependencies;
  for (const [name, version] of Object.entries(pinned)) {
    assert.match(version, /^\d+\.\d+\.\d+$/, `${name} must be pinned to an exact version`);
    assert.equal(documented.get(name), version, `docs/clients.md must name ${name}@${version}`);
  }
  const npmRows = [...documented.keys()].filter((name) => !name.includes('.'));
  assert.deepEqual(npmRows.sort(), Object.keys(pinned).sort(), 'docs/clients.md lists a package that is not pinned');
});

test('the client packages are development dependencies only', () => {
  assert.equal(manifest.dependencies, undefined, 'a production dependency would enter pnpm audit --prod');
  assert.equal(manifest.private, true);
});

test('the pinned Go SDKs in docs/clients.md equal tests/clients/go.mod', () => {
  const direct = [...goMod.matchAll(/^\t(\S+) (v\S+)$/gm)].map(([, module, version]) => [module, version]);
  assert.ok(direct.length >= 3, 'the module requires the official SDKs');
  const modules = new Map(direct);
  for (const [module, version] of modules) assert.equal(documented.get(module), version, `docs/clients.md must name ${module} ${version}`);
  const goRows = [...documented.keys()].filter((name) => name.includes('.'));
  assert.deepEqual(goRows.sort(), [...modules.keys()].sort());
});
