// Running a coding agent as a child process: the pinned binary, a clean
// environment, a bounded run, and the means to see what it sent. The agents
// are real releases of Claude Code, Codex and Gemini CLI; nothing here
// patches or stubs them. Suites run through tests/clients/run.sh, which
// already starts them from a clean environment with a private home.
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { accessSync, constants } from 'node:fs';
import http from 'node:http';
import { mkdir, mkdtemp, realpath, writeFile } from 'node:fs/promises';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const scratch = process.env.OLP_CLIENTS_SCRATCH;
assert.ok(scratch, 'OLP_CLIENTS_SCRATCH is not set; run the suites through tests/clients/run.sh');

/** The variables a client inherits: the private home and the proxy trap. Nothing else leaks in. */
const inherited = [
  'PATH', 'LANG', 'TERM', 'NO_COLOR', 'HOME', 'XDG_CONFIG_HOME', 'XDG_DATA_HOME', 'XDG_CACHE_HOME', 'XDG_STATE_HOME', 'TMPDIR',
  'DO_NOT_TRACK', 'NO_UPDATE_NOTIFIER', 'DISABLE_AUTOUPDATER', 'DISABLE_TELEMETRY',
  'HTTP_PROXY', 'HTTPS_PROXY', 'ALL_PROXY', 'NO_PROXY', 'NODE_USE_ENV_PROXY'
];

/**
 * The home the suite owns. A client writes its configuration and state there,
 * so nothing may touch a home that is not under the run's scratch directory.
 */
export function privateHome() {
  assert.ok(process.env.HOME?.startsWith(scratch), 'HOME is not private to the suite; run the suites through tests/clients/run.sh');
  return process.env.HOME;
}

/** The environment of a client: the inherited allowlist, then what the client itself needs. */
export function clientEnvironment(extra = {}) {
  privateHome();
  const env = {};
  for (const name of inherited) if (process.env[name] !== undefined) env[name] = process.env[name];
  // An undefined value removes the variable.
  return Object.fromEntries(Object.entries({ ...env, ...extra }).filter(([, value]) => value !== undefined));
}

/** The path of a client binary the package installed. */
export function binary(name) {
  const file = fileURLToPath(new URL(`../node_modules/.bin/${name}`, import.meta.url));
  try {
    accessSync(file, constants.X_OK);
  } catch {
    assert.fail(`${name} is not installed; run 'pnpm install --frozen-lockfile' in the repository`);
  }
  return file;
}

/** A directory private to one test, holding the given files: the client's working directory. */
export async function workspace(label, files = {}) {
  const dir = await realpath(await mkdtemp(path.join(scratch, `${label}-`)));
  for (const [name, content] of Object.entries(files)) {
    await mkdir(path.dirname(path.join(dir, name)), { recursive: true });
    await writeFile(path.join(dir, name), content);
  }
  return dir;
}

/** The process groups of the clients that are running. */
const running = new Set();

function killGroup(pid) {
  try {
    process.kill(-pid, 'SIGKILL');
  } catch {
    // The group is already gone.
  }
}

// A client runs in a process group of its own, so that its shell commands die
// with it, and a signal to this process does not reach it. When the suite is
// stopped, as run.sh does at its deadline, take the clients down with it
// instead of leaving them running.
process.on('exit', () => running.forEach(killGroup));
for (const signal of ['SIGINT', 'SIGTERM', 'SIGHUP']) {
  process.once(signal, () => {
    running.forEach(killGroup);
    process.kill(process.pid, signal);
  });
}

/**
 * Run a client to completion. stdin is closed so nothing waits for input, and
 * the whole process group is killed at the deadline, shell commands included.
 * A client that outlives it fails the test with everything it printed.
 */
export function run(command, args, { cwd, env, timeoutMs = 80_000 } = {}) {
  return new Promise((resolve, reject) => {
    const started = Date.now();
    const child = spawn(command, args, { cwd, env, stdio: ['ignore', 'pipe', 'pipe'], detached: true });
    let stdout = '';
    let stderr = '';
    child.stdout.setEncoding('utf8').on('data', (chunk) => (stdout += chunk));
    child.stderr.setEncoding('utf8').on('data', (chunk) => (stderr += chunk));
    // The group is the client's process id, which is not set when it fails to start.
    if (child.pid !== undefined) running.add(child.pid);
    const deadline = setTimeout(() => {
      killGroup(child.pid);
      reject(new Error(`${path.basename(command)} did not finish within ${timeoutMs} ms\n--- stdout\n${stdout}\n--- stderr\n${stderr}`));
    }, timeoutMs);
    child.on('error', (error) => {
      clearTimeout(deadline);
      reject(error);
    });
    // The client has exited. Anything it left running dies with it, which also
    // closes the pipes it held, so its output is complete when they close.
    // The grace is for a process that ignores the kill.
    child.on('exit', () => {
      killGroup(child.pid);
      running.delete(child.pid);
      setTimeout(() => {
        child.stdout.destroy();
        child.stderr.destroy();
      }, 2000).unref();
    });
    child.on('close', (code, signal) => {
      clearTimeout(deadline);
      resolve({ code, signal, stdout, stderr, durationMs: Date.now() - started });
    });
  });
}

/** Parse what a client printed as one JSON document, or fail with the output. */
export function parseJSON(result, what = 'stdout') {
  try {
    return JSON.parse(result.stdout);
  } catch (error) {
    assert.fail(`${what} is not JSON (${error.message})\n--- stdout\n${result.stdout}\n--- stderr\n${result.stderr}`);
  }
}

/** Parse what a client printed as one JSON document per line. */
export function parseJSONLines(result) {
  return result.stdout
    .split('\n')
    .filter((line) => line.startsWith('{'))
    .map((line) => {
      try {
        return JSON.parse(line);
      } catch (error) {
        assert.fail(`not a JSON line (${error.message}): ${line}`);
      }
    });
}

/**
 * The POST requests a client sent through a tap, in arrival order: those to
 * exactly this path, or those a predicate on the exchange accepts. A path is
 * exact so that `/v1/messages` does not also match `/v1/messages/count_tokens`.
 */
export async function sentTo(tap, match, method = 'POST') {
  const accepts = typeof match === 'function' ? match : (exchange) => exchange.path === match;
  return (await tap.sent()).filter((exchange) => exchange.method === method && accepts(exchange));
}

/**
 * Hold the upstream's recording against what the client sent through the tap
 * of tap.mjs: every request arrived with the same JSON body, except the model
 * the gateway rewrites in a body that names one, and the same values of the
 * headers that carry semantics. Order across concurrent requests is not
 * assumed.
 */
export function assertPassedThrough(sent, received, { model, headers = [] }) {
  assert.equal(received.length, sent.length, `the client sent ${sent.length} requests and the upstream received ${received.length}`);
  const unmatched = [...received];
  for (const request of sent) {
    assert.ok(request.body !== undefined, `the client's ${request.path} request is not JSON, or is compressed:\n${request.bodyText.slice(0, 500)}`);
    const body = model === undefined ? request.body : { ...request.body, model };
    const at = unmatched.findIndex((candidate) => {
      try {
        assert.deepEqual(candidate.body, body);
        return headers.every((name) => candidate.headers[name] === request.headers[name]);
      } catch {
        return false;
      }
    });
    assert.notEqual(at, -1, `no upstream request equals the client's ${request.path} request; the gateway changed it:\n${request.bodyText.slice(0, 2000)}`);
    unmatched.splice(at, 1);
  }
}

/**
 * A listener a client could reach only by bypassing the proxy trap of run.sh.
 * It binds an address the trap does not exempt (loopback exempts only
 * 127.0.0.1) and is proven reachable before it is handed out, so zero
 * connections means the client was held, not that the bait was deaf.
 */
export async function startBait() {
  const external = Object.values(os.networkInterfaces()).flat().filter((a) => a.family === 'IPv4' && !a.internal).map((a) => a.address);
  const connections = [];
  const server = net.createServer((socket) => {
    connections.push(socket.remoteAddress);
    socket.destroy();
  });
  let host;
  for (const candidate of ['127.0.0.2', ...external]) {
    try {
      await new Promise((resolve, reject) => (server.once('error', reject), server.listen(0, candidate, resolve)));
      host = candidate;
      break;
    } catch {
      server.removeAllListeners('error');
    }
  }
  assert.ok(host, 'no address the proxy trap does not exempt could be bound');
  const { port } = server.address();
  await new Promise((resolve, reject) => {
    const probe = net.connect(port, host, () => (probe.destroy(), resolve()));
    probe.on('error', reject);
  });
  for (let waited = 0; connections.length === 0 && waited < 50; waited++) await new Promise((resolve) => setTimeout(resolve, 20));
  assert.equal(connections.length, 1, 'the bait did not see its own probe');
  connections.length = 0;
  return { host, port, origin: `http://${host}:${port}`, connections, close: () => new Promise((resolve) => server.close(resolve)) };
}

/**
 * Assert a client pointed at the bait was held by the proxy trap: it never
 * connected, and it did not succeed either, since nothing answered it. A run
 * that timed out was still trying when it was killed.
 */
export function assertHeld(bait, outcome) {
  assert.deepEqual(bait.connections, [], 'the client reached an address outside OLP');
  if (outcome instanceof Error) {
    assert.match(outcome.message, /did not finish within/, 'the client could not be run');
  } else {
    assert.notEqual(outcome.code, 0, `the client succeeded without reaching anything:\n${outcome.stdout}`);
  }
}

/**
 * A stand-in for the dead proxy of the trap, which records where a client asks
 * it to go: the authority of each absolute-form request and CONNECT, and which
 * it refuses all. A client that honors the proxy environment shows up in
 * `attempts`; one that goes round it shows up at the bait.
 */
async function startTrap() {
  const attempts = [];
  const server = http.createServer((request, response) => {
    attempts.push(/^[a-z][a-z0-9+.-]*:\/\//i.test(request.url) ? new URL(request.url).host : request.headers.host);
    request.resume();
    response.writeHead(403, { connection: 'close' }).end();
  });
  server.on('connect', (request, socket) => {
    attempts.push(request.url);
    socket.end('HTTP/1.1 403 Forbidden\r\nconnection: close\r\n\r\n');
  });
  await new Promise((resolve, reject) => server.once('error', reject).listen(0, '127.0.0.1', resolve));
  const url = `http://127.0.0.1:${server.address().port}`;
  const close = () =>
    new Promise((resolve) => {
      server.close(resolve);
      server.closeAllConnections();
    });
  return { url, attempts, close };
}

/**
 * Assert that the proxy trap, and nothing else, holds a client pointed at the
 * bait: the client asks the proxy for the bait, as a client that honors the
 * proxy environment does, and never connects to it. A client that fails for
 * another reason, such as a flag it no longer knows or an authentication it is
 * not configured for, never asks for anything either, and would pass
 * {@link assertHeld} without the trap holding anything. `attempt` runs the
 * client against the bait with the proxy variables it is given on top of its own
 * environment, and resolves to the outcome of {@link run} or rejects with its
 * error. The proxy it is given is a stand-in that records the request and
 * refuses it, so the client reaches nothing outside the machine either way.
 */
export async function assertHeldByTheTrap(bait, attempt) {
  const trap = await startTrap();
  try {
    const outcome = await attempt({ HTTP_PROXY: trap.url, HTTPS_PROXY: trap.url, ALL_PROXY: trap.url }).catch((error) => error);
    assertHeld(bait, outcome);
    const described = outcome instanceof Error ? outcome.message : `exit code ${outcome.code}\n--- stdout\n${outcome.stdout}\n--- stderr\n${outcome.stderr}`;
    assert.ok(trap.attempts.includes(`${bait.host}:${bait.port}`), `the client never asked the proxy for the bait, so being held proves nothing (it asked for: ${trap.attempts.join(', ') || 'nothing'}): ${described}`);
  } finally {
    await trap.close();
  }
}
