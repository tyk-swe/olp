// The helpers the coding-agent suites measure with are checked without any
// agent: the environment a client inherits, the bounded run, the tap that
// records what a client sent, the comparison against the upstream's recording,
// and the bait that proves the proxy trap holds.
import assert from 'node:assert/strict';
import { chmod, readFile } from 'node:fs/promises';
import path from 'node:path';
import { setTimeout as sleep } from 'node:timers/promises';
import { describe, test } from 'node:test';
import { assertHeld, assertHeldByTheTrap, assertPassedThrough, binary, clientEnvironment, run, startBait, verifyVersion, workspace } from '../../lib/cli.mjs';

describe('the environment of a client', () => {
  test('is the private home and the proxy trap, never the harness contract', () => {
    const env = clientEnvironment({ ANTHROPIC_API_KEY: 'k', EMPTY: undefined });
    assert.equal(env.HOME, process.env.HOME);
    assert.equal(env.NODE_USE_ENV_PROXY, '1');
    assert.match(env.HTTP_PROXY, /^http:\/\/127\.0\.0\.1:9$/);
    assert.equal(env.ANTHROPIC_API_KEY, 'k');
    assert.ok(!('EMPTY' in env), 'an undefined value removes the variable');
    for (const name of Object.keys(env)) {
      assert.ok(!name.startsWith('OLP_CLIENTS_'), `${name} leaked into a client`);
    }
  });

  test('an unknown binary is a failure that says how to install it', () => {
    assert.throws(() => binary('no-such-client'), /no-such-client is not installed/);
  });

  test('coding clients use shared binaries and resolved versions without leaking metadata', async (t) => {
    for (const [name, prefix, output] of [['codex', 'OLP_CODEX', 'codex-cli 7.8.9'], ['claude', 'OLP_CLAUDE_CODE', '7.8.9 (Claude Code)']]) {
      const dir = await workspace('resolved', { client: `#!/bin/sh\nprintf '%s\\n' '${output}'\n` });
      const file = path.join(dir, 'client');
      await chmod(file, 0o755);
      for (const [key, value] of [[`${prefix}_BINARY`, file], [`${prefix}_VERSION`, '7.8.9']]) {
        const previous = process.env[key];
        t.after(() => previous === undefined ? delete process.env[key] : process.env[key] = previous);
        process.env[key] = value;
      }
      assert.equal(binary(name), file);
      assert.equal(await verifyVersion(name), output);
      assert.equal(clientEnvironment()[`${prefix}_BINARY`], undefined);
      assert.equal(clientEnvironment()[`${prefix}_VERSION`], undefined);
      process.env[`${prefix}_VERSION`] = '7.8.10';
      await assert.rejects(verifyVersion(name), /must match the resolved release/);
      delete process.env[`${prefix}_BINARY`];
      assert.throws(() => binary(name), /has no resolved binary/);
    }
  });
});

describe('running a client', () => {
  const node = process.execPath;

  test('does not wait for a helper the client left holding its output', async () => {
    // One in the client's process group, which dies with it, and one in a group
    // of its own, which keeps the pipes open until the run stops waiting.
    for (const detached of [false, true]) {
      const program = `require('node:child_process').spawn('sleep', ['15'], { stdio: 'inherit', detached: ${detached} }).unref(); console.log('done');`;
      const result = await run(node, ['-e', program], { timeoutMs: 20_000 });
      assert.equal(result.code, 0);
      assert.equal(result.stdout, 'done\n');
      assert.ok(result.durationMs < 8_000, `the run waited ${result.durationMs} ms for the helper (detached ${detached})`);
    }
  });

  test('kills a client that outlives its deadline, with everything it started', async () => {
    const cwd = await workspace('deadline');
    const program = `const { spawn } = require('node:child_process');
      const child = spawn('sleep', ['60'], { stdio: 'ignore' });
      require('node:fs').writeFileSync('child.pid', String(child.pid));
      console.log('started'); setInterval(() => {}, 1000);`;
    await assert.rejects(run(node, ['-e', program], { cwd, timeoutMs: 1500 }), /did not finish within 1500 ms[\s\S]*started/);
    const pid = Number(await readFile(path.join(cwd, 'child.pid'), 'utf8'));
    for (let waited = 0; waited < 50; waited++) {
      try {
        process.kill(pid, 0);
      } catch {
        return; // The grandchild is gone.
      }
      await sleep(100);
    }
    assert.fail('the shell command the client started survived its deadline');
  });
});

describe('holding the upstream against what the client sent', () => {
  const sent = (body, headers = {}) => ({ path: '/v1/messages', headers: { 'anthropic-beta': 'b', ...headers }, body, bodyText: JSON.stringify(body) });
  const received = (body, headers = {}) => ({ body, headers: { 'anthropic-beta': 'b', ...headers } });
  const options = { model: 'upstream', headers: ['anthropic-beta'] };

  test('rejects an altered body, a dropped header and a missing or extra request', () => {
    const one = [sent({ model: 'route', n: 1 })];
    assert.throws(() => assertPassedThrough(one, [received({ model: 'upstream', n: 2 })], options), /changed it/);
    assert.throws(() => assertPassedThrough(one, [received({ model: 'upstream', n: 1, extra: true })], options), /changed it/);
    assert.throws(() => assertPassedThrough(one, [received({ model: 'upstream' })], options), /changed it/);
    assert.throws(() => assertPassedThrough(one, [received({ model: 'route', n: 1 })], options), /changed it/, 'the model is rewritten');
    assert.throws(() => assertPassedThrough(one, [{ body: { model: 'upstream', n: 1 }, headers: {} }], options), /changed it/);
    assert.throws(() => assertPassedThrough(one, [], options), /sent 1 requests and the upstream received 0/);
    assert.throws(() => assertPassedThrough([], [received({})], options), /sent 0 requests and the upstream received 1/);
    assert.throws(() => assertPassedThrough([{ path: '/v1/messages', headers: {}, body: undefined, bodyText: 'not json' }], [received({})], options), /not JSON, or is compressed/);
    // One upstream request cannot stand for two identical client requests.
    assert.throws(() => assertPassedThrough([...one, ...one], [received({ model: 'upstream', n: 1 }), received({ model: 'upstream', n: 9 })], options), /changed it/);
  });
});

describe('the bait', () => {

  test('accepts a client that was held and refuses one that got through', () => {
    const outcome = (code) => ({ code, stdout: 'answer' });
    const timedOut = new Error('claude did not finish within 10000 ms');
    assertHeld({ connections: [] }, timedOut);
    assertHeld({ connections: [] }, outcome(1));
    assert.throws(() => assertHeld({ connections: ['10.0.0.1'] }, timedOut), /reached an address outside OLP/);
    assert.throws(() => assertHeld({ connections: [] }, outcome(0)), /succeeded without reaching anything/);
    assert.throws(() => assertHeld({ connections: [] }, new Error('spawn claude ENOENT')), /could not be run/);
  });
});

describe('the proxy trap, held against the requests it refused', () => {
  const node = process.execPath;

  /** Run a client program against the bait with the proxy variables of the attempt. */
  const attempt = (bait, program, timeoutMs = 15_000) => (trap, signal) =>
    run(node, ['-e', program], { timeoutMs, signal, env: clientEnvironment({ BAIT: bait.origin, BAIT_HOST: bait.host, BAIT_PORT: String(bait.port), ...trap }) });

  test('accepts a client that asks the proxy for the bait and is refused', async (t) => {
    const bait = await startBait();
    t.after(bait.close);
    // Node honors the proxy environment, and fails when the proxy refuses.
    await assertHeldByTheTrap(bait, attempt(bait, 'fetch(process.env.BAIT).then((response) => process.exit(response.ok ? 0 : 1), () => process.exit(1))'));
    assert.deepEqual(bait.connections, [], 'the held run left the bait untouched');
  });

  test('refuses a client that never tries to go anywhere, which a bare held-check would have accepted', async (t) => {
    const bait = await startBait();
    t.after(bait.close);
    const client = attempt(bait, "console.error('unknown option'); process.exit(2)");
    assertHeld(bait, await client({})); // A check of the outcome alone takes it for held.
    await assert.rejects(assertHeldByTheTrap(bait, client), /never asked the proxy for the bait[\s\S]*asked for: nothing[\s\S]*unknown option/);
    // A client that cannot even be started is no better.
    await assert.rejects(assertHeldByTheTrap(bait, () => run('/nonexistent/client', [])), /the client could not be run/);
  });

  test('refuses a client that asks for something else than the bait', async (t) => {
    const bait = await startBait();
    t.after(bait.close);
    await assert.rejects(assertHeldByTheTrap(bait, attempt(bait, "fetch('http://127.0.0.3:18080/').then(() => process.exit(1), () => process.exit(1))")), /never asked the proxy for the bait[\s\S]*asked for: 127\.0\.0\.3:18080/);
  });

  test('refuses a client that gets past the trap', async (t) => {
    const bait = await startBait();
    t.after(bait.close);
    // It connects directly, whatever the environment says.
    const program = "require('node:net').connect(Number(process.env.BAIT_PORT), process.env.BAIT_HOST).on('connect', () => process.exit(1)).on('error', () => process.exit(1))";
    await assert.rejects(assertHeldByTheTrap(bait, attempt(bait, program)), /reached an address outside OLP/);
  });

  test('stops a client that keeps retrying soon after it asks for the bait, not at its deadline', async (t) => {
    const bait = await startBait();
    t.after(bait.close);
    const program = 'setInterval(() => fetch(process.env.BAIT).catch(() => {}), 100)';
    const started = Date.now();
    await assertHeldByTheTrap(bait, attempt(bait, program, 60_000), { watchMs: 500 });
    assert.ok(Date.now() - started < 30_000, `the retrying client ran ${Date.now() - started} ms`);
  });

  test('refuses a client that goes round the proxy after it was refused', async (t) => {
    const bait = await startBait();
    t.after(bait.close);
    // It honors the proxy first, then connects directly while it is still watched.
    const program =
      "fetch(process.env.BAIT).catch(() => {}).then(() => setTimeout(() => require('node:net').connect(Number(process.env.BAIT_PORT), process.env.BAIT_HOST).on('error', () => {}), 200))";
    await assert.rejects(assertHeldByTheTrap(bait, attempt(bait, `${program}; setInterval(() => {}, 1000)`, 60_000), { watchMs: 2_000 }), /reached an address outside OLP/);
  });

  test('refuses a client that succeeds although it was refused', async (t) => {
    const bait = await startBait();
    t.after(bait.close);
    const program = 'fetch(process.env.BAIT).catch(() => {}).then(() => process.exit(0))';
    await assert.rejects(assertHeldByTheTrap(bait, attempt(bait, program)), /succeeded without reaching anything/);
  });
});
