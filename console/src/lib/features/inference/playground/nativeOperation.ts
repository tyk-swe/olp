import { gatewayFetch } from '$lib/api/gateway';
import {
  nativeObject,
  parseNativeJSON,
  replaceNative,
  stringifyNativeJSON,
  type NativeObject,
  type NativeValue
} from '$lib/json/nativeJson';

export const nativeOperationDialects = {
  completions: ['openai-completions'],
  decision: ['openai-decisions', 'systemone'],
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
  ocr: ['mistral-ocr', 'vertex-document-processing-ocr'],
  search: [
    'perplexity-search',
    'tavily-search',
    'exa-search',
    'parallel-search',
    'brave-search'
  ]
} as const;

/** Operations that serve publicly on strict and transformed routes: they need
 * the public native client rather than the negotiated composer. */
export const publicNativeOperations: ReadonlySet<string> = new Set([
  'decision',
  'ocr',
  'search'
]);

export type NativeOperation = keyof typeof nativeOperationDialects;
const bodyModelDialects = new Set([
  'openai-completions',
  'openai-decisions',
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
  'mistral-ocr'
]);

// Search dialects carry the route in a header or the native path, never a
// body model field.
const routeCarriedDialects: ReadonlySet<string> = new Set(
  nativeOperationDialects.search
);

export function nativeDialects(operation: string): readonly string[] {
  return Object.hasOwn(nativeOperationDialects, operation)
    ? nativeOperationDialects[operation as NativeOperation]
    : [];
}

export type NativeOperationResult = {
  request: NativeObject;
  response: NativeValue;
  raw: string;
};

/** The UI advertises only code-registered dialects. Route identity in a
 * model-bearing native body is bound from the chosen public slug. URL-bound
 * bodies such as TEI remain source-exact. */
export function nativeOperationRequest(
  source: string,
  route: string,
  dialect: string
): NativeObject {
  const parsed = parseNativeJSON(source);
  if (!nativeObject(parsed))
    throw new Error('The native operation request must be a JSON object.');
  const model = parsed.model;
  if (model !== undefined && routeCarriedDialects.has(dialect))
    throw new Error(
      'Search requests carry no model field; the route goes in X-OLP-Route or the native path.'
    );
  if (model !== undefined && model !== '' && model !== route)
    throw new Error('The native request names another route.');
  if (model === '' && bodyModelDialects.has(dialect))
    return replaceNative(parsed, ['model'], route) as NativeObject;
  if (model === '')
    throw new Error(
      'This URL-bound native dialect does not use a body model; remove the empty field.'
    );
  return parsed;
}

export async function runNativeOperation(
  operation: NativeOperation,
  dialect: string,
  route: string,
  key: string,
  source: string,
  signal?: AbortSignal
): Promise<NativeOperationResult> {
  if (!nativeDialects(operation).includes(dialect))
    throw new Error('Choose a registered native dialect for this operation.');
  if (!/^[a-z0-9][a-z0-9._-]{0,127}$/.test(route))
    throw new Error('Choose an active published route slug.');
  if (!key.startsWith('olp_') || key.length > 512)
    throw new Error('Enter an inference API key for this route.');
  const request = nativeOperationRequest(source, route, dialect);
  if (operation === 'completions' && request.stream === true)
    throw new Error(
      'This public JSON form supports unary completions. Use the SDK for streaming.'
    );
  const body = stringifyNativeJSON(request);
  if (new TextEncoder().encode(body).byteLength > 1 << 20)
    throw new Error(
      'The native operation request exceeds the public body limit.'
    );
  const headers = new Headers({
    Authorization: `Bearer ${key}`,
    'Content-Type': 'application/json',
    Accept: 'application/json',
    'Cache-Control': 'no-store'
  });
  if (operation === 'embeddings')
    headers.set('X-OLP-Client-Contract', 'raw-vector-storage/1');
  if (dialect === 'perplexity-search') headers.set('X-OLP-Route', route);
  const response = await gatewayFetch(
    operation === 'completions'
      ? '/v1/completions'
      : operation === 'decision'
        ? dialect === 'systemone'
          ? '/v1/systemone'
          : '/v1/decisions'
        : operation === 'ocr' && dialect === 'mistral-ocr'
          ? '/v1/ocr'
          : dialect === 'perplexity-search'
            ? '/v1/search'
            : `/native/${dialect}/models/${encodeURIComponent(route)}`,
    {
      method: 'POST',
      headers,
      body,
      signal
    }
  );
  if (!response.ok)
    throw new Error(
      `The public operation was not admitted (${response.status}). Check the route, key and native dialect.`
    );
  const raw = await response.text();
  if (new TextEncoder().encode(raw).byteLength > 8 << 20)
    throw new Error('The native operation result exceeds the browser limit.');
  let document: NativeValue;
  try {
    document = parseNativeJSON(raw);
  } catch {
    throw new Error('The provider returned an invalid native JSON result.');
  }
  return { request, response: document, raw };
}
