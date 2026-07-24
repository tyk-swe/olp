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

  const count = await client.responses.inputTokens.count({
    model: routeSlug,
    input: 'official token count smoke'
  });
  assert.equal(count.input_tokens, 7);
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

  const count = await client.messages.countTokens({
    model: routeSlug,
    messages: [{ role: 'user', content: 'official token count smoke' }]
  });
  assert.equal(count.input_tokens, 7);
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

  const count = await client.models.countTokens({
    model: routeSlug,
    contents: 'official token count smoke'
  });
  assert.equal(count.totalTokens, 7);
}

await smokeOpenAI();
await smokeAnthropic();
await smokeGoogle();
process.stdout.write('Official OpenAI, Anthropic, and Google GenAI SDK smoke passed.\n');
