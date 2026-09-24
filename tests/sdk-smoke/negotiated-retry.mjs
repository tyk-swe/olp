import assert from 'node:assert/strict';
import OpenAI from 'openai';
import { recoverSubmission, streamTurn, submissionID } from '../../clients/continuation/javascript.mjs';

const { OLP_CONTINUATION_ORIGIN: origin, OLP_CONTINUATION_KEY: key, OLP_CONTINUATION_ROUTE: route } = process.env;
assert.match(origin, /^http:\/\/127\.0\.0\.1:\d+$/);
assert.ok(key?.startsWith('olp_') && route?.startsWith('strict-'));
const nativeFetch = globalThis.fetch.bind(globalThis);
const headersSeen = [];
let loseAfterCommit = true;
const losingFetch = async (input, init) => {
  const request = input instanceof Request ? input : new Request(input, init);
  assert.equal(new URL(request.url).origin, origin);
  if (new URL(request.url).pathname === '/v1/chat/completions') {
    headersSeen.push(request.headers.get('X-OLP-Submission-ID'));
  }
  const response = await nativeFetch(input, init);
  if (loseAfterCommit && new URL(request.url).pathname === '/v1/chat/completions') {
    loseAfterCommit = false;
    assert.equal(response.status, 200);
    // The gateway and provider have completed and committed. Lose the
    // response at the SDK transport boundary before its caller sees a byte.
    await response.arrayBuffer();
    throw new TypeError('simulated connection loss after accepted work');
  }
  return response;
};
const client = new OpenAI({ apiKey: key, baseURL: `${origin}/v1`, fetch: losingFetch, maxRetries: 1, timeout: 15_000 });
const source = {
  model: route, stream: true,
  tools: [
    { type: 'function', function: { name: 'weather', description: 'Weather in a city', parameters: { type: 'object', properties: { city: { type: 'string' } }, required: ['city'] } } },
    { type: 'function', function: { name: 'clock', description: 'Time in a zone', parameters: { type: 'object', properties: { zone: { type: 'string' } }, required: ['zone'] } } }
  ],
  messages: [{ role: 'user', content: 'Weather and time in Paris?' }]
};
const submission = submissionID();
const result = await streamTurn(client, source, { submission });
assert.equal(result.submission, submission);
assert.equal(result.assistant.content, 'beforeafter');
assert.deepEqual(result.assistant.tool_calls.map((call) => call.id), ['call-weather', 'call-clock']);
assert.equal(result.finish, 'tool_calls');
assert.deepEqual(result.nativeUsage, { input_tokens: 18, output_tokens: 28 });
assert.deepEqual(headersSeen, [submission, submission], 'SDK retry must reuse one accepted-work identity');
const recovered = await recoverSubmission(origin, key, submission, nativeFetch);
assert.equal(recovered.handle, result.handle);
assert.deepEqual(recovered.assistant, result.assistant);
process.stdout.write('Official OpenAI JS SDK recovered one accepted tool stream after its automatic retry.\n');
