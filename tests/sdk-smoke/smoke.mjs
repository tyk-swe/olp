import { readFile } from 'node:fs/promises';
import assert from 'node:assert/strict';
import Anthropic from '@anthropic-ai/sdk';
import { GoogleGenAI } from '@google/genai';
import OpenAI from 'openai';

const metadataPath = process.env.OLP_SDK_SMOKE_METADATA;
assert.ok(metadataPath, 'OLP_SDK_SMOKE_METADATA is required');
const metadata = JSON.parse(await readFile(metadataPath, 'utf8'));
const { origin, api_key: apiKey, route_slug: routeSlug } = metadata;
assert.match(origin, /^http:\/\/127\.0\.0\.1:\d+$/);
assert.equal(routeSlug, 'sdk-smoke-route');
assert.ok(apiKey.startsWith('olp_'), 'fixture returned an OLP proxy key');

const nativeFetch = globalThis.fetch.bind(globalThis);
const localOnlyFetch = async (input, init) => {
  const url = new URL(input instanceof Request ? input.url : String(input));
  assert.equal(url.origin, origin, `SDK attempted non-fixture request: ${url.origin}`);
  return nativeFetch(input, init);
};
globalThis.fetch = localOnlyFetch;

async function smokeOpenAI() {
  const client = new OpenAI({
    apiKey,
    baseURL: `${origin}/openai/v1`,
    fetch: localOnlyFetch,
    maxRetries: 0,
    timeout: 5_000
  });
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
  assert.ok(page.data.some((model) => model.id === routeSlug));
}

async function smokeAnthropic() {
  const client = new Anthropic({
    apiKey,
    baseURL: `${origin}/anthropic`,
    fetch: localOnlyFetch,
    maxRetries: 0,
    timeout: 5_000
  });
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
}

async function smokeGoogle() {
  const client = new GoogleGenAI({
    apiKey,
    apiVersion: 'v1beta',
    httpOptions: {
      baseUrl: `${origin}/gemini`,
      apiVersion: 'v1beta',
      timeout: 5_000,
      retryOptions: { attempts: 1 }
    }
  });
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
async function errorContractOpenAI() {
  const wrongKey = new OpenAI({
    apiKey: 'olp_not-a-real-key',
    baseURL: `${origin}/openai/v1`,
    fetch: localOnlyFetch,
    maxRetries: 0,
    timeout: 5_000
  });
  const unauthorized = await rejection('an OpenAI call with an invalid key', () =>
    wrongKey.chat.completions.create({
      model: routeSlug,
      max_tokens: 32,
      messages: [{ role: 'user', content: 'invalid credential' }]
    })
  );
  assert.ok(
    unauthorized instanceof OpenAI.AuthenticationError,
    `an invalid key must raise OpenAI.AuthenticationError, got ${unauthorized?.constructor?.name}: ${unauthorized}`
  );
  assert.equal(unauthorized.status, 401, 'an invalid credential is 401, not another 4xx');

  const client = new OpenAI({
    apiKey,
    baseURL: `${origin}/openai/v1`,
    fetch: localOnlyFetch,
    maxRetries: 0,
    timeout: 5_000
  });
  const missing = await rejection('an OpenAI call naming an unknown model', () =>
    client.chat.completions.create({
      model: 'sdk-smoke-no-such-route',
      max_tokens: 32,
      messages: [{ role: 'user', content: 'unknown model' }]
    })
  );
  assert.ok(
    missing instanceof OpenAI.NotFoundError,
    `an unknown model must raise OpenAI.NotFoundError, got ${missing?.constructor?.name}: ${missing}`
  );
  assert.equal(missing.status, 404, 'an unknown model is 404, not another 4xx');
}

async function errorContractAnthropic() {
  const wrongKey = new Anthropic({
    apiKey: 'olp_not-a-real-key',
    baseURL: `${origin}/anthropic`,
    fetch: localOnlyFetch,
    maxRetries: 0,
    timeout: 5_000
  });
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
  const wrongKey = new GoogleGenAI({
    apiKey: 'olp_not-a-real-key',
    apiVersion: 'v1beta',
    // No retryOptions here: the SDK's retry helper replaces a typed ApiError
    // with a generic Error, which would hide what the gateway actually sent.
    httpOptions: {
      baseUrl: `${origin}/gemini`,
      apiVersion: 'v1beta',
      timeout: 5_000
    }
  });
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

await smokeOpenAI();
await smokeAnthropic();
await smokeGoogle();
await errorContractOpenAI();
await errorContractAnthropic();
await errorContractGoogle();
process.stdout.write(
  'Official OpenAI, Anthropic, and Google GenAI SDK success and error contracts passed.\n'
);
