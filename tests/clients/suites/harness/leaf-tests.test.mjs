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
import leafReporter, { leafTest } from '../../lib/leaf-tests.mjs';

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

  test('are the groups that are skipped or left to do, as themselves', () => {
    const group = { details: { type: 'suite' } };
    assert.deepEqual(leafTest(event('test:pass', { ...group, name: 'a group', skip: true })), { file: 'a.test.mjs', name: 'a group', skip: true, todo: false, failed: false });
    assert.equal(leafTest(event('test:pass', { ...group, name: 'a group', skip: 'not yet' })).skip, true);
    assert.equal(leafTest(event('test:pass', { ...group, name: 'a group', todo: true })).todo, true);
    assert.equal(leafTest(event('test:pass', { ...group, name: 'a group', nesting: 1, skip: true })).skip, true, 'a group inside a group');
  });

  test('are not suites, nor the stand-in for a file that defines nothing, nor other events', () => {
    assert.equal(leafTest(event('test:pass', { name: 'a suite', details: { type: 'suite' } })), undefined);
    assert.equal(leafTest(event('test:fail', { name: 'a suite', details: { type: 'suite' } })), undefined);
    assert.equal(leafTest(event('test:pass', { name: 'a suite', details: { type: 'suite' }, skip: false, todo: false })), undefined);
    assert.equal(leafTest(event('test:pass', { name: 'suites/a.test.mjs' })), undefined, 'named after the path it was given');
    assert.equal(leafTest(event('test:pass', { name: 'a.test.mjs' })), undefined);
    assert.equal(leafTest(event('test:fail', { name: '/suites/a.test.mjs' })), undefined, 'a file that failed to load');
    assert.notEqual(leafTest(event('test:pass', { name: 'a.test.mjs', nesting: 1 })), undefined, 'a test of that name inside a suite is a test');
    for (const type of ['test:start', 'test:enqueue', 'test:dequeue', 'test:diagnostic', 'test:stdout', 'test:summary']) {
      assert.equal(leafTest(event(type, { name: 'one' })), undefined, type);
    }
    assert.equal(leafTest({ type: 'test:pass', data: { name: 'one', nesting: 0 } }), undefined, 'an event without details');
  });

  test('are written one JSON object to a line', async () => {
    async function* events() {
      yield event('test:start', { name: 'one' });
      yield event('test:pass', { name: 'one' });
      yield event('test:pass', { name: 'group', details: { type: 'suite' } });
      yield event('test:fail', { name: 'two' });
    }
    const lines = [];
    for await (const line of leafReporter(events())) lines.push(line);
    assert.deepEqual(lines.map((line) => JSON.parse(line).name), ['one', 'two']);
    assert.ok(lines.every((line) => line.endsWith('\n') && !line.slice(0, -1).includes('\n')));
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

  test('a file that defines none lists none', async () => {
    assert.deepEqual((await listed('none.test.mjs', "console.log('no tests here');")).tests, []);
  });

  test('a file with only an empty describe lists none', async () => {
    const source = "import { describe } from 'node:test'; describe('nothing', () => {}); describe('still nothing', () => { describe('nested', () => {}); });";
    assert.deepEqual((await listed('empty.test.mjs', source)).tests, []);
  });

  test('a file that cannot be loaded lists none, and fails', async () => {
    const { code, tests } = await listed('broken.test.mjs', "import { test } from 'node:test'; test('never reached', () => {}); }");
    assert.deepEqual(tests, []);
    assert.notEqual(code, 0);
  });

  test('a skipped or unfinished group is listed, though node:test reports none of its tests', async () => {
    const source = `import { describe, test } from 'node:test';
      test('real', () => {});
      describe.skip('skipped', () => { test('hidden', () => {}); });
      describe('skipped by option', { skip: 'later' }, () => { test('also hidden', () => {}); });
      describe('outer', () => { describe.skip('inner', () => { test('hidden too', () => {}); }); });
      describe.todo('unfinished', () => { test('left to do', () => {}); });`;
    const { code, tests } = await listed('groups.test.mjs', source);
    assert.equal(code, 0);
    assert.deepEqual(
      Object.fromEntries(tests.map((entry) => [entry.name, [entry.skip, entry.todo]])),
      { real: [false, false], skipped: [true, false], 'skipped by option': [true, false], inner: [true, false], unfinished: [false, true], 'left to do': [false, true] }
    );
  });

  test('every test of a file is listed, each with how it ended', async () => {
    const source = `import { describe, test } from 'node:test';
      describe('group', () => {
        test('nested', () => {});
        test('skipped', { skip: true }, () => {});
        describe('deeper', () => { test('todo', { todo: 'later' }, () => {}); });
      });
      test('top', () => {});
      test('fails', () => { throw new Error('no'); });`;
    const { code, tests } = await listed('mixed.test.mjs', source);
    assert.notEqual(code, 0, 'a failing test fails the run');
    const byName = Object.fromEntries(tests.map((entry) => [entry.name, entry]));
    assert.deepEqual(Object.keys(byName).sort(), ['fails', 'nested', 'skipped', 'todo', 'top']);
    assert.ok(tests.every((entry) => entry.file === 'mixed.test.mjs'));
    assert.deepEqual(
      Object.fromEntries(tests.map((entry) => [entry.name, [entry.skip, entry.todo, entry.failed]])),
      { nested: [false, false, false], skipped: [true, false, false], todo: [false, true, false], top: [false, false, false], fails: [false, false, true] }
    );
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

  test('a suite whose every test ran is accepted', async () => {
    const check = await judged({ 'a.test.mjs': passing, 'b.test.mjs': passing });
    assert.equal(check.code, 0, check.stderr);
  });

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
