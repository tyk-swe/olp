import { gatewayFetch } from '#lib/management/gateway.js';
import type { components } from '#lib/management/schema.js';
import {
  nativeObject,
  replaceNative,
  stringifyNativeJSON,
  type NativeObject,
  type NativeValue
} from '#lib/nativeJson.js';
import {
  fail,
  MAX_PUBLIC_REQUEST_BYTES,
  MAX_PUBLIC_RESPONSE_BYTES,
  parseNativeValue,
  publicGatewayHeaders,
  unwrapOutcome,
  utf8ByteLength,
  validateApiKey,
  validatePublicRoute,
  type Outcome
} from './publicGateway';

type SimulationDialect = components['schemas']['SimulationDialect'];

const nativeOperationDialects = {
  completions: ['openai-completions'],
  decision: ['openai-decisions', 'systemone', 'gemini-decisions'],
  embeddings: [
    'openai-embeddings',
    'voyage-embeddings',
    'cohere-embed-v2',
    'tei-embeddings',
    'tei-sparse-embeddings',
    'tei-multivector-embeddings',
    'gemini-embeddings',
    'gemini-batch-embeddings',
    'vertex-embeddings',
    'bedrock-embeddings'
  ],
  rerank: ['rerank', 'voyage-rerank', 'tei-rerank', 'cohere-rerank-v2'],
  moderation: ['openai-moderation'],
  classification: ['tei-classification'],
  scoring: ['tei-scoring'],
  token_count: [
    'openai-input-tokens',
    'anthropic-count-tokens',
    'gemini-count-tokens',
    'bedrock-count-tokens',
    'tei-tokenize'
  ],
  ocr: [
    'mistral-ocr',
    'azure-document-intelligence-ocr',
    'vertex-document-processing-ocr'
  ],
  search: [
    'perplexity-search',
    'tavily-search',
    'exa-search',
    'parallel-search',
    'brave-search'
  ]
} as const satisfies Record<string, readonly SimulationDialect[]>;

export const publicNativeOperations: ReadonlySet<string> = new Set([
  'decision',
  'ocr',
  'search'
]);

export type NativeOperation = keyof typeof nativeOperationDialects;

const bodyModelDialects: ReadonlySet<string> = new Set([
  'openai-completions',
  'openai-decisions',
  'gemini-decisions',
  'systemone',
  'openai-embeddings',
  'voyage-embeddings',
  'cohere-embed-v2',
  'rerank',
  'voyage-rerank',
  'cohere-rerank-v2',
  'openai-moderation',
  'openai-input-tokens',
  'anthropic-count-tokens',
  'mistral-ocr',
  'azure-document-intelligence-ocr'
] satisfies SimulationDialect[]);

const publicPaths: Readonly<Record<string, string | undefined>> = {
  'openai-completions': '/v1/completions',
  'openai-decisions': '/v1/decisions',
  'gemini-decisions': '/gemini/v1beta/decisions',
  systemone: '/v1/systemone',
  'mistral-ocr': '/v1/ocr',
  'perplexity-search': '/v1/search'
} satisfies Partial<Record<SimulationDialect, string>>;

const routeCarriedDialects: ReadonlySet<string> = new Set(
  nativeOperationDialects.search
);

function isNativeOperation(value: string): value is NativeOperation {
  return Object.hasOwn(nativeOperationDialects, value);
}

export function nativeDialects(operation: string): readonly string[] {
  return isNativeOperation(operation) ? nativeOperationDialects[operation] : [];
}

export type NativeOperationResult = {
  request: NativeObject;
  response: NativeValue;
  raw: string;
};

export function nativeOperationRequest(
  source: string,
  route: string,
  dialect: string
): Outcome<NativeObject> {
  const parsed = parseNativeValue(
    source,
    'The native operation request must be valid JSON.'
  );
  if (!parsed.ok) return parsed;
  if (!nativeObject(parsed.value))
    return fail('The native operation request must be a JSON object.');
  const model = parsed.value.model;
  if (model !== undefined && routeCarriedDialects.has(dialect))
    return fail(
      'Search requests carry no model field; the route goes in X-OLP-Route or the native path.'
    );
  if (model !== undefined && model !== '' && model !== route)
    return fail('The native request names another route.');
  if (model === '' && bodyModelDialects.has(dialect)) {
    const bound = replaceNative(parsed.value, ['model'], route);
    return nativeObject(bound)
      ? { ok: true, value: bound }
      : fail('The native operation request must be a JSON object.');
  }
  if (model === '')
    return fail(
      'This URL-bound native dialect does not use a body model; remove the empty field.'
    );
  return { ok: true, value: parsed.value };
}

function prepareNativeOperation(
  operation: NativeOperation,
  dialect: string,
  route: string,
  key: string,
  source: string
): Outcome<{ request: NativeObject; body: string; headers: Headers }> {
  if (!nativeDialects(operation).includes(dialect))
    return fail('Choose a registered native dialect for this operation.');
  const validRoute = validatePublicRoute(route);
  if (!validRoute.ok) return validRoute;
  const validKey = validateApiKey(key);
  if (!validKey.ok) return validKey;
  const request = nativeOperationRequest(source, validRoute.value, dialect);
  if (!request.ok) return request;
  if (operation === 'completions' && request.value.stream === true)
    return fail(
      'This public JSON form supports unary completions. Use the SDK for streaming.'
    );
  const body = stringifyNativeJSON(request.value);
  if (utf8ByteLength(body) > MAX_PUBLIC_REQUEST_BYTES)
    return fail('The native operation request exceeds the public body limit.');
  const headers = publicGatewayHeaders(validKey.value, {
    'Content-Type': 'application/json',
    Accept: 'application/json',
    ...(operation === 'embeddings'
      ? { 'X-OLP-Client-Contract': 'raw-vector-storage/1' }
      : {}),
    ...(dialect === 'perplexity-search'
      ? { 'X-OLP-Route': validRoute.value }
      : {})
  });
  return { ok: true, value: { request: request.value, body, headers } };
}

export async function runNativeOperation(
  operation: NativeOperation,
  dialect: string,
  route: string,
  key: string,
  source: string,
  signal?: AbortSignal
): Promise<NativeOperationResult> {
  const prepared = unwrapOutcome(
    prepareNativeOperation(operation, dialect, route, key, source)
  );
  const response = await gatewayFetch(
    publicPaths[dialect] ??
      `/native/${dialect}/models/${encodeURIComponent(route)}`,
    {
      method: 'POST',
      headers: prepared.headers,
      body: prepared.body,
      signal
    }
  );
  if (!response.ok)
    throw new Error(
      `The public operation was not admitted (${response.status}). Check the route, key and native dialect.`
    );
  const raw = await response.text();
  if (utf8ByteLength(raw) > MAX_PUBLIC_RESPONSE_BYTES)
    throw new Error('The native operation result exceeds the browser limit.');
  const parsed = parseNativeValue(
    raw,
    'The provider returned an invalid native JSON result.',
    MAX_PUBLIC_RESPONSE_BYTES
  );
  if (!parsed.ok)
    throw new Error('The provider returned an invalid native JSON result.');
  return { request: prepared.request, response: parsed.value, raw };
}
