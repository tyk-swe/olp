// The tap and the relay assertions are what the framework suites measure with:
// four of them rest their "the body arrived unchanged apart from the model"
// guarantee on them. A comparison that passes whatever it is given would leave
// every one of those suites green however the gateway changed a request, so the
// instruments are checked here, first with a real tap in front of the real
// gateway, then with records built by hand, each altered in the one way a
// regression would alter it.
import assert from 'node:assert/strict';
import { setTimeout as sleep } from 'node:timers/promises';
import { beforeEach, describe, test } from 'node:test';
import { gzipSync } from 'node:zlib';
import { apiKey, baseURLs, localFetch, models, recorded, resetRecorded, script, upstreamModels } from '../../lib/harness.mjs';
import {
  assertJSONSchemaTypes,
  assertRelayed,
  assertSameVector,
  declaredForUpstream,
  differences,
  generationParameters,
  images,
  namedModels,
  png,
  referenceEmbeddings,
  textOf,
  toolDeclarations,
  upstreamModelOf
} from '../../lib/relay.mjs';
import { sseData, startTap } from '../../lib/tap.mjs';

beforeEach(resetRecorded);

const bearer = { authorization: `Bearer ${apiKey}` };

/** A tap that closes with the test. */
async function tapFor(t) {
  const tap = await startTap();
  t.after(() => tap.close());
  return tap;
}

/** POST JSON to a URL through the tap, as a client would. */
async function post(url, body, headers = bearer) {
  const response = await fetch(url, { method: 'POST', headers: { 'content-type': 'application/json', ...headers }, body: typeof body === 'string' ? body : JSON.stringify(body) });
  return { response, text: await response.text() };
}

describe('differences', () => {
  test('is empty for equal values, whatever the order of their keys', () => {
    assert.deepEqual(differences({ a: 1, b: { c: [1, 2] } }, { b: { c: [1, 2] }, a: 1 }), []);
    assert.deepEqual(differences([], []), []);
    assert.deepEqual(differences(null, null), []);
    assert.deepEqual(differences('x', 'x'), []);
  });

  test('names every changed, added and dropped member by its pointer', () => {
    assert.deepEqual(differences({ a: 1 }, { a: 2 }), ['/a: 1 -> 2']);
    assert.deepEqual(differences({ a: 1 }, { a: 1, b: 3 }), ['/b: absent -> 3']);
    assert.deepEqual(differences({ a: 1, c: 4 }, { a: 1 }), ['/c: 4 -> absent']);
    assert.deepEqual(differences({ x: [{ y: 1 }, { y: 2 }] }, { x: [{ y: 1 }, { y: 9 }] }), ['/x/1/y: 2 -> 9']);
    assert.deepEqual(differences({ a: 1, b: 2 }, { a: 2, b: 3 }).sort(), ['/a: 1 -> 2', '/b: 2 -> 3']);
  });

  test('sees a change of type, of null, and of array length', () => {
    assert.deepEqual(differences({ a: 1 }, { a: '1' }), ['/a: 1 -> "1"']);
    assert.deepEqual(differences({ a: null }, { a: 0 }), ['/a: null -> 0']);
    assert.deepEqual(differences({ a: null }, {}), ['/a: null -> absent']);
    assert.deepEqual(differences({ a: [1] }, { a: { 0: 1 } }), ['/a: [1] -> {"0":1}']);
    assert.deepEqual(differences({ a: [1, 2] }, { a: [1] }), ['/a/1: 2 -> absent']);
    assert.deepEqual(differences({ a: [1] }, { a: [1, 2] }), ['/a/1: absent -> 2']);
    assert.deepEqual(differences({ a: true }, { a: false }), ['/a: true -> false']);
  });
});

describe('the tap', () => {
  test('relays a request and its answer unchanged, and records both', async (t) => {
    const tap = await tapFor(t);
    const body = { model: models.openai, messages: [{ role: 'user', content: 'Say hello.' }] };
    const { response, text } = await post(`${tap.baseURLs.openai}/chat/completions`, body);
    assert.equal(response.status, 200);
    assert.equal(JSON.parse(text).model, models.openai, 'the client is answered by the gateway');

    const [exchange, ...rest] = await tap.sent();
    assert.deepEqual(rest, []);
    assert.equal(exchange.method, 'POST');
    assert.equal(exchange.path, '/v1/chat/completions');
    assert.equal(exchange.query, '');
    assert.equal(exchange.headers.authorization, bearer.authorization, 'the client credential is recorded as sent');
    assert.equal(exchange.bodyText, JSON.stringify(body));
    assert.deepEqual(exchange.body, body);
    assert.equal(exchange.status, 200);
    assert.ok(exchange.responseHeaders['x-request-id'], 'the gateway answered with its request id');
    assert.equal(exchange.responseHeaders['content-type'], response.headers.get('content-type'));
    assert.equal(exchange.responseText, text, 'the answer is the one the client read');
    assert.equal(exchange.error, undefined);
    assert.equal((await recorded()).length, 1, 'the request reached the upstream through the gateway');
  });

  test('keeps the query, relays a stream whole and parses its events', async (t) => {
    const tap = await tapFor(t);
    const url = `${tap.baseURLs.gemini}/v1beta/models/${models.gemini}:streamGenerateContent?alt=sse`;
    const { response, text } = await post(url, { contents: [{ role: 'user', parts: [{ text: script.reply('alpha beta gamma') }] }] }, { 'x-goog-api-key': apiKey });
    assert.equal(response.status, 200);
    assert.match(response.headers.get('content-type'), /text\/event-stream/);

    const [exchange] = await tap.sent();
    assert.equal(exchange.path, `/gemini/v1beta/models/${models.gemini}:streamGenerateContent`);
    assert.equal(exchange.query, 'alt=sse');
    assert.equal(exchange.responseText, text, 'the stream is the one the client read');
    const streamed = sseData(exchange.responseText).flatMap((chunk) => (chunk.candidates?.[0]?.content?.parts ?? []).map((part) => part.text ?? ''));
    assert.equal(streamed.join(''), 'alpha beta gamma');
  });

  test('lists exchanges in the order they arrived, a refused one included, and forgets them on reset', async (t) => {
    const tap = await tapFor(t);
    const chat = (content) => ({ model: models.openai, messages: [{ role: 'user', content }] });
    await post(`${tap.baseURLs.openai}/chat/completions`, chat('first'));
    const refused = await post(`${tap.baseURLs.openai}/chat/completions`, chat('second'), { authorization: 'Bearer olp_unknown' });
    assert.equal(refused.response.status, 401);
    await post(`${tap.baseURLs.openai}/chat/completions`, chat('third'));

    const exchanges = await tap.sent();
    assert.deepEqual(exchanges.map((e) => e.body.messages[0].content), ['first', 'second', 'third']);
    assert.deepEqual(exchanges.map((e) => e.status), [200, 401, 200]);
    assert.deepEqual(
      (await recorded()).map((r) => r.body.messages[0].content),
      ['first', 'third'],
      'the upstream recorded the requests the gateway admitted, in the same order'
    );

    tap.reset();
    assert.deepEqual(await tap.sent(), []);
  });

  test('keeps the text of a body that is not JSON or is compressed, and no parsed body', async (t) => {
    const tap = await tapFor(t);
    const malformed = await post(`${tap.baseURLs.openai}/chat/completions`, '{"model":');
    assert.equal(malformed.response.status, 400);
    const compressed = await fetch(`${tap.baseURLs.openai}/chat/completions`, {
      method: 'POST',
      headers: { 'content-type': 'application/json', 'content-encoding': 'gzip', ...bearer },
      body: gzipSync(JSON.stringify({ model: models.openai, messages: [{ role: 'user', content: 'hi' }] }))
    });
    assert.equal(compressed.status, 200, 'the gateway accepts a compressed body');
    await compressed.arrayBuffer();

    const [first, second] = await tap.sent();
    assert.equal(first.bodyText, '{"model":');
    assert.equal(first.body, undefined);
    assert.equal(first.status, 400);
    assert.equal(second.body, undefined, 'a compressed body is not parsed');
    assert.notEqual(second.bodyText, '');
  });

  describe('a request still in flight', () => {
    /**
     * Send half of a request body through the tap and hold the rest, so the
     * gateway waits for it. `finish` sends the rest. The sleep lets the tap, which
     * runs in this process, take the request.
     */
    async function held(tap) {
      const text = JSON.stringify({ model: models.openai, messages: [{ role: 'user', content: 'held' }] });
      const encoder = new TextEncoder();
      const half = Math.floor(text.length / 2);
      let finish;
      const body = new ReadableStream({
        start(controller) {
          controller.enqueue(encoder.encode(text.slice(0, half)));
          finish = () => {
            controller.enqueue(encoder.encode(text.slice(half)));
            controller.close();
          };
        }
      });
      const answer = fetch(`${tap.baseURLs.openai}/chat/completions`, { method: 'POST', headers: { 'content-type': 'application/json', ...bearer }, body, duplex: 'half' }).then((response) => response.text());
      await sleep(150);
      return { text, finish, answer };
    }

    test('is waited for, so that a stream the client has read is complete when it is listed', async (t) => {
      const tap = await tapFor(t);
      const request = await held(tap);
      const listed = tap.sent();
      await sleep(100);
      request.finish();
      const [exchange] = await listed;
      assert.equal(exchange.status, 200, 'the exchange was listed once it was answered');
      assert.equal(exchange.bodyText, request.text);
      assert.equal(exchange.responseText, await request.answer);
    });

    test('is not waited for forever', async (t) => {
      const tap = await tapFor(t);
      const request = await held(tap);
      await assert.rejects(tap.sent(), /client requests are still in flight/);
      request.finish();
      await request.answer;
      assert.equal((await tap.sent())[0].status, 200);
    });
  });

  test('closes: nothing answers on its origin afterwards', async () => {
    const tap = await startTap();
    await tap.close();
    await assert.rejects(fetch(`${tap.origin}/v1/models`, { headers: bearer }));
  });
});

describe('sseData', () => {
  test('is the JSON of every data line, without the end marker, comments or other fields', () => {
    const text = ': keep-alive\r\n\r\nevent: message\r\ndata: {"a":1}\r\n\r\ndata:{"b":2}\n\nid: 7\n\ndata: [DONE]\n\n';
    assert.deepEqual(sseData(text), [{ a: 1 }, { b: 2 }]);
    assert.deepEqual(sseData(''), []);
  });
});

describe('assertRelayed with a real tap and the real gateway', () => {
  test('accepts a request that keeps its dialect, unary and streamed, and one that is translated', async (t) => {
    const tap = await tapFor(t);
    const chat = (extra) => ({ model: models.openai, messages: [{ role: 'user', content: 'Say hello.' }], max_tokens: 16, temperature: 0.5, ...extra });
    await post(`${tap.baseURLs.openai}/chat/completions`, chat());
    await post(`${tap.baseURLs.openai}/chat/completions`, chat({ stream: true }));
    await post(`${tap.baseURLs.anthropic}/v1/messages`, { model: models.openai, max_tokens: 32, messages: [{ role: 'user', content: 'Say hello.' }] }, { 'x-api-key': apiKey, 'anthropic-version': '2023-06-01' });
    const { sent, upstream } = await assertRelayed(tap, [
      { api: 'openai.chat', model: models.openai, maxTokens: 16, temperature: 0.5, contains: ['Say hello.'] },
      { api: 'openai.chat', model: models.openai, stream: true },
      { api: 'anthropic.messages', to: 'openai.chat', model: models.openai, maxTokens: 32 }
    ]);
    assert.equal(sent.length, 3);
    assert.equal(upstream[0].model, upstreamModels.openai);
  });

  test('rejects what a gateway that changed the request would have sent', async (t) => {
    const tap = await tapFor(t);
    await post(`${tap.baseURLs.openai}/chat/completions`, { model: models.openai, messages: [{ role: 'user', content: 'Say hello.' }], max_tokens: 16 });
    const upstream = await recorded();
    const expected = [{ api: 'openai.chat', model: models.openai }];
    await assertRelayed(tap, expected, { upstream });
    const changed = (change) => upstream.map((u) => ({ ...u, body: change(structuredClone(u.body)) }));
    await assert.rejects(assertRelayed(tap, expected, { upstream: changed((b) => ({ ...b, max_tokens: 17 })) }), /the gateway changed the request/);
    await assert.rejects(assertRelayed(tap, expected, { upstream: changed((b) => ({ ...b, stream_options: { include_obfuscation: false } })) }), /the gateway changed the request/);
    await assert.rejects(assertRelayed(tap, expected, { upstream: changed(({ max_tokens, ...b }) => b) }), /the gateway changed the request/);
    await assert.rejects(assertRelayed(tap, expected, { upstream: changed((b) => ({ ...b, model: models.openai })) }), /the gateway changed the request/);
  });
});

// A client and the upstream as the relay assertions see them, built by hand.
const chatBody = { model: models.openai, messages: [{ role: 'user', content: 'Say hello.' }], max_tokens: 16, temperature: 0.5 };

const clientRequest = (extra = {}) => ({
  method: 'POST',
  path: '/v1/chat/completions',
  query: '',
  headers: { authorization: `Bearer ${apiKey}` },
  bodyText: JSON.stringify(chatBody),
  body: chatBody,
  status: 200,
  responseHeaders: { 'x-request-id': 'request-1', 'content-type': 'application/json' },
  responseText: '{}',
  ...extra
});

const upstreamRequest = (extra = {}) => ({
  dialect: 'openai.chat',
  method: 'POST',
  path: '/openai/v1/chat/completions',
  query: '',
  model: upstreamModels.openai,
  headers: { authorization: '[present]' },
  body: { ...chatBody, model: upstreamModels.openai },
  authorized: true,
  leaked_client_credential: false,
  status: 200,
  stream: false,
  ...extra
});

/** The assertion over a tap that saw `sent` and an upstream that recorded `upstream`. */
const relayed = (sent, upstream, expected) => assertRelayed({ sent: async () => sent }, expected, { upstream });
const chat = { api: 'openai.chat', model: models.openai };

describe('assertRelayed with records built by hand', () => {
  test('accepts a request relayed as it should be', async () => {
    const result = await relayed([clientRequest()], [upstreamRequest()], [chat]);
    assert.equal(result.sent.length, 1);
    assert.equal(result.upstream.length, 1);
  });

  test('accepts the transport option the gateway adds to a streamed chat request, and nothing else', async () => {
    const streamed = { ...chatBody, stream: true };
    const sent = clientRequest({ body: streamed, responseHeaders: { 'x-request-id': 'r', 'content-type': 'text/event-stream' } });
    const optioned = { ...streamed, model: upstreamModels.openai, stream_options: { include_usage: true } };
    await relayed([sent], [upstreamRequest({ body: optioned, stream: true })], [{ ...chat, stream: true }]);
    const other = { ...optioned, stream_options: { include_usage: true, include_obfuscation: false } };
    await assert.rejects(relayed([sent], [upstreamRequest({ body: other, stream: true })], [{ ...chat, stream: true }]), /the gateway changed the request/);
    // The same option on a request that does not stream is a change.
    await assert.rejects(relayed([clientRequest()], [upstreamRequest({ body: { ...upstreamRequest().body, stream_options: { include_usage: true } } })], [chat]), /the gateway changed the request/);
  });

  test('rejects a body the gateway altered, extended, shortened or left with the route slug', async () => {
    const body = upstreamRequest().body;
    for (const [what, altered] of [
      ['an altered field', { ...body, temperature: 0.6 }],
      ['an altered nested field', { ...body, messages: [{ role: 'user', content: 'Say goodbye.' }] }],
      ['an added field', { ...body, user: 'someone' }],
      ['a dropped field', (({ temperature, ...rest }) => rest)(body)],
      ['a reordered array', { ...body, messages: [{ role: 'system', content: 's' }, ...body.messages] }],
      ['the route slug left in place of the model', { ...body, model: models.openai }]
    ]) {
      await assert.rejects(relayed([clientRequest()], [upstreamRequest({ body: altered })], [chat]), /the gateway changed the request/, what);
    }
  });

  test('rejects a request that went to the wrong place or under the wrong credentials', async () => {
    const reject = (sent, upstream, pattern, expected = [chat]) => assert.rejects(relayed([sent], [upstream], expected), pattern);
    await reject(clientRequest({ path: '/v1/responses' }), upstreamRequest(), /client path/);
    await reject(clientRequest({ method: 'PUT' }), upstreamRequest(), /request 1/);
    await reject(clientRequest({ query: 'alt=sse' }), upstreamRequest(), /client query/);
    await reject(clientRequest({ headers: {} }), upstreamRequest(), /authenticates with the OLP key/);
    await reject(clientRequest({ headers: { authorization: 'Bearer another' } }), upstreamRequest(), /authenticates with the OLP key/);
    await reject(clientRequest({ headers: { 'x-api-key': apiKey } }), upstreamRequest(), /authenticates with the OLP key/);
    await reject(clientRequest(), upstreamRequest({ path: '/openai/v1/responses' }), /upstream path/);
    await reject(clientRequest(), upstreamRequest({ dialect: 'openai.responses' }), /upstream dialect/);
    await reject(clientRequest(), upstreamRequest({ model: 'another-model' }), /upstream model/);
    await reject(clientRequest(), upstreamRequest({ authorized: false }), /accepted the gateway's credential/);
    await reject(clientRequest(), upstreamRequest({ leaked_client_credential: true }), /no caller credential reaches the upstream/);
    await reject(clientRequest(), upstreamRequest({ headers: {} }), /upstream credential header/);
    await reject(clientRequest(), upstreamRequest({ headers: { authorization: 'Bearer ' + apiKey } }), /upstream credential header/);
    await reject(clientRequest(), upstreamRequest({ status: 500 }), /upstream status/);
    await reject(clientRequest({ status: 502 }), upstreamRequest(), /client status/);
    await reject(clientRequest({ responseHeaders: { 'content-type': 'application/json' } }), upstreamRequest(), /request id/);
  });

  test('holds the framing of the answer and of the upstream request to what the test expects', async () => {
    const event = { 'x-request-id': 'r', 'content-type': 'text/event-stream' };
    await assert.rejects(relayed([clientRequest({ responseHeaders: event })], [upstreamRequest()], [chat]), /response framing/);
    await assert.rejects(relayed([clientRequest()], [upstreamRequest({ stream: true })], [chat]), /upstream streaming/);
    await assert.rejects(relayed([clientRequest()], [upstreamRequest()], [{ ...chat, stream: true }]), /response framing/);
  });

  test('takes the status of a refusal as it is given, and does not check the framing of an error', async () => {
    const refused = clientRequest({ status: 502, responseHeaders: { 'x-request-id': 'r', 'content-type': 'text/event-stream' } });
    await relayed([refused], [upstreamRequest({ status: 500 })], [{ ...chat, status: 502, upstreamStatus: 500 }]);
    await assert.rejects(relayed([refused], [upstreamRequest({ status: 200 })], [{ ...chat, status: 502, upstreamStatus: 500 }]), /upstream status/);
  });

  test('requires one upstream request for each request of the client, in the order they were sent', async () => {
    await assert.rejects(relayed([clientRequest()], [], [chat]), /upstream requests/);
    await assert.rejects(relayed([clientRequest()], [upstreamRequest(), upstreamRequest()], [chat]), /upstream requests/);
    await assert.rejects(relayed([], [upstreamRequest()], [chat]), /client requests/);
    await assert.rejects(relayed([clientRequest(), clientRequest()], [upstreamRequest(), upstreamRequest()], [chat]), /client requests/);
    const first = { ...chatBody, messages: [{ role: 'user', content: 'one' }] };
    const second = { ...chatBody, messages: [{ role: 'user', content: 'two' }] };
    const sent = [clientRequest({ body: first }), clientRequest({ body: second })];
    const received = [upstreamRequest({ body: { ...first, model: upstreamModels.openai } }), upstreamRequest({ body: { ...second, model: upstreamModels.openai } })];
    await relayed(sent, received, [chat, chat]);
    await assert.rejects(relayed(sent, received.toReversed(), [chat, chat]), /the gateway changed the request/);
  });

  test('rewrites the model of the Gemini embedding bodies, which carry it in the body', async () => {
    const single = { model: `models/${models.gemini}`, content: { parts: [{ text: 'alpha' }] } };
    const sent = {
      ...clientRequest(),
      path: `/gemini/v1beta/models/${models.gemini}:embedContent`,
      headers: { 'x-goog-api-key': apiKey },
      body: single,
      bodyText: JSON.stringify(single)
    };
    const upstream = (body) => ({
      ...upstreamRequest(),
      dialect: 'gemini.embed',
      path: `/gemini/v1beta/models/${upstreamModels.gemini}:embedContent`,
      model: upstreamModels.gemini,
      headers: { 'x-goog-api-key': '[present]' },
      body
    });
    const expected = [{ api: 'gemini.embed', model: models.gemini }];
    await relayed([sent], [upstream({ ...single, model: `models/${upstreamModels.gemini}` })], expected);
    await assert.rejects(relayed([sent], [upstream(single)], expected), /the gateway changed the request/);

    const batch = { requests: [{ model: `models/${models.gemini}`, content: { parts: [{ text: 'a' }] } }, { model: `models/${models.gemini}`, content: { parts: [{ text: 'b' }] } }] };
    const sentBatch = { ...sent, path: sent.path.replace(':embedContent', ':batchEmbedContents'), body: batch, bodyText: JSON.stringify(batch) };
    const upstreamBatch = (body) => ({ ...upstream(body), dialect: 'gemini.batch_embed', path: upstream(body).path.replace(':embedContent', ':batchEmbedContents') });
    const rewritten = { requests: batch.requests.map((request) => ({ ...request, model: `models/${upstreamModels.gemini}` })) };
    const expectedBatch = [{ api: 'gemini.batch_embed', model: models.gemini }];
    await relayed([sentBatch], [upstreamBatch(rewritten)], expectedBatch);
    await assert.rejects(relayed([sentBatch], [upstreamBatch(batch)], expectedBatch), /the gateway changed the request/);
  });

  test('on a route that translates, checks the semantics and not the body', async () => {
    const message = { model: models.openai, max_tokens: 32, temperature: 0.2, tools: [{ name: 'get_weather', description: 'Weather', input_schema: { type: 'object' } }], messages: [{ role: 'user', content: 'Weather?' }] };
    const sent = {
      ...clientRequest(),
      path: '/anthropic/v1/messages',
      headers: { 'x-api-key': apiKey },
      body: message,
      bodyText: JSON.stringify(message)
    };
    const wire = { model: upstreamModels.openai, max_completion_tokens: 32, temperature: 0.2, messages: [{ role: 'user', content: 'Weather?' }], tools: [{ type: 'function', function: { name: 'get_weather', description: 'Weather', parameters: { type: 'object' } } }] };
    const upstream = (extra = {}) => upstreamRequest({ body: wire, ...extra });
    const expected = (extra = {}) => [{ api: 'anthropic.messages', to: 'openai.chat', model: models.openai, tools: ['get_weather'], maxTokens: 32, temperature: 0.2, contains: ['Weather?'], ...extra }];
    await relayed([sent], [upstream()], expected());
    await assert.rejects(relayed([sent], [upstream({ body: { ...wire, tools: [] } })], expected()), /tools offered upstream/);
    await assert.rejects(relayed([sent], [upstream()], expected({ tools: ['get_forecast'] })), /tools offered upstream/);
    await assert.rejects(relayed([sent], [upstream({ body: { ...wire, max_completion_tokens: 33 } })], expected()), /max tokens/);
    await assert.rejects(relayed([sent], [upstream({ body: { ...wire, temperature: 0.3 } })], expected()), /temperature/);
    await assert.rejects(relayed([sent], [upstream()], expected({ contains: ['Forecast?'] })), /lacks "Forecast\?"/);
    await assert.rejects(relayed([sent], [upstream({ dialect: 'anthropic.messages' })], expected()), /upstream dialect/);
  });

  test('requires a tool schema that reaches a vendor other than Gemini in JSON Schema type names', async () => {
    const geminiBody = (type) => ({ contents: [], tools: [{ functionDeclarations: [{ name: 'f', parameters: { type, properties: { city: { type } } } }] }] });
    const sent = { ...clientRequest(), path: `/gemini/v1beta/models/${models.openai}:generateContent`, headers: { 'x-goog-api-key': apiKey }, body: geminiBody('OBJECT') };
    const openai = (type) => upstreamRequest({ body: { model: upstreamModels.openai, messages: [], tools: [{ type: 'function', function: { name: 'f', parameters: { type, properties: { city: { type } } } } }] } });
    const expected = [{ api: 'gemini.generate', to: 'openai.chat', model: models.openai, tools: ['f'] }];
    await relayed([sent], [openai('object')], expected);
    await assert.rejects(relayed([sent], [openai('OBJECT')], expected), /schema of f: \/type is "OBJECT"/);
  });

  test('refuses an API or a route it does not know', async () => {
    await assert.rejects(relayed([clientRequest()], [upstreamRequest()], [{ api: 'openai.nothing', model: models.openai }]), /unknown API/);
    await assert.rejects(relayed([clientRequest()], [upstreamRequest()], [{ ...chat, model: 'no-such-route' }]), /no-such-route is not a route of the harness/);
  });
});

describe('the routes of the harness', () => {
  test('name the vendor model each one reaches', () => {
    assert.equal(upstreamModelOf(models.openai), upstreamModels.openai);
    assert.equal(upstreamModelOf(models.openaiStrict), upstreamModels.openai);
    assert.equal(upstreamModelOf(models.anthropic), upstreamModels.anthropic);
    assert.equal(upstreamModelOf(models.anthropicStrict), upstreamModels.anthropic);
    assert.equal(upstreamModelOf(models.gemini), upstreamModels.gemini);
    assert.equal(upstreamModelOf(models.geminiStrict), upstreamModels.gemini);
    assert.equal(upstreamModelOf(models.geminiEmbedStrict), upstreamModels.gemini);
    assert.equal(upstreamModelOf(namedModels.anthropic), upstreamModels.anthropic);
    assert.equal(upstreamModelOf(namedModels.gemini), upstreamModels.gemini);
    assert.throws(() => upstreamModelOf('no-such-route'), /not a route of the harness/);
  });
});

describe('reading a request in any dialect', () => {
  const schema = { type: 'object', properties: { city: { type: 'string' } } };
  const tools = {
    'openai.chat': { tools: [{ type: 'function', function: { name: 'f', description: 'd', parameters: schema } }] },
    'openai.responses': { tools: [{ type: 'function', name: 'f', description: 'd', parameters: schema }, { type: 'web_search' }] },
    'anthropic.messages': { tools: [{ name: 'f', description: 'd', input_schema: schema }] },
    'gemini.generate': { tools: [{ functionDeclarations: [{ name: 'f', description: 'd', parametersJsonSchema: schema }] }] },
    'gemini.stream': { tools: [{ functionDeclarations: [{ name: 'f', description: 'd', parameters: schema }] }, {}] }
  };

  test('finds the tools a request declares, hosted tools excepted', () => {
    for (const [api, body] of Object.entries(tools)) assert.deepEqual(toolDeclarations(api, body), [{ name: 'f', description: 'd', schema }], api);
    assert.deepEqual(toolDeclarations('openai.chat', {}), []);
    assert.throws(() => toolDeclarations('openai.embeddings', {}), /declares no tools/);
  });

  test('finds the images of a request, as base64 with their type', () => {
    const data = png.toString('base64');
    const url = `data:image/png;base64,${data}`;
    const want = [{ mediaType: 'image/png', data }];
    assert.deepEqual(images('openai.chat', { messages: [{ role: 'user', content: [{ type: 'text', text: 'x' }, { type: 'image_url', image_url: { url } }] }, { role: 'user', content: 'text' }] }), want);
    assert.deepEqual(images('openai.responses', { input: [{ role: 'user', content: [{ type: 'input_image', image_url: url }] }] }), want);
    assert.deepEqual(images('anthropic.messages', { messages: [{ role: 'user', content: [{ type: 'image', source: { type: 'base64', media_type: 'image/png', data } }] }] }), want);
    assert.deepEqual(images('gemini.generate', { contents: [{ role: 'user', parts: [{ inlineData: { mimeType: 'image/png', data } }, { text: 'x' }] }] }), want);
    assert.throws(() => images('openai.chat', { messages: [{ role: 'user', content: [{ type: 'image_url', image_url: { url: 'https://example.com/a.png' } }] }] }), /not a base64 data URL/);
    assert.throws(() => images('openai.embeddings', {}), /carries no images/);
  });

  test('finds the generation parameters of a request', () => {
    assert.deepEqual(generationParameters('openai.chat', { max_completion_tokens: 8, max_tokens: 9, temperature: 1 }), { maxTokens: 8, temperature: 1 });
    assert.deepEqual(generationParameters('openai.chat', { max_tokens: 9 }), { maxTokens: 9, temperature: undefined });
    assert.deepEqual(generationParameters('openai.responses', { max_output_tokens: 7, temperature: 0 }), { maxTokens: 7, temperature: 0 });
    assert.deepEqual(generationParameters('anthropic.messages', { max_tokens: 6, temperature: 0.5 }), { maxTokens: 6, temperature: 0.5 });
    assert.deepEqual(generationParameters('gemini.stream', { generationConfig: { maxOutputTokens: 5, temperature: 0.1 } }), { maxTokens: 5, temperature: 0.1 });
    assert.deepEqual(generationParameters('gemini.generate', {}), { maxTokens: undefined, temperature: undefined });
    assert.deepEqual(generationParameters('openai.embeddings', {}), {});
  });

  test('reads the text of a content value', () => {
    assert.equal(textOf('plain'), 'plain');
    assert.equal(textOf([{ type: 'text', text: 'a' }, 'b', { type: 'image' }, { text: 'c' }]), 'abc');
    assert.equal(textOf(undefined), '');
  });
});

describe('the schema a tool is declared with', () => {
  const capitals = { type: 'OBJECT', properties: { type: { type: 'STRING', enum: ['OBJECT'], default: 'OBJECT' }, tags: { type: 'ARRAY', items: { type: 'INTEGER' } } }, required: ['type'] };
  const standard = { type: 'object', properties: { type: { type: 'string', enum: ['OBJECT'], default: 'OBJECT' }, tags: { type: 'array', items: { type: 'integer' } } }, required: ['type'] };

  test('reaches another vendor with the type names of JSON Schema, and nothing else changed', () => {
    const declaration = { name: 'f', description: 'd', schema: capitals };
    assert.deepEqual(declaredForUpstream('gemini.generate', declaration), { name: 'f', description: 'd', schema: standard });
    assert.deepEqual(declaredForUpstream('gemini.stream', declaration).schema, standard);
    assert.deepEqual(declaredForUpstream('openai.chat', declaration), declaration, 'only Gemini spells types in capitals');
    assert.deepEqual(Object.keys(declaredForUpstream('gemini.generate', { schema: capitals }).schema.properties), ['type', 'tags'], 'the order of the properties is the client\'s');
    assert.deepEqual(declaredForUpstream('gemini.generate', { schema: undefined }), { schema: undefined });
  });

  test('is JSON Schema only when every type is a lower-case name', () => {
    assertJSONSchemaTypes(standard, 'standard');
    assertJSONSchemaTypes({ anyOf: [{ type: 'string' }, { type: 'null' }], items: [{ type: 'number' }, { type: 'boolean' }] }, 'nested');
    assert.throws(() => assertJSONSchemaTypes(capitals, 'capitals'), /\/type is "OBJECT"/);
    assert.throws(() => assertJSONSchemaTypes({ type: 'object', properties: { a: { type: 'STRING' } } }, 'property'), /\/properties\/a\/type is "STRING"/);
    assert.throws(() => assertJSONSchemaTypes({ type: 'object', properties: { a: { type: 'array', items: { type: 'Integer' } } } }, 'items'), /\/properties\/a\/items\/type is "Integer"/);
    assert.throws(() => assertJSONSchemaTypes({ type: 'text' }, 'unknown'), /is "text"/);
    assert.throws(() => assertJSONSchemaTypes({ anyOf: [{ type: 'STRING' }] }, 'union'), /\/anyOf\/0\/type is "STRING"/);
  });
});

describe('the reference embeddings', () => {
  test('are one vector per text, equal for equal text, and as long as the dimensions asked for', async () => {
    const vectors = await referenceEmbeddings(['alpha', 'beta', 'alpha']);
    assert.equal(vectors.length, 3);
    assertSameVector(vectors[2], vectors[0], 'equal text');
    assert.notDeepEqual(vectors[0], vectors[1]);
    const short = await referenceEmbeddings(['alpha'], 8);
    assert.equal(short[0].length, 8);
    assert.deepEqual(await recorded(), [], 'the reference fetch leaves no recording behind');
    // The route answers the same texts the same way through the Gemini upstream.
    const response = await localFetch(`${baseURLs.openai}/embeddings`, { method: 'POST', headers: { 'content-type': 'application/json', ...bearer }, body: JSON.stringify({ model: models.gemini, input: ['alpha', 'beta'] }) });
    assert.equal(response.status, 200);
    const { data } = await response.json();
    assertSameVector(data[0].embedding, vectors[0], 'alpha through Gemini');
    assertSameVector(data[1].embedding, vectors[1], 'beta through Gemini');
  });

  test('are the same vector only within float32 precision', () => {
    assertSameVector([1, 0.5], [1, 0.5 + 1e-7], 'within');
    assert.throws(() => assertSameVector([1, 0.5], [1, 0.5001], 'component'), /component 1 is 0.5, expected 0.5001/);
    assert.throws(() => assertSameVector([1], [1, 2], 'dimension'), /dimension: dimension/);
  });
});
