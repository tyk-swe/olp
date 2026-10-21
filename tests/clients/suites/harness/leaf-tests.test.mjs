// run.sh refuses a suite that ran nothing, a test file that defines nothing, and
// any test or group that was skipped or left to do, from the list of tests that
// lib/leaf-tests.mjs reports, by lib/check-ran.sh. node:test counts a file that
// defines no tests as one test, an empty `describe` as a test with no body, and
// does not report the tests of a skipped `describe` at all, so what counts as a
// test that ran is checked here against node:test itself, and so is the refusal.
import assert from 'node:assert/strict';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { describe, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { clientEnvironment, run, workspace } from '../../lib/cli.mjs';
import { leafTest } from '../../lib/leaf-tests.mjs';

const reporter = fileURLToPath(new URL('../../lib/leaf-tests.mjs', import.meta.url));
const checkRan = fileURLToPath(new URL('../../lib/check-ran.sh', import.meta.url));

describe('the tests that ran, from the events of node:test', () => {
  const event = (type, data) => ({ type, data: { file: '/suites/a.test.mjs', nesting: 0, details: { type: 'test' }, ...data } });

  test('are the leaf tests that passed or failed, with how they ended', () => {
    assert.deepEqual(leafTest(event('test:pass', { name: 'one', nesting: 1 })), { file: 'a.test.mjs', name: 'one', skip: false, todo: false, failed: false });
    assert.deepEqual(leafTest(event('test:fail', { name: 'two' })), { file: 'a.test.mjs', name: 'two', skip: false, todo: false, failed: true });
    assert.equal(leafTest(event('test:pass', { name: 'three', skip: true })).skip, true);
    assert.equal(leafTest(event('test:pass', { name: 'four', skip: 'not yet' })).skip, true);
    assert.equal(leafTest(event('test:pass', { name: 'five', todo: 'later' })).todo, true);
  });
});

describe('the tests that ran, in test files run by node:test', () => {
  /** Run one test file under the reporter, and list what it reported. */
  async function listed(name, source) {
    const dir = await workspace('leaf');
    const file = path.join(dir, name);
    const out = path.join(dir, 'tests.jsonl');
    await writeFile(file, source);
    const result = await run(process.execPath, ['--test', `--test-reporter=${reporter}`, `--test-reporter-destination=${out}`, name], { cwd: dir, env: clientEnvironment(), timeoutMs: 60_000 });
    const tests = (await readFile(out, 'utf8')).split('\n').filter(Boolean).map((line) => JSON.parse(line));
    return { code: result.code, tests };
  }

  test('a file that cannot be loaded lists none, and fails', async () => {
    const { code, tests } = await listed('broken.test.mjs', "import { test } from 'node:test'; test('never reached', () => {}); }");
    assert.deepEqual(tests, []);
    assert.notEqual(code, 0);
  });
});

describe('a suite that has qualified nothing is refused', () => {
  /** Run the test files of a throwaway suite as run.sh does, then have check-ran.sh judge what ran. */
  async function judged(files) {
    const dir = await workspace('judge', files);
    const out = path.join(dir, 'tests.jsonl');
    const env = clientEnvironment();
    const suite = await run(process.execPath, ['--test', '--test-concurrency=1', `--test-reporter=${reporter}`, `--test-reporter-destination=${out}`, '*.test.mjs'], { cwd: dir, env, timeoutMs: 60_000 });
    assert.equal(suite.code, 0, `the throwaway suite itself passes: ${suite.stdout}${suite.stderr}`);
    return run(checkRan, ['throwaway', out, ...Object.keys(files).map((name) => path.join(dir, name))], { cwd: dir, env, timeoutMs: 20_000 });
  }

  const passing = "import { test } from 'node:test'; test('one', () => {}); test('two', () => {});";

  for (const [what, source, name] of [
    ['a skipped test', "import { test } from 'node:test'; test('real', () => {}); test('skipped', { skip: true }, () => {});", 'skipped'],
    ['a test skipped from inside', "import { test } from 'node:test'; test('real', () => {}); test('self', (t) => { t.skip('why'); });", 'self'],
    ['a test left to do', "import { test } from 'node:test'; test('real', () => {}); test.todo('later');", 'later'],
    ['a skipped group', "import { describe, test } from 'node:test'; test('real', () => {}); describe.skip('group', () => { test('hidden', () => { throw new Error('never runs'); }); });", 'group'],
    ['a group skipped by option', "import { describe, test } from 'node:test'; test('real', () => {}); describe('group', { skip: 'later' }, () => { test('hidden', () => {}); });", 'group'],
    ['a group left to do', "import { describe, test } from 'node:test'; test('real', () => {}); describe.todo('group', () => { test('left', () => {}); });", 'group'],
    ['a skipped group inside a group', "import { describe, test } from 'node:test'; describe('outer', () => { test('real', () => {}); describe.skip('inner', () => { test('hidden', () => {}); }); });", 'inner']
  ]) {
    test(`${what} refuses the suite, and names it`, async () => {
      const check = await judged({ 'a.test.mjs': passing, 'b.test.mjs': source });
      assert.equal(check.code, 1, check.stderr);
      assert.match(check.stderr, /suite throwaway skipped \d+ test\(s\); record the open item in the table of run\.sh instead/);
      assert.match(check.stderr, new RegExp(`^  b\\.test\\.mjs: ${name}$`, 'm'));
    });
  }

  test('a file that defines no tests refuses the suite, though another file ran', async () => {
    const empty = "import { describe } from 'node:test'; describe('nothing', () => {});";
    const check = await judged({ 'a.test.mjs': passing, 'empty.test.mjs': empty });
    assert.equal(check.code, 1);
    assert.match(check.stderr, /suite throwaway: empty\.test\.mjs defines no tests/);
  });

  test('a suite that ran nothing is refused', async () => {
    const check = await judged({ 'none.test.mjs': "console.log('no tests here');" });
    assert.equal(check.code, 1);
    assert.match(check.stderr, /none\.test\.mjs defines no tests/);
  });

  test('a list that cannot be read refuses the suite', async () => {
    const dir = await workspace('judge');
    const check = await run(checkRan, ['throwaway', path.join(dir, 'missing.jsonl'), path.join(dir, 'a.test.mjs')], { cwd: dir, env: clientEnvironment(), timeoutMs: 20_000 });
    assert.equal(check.code, 1);
    assert.match(check.stderr, /a\.test\.mjs defines no tests/);
  });
});
