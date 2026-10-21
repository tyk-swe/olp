// The harness is the instrument every client suite measures with, so it is
// checked first, without any client: each surface, mode and script the suites
// rely on is exercised with plain HTTP, and both the answer and what the
// upstream received are asserted.
import assert from 'node:assert/strict';
import { beforeEach, describe, test } from 'node:test';
import { apiKey, assertClean, baseURLs, defaultReply, localFetch, models, onlyRequest, origin, recorded, resetRecorded, restrictedApiKey, script, upstreamModels, upstreamURL } from '../../lib/harness.mjs';

beforeEach(resetRecorded);

const bearer = { authorization: `Bearer ${apiKey}` };
const anthropicHeaders = { 'x-api-key': apiKey, 'anthropic-version': '2023-06-01' };

async function post(url, body, headers = bearer) {
  const response = await localFetch(url, {
    method: 'POST',
    headers: { 'content-type': 'application/json', ...headers },
    body: JSON.stringify(body)
  });
  return { response, text: await response.text() };
}

async function ok(url, body, headers) {
  const { response, text } = await post(url, body, headers);
  assert.equal(response.status, 200, text);
  return JSON.parse(text);
}

describe('keys and routes', () => {
  test('every published route is listed for the key', async () => {
    const response = await localFetch(`${baseURLs.openai}/models`, { headers: bearer });
    assert.equal(response.status, 200);
    const listed = (await response.json()).data.map((model) => model.id);
    for (const slug of Object.values(models)) assert.ok(listed.includes(slug), `${slug} is not published`);
  });

  test('an unknown key is 401 and a restricted key is 403, and neither reaches the upstream', async () => {
    const body = { model: models.openai, messages: [{ role: 'user', content: 'hi' }] };
    const unknown = await post(`${baseURLs.openai}/chat/completions`, body, { authorization: 'Bearer olp_unknown' });
    assert.equal(unknown.response.status, 401);
    assert.equal(JSON.parse(unknown.text).error.code, 'invalid_api_key');
    const restricted = await post(`${baseURLs.openai}/chat/completions`, body, { authorization: `Bearer ${restrictedApiKey}` });
    assert.equal(restricted.response.status, 403);
    assert.equal(JSON.parse(restricted.text).error.code, 'route_forbidden');
    assert.deepEqual(await recorded(), []);
  });
});

describe('OpenAI surface', () => {
  test('the gateway rewrites the model and swaps the credential', async () => {
    const body = await ok(
      `${baseURLs.openai}/chat/completions`,
      { model: models.openai, messages: [{ role: 'user', content: 'Say hello.' }] },
      { ...bearer, 'x-olp-routing': '{"strategy":"weighted"}' }
    );
    assert.equal(body.model, models.openai);
    assert.equal(body.choices[0].message.content, defaultReply);
    const request = await onlyRequest();
    assert.equal(request.path, '/openai/v1/chat/completions');
    assert.equal(request.body.model, upstreamModels.openai);
    assert.equal(request.headers.authorization, '[present]');
    assert.equal(request.headers['x-api-key'], undefined);
    assert.equal(request.headers['x-olp-routing'], undefined, 'gateway controls must not reach the provider');
  });
});

describe('Anthropic surface', () => {
  const message = (extra = {}) => ({
    model: models.anthropic,
    max_tokens: 64,
    system: [{ type: 'text', text: 'shared project context. '.repeat(40), cache_control: { type: 'ephemeral' } }],
    messages: [{ role: 'user', content: 'Say hello.' }],
    ...extra
  });

  test('a scripted upstream failure is a typed Anthropic error', async () => {
    const { response, text } = await post(`${baseURLs.anthropic}/v1/messages`, message({ messages: [{ role: 'user', content: script.fail(500) }] }), anthropicHeaders);
    assert.equal(response.status, 502, text);
    assert.equal(JSON.parse(text).type, 'error');
    assertClean(await recorded(), { status: 500 });
  });
});

describe('isolation', () => {
  test('a suite sees a private home and none of the developer credentials', () => {
    const scratch = process.env.OLP_CLIENTS_SCRATCH;
    assert.ok(scratch, 'OLP_CLIENTS_SCRATCH is set by run.sh');
    for (const name of ['HOME', 'XDG_CONFIG_HOME', 'XDG_DATA_HOME', 'XDG_CACHE_HOME', 'XDG_STATE_HOME', 'TMPDIR'])
      assert.ok(process.env[name].startsWith(`${scratch}/`), `${name} is outside the suite scratch directory`);
    for (const name of ['OPENAI_API_KEY', 'ANTHROPIC_API_KEY', 'ANTHROPIC_AUTH_TOKEN', 'GEMINI_API_KEY', 'GOOGLE_API_KEY', 'OPENAI_BASE_URL', 'ANTHROPIC_BASE_URL'])
      assert.equal(process.env[name], undefined, `${name} leaked into the suite`);
  });

  test('a client that bypasses OLP cannot reach the network', async () => {
    for (const target of ['https://api.openai.com/v1/models', 'https://api.anthropic.com/v1/models', 'http://example.com/']) {
      await assert.rejects(fetch(target, { signal: AbortSignal.timeout(5000) }), `${target} must be unreachable`);
    }
    // Loopback, where the gateway lives, is not proxied.
    assert.equal((await fetch(`${origin}/v1/models`, { headers: bearer })).status, 200);
  });
});

describe('recording', () => {
  test('is bounded to the gateway: the origin is loopback and DELETE resets', async () => {
    assert.match(origin, /^http:\/\/127\.0\.0\.1:\d+$/);
    await ok(`${baseURLs.openai}/chat/completions`, { model: models.openai, messages: [{ role: 'user', content: 'hi' }] });
    assert.equal((await recorded()).length, 1);
    await resetRecorded();
    assert.deepEqual(await recorded(), []);
  });

  // Every suite asserts that no caller credential reached the upstream, which
  // holds only if the recording flags one that did. The requests go to the
  // upstream directly, with the credentials a client holds, so each is refused.
  test('flags each key of a caller wherever the upstream received it', async () => {
    const generate = `${upstreamURL}/gemini/v1beta/models/${upstreamModels.gemini}:generateContent`;
    const contents = (text) => JSON.stringify({ contents: [{ role: 'user', parts: [{ text }] }] });
    for (const [name, key] of [['the key', apiKey], ['the restricted key', restrictedApiKey]]) {
      const carriers = {
        'an authorization header': [`${upstreamURL}/openai/v1/chat/completions`, { authorization: `Bearer ${key}` }, '{}'],
        'an api key header': [generate, { 'x-goog-api-key': key }, contents('hi')],
        'the query string': [`${generate}?key=${key}`, {}, contents('hi')],
        'the body': [generate, {}, contents(`my key is ${key}`)]
      };
      for (const [carrier, [url, headers, body]] of Object.entries(carriers)) {
        await resetRecorded();
        const response = await fetch(url, { method: 'POST', headers: { 'content-type': 'application/json', ...headers }, body });
        assert.equal(response.status, 401, `${name} in ${carrier}`);
        const requests = await recorded();
        assert.equal(requests.length, 1, `${name} in ${carrier}`);
        assert.equal(requests[0].authorized, false, `${name} in ${carrier}`);
        assert.equal(requests[0].leaked_client_credential, true, `${name} in ${carrier} is not flagged`);
      }
    }
  });

  test('does not flag a request that carries no key of a caller', async () => {
    const response = await fetch(`${upstreamURL}/openai/v1/chat/completions?key=olp_unknown`, {
      method: 'POST',
      headers: { 'content-type': 'application/json', authorization: 'Bearer olp_unknown' },
      body: '{}'
    });
    assert.equal(response.status, 401);
    const requests = await recorded();
    assert.equal(requests.length, 1);
    assert.equal(requests[0].leaked_client_credential, false);
  });
});
