// The tap and the relay assertions are what the framework suites measure with:
// four of them rest their "the body arrived unchanged apart from the model"
// guarantee on them. A comparison that passes whatever it is given would leave
// every one of those suites green however the gateway changed a request, so the
// instruments are checked here, first with a real tap in front of the real
// gateway, then with records built by hand, each altered in the one way a
// regression would alter it.
import assert from 'node:assert/strict';
import { beforeEach, describe, test } from 'node:test';
import { apiKey, stateApiKey, models, recorded, resetRecorded, upstreamModels } from '../../lib/harness.mjs';
import { assertRelayed } from '../../lib/relay.mjs';
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

describe('the tap', () => {

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
});

describe('sseData', () => {
  test('is the JSON of every data line, without the end marker, comments or other fields', () => {
    const text = ': keep-alive\r\n\r\nevent: message\r\ndata: {"a":1}\r\n\r\ndata:{"b":2}\n\nid: 7\n\ndata: [DONE]\n\n';
    assert.deepEqual(sseData(text), [{ a: 1 }, { b: 2 }]);
    assert.deepEqual(sseData(''), []);
  });
});

describe('assertRelayed with a real tap and the real gateway', () => {
  test('holds default-deny and opted-in Responses to their distinct retention contracts', async (t) => {
    const tap = await tapFor(t);
    const body = { model: models.openai, input: [{ type: 'message', role: 'user', content: [{ type: 'input_text', text: 'Private prompt' }] }] };
    await post(`${tap.baseURLs.openai}/responses`, body);
    await post(`${tap.baseURLs.openai}/responses`, body, { authorization: `Bearer ${stateApiKey}` });
    const { upstream } = await assertRelayed(tap, [
      { api: 'openai.responses', model: models.openai },
      { api: 'openai.responses', model: models.openai, allowProviderState: true }
    ]);
    assert.equal(upstream[0].body.store, false);
    assert.equal(Object.hasOwn(upstream[1].body, 'store'), false);
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

  test('requires transformed Responses to disable default storage without masking other changes', async () => {
    const want = { api: 'openai.responses', model: models.openai };
    for (const store of [undefined, null, false]) {
      const body = { model: models.openai, input: 'Private prompt', ...(store === undefined ? {} : { store }) };
      const sent = clientRequest({ path: '/v1/responses', body });
      const expected = { ...body, model: upstreamModels.openai, store: false };
      const upstream = (body) => upstreamRequest({ dialect: 'openai.responses', path: '/openai/v1/responses', body });
      await relayed([sent], [upstream(expected)], [want]);
      for (const wrong of [
        (({ store, ...rest }) => rest)(expected),
        { ...expected, store: true },
        { ...expected, input: 'Altered prompt' },
        { ...expected, extra: 'unexpected' }
      ]) await assert.rejects(relayed([sent], [upstream(wrong)], [want]), /the gateway changed the request/);
    }
  });

  test('requires transparency when the fixture key explicitly permits provider state', async () => {
    const body = { model: models.openai, input: 'Private prompt' };
    const sent = clientRequest({ path: '/v1/responses', headers: { authorization: `Bearer ${stateApiKey}` }, body });
    const upstream = upstreamRequest({ dialect: 'openai.responses', path: '/openai/v1/responses', body: { ...body, model: upstreamModels.openai } });
    const expected = [{ api: 'openai.responses', model: models.openai, allowProviderState: true }];
    await relayed([sent], [upstream], expected);
    await assert.rejects(relayed([sent], [{ ...upstream, body: { ...upstream.body, store: false } }], expected), /the gateway changed the request/);
  });

  test('does not normalize strict Responses or mask an explicit storage request', async () => {
    for (const [model, store] of [[models.openaiStrict, undefined], [models.openai, true]]) {
      const body = { model, input: 'Private prompt', ...(store === undefined ? {} : { store }) };
      const sent = clientRequest({ path: '/v1/responses', body });
      const upstream = upstreamRequest({ dialect: 'openai.responses', path: '/openai/v1/responses', body: { ...body, model: upstreamModels.openai, store: false } });
      await assert.rejects(relayed([sent], [upstream], [{ api: 'openai.responses', model }]), /the gateway changed the request/);
    }
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

  test('takes the status of a refusal as it is given, and does not check the framing of an error', async () => {
    const refused = clientRequest({ status: 502, responseHeaders: { 'x-request-id': 'r', 'content-type': 'text/event-stream' } });
    await relayed([refused], [upstreamRequest({ status: 500 })], [{ ...chat, status: 502, upstreamStatus: 500 }]);
    await assert.rejects(relayed([refused], [upstreamRequest({ status: 200 })], [{ ...chat, status: 502, upstreamStatus: 500 }]), /upstream status/);
  });

  test('refuses an API or a route it does not know', async () => {
    await assert.rejects(relayed([clientRequest()], [upstreamRequest()], [{ api: 'openai.nothing', model: models.openai }]), /unknown API/);
    await assert.rejects(relayed([clientRequest()], [upstreamRequest()], [{ ...chat, model: 'no-such-route' }]), /no-such-route is not a route of the harness/);
  });
});
