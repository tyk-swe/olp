import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { checkReleaseVersion, versionCopies } from './check-release-version.mjs';

const files = new Map(
  ['package.json', ...versionCopies.map(([file]) => file)].map((file) => [file, readFileSync(file, 'utf8')])
);
const reader = (overrides = new Map()) => (file) => overrides.get(file) ?? files.get(file);

function replaceCopy(text, extract, value) {
  const copy = extract(text);
  for (let index = text.indexOf(copy); index >= 0; index = text.indexOf(copy, index + 1)) {
    const changed = text.slice(0, index) + value + text.slice(index + copy.length);
    if (extract(changed) === value) return changed;
  }
  throw new Error('No replaceable copy');
}

test('unmodified workspace version copies agree and match their release tag', () => {
  const version = checkReleaseVersion(reader());
  assert.equal(checkReleaseVersion(reader(), `v${version}`), version);
  assert.throws(() => checkReleaseVersion(reader(), 'v0.99.99'), /Tag must equal/);
});

test('a non-0.x workspace version is rejected', () => {
  const bumped = files.get('package.json').replace(/"version": "[^"]+"/, '"version": "1.0.0"');
  assert.throws(
    () => checkReleaseVersion(reader(new Map([['package.json', bumped]]))),
    /Expected a 0\.x\.y workspace version/
  );
});

test('a copy that disagrees with the workspace version is rejected', () => {
  for (const [file, extract] of versionCopies) {
    const changed = replaceCopy(files.get(file), extract, '0.99.0');
    assert.throws(() => checkReleaseVersion(reader(new Map([[file, changed]]))), new RegExp(file));
  }
});
