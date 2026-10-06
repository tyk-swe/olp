// Codex CLI, the pinned release, in `codex exec` against OLP's OpenAI surface
// as a custom model provider speaking the Responses API: the configuration
// docs/clients.md documents. It runs against a transformed route with the stock
// tool set, and against a strict route with the provider-hosted tools turned
// off, which a strict route refuses.
//
// Codex keeps no state at the provider: it sends `store: false` and replays the
// whole conversation each turn, reasoning items included, so continuation is
// asserted as the replay arriving intact at the upstream.
import assert from 'node:assert/strict';
import { writeFile } from 'node:fs/promises';
import path from 'node:path';
import { beforeEach, describe, test } from 'node:test';
import {
  apiKey,
  assertClean,
  defaultReply,
  models,
  onlyRequest,
  recorded,
  resetRecorded,
  restrictedApiKey,
  script,
  toolResultsPrefix,
  upstreamModels
} from '../../lib/harness.mjs';
import { assertHeldByTheTrap, assertPassedThrough, binary, clientEnvironment, parseJSONLines, run, sentTo, startBait, workspace } from '../../lib/cli.mjs';
import { startTap } from '../../lib/tap.mjs';

const codex = binary('codex');

beforeEach(resetRecorded);

/**
 * The configuration of a custom provider. A strict route refuses the hosted
 * tools Codex adds by default, web search and the multi-agent namespace, so
 * the strict configuration turns them off.
 */
function configuration({ baseURL, route, hosted }) {
  return [
    `model = "${route}"`,
    'model_provider = "olp"',
    'approval_policy = "never"',
    'sandbox_mode = "read-only"',
    'check_for_update_on_startup = false',
    ...(hosted ? [] : ['web_search = "disabled"']),
    '',
    '[analytics]',
    'enabled = false',
    '',
    '[feedback]',
    'enabled = false',
    '',
    ...(hosted ? [] : ['[features]', 'multi_agent = false', '']),
    '[model_providers.olp]',
    'name = "OLP"',
    `base_url = "${baseURL}"`,
    'env_key = "OLP_API_KEY"',
    'wire_api = "responses"',
    ''
  ].join('\n');
}

/** A session: a private CODEX_HOME and working directory, and a tap in front of the gateway. */
async function session(t, { route, hosted, files = {}, key = apiKey }) {
  const tap = await startTap();
  t.after(() => tap.close());
  const home = await workspace('codex-home');
  const cwd = await workspace('codex', files);
  await writeFile(path.join(home, 'config.toml'), configuration({ baseURL: tap.baseURLs.openai, route, hosted }));
  const exec = async (prompt, { resume = false, timeoutMs } = {}) => {
    const args = ['exec', '--strict-config', '--skip-git-repo-check', '--json', ...(resume ? ['resume', '--last'] : []), prompt];
    const result = await run(codex, args, { cwd, timeoutMs, env: clientEnvironment({ CODEX_HOME: home, OLP_API_KEY: key }) });
    return { result, events: parseJSONLines(result) };
  };
  return { tap, cwd, exec };
}

const completed = (events, type) => events.filter((e) => e.type === 'item.completed' && e.item.type === type).map((e) => e.item);

/** Assert a turn ended in success, and return the agent's final message. */
function finalMessage({ result, events }) {
  assert.equal(result.code, 0, `${result.stderr}\n${result.stdout}`);
  assert.ok(events.some((e) => e.type === 'turn.completed'), 'the turn completed');
  assert.ok(!events.some((e) => e.type === 'turn.failed' || e.type === 'error'), 'the turn did not fail');
  const messages = completed(events, 'agent_message');
  assert.ok(messages.length >= 1, 'the agent answered');
  return messages.at(-1).text;
}

const responsesFilter = { dialect: 'openai.responses' };
const semanticHeaders = [];

// The same assertions hold on both routes; what differs is the configuration.
for (const { name, route, hosted } of [
  { name: 'a transformed route with the stock tools', route: models.openai, hosted: true },
  { name: 'a strict route with the hosted tools off', route: models.openaiStrict, hosted: false }
]) {
  describe(`Codex CLI through ${name}`, () => {
    const strict = !hosted;

    test('streams a reply with its reasoning, and the upstream receives the request unchanged', async (t) => {
      const { tap, exec } = await session(t, { route, hosted });
      const turn = await exec(`Say hello. ${script.think('thinking it over')}`);
      assert.equal(finalMessage(turn), defaultReply);
      assert.deepEqual(completed(turn.events, 'reasoning').map((r) => r.text), ['thinking it over']);
      assert.ok(turn.events.find((e) => e.type === 'turn.completed').usage.input_tokens > 0, 'usage came back through the gateway');

      const request = await onlyRequest(responsesFilter);
      assert.equal(request.path, '/openai/v1/responses');
      assert.equal(request.stream, true);
      assert.equal(request.body.model, upstreamModels.openai, 'the gateway rewrites the route slug');
      assert.equal(request.body.stream, true);
      assert.equal(request.body.store, false, 'Codex keeps no state at the provider');
      assert.ok(request.body.include.includes('reasoning.encrypted_content'), 'Codex asks for the encrypted reasoning it replays');
      assert.ok(request.body.tools.some((tool) => tool.name === 'exec_command'));
      assert.match(JSON.stringify(request.body.input), /Say hello\./);
      assert.equal(request.headers.authorization, '[present]');
      if (strict) assert.ok(!request.body.tools.some((tool) => tool.type !== 'function'), 'no hosted tool is configured');
      else assert.ok(request.body.tools.some((tool) => tool.type === 'web_search'), 'the stock tools include hosted web search');
      assertPassedThrough(await sentTo(tap, '/v1/responses'), [request], { model: upstreamModels.openai, headers: semanticHeaders });
    });

    test('runs a shell tool call and returns its output, replaying the reasoning with the call', async (t) => {
      const secret = 'the vault code is marmalade-7';
      const { tap, exec } = await session(t, { route, hosted, files: { 'note.txt': `${secret}\n` } });
      const turn = await exec(`Read note.txt ${script.tool('exec_command', { cmd: 'cat note.txt' })} ${script.think('I should read the file')}`);
      const text = finalMessage(turn);
      assert.ok(text.startsWith(toolResultsPrefix) && text.includes(secret), text);
      const [command] = completed(turn.events, 'command_execution');
      assert.equal(command.exit_code, 0);
      assert.equal(command.status, 'completed');
      assert.equal(command.aggregated_output, `${secret}\n`);

      const requests = assertClean(await recorded(responsesFilter));
      assert.deepEqual(requests.map((r) => r.script), ['tool_call', 'tool_result']);
      const replay = requests[1].body.input;
      const kinds = replay.map((item) => item.type);
      const at = kinds.indexOf('reasoning');
      assert.ok(at > 0 && kinds[at + 1] === 'function_call' && kinds[at + 2] === 'function_call_output', `the replay keeps the order: ${kinds}`);
      assert.equal(replay[at].encrypted_content, 'fixture-encrypted-reasoning', 'the encrypted reasoning came back untouched');
      assert.equal(replay[at + 1].name, 'exec_command');
      assert.equal(replay[at + 2].call_id, replay[at + 1].call_id, 'the output answers the call');
      assert.match(replay[at + 2].output, /marmalade-7/);
      assertPassedThrough(await sentTo(tap, '/v1/responses'), requests, { model: upstreamModels.openai, headers: semanticHeaders });
    });

    test('runs parallel tool calls and answers every one', async (t) => {
      const { exec } = await session(t, { route, hosted, files: { 'a.txt': 'alpha\n', 'b.txt': 'bravo\n' } });
      const turn = await exec(`Read both. ${script.tool('exec_command', { cmd: 'cat a.txt' })} ${script.also('exec_command', { cmd: 'cat b.txt' })}`);
      const text = finalMessage(turn);
      assert.ok(text.includes('alpha') && text.includes('bravo'), text);
      assert.deepEqual(completed(turn.events, 'command_execution').map((c) => c.aggregated_output).sort(), ['alpha\n', 'bravo\n']);

      const requests = assertClean(await recorded(responsesFilter));
      const replay = requests.at(-1).body.input;
      const calls = replay.filter((item) => item.type === 'function_call');
      const outputs = replay.filter((item) => item.type === 'function_call_output');
      assert.equal(calls.length, 2);
      assert.deepEqual(outputs.map((o) => o.call_id).sort(), calls.map((c) => c.call_id).sort(), 'each call has its own output');
    });

    test('continues a session by replaying its history, reasoning items included', async (t) => {
      const { exec } = await session(t, { route, hosted });
      const first = await exec(`Remember the word kumquat. ${script.think('first thoughts')} ${script.reply('noted kumquat')}`);
      assert.equal(finalMessage(first), 'noted kumquat');
      const second = await exec('What was the word?', { resume: true });
      assert.equal(finalMessage(second), defaultReply);
      assert.equal(second.events.find((e) => e.type === 'thread.started').thread_id, first.events.find((e) => e.type === 'thread.started').thread_id, 'the same thread');

      const requests = assertClean(await recorded(responsesFilter));
      assert.equal(requests.length, 2);
      assert.equal(requests[1].body.previous_response_id ?? null, null, 'Codex replays instead of referring to a stored response');
      const replay = requests[1].body.input;
      const kinds = replay.map((item) => item.type === 'message' ? `${item.type}:${item.role}` : item.type);
      const at = kinds.indexOf('reasoning');
      assert.ok(at > 0, `the replay carries the first turn's reasoning: ${kinds}`);
      assert.equal(replay[at].encrypted_content, 'fixture-encrypted-reasoning');
      assert.equal(kinds[at + 1], 'message:assistant');
      assert.match(JSON.stringify(replay[at + 1]), /noted kumquat/);
      assert.match(JSON.stringify(replay), /Remember the word kumquat/);
      assert.match(JSON.stringify(replay.at(-1)), /What was the word\?/);
    });
  });
}

describe('Codex CLI against the gateway', () => {
  test('reports a route the key may not use as the gateway\'s typed error', async (t) => {
    const { exec } = await session(t, { route: models.openai, hosted: true, key: restrictedApiKey });
    const { result, events } = await exec('Say hello.');
    assert.equal(result.code, 1);
    const failure = events.find((e) => e.type === 'turn.failed');
    assert.match(failure.error.message, /403/);
    assert.match(failure.error.message, new RegExp(`not allowed to use the model .${models.openai}.`));
    assert.deepEqual(await recorded(), [], 'a refused request never reaches the upstream');
  });

  test('cannot reach past the gateway', async (t) => {
    const bait = await startBait();
    t.after(() => bait.close());
    const home = await workspace('codex-home');
    await writeFile(path.join(home, 'config.toml'), configuration({ baseURL: `${bait.origin}/v1`, route: models.openai, hosted: true }));
    const cwd = await workspace('codex');
    // The client retries through the dead proxy until it is stopped, which is
    // expected; the deadline only bounds how long it may take to start.
    await assertHeldByTheTrap(bait, (trap, signal) =>
      run(codex, ['exec', '--strict-config', '--skip-git-repo-check', '--json', 'Say hello.'], {
        cwd,
        timeoutMs: 60_000,
        signal,
        env: clientEnvironment({ CODEX_HOME: home, OLP_API_KEY: apiKey, ...trap })
      })
    );
  });
});
