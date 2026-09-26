import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { checkReleaseVersion, versionCopies } from './check-release-version.mjs';

const files = new Map(
  ['package.json', ...versionCopies.map(([file]) => file)].map((file) => [file, readFileSync(file, 'utf8')])
);
const reader = (overrides = new Map()) => (file) => overrides.get(file) ?? files.get(file);
const workspace = JSON.parse(files.get('package.json')).version;

// Rewrites only the occurrence that the extractor reads.
function replaceCopy(text, extract, value) {
  const copy = extract(text);
  for (let index = text.indexOf(copy); index >= 0; index = text.indexOf(copy, index + 1)) {
    const changed = text.slice(0, index) + value + text.slice(index + copy.length);
    if (extract(changed) === value) return changed;
  }
  throw new Error('No replaceable copy');
}

test('every checked-in copy agrees with the workspace version', () => {
  assert.match(workspace, /^0\.\d+\.\d+$/);
  assert.equal(checkReleaseVersion(reader()), workspace);
  assert.equal(checkReleaseVersion(reader(), `v${workspace}`), workspace);
});

test('a copy that disagrees with the workspace version is rejected', () => {
  for (const [file, extract] of versionCopies) {
    const changed = replaceCopy(files.get(file), extract, '0.99.0');
    assert.throws(() => checkReleaseVersion(reader(new Map([[file, changed]]))), new RegExp(file));
  }
});

test('the workspace version must be 0.x.y', () => {
  for (const version of ['1.0.0', '10.1.0', '0.1', '0.1.0-rc.1']) {
    const root = JSON.stringify({ ...JSON.parse(files.get('package.json')), version });
    assert.throws(() => checkReleaseVersion(reader(new Map([['package.json', root]]))), /0\.x\.y/);
  }
});

test('a release tag must name the workspace version', () => {
  for (const tag of [workspace, 'v0.0.0', `v${workspace}-rc.1`])
    assert.throws(() => checkReleaseVersion(reader(), tag), /Tag must equal/);
});
