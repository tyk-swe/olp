// Gemini CLI, the pinned release, in headless mode (`-p`) against OLP's Gemini
// surface: the configuration docs/clients.md documents. GOOGLE_GEMINI_BASE_URL
// points it at the gateway, and its settings select the Gemini API key as the
// authentication method, which headless mode needs: a base URL alone selects
// the gateway method, which it then refuses. It runs against a transformed
// route and a strict route bound to the gemini-generation profile.
import assert from 'node:assert/strict';
import { mkdir, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { before, beforeEach, describe, test } from 'node:test';
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
import { assertHeldByTheTrap, assertPassedThrough, binary, clientEnvironment, parseJSON, privateHome, run, sentTo, startBait, workspace } from '../../lib/cli.mjs';
import { startTap } from '../../lib/tap.mjs';

const gemini = binary('gemini');

// A 1x1 PNG. An attachment makes the CLI count tokens before it sends a prompt.
const pixel = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==', 'base64');

before(async () => {
  const settings = path.join(privateHome(), '.gemini');
  await mkdir(settings, { recursive: true });
  await writeFile(
    path.join(settings, 'settings.json'),
    JSON.stringify({
      security: { auth: { selectedType: 'gemini-api-key' } },
      telemetry: { enabled: false },
      privacy: { usageStatisticsEnabled: false },
      general: { enableAutoUpdate: false, enableAutoUpdateNotification: false }
    })
  );
});

beforeEach(resetRecorded);

/** Run `gemini -p` against the gateway through a tap. --skip-trust trusts the workspace headless mode needs trusted. */
async function ask(t, prompt, { route, cwd, key = apiKey, timeoutMs } = {}) {
  const tap = await startTap();
  t.after(() => tap.close());
  const result = await run(gemini, ['--skip-trust', '--model', route, '--prompt', prompt, '--output-format', 'json'], {
    cwd: cwd ?? (await workspace('gemini')),
    timeoutMs,
    env: clientEnvironment({ GOOGLE_GEMINI_BASE_URL: tap.baseURLs.gemini, GEMINI_API_KEY: key })
  });
  return { tap, result };
}

const streamFilter = { dialect: 'gemini.stream' };

for (const { name, route } of [
  { name: 'a transformed route', route: models.gemini },
  { name: 'a strict route', route: models.geminiStrict }
]) {
  describe(`Gemini CLI through ${name}`, () => {
    test('streams a reply, and the upstream receives the request unchanged', async (t) => {
      const { tap, result } = await ask(t, 'Say hello.', { route });
      assert.equal(result.code, 0, result.stderr);
      const out = parseJSON(result);
      assert.equal(out.response, defaultReply);
      assert.equal(out.stats.models[route].api.totalErrors, 0);
      assert.ok(out.stats.models[route].tokens.prompt > 0, 'usage came back through the gateway');

      const request = await onlyRequest(streamFilter);
      assert.equal(request.path, `/gemini/v1beta/models/${upstreamModels.gemini}:streamGenerateContent`);
      assert.equal(request.query, 'alt=sse');
      assert.equal(request.headers['x-goog-api-key'], '[present]');
      assert.equal(request.body.contents[0].role, 'user');
      assert.match(JSON.stringify(request.body.contents), /Say hello\./);
      assert.equal(request.body.generationConfig.thinkingConfig.includeThoughts, true);
      assert.ok(request.body.systemInstruction, 'the system instruction arrived');
      assert.ok(request.body.tools[0].functionDeclarations.some((tool) => tool.name === 'read_file'));
      assertPassedThrough(await sentTo(tap, (r) => r.path.endsWith(':streamGenerateContent')), [request], { headers: [] });
    });

    test('calls a built-in tool and returns its result, with the thought signature intact', async (t) => {
      const secret = 'the vault code is marmalade-7';
      const cwd = await workspace('gemini', { 'note.txt': `${secret}\n` });
      const { tap, result } = await ask(t, `Read note.txt ${script.tool('read_file', { file_path: 'note.txt' })}`, { route, cwd });
      assert.equal(result.code, 0, result.stderr);
      const out = parseJSON(result);
      assert.ok(out.response.startsWith(toolResultsPrefix) && out.response.includes(secret), out.response);
      assert.equal(out.stats.tools.byName.read_file.success, 1);
      assert.equal(out.stats.tools.totalFail, 0);

      const requests = assertClean(await recorded(streamFilter));
      assert.deepEqual(requests.map((r) => r.script), ['tool_call', 'tool_result']);
      const contents = requests[1].body.contents;
      assert.deepEqual(contents.map((c) => c.role), ['user', 'model', 'user']);
      const call = contents[1].parts.find((p) => p.functionCall);
      assert.equal(call.functionCall.name, 'read_file');
      assert.deepEqual(call.functionCall.args, { file_path: 'note.txt' });
      assert.equal(call.thoughtSignature, 'fixture-thought-signature', 'the signed call came back as it left the upstream');
      const response = contents[2].parts.find((p) => p.functionResponse);
      assert.equal(response.functionResponse.name, 'read_file');
      assert.match(JSON.stringify(response.functionResponse.response), /marmalade-7/);
      assertPassedThrough(await sentTo(tap, (r) => r.path.endsWith(':streamGenerateContent')), requests, { headers: [] });
    });

    test('counts the tokens of an attachment before it sends the prompt', async (t) => {
      const cwd = await workspace('gemini');
      await writeFile(path.join(cwd, 'pixel.png'), pixel);
      const { tap, result } = await ask(t, 'Describe @pixel.png', { route, cwd });
      assert.equal(result.code, 0, result.stderr);
      assert.equal(parseJSON(result).response, defaultReply);

      const counts = assertClean(await recorded({ dialect: 'gemini.count_tokens' }));
      assert.equal(counts.length, 1);
      assert.equal(counts[0].path, `/gemini/v1beta/models/${upstreamModels.gemini}:countTokens`);
      assert.equal(counts[0].script, 'count_tokens');
      assert.ok(JSON.stringify(counts[0].body.contents).includes('inlineData'), 'the attachment is what is counted');
      const [stream] = assertClean(await recorded(streamFilter));
      assert.ok(JSON.stringify(stream.body.contents).includes('inlineData'), 'the attachment reaches the model');
      assertPassedThrough(await sentTo(tap, (r) => r.path.endsWith(':countTokens')), counts, { headers: [] });
    });
  });
}

describe('Gemini CLI against the gateway', () => {
  test('reports a route the key may not use as the gateway\'s typed error', async (t) => {
    const { result } = await ask(t, 'Say hello.', { route: models.geminiStrict, key: restrictedApiKey });
    assert.notEqual(result.code, 0);
    assert.match(result.stderr, /"code":403/);
    assert.match(result.stderr, /PERMISSION_DENIED/);
    assert.match(result.stderr, /route_forbidden/);
    assert.match(result.stderr, new RegExp(`not allowed to use the model .${models.geminiStrict}.`));
    assert.deepEqual(await recorded(), [], 'a refused request never reaches the upstream');
  });

  test('cannot reach past the gateway', async (t) => {
    const bait = await startBait();
    t.after(() => bait.close());
    const cwd = await workspace('gemini');
    // The client retries through the dead proxy until it is stopped, which is
    // expected; the deadline only bounds how long it may take to start.
    await assertHeldByTheTrap(bait, (trap, signal) =>
      run(gemini, ['--skip-trust', '--model', models.gemini, '--prompt', 'Say hello.', '--output-format', 'json'], {
        cwd,
        timeoutMs: 60_000,
        signal,
        env: clientEnvironment({ GOOGLE_GEMINI_BASE_URL: `${bait.origin}/gemini`, GEMINI_API_KEY: apiKey, ...trap })
      })
    );
  });
});
