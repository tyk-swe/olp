import assert from 'node:assert/strict';
import OpenAI from 'openai';
import { nextTurn, recoverSubmission, streamTurn, unaryTurn } from '../../clients/continuation/javascript.mjs';

const { OLP_CONTINUATION_ORIGIN: origin, OLP_CONTINUATION_KEY: key, OLP_CONTINUATION_ROUTE: route } = process.env;
assert.match(origin, /^http:\/\/127\.0\.0\.1:\d+$/);
assert.ok(key?.startsWith('olp_'));
assert.ok(route?.startsWith('strict-'));
const local = globalThis.fetch.bind(globalThis);
const localOnlyFetch = (input, init) => {
  assert.equal(new URL(input instanceof Request ? input.url : String(input)).origin, origin);
  return local(input, init);
};
const client = new OpenAI({
  apiKey: key,
  baseURL: `${origin}/v1`,
  maxRetries: 0,
  timeout: 15_000,
  fetch: localOnlyFetch
});
let unsupportedCalls = 0;
await assert.rejects(() => streamTurn({
  getUserAgent: () => 'OpenAI/JS 0.0.0',
  chat: { completions: { create: () => { unsupportedCalls++; } } }
}, { model: route, messages: [] }), /supports the qualified/);
assert.equal(unsupportedCalls, 0, 'unsupported SDK rejected before dispatch');
const request = {
  model: route,
  stream: true,
  tools: [
    { type: 'function', function: { name: 'weather', description: 'Weather in a city', parameters: { type: 'object', properties: { city: { type: 'string' } }, required: ['city'] } } },
    { type: 'function', function: { name: 'clock', description: 'Time in a zone', parameters: { type: 'object', properties: { zone: { type: 'string' } }, required: ['zone'] } } }
  ],
  messages: [{ role: 'user', content: 'Weather and time in Paris?' }]
};
try {
  await client.chat.completions.create(request);
  assert.fail('unnegotiated SDK unexpectedly dispatched');
} catch (error) {
  assert.equal(error.status, 400);
  assert.equal(error.code, 'state_carrier');
}
const first = await streamTurn(client, request);
assert.equal(first.finish, 'tool_calls');
assert.equal(first.assistant.content, 'beforeafter');
assert.deepEqual(first.assistant.tool_calls.map((call) => call.id), ['call-weather', 'call-clock']);
assert.deepEqual(first.observations.filter((item) => item.phase === 'start').map((item) => item.type),
  ['thinking', 'text', 'tool_use', 'tool_use', 'text']);
assert.ok(first.observations.some((item) => item.type === 'thinking' && item.opaque_state === true));
assert.deepEqual(first.nativeUsage, { input_tokens: 18, output_tokens: 28 });
assert.deepEqual(first.nativeTerminal, { stop_reason: 'tool_use', stop_sequence: null, finish_reason: 'tool_calls' });
assert.ok(!JSON.stringify(first.chunks).includes('opaque-fixture-signature-do-not-log'));
const recovered = await recoverSubmission(origin, key, first.submission, localOnlyFetch);
assert.equal(recovered.handle, first.handle);
assert.deepEqual(recovered.assistant, first.assistant);
assert.deepEqual(recovered.native_terminal, first.nativeTerminal);
assert.ok(Array.isArray(recovered.delivery.frames) && recovered.delivery.frames.length > 0);
const replay = await streamTurn(client, request, { submission: first.submission });
assert.equal(replay.handle, first.handle);
assert.deepEqual(replay.assistant, first.assistant);
const next = nextTurn(request, first, [
  { tool_call_id: 'call-weather', content: 'sunny' },
  { tool_call_id: 'call-clock', content: '14:00' }
]);
const final = await unaryTurn(client, next, { handle: first.handle });
assert.equal(final.response.choices[0].message.content, 'Both tools completed.');
assert.equal(final.response.choices[0].finish_reason, 'stop');
assert.deepEqual(final.nativeUsage, { input_tokens: 30, output_tokens: 4 });
assert.deepEqual(final.nativeTerminal, { stop_reason: 'end_turn', stop_sequence: null, finish_reason: 'stop' });
process.stdout.write(JSON.stringify({ sdk: 'openai-js-7.4.0', first: first.handle, final: final.handle, observations: first.observations.length }) + '\n');
