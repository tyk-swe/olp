import { readFile } from 'node:fs/promises';
import assert from 'node:assert/strict';
import { isDeepStrictEqual } from 'node:util';

const metadataPath = process.env.OLP_SDK_SMOKE_METADATA;
assert.ok(metadataPath, 'OLP_SDK_SMOKE_METADATA is required');
const metadata = JSON.parse(await readFile(metadataPath, 'utf8'));
const {
  origin,
  api_key: apiKey,
  conflict_api_key: conflictApiKey,
  route_slug: routeSlug,
  native_tool_route: nativeToolRoute,
  verification_origin: verificationOrigin
} = metadata;
const invalidApiKey = 'olp_not-a-real-key';
assert.match(origin, /^http:\/\/127\.0\.0\.1:\d+$/);
assert.equal(routeSlug, 'sdk-smoke-route');
assert.ok(apiKey.startsWith('olp_'), 'fixture returned an OLP proxy key');
assert.ok(conflictApiKey.startsWith('olp_'), 'fixture returned a second OLP proxy key');
assert.notEqual(conflictApiKey, apiKey, 'fixture keys must be distinct for conflict coverage');
assert.equal(nativeToolRoute, 'sdk-native-tools-route');
assert.match(verificationOrigin, /^http:\/\/127\.0\.0\.1:\d+$/);
assert.notEqual(verificationOrigin, origin, 'fixture verification is separate from the gateway');

if (process.argv.includes('--check-metadata')) process.exit(0);

// The Go backend gains surfaces milestone by milestone; a run names the ones
// its fixture serves, defaulting to every surface.
const surfaces = new Set((process.env.OLP_SDK_SMOKE_SURFACES ?? 'openai,anthropic,gemini').split(','));

const [{ default: Anthropic }, { GoogleGenAI }, { default: OpenAI }] = await Promise.all([
  import('@anthropic-ai/sdk'),
  import('@google/genai'),
  import('openai')
]);

const nativeFetch = globalThis.fetch.bind(globalThis);
const localOnlyFetch = async (input, init) => {
  const url = new URL(input instanceof Request ? input.url : String(input));
  assert.equal(url.origin, origin, `SDK attempted non-fixture request: ${url.origin}`);
  return nativeFetch(input, init);
};
globalThis.fetch = localOnlyFetch;

function openAIClient(baseURL, options = {}) {
  return new OpenAI({
    apiKey,
    baseURL,
    defaultHeaders: { 'X-OLP-Routing': JSON.stringify({ strategy: 'weighted' }) },
    fetch: localOnlyFetch,
    maxRetries: 0,
    timeout: 5_000,
    ...options
  });
}

function anthropicClient(clientApiKey = apiKey) {
  return new Anthropic({
    apiKey: clientApiKey,
    baseURL: `${origin}/anthropic`,
    defaultHeaders: { 'X-OLP-Routing': JSON.stringify({ strategy: 'weighted' }) },
    fetch: localOnlyFetch,
    maxRetries: 0,
    timeout: 5_000
  });
}

function googleClient(clientApiKey = apiKey, retryOptions) {
  return new GoogleGenAI({
    apiKey: clientApiKey,
    apiVersion: 'v1beta',
    httpOptions: {
      baseUrl: `${origin}/gemini`,
      apiVersion: 'v1beta',
      headers: { 'X-OLP-Routing': JSON.stringify({ strategy: 'weighted' }) },
      timeout: 5_000,
      ...(retryOptions && { retryOptions })
    }
  });
}

const openAIBaseURLs = [
  ['canonical OpenAI base', `${origin}/v1`],
  ['canonical OpenAI base with trailing slash', `${origin}/v1/`],
  ['OpenAI compatibility base', `${origin}/v1`],
  ['OpenAI compatibility base with trailing slash', `${origin}/v1/`]
];

async function smokeOpenAI(baseURL, label) {
  const client = openAIClient(baseURL);
  const completion = await client.chat.completions.create({
    model: routeSlug,
    max_tokens: 32,
    messages: [{ role: 'user', content: 'official SDK smoke' }]
  });
  assert.equal(completion.model, routeSlug);
  assert.equal(
    completion.choices[0]?.message.content,
    `official openai sdk reached ${routeSlug}`
  );

  const response = await client.responses.create({
    model: routeSlug,
    input: 'official Responses SDK smoke'
  });
  assert.equal(response.output_text, `official openai sdk reached ${routeSlug}`);

  const streaming = await client.chat.completions.create({
    model: routeSlug,
    max_tokens: 32,
    stream: true,
    messages: [{ role: 'user', content: 'official streaming SDK smoke' }]
  });
  let streamedText = '';
  for await (const chunk of streaming) {
    streamedText += chunk.choices[0]?.delta.content ?? '';
  }
  assert.equal(streamedText, `official openai sdk reached ${routeSlug}`);

  const page = await client.models.list();
  assert.ok(page.data.some((model) => model.id === routeSlug), label);

  const model = await client.models.retrieve(routeSlug);
  assert.equal(model.id, routeSlug, label);
}

async function smokeAnthropic() {
  const client = anthropicClient();
  const message = await client.messages.create({
    model: routeSlug,
    max_tokens: 32,
    messages: [{ role: 'user', content: 'official SDK smoke' }]
  });
  assert.equal(message.model, routeSlug);
  assert.equal(message.content[0]?.type, 'text');
  assert.equal(message.content[0]?.text, `official anthropic sdk reached ${routeSlug}`);

  const streamed = await client.messages
    .stream({
      model: routeSlug,
      max_tokens: 32,
      messages: [{ role: 'user', content: 'official streaming SDK smoke' }]
    })
    .finalMessage();
  assert.equal(streamed.content[0]?.type, 'text');
  assert.equal(streamed.content[0]?.text, `official anthropic sdk reached ${routeSlug}`);

  const page = await client.models.list({ limit: 10 });
  assert.ok(page.data.some((model) => model.id === routeSlug));

  const count = await client.messages.countTokens({
    model: routeSlug,
    messages: [{ role: 'user', content: 'official token count SDK smoke' }]
  });
  assert.equal(count.input_tokens, 13);
}

async function nativeAnthropicToolWorkflow() {
  const reference = JSON.parse(await readFile(
    new URL('../fixtures/fidelity/v1/anthropic-tool-next-request.json', import.meta.url),
    'utf8'
  ));
  const { model: nativeModel, messages: expectedMessages, ...controls } = reference;
  assert.equal(nativeModel, 'fixture-model');
  const counts = async () => {
    // This is a separate fixture listener, never an endpoint added to OLP.
    const response = await nativeFetch(`${verificationOrigin}/native-tool-workflow`);
    assert.equal(response.status, 200);
    return response.json();
  };
  assert.deepEqual(await counts(), {
    dispatches: 0, initial_requests: 0, next_requests: 0,
    rejected_requests: 0, complete: false
  });
  const client = anthropicClient();
  const history = [expectedMessages[0]];
  const stream = client.messages.stream({
    ...controls, model: nativeToolRoute, messages: history
  });
  const events = [];
  for await (const event of stream) events.push(event);
  const assistant = await stream.finalMessage();
  assert.equal(events.length, 19, 'all frozen native events reached the actual SDK');
  assert.equal(events.at(-1)?.type, 'message_stop');
  assert.deepEqual(
    events.filter((event) => event.type === 'content_block_start').map((event) => event.index),
    [0, 1, 2, 3, 4]
  );
  assert.equal(assistant.model, nativeToolRoute);
  assert.equal(assistant.stop_reason, 'tool_use');
  assert.equal(assistant.usage.input_tokens, 18);
  assert.equal(assistant.usage.output_tokens, 28);
  assert.ok(isDeepStrictEqual(assistant.content, expectedMessages[1].content),
    'SDK assembly must retain thinking/signature and text/tool/text order');

  const calls = assistant.content.filter((block) => block.type === 'tool_use');
  assert.deepEqual(calls.map((call) => call.id), ['call-weather', 'call-clock']);
  const executed = [];
  const results = await Promise.all(calls.map(async (call) => {
    executed.push(call.id);
    let content;
    if (call.name === 'weather') {
      assert.deepEqual(call.input, { city: 'Paris' });
      content = 'sunny';
    } else if (call.name === 'clock') {
      assert.deepEqual(call.input, { zone: 'Europe/Paris' });
      content = '14:00';
    } else {
      assert.fail('unexpected native tool');
    }
    return { type: 'tool_result', tool_use_id: call.id, content };
  }));
  assert.equal(executed.length, 2);
  assert.equal(new Set(executed).size, 2);

  // Pass the actual SDK-assembled blocks back through its next-request
  // serializer. The provider compares the entire received body to the frozen
  // reference, including all controls, schemas, opaque state and both results.
  const final = await client.messages.create({
    ...controls,
    model: nativeToolRoute,
    messages: [
      ...history,
      { role: assistant.role, content: assistant.content },
      { role: 'user', content: results }
    ]
  });
  assert.equal(final.id, 'msg-native-tool-final');
  assert.equal(final.model, nativeToolRoute);
  assert.equal(final.stop_reason, 'end_turn');
  assert.deepEqual(final.content, [{ type: 'text', text: 'Weather: sunny. Time: 14:00.' }]);
  assert.equal(final.usage.input_tokens, 64);
  assert.equal(final.usage.output_tokens, 8);
  assert.deepEqual(await counts(), {
    dispatches: 2, initial_requests: 1, next_requests: 1,
    rejected_requests: 0, complete: true
  });
  process.stdout.write('Native Anthropic SDK reasoning/tool continuation passed: 19 events, 2 tools, 2 verified dispatches.\n');
}

async function smokeGoogle() {
  const client = googleClient(apiKey, { attempts: 1 });
  const response = await client.models.generateContent({
    model: routeSlug,
    contents: 'official SDK smoke'
  });
  assert.equal(response.text, `official gemini sdk reached ${routeSlug}`);
  assert.equal(response.modelVersion, routeSlug);

  const streaming = await client.models.generateContentStream({
    model: routeSlug,
    contents: 'official streaming SDK smoke'
  });
  let streamedText = '';
  for await (const chunk of streaming) streamedText += chunk.text ?? '';
  assert.equal(streamedText, `official gemini sdk reached ${routeSlug}`);

  const pager = await client.models.list({ config: { pageSize: 10 } });
  const modelNames = [];
  for await (const model of pager) modelNames.push(model.name);
  assert.ok(modelNames.includes(`models/${routeSlug}`));
}

/// Runs `attempt`, requiring it to reject, and returns the rejection.
async function rejection(what, attempt) {
  try {
    await attempt();
  } catch (error) {
    return error;
  }
  throw new assert.AssertionError({ message: `${what} was expected to fail but succeeded` });
}

// README.md calls these surfaces OpenAI-, Anthropic- and Gemini-compatible.
// Compatibility is what the official client can do with a response, and a
// client's error handling is the half a happy-path smoke never reaches: an
// application catches `AuthenticationError`, not "some rejection". A gateway
// whose failures do not land in the SDK's own typed hierarchy, with the status
// each vendor documents for that condition, is not compatible however well its
// successes are shaped.
async function errorContractOpenAI(baseURL, label) {
  const wrongKey = openAIClient(baseURL, { apiKey: invalidApiKey });
  const unauthorized = await rejection(`${label} with an invalid key`, () =>
    wrongKey.chat.completions.create({
      model: routeSlug,
      max_tokens: 32,
      messages: [{ role: 'user', content: 'invalid credential' }]
    })
  );
  assert.ok(
    unauthorized instanceof OpenAI.AuthenticationError,
    `${label} invalid key must raise OpenAI.AuthenticationError, got ${unauthorized?.constructor?.name}: ${unauthorized}`
  );
  assert.equal(unauthorized.status, 401, `${label} invalid key is 401`);

  const client = openAIClient(baseURL);
  const missing = await rejection(`${label} with an unknown model`, () =>
    client.chat.completions.create({
      model: 'sdk-smoke-no-such-route',
      max_tokens: 32,
      messages: [{ role: 'user', content: 'unknown model' }]
    })
  );
  assert.ok(
    missing instanceof OpenAI.NotFoundError,
    `${label} unknown model must raise OpenAI.NotFoundError, got ${missing?.constructor?.name}: ${missing}`
  );
  assert.equal(missing.status, 404, `${label} unknown model is 404`);
}

async function directNegativeContracts() {
  const malformedRouting = await rejection('malformed OLP preferences', () => openAIClient(`${origin}/v1`, { defaultHeaders: { 'X-OLP-Routing': '{"max_attempts":999}' } }).chat.completions.create({ model: routeSlug, messages: [{ role: 'user', content: 'routing rejection' }] }));
  assert.equal(malformedRouting.status, 400, 'request preferences cannot increase the attempt budget');

  const retiredAuth = await localOnlyFetch(`${origin}/v1/models`, {
    headers: { 'x-litellm-api-key': apiKey }
  });
  assert.equal(retiredAuth.status, 401, 'the retired header does not authenticate');
  await retiredAuth.text();
  const retiredPrefix = await localOnlyFetch(`${origin}/openai/v1/models`, {
    headers: { Authorization: `Bearer ${apiKey}` }
  });
  assert.equal(retiredPrefix.status, 404, 'the retired prefix is absent');
  await retiredPrefix.text();

  const unknownRoute = await localOnlyFetch(`${origin}/v1/not-enabled`, {
    headers: { Authorization: `Bearer ${apiKey}` }
  });
  assert.equal(unknownRoute.status, 404, 'an unknown /v1 route must remain unsupported');
  await unknownRoute.text();
}

async function errorContractAnthropic() {
  const wrongKey = anthropicClient(invalidApiKey);
  const unauthorized = await rejection('an Anthropic call with an invalid key', () =>
    wrongKey.messages.create({
      model: routeSlug,
      max_tokens: 32,
      messages: [{ role: 'user', content: 'invalid credential' }]
    })
  );
  assert.ok(
    unauthorized instanceof Anthropic.AuthenticationError,
    `an invalid key must raise Anthropic.AuthenticationError, got ${unauthorized?.constructor?.name}: ${unauthorized}`
  );
  assert.equal(unauthorized.status, 401, 'an invalid credential is 401, not another 4xx');
  // The Anthropic dialect carries its own error envelope; the SDK exposes it
  // as `error`, and an application reads `error.error.type` to branch.
  assert.equal(
    unauthorized.error?.error?.type,
    'authentication_error',
    `the Anthropic error envelope must name the condition: ${JSON.stringify(unauthorized.error)}`
  );
}

async function errorContractGoogle() {
  // No retry options here: the SDK's retry helper replaces a typed ApiError
  // with a generic Error, which would hide what the gateway actually sent.
  const wrongKey = googleClient(invalidApiKey);
  const unauthorized = await rejection('a Gemini call with an invalid key', () =>
    wrongKey.models.generateContent({ model: routeSlug, contents: 'invalid credential' })
  );
  // @google/genai raises ApiError with the upstream status attached; the class
  // is not exported, so the status is what an application can rely on.
  assert.equal(
    unauthorized.status,
    401,
    `an invalid credential must reach the Gemini SDK as a 401: ${unauthorized}`
  );
}

if (surfaces.has('openai')) {
  for (const [label, baseURL] of openAIBaseURLs) await smokeOpenAI(baseURL, label);
}
if (surfaces.has('anthropic')) {
  await smokeAnthropic();
  await nativeAnthropicToolWorkflow();
}
if (surfaces.has('gemini')) await smokeGoogle();
if (surfaces.has('openai')) {
  for (const [label, baseURL] of openAIBaseURLs) await errorContractOpenAI(baseURL, label);
  await directNegativeContracts();
}
if (surfaces.has('anthropic')) await errorContractAnthropic();
if (surfaces.has('gemini')) await errorContractGoogle();
process.stdout.write(
  `Official SDK success and error contracts passed for ${[...surfaces].join(', ')}.\n`
);
