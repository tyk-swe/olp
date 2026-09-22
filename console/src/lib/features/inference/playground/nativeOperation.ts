import {
  nativeObject,
  parseNativeJSON,
  replaceNative,
  stringifyNativeJSON,
  type NativeObject,
  type NativeValue
} from '$lib/json/nativeJson';

export const nativeOperationDialects = {
  embeddings: [
    'openai-embeddings',
    'voyage-embeddings',
    'tei-embeddings',
    'tei-sparse-embeddings',
    'tei-multivector-embeddings',
    'gemini-embeddings',
    'gemini-batch-embeddings',
    'vertex-embeddings',
    'bedrock-embeddings'
  ],
  rerank: ['rerank', 'voyage-rerank', 'tei-rerank'],
  moderation: ['openai-moderation'],
  classification: ['tei-classification'],
  scoring: ['tei-scoring'],
  token_count: [
    'openai-input-tokens',
    'anthropic-count-tokens',
    'gemini-count-tokens',
    'bedrock-count-tokens',
    'tei-tokenize'
  ]
} as const;

export type NativeOperation = keyof typeof nativeOperationDialects;
const bodyModelDialects = new Set([
  'openai-embeddings',
  'voyage-embeddings',
  'rerank',
  'voyage-rerank',
  'openai-moderation',
  'openai-input-tokens',
  'anthropic-count-tokens'
]);

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
  const response = await fetch(
    `/native/${dialect}/models/${encodeURIComponent(route)}`,
    {
      method: 'POST',
      headers,
      body,
      cache: 'no-store',
      credentials: 'omit',
      redirect: 'error',
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
