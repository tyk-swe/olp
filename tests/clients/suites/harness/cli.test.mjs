// The helpers the coding-agent suites measure with are checked without any
// agent: the environment a client inherits, the bounded run, the tap that
// records what a client sent, the comparison against the upstream's recording,
// and the bait that proves the proxy trap holds.
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { readFile } from 'node:fs/promises';
import net from 'node:net';
import path from 'node:path';
import { setTimeout as sleep } from 'node:timers/promises';
import { describe, test } from 'node:test';
import { assertHeld, assertHeldByTheTrap, assertPassedThrough, binary, clientEnvironment, parseJSON, parseJSONLines, privateHome, run, sentTo, startBait, workspace } from '../../lib/cli.mjs';

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

  test('the home is the suite\'s own', () => {
    assert.equal(privateHome(), process.env.HOME);
    const home = process.env.HOME;
    process.env.HOME = '/root';
    try {
      assert.throws(() => privateHome(), /HOME is not private/);
      assert.throws(() => clientEnvironment(), /HOME is not private/);
    } finally {
      process.env.HOME = home;
    }
  });

  test('an unknown binary is a failure that says how to install it', () => {
    assert.throws(() => binary('no-such-client'), /no-such-client is not installed/);
  });

  test('a workspace is private to the suite and holds its files', async () => {
    const dir = await workspace('check', { 'a.txt': 'one', 'sub/b.txt': 'two' });
    assert.ok(dir.startsWith(process.env.OLP_CLIENTS_SCRATCH));
    assert.equal(await readFile(path.join(dir, 'sub/b.txt'), 'utf8'), 'two');
  });
});

describe('running a client', () => {
  const node = process.execPath;

  test('captures both streams and the exit code, with stdin closed', async () => {
    const result = await run(node, ['-e', "process.stdin.on('end', () => { console.log('{\"a\":1}'); console.error('warn'); process.exit(3); }); process.stdin.resume();"]);
    assert.equal(result.code, 3);
    assert.equal(result.stderr, 'warn\n');
    assert.deepEqual(parseJSON(result), { a: 1 });
    assert.deepEqual(parseJSONLines({ stdout: 'noise\n{"b":2}\n{"c":3}\n' }), [{ b: 2 }, { c: 3 }]);
    assert.throws(() => parseJSON({ stdout: 'not json', stderr: 'why' }), /stdout is not JSON/);
  });

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

describe('stopping the suite', () => {
  /** Whether a process is alive, polling for up to the given time. */
  async function dies(pid, withinMs) {
    for (let waited = 0; waited <= withinMs; waited += 50) {
      try {
        process.kill(pid, 0);
      } catch {
        return true;
      }
      await sleep(50);
    }
    return false;
  }

  // run.sh stops a suite at its deadline with a TERM, and when it is itself
  // interrupted with an INT. A client runs in a process group of its own, so
  // a signal to the suite does not reach it, and it would run on unseen.
  for (const signal of ['SIGTERM', 'SIGINT']) {
    test(`takes the clients with it on ${signal}`, async () => {
      const cwd = await workspace(`stopped-${signal}`);
      const cli = new URL('../../lib/cli.mjs', import.meta.url).href;
      const program = `import { run } from ${JSON.stringify(cli)};
        run('sh', ['-c', 'echo $$ > agent.pid.tmp && mv agent.pid.tmp agent.pid; exec sleep 300'], { cwd: ${JSON.stringify(cwd)}, timeoutMs: 120000 }).catch(() => {});`;
      const suite = spawn(process.execPath, ['--input-type=module', '-e', program], {
        env: clientEnvironment({ OLP_CLIENTS_SCRATCH: process.env.OLP_CLIENTS_SCRATCH }),
        stdio: 'ignore'
      });
      let agent;
      for (let waited = 0; agent === undefined && waited < 10_000; waited += 50) {
        agent = Number(await readFile(path.join(cwd, 'agent.pid'), 'utf8').catch(() => '')) || undefined;
        if (agent === undefined) await sleep(50);
      }
      assert.ok(agent, 'the client started');
      assert.ok(!(await dies(agent, 0)), 'the client is running');

      suite.kill(signal);
      const [, killedBy] = await once(suite, 'exit');
      assert.equal(killedBy, signal, 'the suite still ends as the signal says');
      assert.ok(await dies(agent, 3000), 'the client outlived the suite');
    });
  }

  test('takes the clients with it when it exits with one still running', async () => {
    const cwd = await workspace('abandoned');
    const cli = new URL('../../lib/cli.mjs', import.meta.url).href;
    const program = `import { existsSync } from 'node:fs';
      import { run } from ${JSON.stringify(cli)};
      run('sh', ['-c', 'echo $$ > agent.pid.tmp && mv agent.pid.tmp agent.pid; exec sleep 300'], { timeoutMs: 120000 }).catch(() => {});
      setInterval(() => existsSync('agent.pid') && process.exit(0), 20);`;
    const suite = spawn(process.execPath, ['--input-type=module', '-e', program], {
      cwd,
      env: clientEnvironment({ OLP_CLIENTS_SCRATCH: process.env.OLP_CLIENTS_SCRATCH }),
      stdio: 'ignore'
    });
    const [code] = await once(suite, 'exit');
    assert.equal(code, 0);
    const agent = Number(await readFile(path.join(cwd, 'agent.pid'), 'utf8'));
    assert.ok(await dies(agent, 3000), 'the client outlived the suite');
  });
});

describe('the requests a client sent', () => {
  test('are the POSTs to a path or accepted by a predicate, from the tap', async () => {
    const exchanges = [
      { method: 'POST', path: '/anthropic/v1/messages' },
      { method: 'POST', path: '/anthropic/v1/messages/count_tokens' },
      { method: 'HEAD', path: '/anthropic/v1/messages' },
      { method: 'POST', path: '/v1/responses' }
    ];
    const tap = { sent: async () => exchanges };
    assert.deepEqual(await sentTo(tap, '/anthropic/v1/messages'), [exchanges[0]], 'a path is exact');
    assert.deepEqual(await sentTo(tap, (e) => e.path.startsWith('/anthropic/')), exchanges.slice(0, 2), 'a predicate decides');
    assert.deepEqual(await sentTo(tap, '/anthropic/v1/messages', 'HEAD'), [exchanges[2]]);
    assert.deepEqual(await sentTo(tap, '/gemini'), []);
  });
});

describe('holding the upstream against what the client sent', () => {
  const sent = (body, headers = {}) => ({ path: '/v1/messages', headers: { 'anthropic-beta': 'b', ...headers }, body, bodyText: JSON.stringify(body) });
  const received = (body, headers = {}) => ({ body, headers: { 'anthropic-beta': 'b', ...headers } });
  const options = { model: 'upstream', headers: ['anthropic-beta'] };

  test('accepts the same requests, the model rewritten, in any order', () => {
    assertPassedThrough(
      [sent({ model: 'route', n: 1 }), sent({ model: 'route', n: 2 })],
      [received({ model: 'upstream', n: 2 }), received({ model: 'upstream', n: 1 })],
      options
    );
    assertPassedThrough([sent({ n: 1 })], [received({ n: 1 })], { headers: [] });
  });

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
  test('is reachable, counts what connects and starts from zero', async (t) => {
    const bait = await startBait();
    t.after(bait.close);
    assert.deepEqual(bait.connections, [], 'its own probe is not counted');
    assert.notEqual(bait.host, '127.0.0.1', 'it sits where the proxy trap does not exempt');
    await new Promise((resolve, reject) => {
      const socket = net.connect(bait.port, bait.host, () => (socket.destroy(), resolve()));
      socket.on('error', reject);
    });
    for (let waited = 0; bait.connections.length === 0 && waited < 50; waited++) await sleep(20);
    assert.equal(bait.connections.length, 1);
  });

  test('accepts a client that was held and refuses one that got through', () => {
    const outcome = (code) => ({ code, stdout: 'answer' });
    const timedOut = new Error('claude did not finish within 10000 ms');
    assertHeld({ connections: [] }, timedOut);
    assertHeld({ connections: [] }, outcome(1));
    assert.throws(() => assertHeld({ connections: ['10.0.0.1'] }, timedOut), /reached an address outside OLP/);
    assert.throws(() => assertHeld({ connections: [] }, outcome(0)), /succeeded without reaching anything/);
    assert.throws(() => assertHeld({ connections: [] }, new Error('spawn claude ENOENT')), /could not be run/);
  });

  test('is not reached by a client that honors the proxy trap', async (t) => {
    const bait = await startBait();
    t.after(bait.close);
    // Node honors the trap, so a request to the bait is sent to the dead proxy instead.
    const attempt = await fetch(`${bait.origin}/`).then(() => 'reached', () => 'refused');
    assert.equal(attempt, 'refused');
    assert.deepEqual(bait.connections, []);
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

  test('records what a client asks for by its CONNECT, or by the URL of a plain request whatever its Host header says', async (t) => {
    const bait = await startBait();
    t.after(bait.close);
    const raw = (request) =>
      `const proxy = new URL(process.env.HTTP_PROXY); require('node:net').connect(Number(proxy.port), proxy.hostname, function () { this.write(${request}); }).on('data', () => process.exit(1)).on('error', () => process.exit(1))`;
    await assertHeldByTheTrap(bait, attempt(bait, raw("'CONNECT ' + process.env.BAIT_HOST + ':' + process.env.BAIT_PORT + ' HTTP/1.1\\r\\nhost: elsewhere\\r\\n\\r\\n'")));
    await assertHeldByTheTrap(bait, attempt(bait, raw("'GET http://' + process.env.BAIT_HOST + ':' + process.env.BAIT_PORT + '/ HTTP/1.1\\r\\nhost: elsewhere\\r\\n\\r\\n'")));
  });
});
