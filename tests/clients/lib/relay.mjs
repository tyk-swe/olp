// Holds what a client sent against what the scripted upstream received. A suite
// states which wire API each client request should have used and which route it
// addressed; `assertRelayed` then checks the client's side (path, credential,
// status, framing), the upstream's side (path, rewritten model, upstream
// credential, no caller credential), and, on a route that keeps the client's
// dialect, that the body arrived unchanged apart from the model and the
// fixture key's explicit no-retention policy on transformed Responses.
//
// The suites use the recording proxy in tap.mjs for the client's side and
// `recorded()` in harness.mjs for the upstream's.
import assert from 'node:assert/strict';
import { apiKey, stateApiKey, localFetch, models, baseURLs, recorded, resetRecorded, upstreamModels } from './harness.mjs';

function required(name) {
  const value = process.env[name];
  assert.ok(value, `${name} is not set; run the suites through tests/clients/run.sh`);
  return value;
}

/**
 * Routes named after the vendor model a client library recognizes, as an
 * operator names a route after the model it replaces. LlamaIndex.TS decides
 * whether a model can call tools from its name.
 */
export const namedModels = {
  anthropic: required('OLP_CLIENTS_MODEL_ANTHROPIC_CLAUDE'),
  gemini: required('OLP_CLIENTS_MODEL_GEMINI_FLASH')
};

/** The vendor behind each route slug, to know which model the upstream should see. */
const vendors = new Map([
  [models.openai, 'openai'],
  [models.openaiStrict, 'openai'],
  [models.anthropic, 'anthropic'],
  [models.anthropicStrict, 'anthropic'],
  [models.gemini, 'gemini'],
  [models.geminiStrict, 'gemini'],
  [models.geminiEmbedStrict, 'gemini'],
  [namedModels.anthropic, 'anthropic'],
  [namedModels.gemini, 'gemini']
]);

/** The model name the upstream receives for a route slug. */
export function upstreamModelOf(slug) {
  const vendor = vendors.get(slug);
  assert.ok(vendor, `${slug} is not a route of the harness`);
  return upstreamModels[vendor];
}

const bearer = { header: 'authorization', value: (allowProviderState) => `Bearer ${allowProviderState ? stateApiKey : apiKey}` };
const anthropicKey = { header: 'x-api-key', value: (allowProviderState) => allowProviderState ? stateApiKey : apiKey };
const googleKey = { header: 'x-goog-api-key', value: (allowProviderState) => allowProviderState ? stateApiKey : apiKey };

/**
 * The wire APIs a client can speak and the upstream records, by the dialect
 * names the scripted upstream uses. `path` is where a client sends it, and
 * `upstream` where the vendor receives it.
 */
const apis = {
  'openai.chat': {
    path: () => '/v1/chat/completions',
    upstream: () => '/openai/v1/chat/completions',
    credential: bearer
  },
  'openai.responses': {
    path: () => '/v1/responses',
    upstream: () => '/openai/v1/responses',
    credential: bearer
  },
  'openai.embeddings': {
    path: () => '/v1/embeddings',
    upstream: () => '/openai/v1/embeddings',
    credential: bearer
  },
  'anthropic.messages': {
    path: () => '/anthropic/v1/messages',
    upstream: () => '/anthropic/v1/messages',
    credential: anthropicKey
  },
  'gemini.generate': {
    path: (model) => `/gemini/v1beta/models/${model}:generateContent`,
    upstream: (model) => `/gemini/v1beta/models/${model}:generateContent`,
    credential: googleKey
  },
  'gemini.stream': {
    path: (model) => `/gemini/v1beta/models/${model}:streamGenerateContent`,
    upstream: (model) => `/gemini/v1beta/models/${model}:streamGenerateContent`,
    query: 'alt=sse',
    stream: true,
    credential: googleKey
  },
  'gemini.embed': {
    path: (model) => `/gemini/v1beta/models/${model}:embedContent`,
    upstream: (model) => `/gemini/v1beta/models/${model}:embedContent`,
    credential: googleKey
  },
  'gemini.batch_embed': {
    path: (model) => `/gemini/v1beta/models/${model}:batchEmbedContents`,
    upstream: (model) => `/gemini/v1beta/models/${model}:batchEmbedContents`,
    credential: googleKey
  }
};

/**
 * What the gateway adds to a streamed request on purpose, by API: a transport
 * option that makes the provider report usage, which the gateway accounts for
 * and the client may ignore. Nothing else may differ.
 */
const transportOptions = {
  'openai.chat': ['/stream_options: absent -> {"include_usage":true}']
};

/** Model binding and the fixture key's no-retention policy are declared rewrites. */
function relayedBody(api, body, upstreamModel, route, allowProviderState) {
  const expected = structuredClone(body);
  if (typeof expected?.model === 'string') expected.model = upstreamModel;
  // tests/clientfixture creates this key without allow_provider_state. Only
  // transformed Responses can normalize omitted/null storage to false; strict
  // requests must state false themselves. Require false rather than ignoring it.
  if (!allowProviderState && api === 'openai.responses' && route !== models.openaiStrict && (expected.store === undefined || expected.store === null)) expected.store = false;
  if (api === 'gemini.batch_embed') {
    for (const request of expected.requests ?? []) request.model = `models/${upstreamModel}`;
  }
  if (api === 'gemini.embed' && typeof expected.model === 'string') expected.model = `models/${upstreamModel}`;
  return expected;
}

/** Every difference between two JSON values, as `pointer: before -> after`. */
export function differences(before, after, pointer = '') {
  if (JSON.stringify(before) === JSON.stringify(after)) return [];
  const isObject = (value) => value !== null && typeof value === 'object';
  if (isObject(before) && isObject(after) && Array.isArray(before) === Array.isArray(after)) {
    const out = [];
    for (const key of new Set([...Object.keys(before), ...Object.keys(after)])) {
      if (!(key in before)) out.push(`${pointer}/${key}: absent -> ${JSON.stringify(after[key])}`);
      else if (!(key in after)) out.push(`${pointer}/${key}: ${JSON.stringify(before[key])} -> absent`);
      else out.push(...differences(before[key], after[key], `${pointer}/${key}`));
    }
    return out;
  }
  return [`${pointer}: ${JSON.stringify(before)} -> ${JSON.stringify(after)}`];
}

/**
 * Assert the client requests of a test and the upstream requests they became.
 * `expected` has one entry per request in arrival order:
 *
 * - `api`: the wire API the client should have used, a key of {@link apis}.
 * - `model`: the route slug the client addressed.
 * - `to`: the wire API the upstream should have received; `api` when the route
 *   keeps the client's dialect, in which case the body must also be unchanged
 *   but for the model, the declared no-retention normalization on transformed
 *   Responses, and the transport options in `transportOptions`.
 * - `allowProviderState`: use the fixture's state-enabled key and preserve native storage semantics.
 * - `stream`: whether the exchange streams; the API decides when omitted.
 * - `status`: the status the client saw, 200 unless given.
 * - `tools`: the tool names the upstream should have been offered.
 * - `contains`: strings the upstream request body must contain.
 * - `maxTokens`, `temperature`: generation parameters the upstream should have received.
 *
 * `upstream` takes the upstream's recording, `recorded()` when omitted; the
 * harness tests hold the assertions against recordings they build by hand.
 *
 * Returns `{ sent, upstream }`, the matching records, for further assertions.
 */
export async function assertRelayed(tap, expected, { upstream: recording } = {}) {
  const sent = await tap.sent();
  const upstream = recording ?? (await recorded());
  assert.equal(sent.length, expected.length, `client requests: ${sent.map((s) => `${s.method} ${s.path}`)}`);
  assert.equal(upstream.length, expected.length, `upstream requests: ${upstream.map((u) => u.path)}`);

  expected.forEach((want, index) => {
    const label = `request ${index + 1} (${want.api})`;
    const from = apis[want.api];
    const to = apis[want.to ?? want.api];
    assert.ok(from && to, `${label}: unknown API`);
    const upstreamModel = upstreamModelOf(want.model);
    const stream = want.stream ?? from.stream ?? false;
    const s = sent[index];
    const u = upstream[index];

    // The client: where it went, with which credential, what it was answered.
    assert.equal(s.method, 'POST', label);
    assert.equal(s.path, from.path(want.model), `${label}: client path`);
    assert.equal(s.query, from.query ?? '', `${label}: client query`);
    assert.equal(s.headers[from.credential.header], from.credential.value(want.allowProviderState), `${label}: the client authenticates with the OLP key`);
    assert.equal(s.status, want.status ?? 200, `${label}: client status: ${s.responseText.slice(0, 300)}`);
    assert.ok(s.responseHeaders['x-request-id'], `${label}: the answer carries the gateway's request id`);
    if ((want.status ?? 200) === 200) {
      const framing = /text\/event-stream/.test(s.responseHeaders['content-type'] ?? '');
      assert.equal(framing, stream, `${label}: response framing is ${s.responseHeaders['content-type']}`);
    }

    // The upstream: where it arrived, under which credential and model.
    assert.equal(u.dialect, want.to ?? want.api, `${label}: upstream dialect`);
    assert.equal(u.path, to.upstream(upstreamModel), `${label}: upstream path`);
    assert.equal(u.model, upstreamModel, `${label}: upstream model`);
    assert.equal(u.authorized, true, `${label}: the upstream accepted the gateway's credential`);
    assert.equal(u.leaked_client_credential, false, `${label}: no caller credential reaches the upstream`);
    assert.equal(u.headers[to.credential.header], '[present]', `${label}: the upstream credential header`);
    assert.equal(u.status, want.upstreamStatus ?? want.status ?? 200, `${label}: upstream status`);
    assert.equal(u.stream, stream, `${label}: upstream streaming`);

    if (want.to === undefined || want.to === want.api) {
      const added = stream ? transportOptions[want.api] ?? [] : [];
      const unexpected = differences(relayedBody(want.api, s.body, upstreamModel, want.model, want.allowProviderState), u.body).filter((d) => !added.includes(d));
      assert.deepEqual(unexpected, [], `${label}: the gateway changed the request`);
    }
    if (want.tools !== undefined) {
      const declared = toolDeclarations(want.to ?? want.api, u.body);
      assert.deepEqual(declared.map((tool) => tool.name), want.tools, `${label}: tools offered upstream`);
      // Gemini's own schema dialect spells types in capitals; every other vendor wants JSON Schema.
      if (!(want.to ?? want.api).startsWith('gemini')) {
        for (const tool of declared) assertJSONSchemaTypes(tool.schema, `${label}: schema of ${tool.name}`);
      }
    }
    const text = JSON.stringify(u.body);
    for (const part of want.contains ?? []) assert.ok(text.includes(part), `${label}: the upstream request lacks ${JSON.stringify(part)}`);
    const params = generationParameters(want.to ?? want.api, u.body);
    if (want.maxTokens !== undefined) assert.equal(params.maxTokens, want.maxTokens, `${label}: max tokens`);
    if (want.temperature !== undefined) assert.equal(params.temperature, want.temperature, `${label}: temperature`);
  });
  return { sent, upstream };
}

/** The tools a request declares, as `{name, description, schema}` whatever the dialect. */
export function toolDeclarations(api, body) {
  switch (api) {
    case 'openai.chat':
      return (body.tools ?? []).map((t) => ({ name: t.function.name, description: t.function.description, schema: t.function.parameters }));
    case 'openai.responses':
      return (body.tools ?? []).filter((t) => t.type === 'function').map((t) => ({ name: t.name, description: t.description, schema: t.parameters }));
    case 'anthropic.messages':
      return (body.tools ?? []).map((t) => ({ name: t.name, description: t.description, schema: t.input_schema }));
    case 'gemini.generate':
    case 'gemini.stream':
      return (body.tools ?? []).flatMap((t) => t.functionDeclarations ?? []).map((d) => ({ name: d.name, description: d.description, schema: d.parametersJsonSchema ?? d.parameters }));
    default:
      assert.fail(`${api} declares no tools`);
  }
}

/**
 * The tool declaration a vendor should receive for one a client declared: the
 * same name, description and schema. A Gemini client may spell the schema's
 * type names in capitals, its own dialect, and the others get JSON Schema.
 */
export function declaredForUpstream(clientApi, declaration) {
  return clientApi.startsWith('gemini') ? { ...declaration, schema: jsonSchemaFromGemini(declaration.schema) } : declaration;
}

/**
 * The JSON Schema a vendor other than Gemini should receive for a tool declared
 * in Gemini's schema dialect: the same schema with type names in lower case.
 */
function jsonSchemaFromGemini(schema) {
  if (Array.isArray(schema)) return schema.map(jsonSchemaFromGemini);
  if (schema === null || typeof schema !== 'object') return schema;
  return Object.fromEntries(
    Object.entries(schema).map(([key, value]) => {
      if (key === 'type' && typeof value === 'string') return [key, value.toLowerCase()];
      if (key === 'properties') return [key, Object.fromEntries(Object.entries(value).map(([name, property]) => [name, jsonSchemaFromGemini(property)]))];
      if (key === 'enum' || key === 'const' || key === 'default' || key === 'required') return [key, value];
      return [key, jsonSchemaFromGemini(value)];
    })
  );
}

/** Assert every `type` in a schema is a JSON Schema type name, in lower case. */
export function assertJSONSchemaTypes(schema, message, pointer = '') {
  if (Array.isArray(schema)) return schema.forEach((item, i) => assertJSONSchemaTypes(item, message, `${pointer}/${i}`));
  if (schema === null || typeof schema !== 'object') return;
  const types = ['string', 'number', 'integer', 'boolean', 'array', 'object', 'null'];
  if (typeof schema.type === 'string') assert.ok(types.includes(schema.type), `${message}: ${pointer}/type is ${JSON.stringify(schema.type)}`);
  for (const [key, value] of Object.entries(schema)) {
    // Property names are free text, and `properties` maps them to schemas.
    if (key === 'properties') Object.entries(value).forEach(([name, property]) => assertJSONSchemaTypes(property, message, `${pointer}/properties/${name}`));
    else if (key !== 'type' && key !== 'enum' && key !== 'const' && key !== 'default' && key !== 'required') assertJSONSchemaTypes(value, message, `${pointer}/${key}`);
  }
}

/** A one-pixel PNG for tests that send an image. */
export const png = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==', 'base64');

/** The images a request carries, as `{mediaType, data}` with base64 data, whatever the dialect. */
export function images(api, body) {
  const fromDataURL = (url) => {
    const [, mediaType, data] = /^data:([^;]+);base64,(.*)$/s.exec(url) ?? assert.fail(`not a base64 data URL: ${String(url).slice(0, 40)}`);
    return { mediaType, data };
  };
  switch (api) {
    case 'openai.chat':
      return body.messages.flatMap((m) => (Array.isArray(m.content) ? m.content : [])).filter((p) => p.type === 'image_url').map((p) => fromDataURL(p.image_url.url));
    case 'openai.responses':
      return body.input.flatMap((m) => (Array.isArray(m.content) ? m.content : [])).filter((p) => p.type === 'input_image').map((p) => fromDataURL(p.image_url));
    case 'anthropic.messages':
      return body.messages.flatMap((m) => (Array.isArray(m.content) ? m.content : [])).filter((p) => p.type === 'image').map((p) => ({ mediaType: p.source.media_type, data: p.source.data }));
    case 'gemini.generate':
    case 'gemini.stream':
      return body.contents.flatMap((c) => c.parts).filter((p) => p.inlineData).map((p) => ({ mediaType: p.inlineData.mimeType, data: p.inlineData.data }));
    default:
      assert.fail(`${api} carries no images`);
  }
}

/** The generation parameters a request carries, whatever the dialect. */
export function generationParameters(api, body) {
  switch (api) {
    case 'openai.chat':
      return { maxTokens: body.max_completion_tokens ?? body.max_tokens, temperature: body.temperature };
    case 'openai.responses':
      return { maxTokens: body.max_output_tokens, temperature: body.temperature };
    case 'anthropic.messages':
      return { maxTokens: body.max_tokens, temperature: body.temperature };
    case 'gemini.generate':
    case 'gemini.stream':
      return { maxTokens: body.generationConfig?.maxOutputTokens, temperature: body.generationConfig?.temperature };
    default:
      return {};
  }
}

/**
 * The embeddings the gateway returns for these texts, fetched with plain HTTP.
 * The scripted upstream derives a vector from the text alone, so a client that
 * returns these vectors, in this order, received what the upstream sent.
 * Fetch them before a test body, since this is an upstream request itself.
 */
export async function referenceEmbeddings(texts, dimensions) {
  const response = await localFetch(`${baseURLs.openai}/embeddings`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', authorization: `Bearer ${apiKey}` },
    body: JSON.stringify({ model: models.openai, input: texts, ...(dimensions && { dimensions }) })
  });
  assert.equal(response.status, 200);
  const { data } = await response.json();
  await resetRecorded();
  return data.map((item) => item.embedding);
}

/** Assert two embeddings are the same vector to float32 precision. */
export function assertSameVector(actual, expected, message) {
  assert.equal(actual.length, expected.length, `${message}: dimension`);
  actual.forEach((value, i) => assert.ok(Math.abs(value - expected[i]) < 1e-6, `${message}: component ${i} is ${value}, expected ${expected[i]}`));
}

/** The text of a content value that is a string or an array of text parts. */
export function textOf(content) {
  if (typeof content === 'string') return content;
  return (content ?? []).map((part) => (typeof part === 'string' ? part : part.text ?? '')).join('');
}
