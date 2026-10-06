// Claude Code, the pinned release, in print mode against OLP's Anthropic
// surface: the configuration docs/clients.md documents. Claude Code treats an
// ANTHROPIC_BASE_URL gateway as the Anthropic API and sends it the beta headers
// and request fields it sends api.anthropic.com. The two travel as pairs, and
// the scripted upstream refuses a field whose beta header is missing, so a
// route has to pass both through. It does on a strict route bound to the
// anthropic-messages profile and on a transformed route to an Anthropic
// provider.
//
// Every test asserts the client's own outcome and what the upstream received,
// and a tap between the client and the gateway proves the upstream received
// what the client sent.
import assert from 'node:assert/strict';
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
import { assertHeldByTheTrap, assertPassedThrough, binary, clientEnvironment, parseJSON, run, sentTo, startBait, workspace } from '../../lib/cli.mjs';
import { startTap } from '../../lib/tap.mjs';

const claude = binary('claude');
const semanticHeaders = ['anthropic-beta', 'anthropic-version'];
const messagesFilter = { dialect: 'anthropic.messages' };
const countFilter = { dialect: 'anthropic.count_tokens' };

beforeEach(resetRecorded);

/**
 * Run `claude -p` against the gateway through a tap. The permission mode is
 * always explicit: the default mode of this release is auto, whose classifier
 * would add requests no test scripted.
 */
async function ask(t, prompt, { route, cwd, args = [], mode = 'default', env = {}, timeoutMs }) {
  const tap = await startTap();
  t.after(() => tap.close());
  const result = await run(claude, ['-p', prompt, '--model', route, '--output-format', 'json', '--permission-mode', mode, ...args], {
    cwd: cwd ?? (await workspace('claude')),
    timeoutMs,
    env: clientEnvironment({
      ANTHROPIC_BASE_URL: tap.baseURLs.anthropic,
      ANTHROPIC_API_KEY: apiKey,
      // Background work goes to the same route instead of a model OLP does not publish.
      ANTHROPIC_DEFAULT_HAIKU_MODEL: route,
      ANTHROPIC_SMALL_FAST_MODEL: route,
      CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: '1',
      ...env
    })
  });
  return { tap, result };
}

const blocks = (messages, type) => messages.flatMap((m) => (Array.isArray(m.content) ? m.content : [])).filter((b) => b.type === type);

// The same assertions hold on both routes.
for (const { name, route } of [
  { name: 'a strict route', route: models.anthropicStrict },
  { name: 'a transformed route', route: models.anthropic }
]) {
  describe(`Claude Code through ${name}`, () => {
    /** What every generation request must satisfy, whichever route served it. */
    function assertRequest(request) {
      assert.equal(request.path, '/anthropic/v1/messages');
      assert.equal(request.body.model, upstreamModels.anthropic, 'the gateway rewrites the route slug');
      assert.equal(request.body.stream, true, 'Claude Code streams');
      assert.equal(request.headers['anthropic-version'], '2023-06-01');
      assert.equal(request.query ?? '', '', 'the SDK query marker is consumed by the gateway');
      assert.equal(request.headers['x-api-key'], '[present]');
      assert.match(request.headers['anthropic-beta'], /(^|,)claude-code-/);
      // The beta fields arrive with their headers; the upstream refuses them otherwise.
      assert.ok(request.body.context_management);
      assert.match(request.headers['anthropic-beta'], /(^|,)context-management-/);
      assert.match(request.headers['anthropic-beta'], /(^|,)effort-/);
    }

    test('streams a reply, and the upstream receives the request unchanged', async (t) => {
      const { tap, result } = await ask(t, 'Say hello.', { route });
      assert.equal(result.code, 0, result.stderr);
      const out = parseJSON(result);
      assert.equal(out.is_error, false);
      assert.equal(out.result, defaultReply);

      const requests = assertClean(await recorded(messagesFilter));
      assert.ok(requests.length >= 1);
      requests.forEach(assertRequest);
      assertPassedThrough(await sentTo(tap, '/anthropic/v1/messages'), requests, { model: upstreamModels.anthropic, headers: semanticHeaders });
    });

    test('completes a tool-use loop with a built-in tool, reading the prompt cache on the second round', async (t) => {
      const secret = 'the vault code is marmalade-7';
      const cwd = await workspace('claude', { 'note.txt': `${secret}\n` });
      const { tap, result } = await ask(t, `Read note.txt ${script.tool('Read', { file_path: `${cwd}/note.txt` })}`, { route, cwd, args: ['--allowedTools', 'Read'] });
      assert.equal(result.code, 0, result.stderr);
      const out = parseJSON(result);
      assert.equal(out.is_error, false);
      assert.deepEqual(out.permission_denials, []);
      assert.equal(out.num_turns, 2);
      assert.ok(out.result.startsWith(toolResultsPrefix) && out.result.includes(secret), `the final reply carries the file the tool read: ${out.result}`);
      // Prompt caching works across the loop: round one writes the prefix and round two reads it.
      assert.ok(out.usage.cache_creation_input_tokens > 0, 'the first round wrote the prompt cache');
      assert.ok(out.usage.cache_read_input_tokens > 0, 'the second round read the prompt cache');

      const requests = assertClean(await recorded(messagesFilter));
      assert.deepEqual(requests.map((r) => r.script), ['tool_call', 'tool_result']);
      requests.forEach(assertRequest);
      const [first, second] = requests;
      const calls = blocks(second.body.messages, 'tool_use');
      assert.equal(calls.length, 1);
      assert.equal(calls[0].name, 'Read');
      const results = blocks(second.body.messages, 'tool_result');
      assert.equal(results.length, 1);
      assert.equal(results[0].tool_use_id, calls[0].id, 'the tool result answers the call');
      assert.match(JSON.stringify(results[0].content), /marmalade-7/);
      // The mid-conversation system messages, the cache markers and the signed
      // thinking Claude Code sends all reach the upstream, which would reject
      // the conversation without them.
      for (const request of requests) {
        assert.ok(request.body.messages.some((m) => m.role === 'system'), 'a mid-conversation system message arrived');
        assert.ok(request.body.system.some((b) => b.cache_control), 'a cache marker on the system prompt arrived');
      }
      assert.ok(blocks(second.body.messages, 'thinking').length > 0, 'the signed thinking arrived');
      assert.ok(first.body.tools.some((tool) => tool.name === 'Read'));
      assertPassedThrough(await sentTo(tap, '/anthropic/v1/messages'), requests, { model: upstreamModels.anthropic, headers: semanticHeaders });
    });

    test('runs parallel tool calls and answers every one in a single message', async (t) => {
      const cwd = await workspace('claude', { 'a.txt': 'alpha\n', 'b.txt': 'bravo\n' });
      const prompt = `Read both. ${script.tool('Read', { file_path: `${cwd}/a.txt` })} ${script.also('Read', { file_path: `${cwd}/b.txt` })}`;
      const { result } = await ask(t, prompt, { route, cwd, args: ['--allowedTools', 'Read'] });
      assert.equal(result.code, 0, result.stderr);
      const out = parseJSON(result);
      assert.ok(out.result.includes('alpha') && out.result.includes('bravo'), out.result);

      const requests = assertClean(await recorded(messagesFilter));
      assert.equal(requests.length, 2);
      const calls = blocks(requests[1].body.messages, 'tool_use');
      assert.equal(calls.length, 2);
      const answered = requests[1].body.messages.filter((m) => Array.isArray(m.content) && m.content.some((b) => b.type === 'tool_result'));
      assert.equal(answered.length, 1, 'both results come back in one user message');
      assert.deepEqual(
        answered[0].content.filter((b) => b.type === 'tool_result').map((b) => b.tool_use_id).sort(),
        calls.map((c) => c.id).sort()
      );
    });

    test('counts tokens through the count_tokens endpoint', async (t) => {
      const { tap, result } = await ask(t, '/context', { route });
      assert.equal(result.code, 0, result.stderr);
      const out = parseJSON(result);
      assert.equal(out.is_error, false);
      assert.match(out.result, /Context Usage/);
      assert.match(out.result, /\*\*Tokens:\*\* [\d.]+k? \/ 200k/);

      const requests = assertClean(await recorded(countFilter));
      assert.ok(requests.length >= 1, '/context counts through the endpoint');
      for (const request of requests) {
        assert.equal(request.path, '/anthropic/v1/messages/count_tokens');
        assert.equal(request.body.model, upstreamModels.anthropic);
        assert.equal(request.script, 'count_tokens');
        assert.equal(request.query ?? '', '');
        assert.match(request.headers['anthropic-beta'], /(^|,)token-counting-/);
      }
      assert.deepEqual(await recorded(messagesFilter), [], 'counting makes no generation request');
      assertPassedThrough(await sentTo(tap, '/anthropic/v1/messages/count_tokens'), requests, { model: upstreamModels.anthropic, headers: semanticHeaders });
    });

    test('resumes a session, replaying its signed thinking and its first turn', async (t) => {
      const cwd = await workspace('claude');
      const first = await ask(t, `Remember the word kumquat. ${script.think('first thoughts')} ${script.reply('noted kumquat')}`, { route, cwd });
      assert.equal(first.result.code, 0, first.result.stderr);
      assert.equal(parseJSON(first.result).result, 'noted kumquat');
      const second = await ask(t, 'What was the word?', { route, cwd, args: ['--continue'] });
      assert.equal(second.result.code, 0, second.result.stderr);
      assert.equal(parseJSON(second.result).result, defaultReply);

      const requests = assertClean(await recorded(messagesFilter));
      assert.equal(requests.length, 2);
      const history = JSON.stringify(requests[1].body.messages);
      assert.match(history, /Remember the word kumquat/);
      assert.match(history, /noted kumquat/);
      assert.match(history, /What was the word\?/);
      assert.ok(blocks(requests[1].body.messages, 'thinking').length > 0, 'the first reply\'s signed thinking came back');
    });
  });
}

describe('Claude Code betas, credentials and egress', () => {
  const route = models.anthropicStrict;

  test('forwards the Anthropic-Beta header verbatim, including a beta the user asked for', async (t) => {
    const { result, tap } = await ask(t, 'Say hello.', { route, args: ['--betas', 'context-1m-2025-08-07'] });
    assert.equal(result.code, 0, result.stderr);
    assert.equal(parseJSON(result).result, defaultReply);
    const [sent] = await sentTo(tap, '/anthropic/v1/messages');
    assert.match(sent.headers['anthropic-beta'], /(^|,)context-1m-2025-08-07(,|$)/, 'the client sent the beta it was asked for');
    const request = await onlyRequest(messagesFilter);
    assert.equal(request.headers['anthropic-beta'], sent.headers['anthropic-beta']);
  });

  test('passes the auto-mode safeguards field and its paired beta through', async (t) => {
    const { result, tap } = await ask(t, 'Say hello.', { route, mode: 'auto' });
    assert.equal(result.code, 0, result.stderr);
    assert.equal(parseJSON(result).result, defaultReply);
    const [sent] = await sentTo(tap, '/anthropic/v1/messages');
    assert.ok(sent.body.safeguards, 'auto mode asks the server for classifier checks with a safeguards field');
    const request = await onlyRequest(messagesFilter);
    assert.deepEqual(request.body.safeguards, sent.body.safeguards);
    assert.match(request.headers['anthropic-beta'], /(^|,)dangerous-tool-use-/);
    assertPassedThrough(await sentTo(tap, '/anthropic/v1/messages'), [request], { model: upstreamModels.anthropic, headers: semanticHeaders });
  });

  test('authenticates with a bearer token as well as an API key', async (t) => {
    const { result } = await ask(t, 'Say hello.', { route, env: { ANTHROPIC_API_KEY: undefined, ANTHROPIC_AUTH_TOKEN: apiKey } });
    assert.equal(result.code, 0, result.stderr);
    assert.equal(parseJSON(result).result, defaultReply);
    const request = await onlyRequest(messagesFilter);
    assert.equal(request.authorized, true);
    assert.equal(request.leaked_client_credential, false);
  });

  test('reports a route the key may not use as the gateway\'s typed error', async (t) => {
    const { result } = await ask(t, 'Say hello.', { route, env: { ANTHROPIC_API_KEY: restrictedApiKey } });
    assert.equal(result.code, 1);
    const out = parseJSON(result);
    assert.equal(out.is_error, true);
    assert.equal(out.api_error_status, 403);
    assert.match(out.result, new RegExp(`not allowed to use the model .${route}.`));
    assert.deepEqual(await recorded(), [], 'a refused request never reaches the upstream');
  });

  test('cannot reach past the gateway', async (t) => {
    const bait = await startBait();
    t.after(() => bait.close());
    const cwd = await workspace('claude');
    // The client retries through the dead proxy until it is stopped, which is
    // expected; the deadline only bounds how long it may take to start.
    await assertHeldByTheTrap(bait, (trap, signal) =>
      run(claude, ['-p', 'Say hello.', '--model', route, '--output-format', 'json', '--permission-mode', 'default'], {
        cwd,
        timeoutMs: 60_000,
        signal,
        env: clientEnvironment({ ANTHROPIC_BASE_URL: `${bait.origin}/anthropic`, ANTHROPIC_API_KEY: apiKey, CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: '1', ...trap })
      })
    );
  });
});
