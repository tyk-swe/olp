// The harness is the instrument every client suite measures with, so it is
// checked first, without any client: each surface, mode and script the suites
// rely on is exercised with plain HTTP, and both the answer and what the
// upstream received are asserted.
import assert from 'node:assert/strict';
import { beforeEach, describe, test } from 'node:test';
import {
  afterTools,
  apiKey,
  assertClean,
  baseURLs,
  defaultReply,
  localFetch,
  models,
  onlyRequest,
  origin,
  recorded,
  resetRecorded,
  restrictedApiKey,
  script,
  upstreamModels,
  upstreamURL
} from '../../lib/harness.mjs';

beforeEach(resetRecorded);

const bearer = { authorization: `Bearer ${apiKey}` };
const anthropicHeaders = { 'x-api-key': apiKey, 'anthropic-version': '2023-06-01' };
const geminiHeaders = { 'x-goog-api-key': apiKey };

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

/** The data of every server-sent event of a body. */
function events(text) {
  return text
    .replaceAll('\r\n', '\n')
    .split('\n\n')
    .filter(Boolean)
    .map((frame) => {
      const data = frame.split('\n').find((line) => line.startsWith('data: ')).slice('data: '.length);
      return data === '[DONE]' ? data : JSON.parse(data);
    });
}

const weather = {
  type: 'function',
  function: { name: 'get_weather', parameters: { type: 'object', properties: { city: { type: 'string' } } } }
};

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

  test('a tool loop completes and the result reaches the upstream intact', async () => {
    const prompt = `Weather in Paris? ${script.tool('get_weather', { city: 'Paris' })}`;
    const messages = [{ role: 'user', content: prompt }];
    const first = await ok(`${baseURLs.openai}/chat/completions`, { model: models.openai, messages, tools: [weather] });
    const call = first.choices[0].message.tool_calls[0];
    assert.equal(first.choices[0].finish_reason, 'tool_calls');
    assert.equal(call.function.name, 'get_weather');
    assert.deepEqual(JSON.parse(call.function.arguments), { city: 'Paris' });

    messages.push(first.choices[0].message, { role: 'tool', tool_call_id: call.id, content: 'sunny' });
    const final = await ok(`${baseURLs.openai}/chat/completions`, { model: models.openai, messages, tools: [weather] });
    assert.equal(final.choices[0].message.content, afterTools('sunny'));

    const requests = assertClean(await recorded());
    assert.deepEqual(requests.map((r) => r.script), ['tool_call', 'tool_result']);
    assert.deepEqual(requests[1].body.messages.at(-1), { role: 'tool', tool_call_id: call.id, content: 'sunny' });
  });

  test('a stream assembles to the same text', async () => {
    const { response, text } = await post(`${baseURLs.openai}/chat/completions`, {
      model: models.openai,
      stream: true,
      stream_options: { include_usage: true },
      messages: [{ role: 'user', content: script.reply('alpha beta gamma') }]
    });
    assert.equal(response.status, 200);
    assert.match(response.headers.get('content-type'), /text\/event-stream/);
    const chunks = events(text);
    assert.equal(chunks.at(-1), '[DONE]');
    const streamed = chunks.filter((c) => c !== '[DONE]').map((c) => c.choices[0]?.delta?.content ?? '').join('');
    assert.equal(streamed, 'alpha beta gamma');
    assert.ok(chunks.some((c) => c.usage?.total_tokens > 0), 'usage chunk');
    assert.equal((await onlyRequest()).stream, true);
  });

  test('Responses carries reasoning and a function call, statelessly', async () => {
    const tool = { type: 'function', name: 'get_weather', parameters: { type: 'object', properties: { city: { type: 'string' } } } };
    const body = await ok(`${baseURLs.openai}/responses`, {
      model: models.openai,
      input: `Weather? ${script.tool('get_weather', { city: 'Paris' })}`,
      tools: [tool],
      store: false,
      reasoning: { effort: 'low', summary: 'auto' },
      include: ['reasoning.encrypted_content']
    });
    assert.deepEqual(body.output.map((item) => item.type), ['reasoning', 'function_call']);
    assert.equal(body.output[0].encrypted_content, 'fixture-encrypted-reasoning');
    assert.deepEqual(JSON.parse(body.output[1].arguments), { city: 'Paris' });

    const request = await onlyRequest();
    assert.equal(request.path, '/openai/v1/responses');
    assert.equal(request.body.model, upstreamModels.openai);
    assert.equal(request.body.store, false);
  });

  test('embeddings arrive as floats or base64', async () => {
    const floats = await ok(`${baseURLs.openai}/embeddings`, { model: models.openai, input: ['alpha', 'beta'], dimensions: 4 });
    assert.equal(floats.data.length, 2);
    assert.equal(floats.data[0].embedding.length, 4);
    const base64 = await ok(`${baseURLs.openai}/embeddings`, { model: models.openai, input: 'alpha', dimensions: 4, encoding_format: 'base64' });
    const bytes = Buffer.from(base64.data[0].embedding, 'base64');
    assert.equal(bytes.length, 16);
    for (let i = 0; i < 4; i++) assert.ok(Math.abs(bytes.readFloatLE(4 * i) - floats.data[0].embedding[i]) < 1e-6);
    assertClean(await recorded({ dialect: 'openai.embeddings' }));
  });

  test('embeddings reach a Gemini upstream by translation', async () => {
    const body = await ok(`${baseURLs.openai}/embeddings`, { model: models.gemini, input: 'alpha', dimensions: 4 });
    assert.equal(body.data[0].embedding.length, 4);
    const request = await onlyRequest();
    assert.equal(request.path, `/gemini/v1beta/models/${upstreamModels.gemini}:embedContent`);
  });

  test('strict routes preserve the native invocation', async () => {
    const body = await ok(`${baseURLs.openai}/chat/completions`, {
      model: models.openaiStrict,
      messages: [{ role: 'user', content: 'hi' }],
      vendor_extension: { kept: true }
    });
    assert.equal(body.choices[0].message.content, defaultReply);
    const request = await onlyRequest();
    assert.deepEqual(request.body.vendor_extension, { kept: true });
    assert.equal(request.body.model, upstreamModels.openai);
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

  test('prompt caching writes then reads', async () => {
    const first = await ok(`${baseURLs.anthropic}/v1/messages`, message(), anthropicHeaders);
    assert.equal(first.content[0].text, defaultReply);
    assert.equal(first.model, models.anthropic);
    assert.ok(first.usage.cache_creation_input_tokens > 0);
    assert.equal(first.usage.cache_read_input_tokens, 0);
    const second = await ok(`${baseURLs.anthropic}/v1/messages`, message({ messages: [{ role: 'user', content: 'Another question.' }] }), anthropicHeaders);
    assert.equal(second.usage.cache_creation_input_tokens, 0);
    assert.equal(second.usage.cache_read_input_tokens, first.usage.cache_creation_input_tokens);

    const requests = assertClean(await recorded());
    assert.equal(requests[0].path, '/anthropic/v1/messages');
    assert.equal(requests[0].body.model, upstreamModels.anthropic);
    assert.equal(requests[0].headers['x-api-key'], '[present]');
    assert.equal(requests[0].body.system[0].cache_control.type, 'ephemeral', 'cache_control survives the gateway');
  });

  test('the anthropic-beta header reaches the upstream on a strict route', async () => {
    const betas = 'interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14';
    await ok(`${baseURLs.anthropic}/v1/messages`, message({ model: models.anthropicStrict }), { ...anthropicHeaders, 'anthropic-beta': betas });
    const request = await onlyRequest();
    assert.equal(request.headers['anthropic-beta'], betas);
    assert.equal(request.headers['anthropic-version'], '2023-06-01');
  });

  test('a tool loop with thinking, streamed', async () => {
    const tool = { name: 'get_weather', input_schema: { type: 'object', properties: { city: { type: 'string' } } } };
    const prompt = `Weather? ${script.tool('get_weather', { city: 'Paris' })}`;
    const { response, text } = await post(
      `${baseURLs.anthropic}/v1/messages`,
      { model: models.anthropic, max_tokens: 64, stream: true, thinking: { type: 'enabled', budget_tokens: 32 }, tools: [tool], messages: [{ role: 'user', content: prompt }] },
      anthropicHeaders
    );
    assert.equal(response.status, 200, text);
    const stream = events(text);
    assert.equal(stream[0].type, 'message_start');
    assert.equal(stream.at(-1).type, 'message_stop');
    assert.equal(stream.find((e) => e.type === 'message_delta').delta.stop_reason, 'tool_use');
    const start = stream.find((e) => e.content_block?.type === 'tool_use');
    const input = stream.filter((e) => e.delta?.type === 'input_json_delta').map((e) => e.delta.partial_json).join('');
    assert.deepEqual(JSON.parse(input), { city: 'Paris' });
    const signature = stream.find((e) => e.delta?.type === 'signature_delta').delta.signature;

    const final = await ok(
      `${baseURLs.anthropic}/v1/messages`,
      {
        model: models.anthropic,
        max_tokens: 64,
        thinking: { type: 'enabled', budget_tokens: 32 },
        tools: [tool],
        messages: [
          { role: 'user', content: prompt },
          { role: 'assistant', content: [{ type: 'thinking', thinking: 'Fixture reasoning: the reply is scripted.', signature }, { type: 'tool_use', id: start.content_block.id, name: 'get_weather', input: { city: 'Paris' } }] },
          { role: 'user', content: [{ type: 'tool_result', tool_use_id: start.content_block.id, content: 'sunny' }] }
        ]
      },
      anthropicHeaders
    );
    assert.equal(final.content.at(-1).text, afterTools('sunny'));
    assertClean(await recorded());
  });

  test('count_tokens answers natively', async () => {
    const body = await ok(`${baseURLs.anthropic}/v1/messages/count_tokens`, { model: models.anthropic, messages: [{ role: 'user', content: 'hello' }] }, anthropicHeaders);
    assert.ok(body.input_tokens > 0);
    const request = await onlyRequest();
    assert.equal(request.path, '/anthropic/v1/messages/count_tokens');
    assert.equal(request.body.model, upstreamModels.anthropic);
  });

  test('a scripted upstream failure is a typed Anthropic error', async () => {
    const { response, text } = await post(`${baseURLs.anthropic}/v1/messages`, message({ messages: [{ role: 'user', content: script.fail(500) }] }), anthropicHeaders);
    assert.equal(response.status, 502, text);
    assert.equal(JSON.parse(text).type, 'error');
    assertClean(await recorded(), { status: 500 });
  });

  test('a request in the Anthropic dialect translates to an OpenAI upstream', async () => {
    const body = await ok(`${baseURLs.anthropic}/v1/messages`, { model: models.openai, max_tokens: 64, messages: [{ role: 'user', content: 'Say hello.' }] }, anthropicHeaders);
    assert.equal(body.content[0].text, defaultReply);
    assert.equal(body.model, models.openai);
    const request = await onlyRequest();
    assert.equal(request.path, '/openai/v1/chat/completions');
    assert.equal(request.body.model, upstreamModels.openai);
  });
});

describe('Gemini surface', () => {
  const generate = (action, extra = '') => `${baseURLs.gemini}/v1beta/models/${models.gemini}:${action}${extra}`;
  const declaration = { functionDeclarations: [{ name: 'get_weather', parameters: { type: 'OBJECT', properties: { city: { type: 'STRING' } } } }] };
  const contents = (text) => [{ role: 'user', parts: [{ text }] }];

  test('function calling streams and completes', async () => {
    const prompt = `Weather? ${script.tool('get_weather', { city: 'Paris' })}`;
    const { response, text } = await post(generate('streamGenerateContent', '?alt=sse'), { contents: contents(prompt), tools: [declaration] }, geminiHeaders);
    assert.equal(response.status, 200, text);
    const parts = events(text).flatMap((chunk) => chunk.candidates[0].content.parts);
    assert.deepEqual(parts.find((p) => p.functionCall).functionCall, { name: 'get_weather', args: { city: 'Paris' } });

    const final = await ok(
      generate('generateContent'),
      {
        contents: [
          ...contents(prompt),
          { role: 'model', parts: [{ functionCall: { name: 'get_weather', args: { city: 'Paris' } } }] },
          { role: 'user', parts: [{ functionResponse: { name: 'get_weather', response: { output: 'sunny' } } }] }
        ],
        tools: [declaration]
      },
      geminiHeaders
    );
    assert.equal(final.candidates[0].content.parts[0].text, afterTools('sunny'));
    const requests = assertClean(await recorded());
    assert.deepEqual(requests.map((r) => r.dialect), ['gemini.stream', 'gemini.generate']);
    assert.equal(requests[0].path, `/gemini/v1beta/models/${upstreamModels.gemini}:streamGenerateContent`);
    assert.equal(requests[0].query, 'alt=sse');
  });

  test('countTokens answers natively', async () => {
    const body = await ok(generate('countTokens'), { contents: contents('hello') }, geminiHeaders);
    assert.ok(body.totalTokens > 0);
    assert.equal((await onlyRequest()).dialect, 'gemini.count_tokens');
  });

  test('native embedContent and batchEmbedContents are served on the strict embedding route', async () => {
    const url = (action) => `${baseURLs.gemini}/v1beta/models/${models.geminiEmbedStrict}:${action}`;
    const content = (text) => ({ content: { parts: [{ text }] }, outputDimensionality: 4 });
    const single = await ok(url('embedContent'), content('alpha'), geminiHeaders);
    assert.equal(single.embedding.values.length, 4);
    const batch = await ok(
      url('batchEmbedContents'),
      { requests: ['alpha', 'beta'].map((text) => ({ model: `models/${models.geminiEmbedStrict}`, ...content(text) })) },
      geminiHeaders
    );
    assert.deepEqual(batch.embeddings[0].values, single.embedding.values);
    const requests = assertClean(await recorded());
    assert.deepEqual(requests.map((r) => r.dialect), ['gemini.embed', 'gemini.batch_embed']);
    assert.ok(requests.every((r) => r.model === upstreamModels.gemini));
  });

  test('a request in the Gemini dialect translates to an Anthropic upstream', async () => {
    const body = await ok(
      `${baseURLs.gemini}/v1beta/models/${models.anthropic}:generateContent`,
      { contents: contents('Say hello.'), generationConfig: { maxOutputTokens: 64 } },
      geminiHeaders
    );
    assert.equal(body.candidates[0].content.parts[0].text, defaultReply);
    const request = await onlyRequest();
    assert.equal(request.path, '/anthropic/v1/messages');
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
