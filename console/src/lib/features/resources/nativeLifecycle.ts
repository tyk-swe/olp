import { getProviderResourceContracts } from './api';
import { gatewayFetch } from '$lib/api/gateway';
import {
  nativeObject,
  parseNativeJSON,
  stringifyNativeJSON
} from '$lib/json/nativeJson';
import { agentSubmissionFingerprint, ResourceRequestError } from './lifecycle';

export type NativeEndpoint = {
  operation: string;
  surface: string;
  kind: string;
  action: string;
  method: 'GET' | 'POST' | 'PATCH' | 'DELETE';
  pattern: string;
  ancestors: string[];
  metered: boolean;
};
export type NativeResourceRequest = {
  endpoint: NativeEndpoint;
  apiKey: string;
  route: string;
  id: string;
  parents: string[];
  body: string;
  query: string;
  multipart: boolean;
  files: File[];
  submissionKey?: string;
  signal?: AbortSignal;
};
export type NativeResourceResult = {
  text: string;
  resourceId: string | null;
  download?: Blob;
};

export const nativeResourceByteLimit = 4 * 1024 * 1024;

const roots: Record<string, string> = {
  'fine_tuning/openai': '/v1/fine_tuning/jobs',
  'fine_tuning/native': '/native/vertex-tuning/jobs',
  'vector_store/openai': '/v1/vector_stores',
  'conversation/openai': '/v1/conversations',
  'container/openai': '/v1/containers',
  'eval/openai': '/v1/evals',
  'skill/anthropic': '/anthropic/v1/skills',
  'context_cache/gemini': '/gemini/v1beta/cachedContents'
};
const actionMethods: Record<string, NativeEndpoint['method']> = {
  create: 'POST',
  list: 'GET',
  get: 'GET',
  update: 'POST',
  patch: 'PATCH',
  delete: 'DELETE',
  cancel: 'POST',
  content: 'GET'
};
const kindPattern = /^[a-z][a-z0-9_]{0,63}$/;

function validEndpoint(endpoint: NativeEndpoint): boolean {
  const root = roots[`${endpoint.operation}/${endpoint.surface}`];
  if (
    !root ||
    typeof endpoint.action !== 'string' ||
    !['GET', 'POST', 'PATCH', 'DELETE'].includes(endpoint.method) ||
    actionMethods[endpoint.action] !== endpoint.method ||
    typeof endpoint.kind !== 'string' ||
    !kindPattern.test(endpoint.kind) ||
    !Array.isArray(endpoint.ancestors) ||
    typeof endpoint.metered !== 'boolean' ||
    endpoint.ancestors.length > 2 ||
    !endpoint.ancestors.every(
      (kind) => typeof kind === 'string' && kindPattern.test(kind)
    ) ||
    typeof endpoint.pattern !== 'string' ||
    !(endpoint.pattern === root || endpoint.pattern.startsWith(`${root}/`))
  )
    return false;
  const segments = endpoint.pattern.split('/').slice(1);
  if (
    !segments.every((segment) =>
      /^(?:[A-Za-z0-9_-]+|\{(?:id|parent[01])\})$/.test(segment)
    )
  )
    return false;
  const placeholders = segments.filter((segment) => segment.startsWith('{'));
  const expected = endpoint.ancestors.map((_, index) => `{parent${index}}`);
  if (
    ['get', 'update', 'patch', 'delete', 'cancel', 'content'].includes(
      endpoint.action
    )
  )
    expected.push('{id}');
  return (
    placeholders.length === expected.length &&
    expected.every(
      (name) => placeholders.filter((value) => value === name).length === 1
    )
  );
}

/** The server registry owns which lifecycle methods appear. Older servers
 * without endpoint metadata expose no guessed or synthesized native actions. */
export async function listNativeEndpoints(
  signal?: AbortSignal
): Promise<NativeEndpoint[]> {
  const contracts = await getProviderResourceContracts(signal);
  const endpoints = (contracts as unknown as { endpoints?: unknown }).endpoints;
  if (!Array.isArray(endpoints)) return [];
  return endpoints
    .filter((value): value is NativeEndpoint => {
      if (!value || typeof value !== 'object') return false;
      const candidate = {
        ...value,
        ancestors: value.ancestors ?? []
      } as NativeEndpoint;
      return validEndpoint(candidate);
    })
    .map((value) => ({ ...value, ancestors: value.ancestors ?? [] }));
}

function gatewayId(kind: string, value: string): string {
  const id = value.trim();
  if (!kindPattern.test(kind) || !new RegExp(`^${kind}_[a-f0-9]{32}$`).test(id))
    throw new Error(`Enter a gateway ${kind.replaceAll('_', ' ')} ID.`);
  return id;
}

export function nativeResourcePath(
  endpoint: NativeEndpoint,
  id: string,
  parents: string[]
): string {
  if (!validEndpoint(endpoint))
    throw new Error('Choose a registered lifecycle endpoint.');
  let path = endpoint.pattern;
  endpoint.ancestors.forEach((kind, index) => {
    path = path.replace(
      `{parent${index}}`,
      encodeURIComponent(gatewayId(kind, parents[index] ?? ''))
    );
  });
  if (path.includes('{id}'))
    path = path.replace(
      '{id}',
      encodeURIComponent(gatewayId(endpoint.kind, id))
    );
  return path;
}

function queryParameters(raw: string): URLSearchParams {
  const value = parseNativeJSON(raw.trim() || '{}');
  if (!nativeObject(value))
    throw new Error('Query parameters must be a JSON object of string values.');
  const params = new URLSearchParams();
  for (const name of Object.keys(value).sort()) {
    if (!name || /[\r\n]/.test(name) || name.includes('\0'))
      throw new Error('Enter a valid query parameter name.');
    const item = value[name];
    const values = Array.isArray(item) ? item : [item];
    for (const part of values) {
      if (typeof part !== 'string')
        throw new Error(
          'Query parameter values must be strings or arrays of strings.'
        );
      params.append(name, part);
    }
  }
  if (params.toString().length > 16384)
    throw new Error('Query parameters are too large.');
  return params;
}

function requestBody(
  request: NativeResourceRequest
): string | FormData | undefined {
  const endpoint = request.endpoint;
  if (endpoint.method !== 'POST' && endpoint.method !== 'PATCH')
    return undefined;
  if (endpoint.action === 'cancel' && !request.body.trim()) return undefined;
  const value = parseNativeJSON(request.body.trim() || '{}');
  if (!nativeObject(value))
    throw new Error('The resource body must be a JSON object.');
  const raw = stringifyNativeJSON(value);
  if (new TextEncoder().encode(raw).byteLength > nativeResourceByteLimit)
    throw new Error('The resource body is too large.');
  if (!request.multipart) {
    if (endpoint.operation === 'skill' && endpoint.action === 'create')
      throw new Error('Skill creation requires multipart files.');
    return raw;
  }
  if (
    endpoint.action !== 'create' ||
    (endpoint.operation !== 'skill' && endpoint.kind !== 'container_file')
  )
    throw new Error('This endpoint does not accept multipart file uploads.');
  if (!request.files.length || request.files.length > 64)
    throw new Error('Choose 1–64 files to upload.');
  if (
    endpoint.kind === 'container_file' &&
    (request.files.length !== 1 || Object.keys(value).length)
  )
    throw new Error(
      'A container upload requires one file and an empty JSON body.'
    );
  const total = request.files.reduce((size, file) => {
    const name = file.webkitRelativePath || file.name;
    if (
      !name ||
      name.length > 4096 ||
      file.type.length > 256 ||
      /[\r\n]/.test(name) ||
      name.includes('\0')
    )
      throw new Error('Choose files with bounded, valid filenames.');
    return size + file.size + new TextEncoder().encode(name).byteLength + 512;
  }, new TextEncoder().encode(raw).byteLength);
  if (total > nativeResourceByteLimit)
    throw new Error('Uploads are limited to 4 MiB in this console.');
  const form = new FormData();
  for (const [name, field] of Object.entries(value)) {
    if (name === 'file' || name === 'files[]' || typeof field !== 'string')
      throw new Error(
        'Multipart metadata must contain string fields; choose files using the file picker.'
      );
    form.append(name, field);
  }
  for (const file of request.files)
    form.append(
      endpoint.kind === 'container_file' ? 'file' : 'files[]',
      file,
      file.webkitRelativePath || file.name
    );
  return form;
}

function requestParts(request: NativeResourceRequest) {
  if (!request.apiKey.trim()) throw new Error('Enter an inference API key.');
  const endpoint = request.endpoint;
  const base = nativeResourcePath(endpoint, request.id, request.parents);
  if (
    endpoint.action === 'create' &&
    !endpoint.ancestors.length &&
    !request.route.trim()
  )
    throw new Error('Choose a published route for resource creation.');
  const query = queryParameters(request.query);
  return {
    path: base + (query.size ? `?${query}` : ''),
    body: requestBody(request)
  };
}

/** Bind one mutation identity to exact content, parents, route and query. JSON
 * key order and multipart boundary changes do not create a new submission. */
export async function nativeResourceFingerprint(
  request: NativeResourceRequest
): Promise<string> {
  const { path, body } = requestParts(request);
  const files = [];
  if (body instanceof FormData)
    for (const file of request.files) {
      const digest = await crypto.subtle.digest(
        'SHA-256',
        await file.arrayBuffer()
      );
      files.push([
        file.webkitRelativePath || file.name,
        file.type,
        file.size,
        Array.from(new Uint8Array(digest), (byte) =>
          byte.toString(16).padStart(2, '0')
        ).join('')
      ]);
    }
  return JSON.stringify([
    request.endpoint.method,
    path,
    request.route.trim(),
    body === undefined
      ? ''
      : agentSubmissionFingerprint('', request.body.trim() || '{}'),
    files
  ]);
}

async function readBoundedResource(response: Response): Promise<Uint8Array> {
  const reader = response.body?.getReader();
  if (!reader) return new Uint8Array();
  const chunks: Uint8Array[] = [];
  let bytes = 0;
  try {
    for (;;) {
      const chunk = await reader.read();
      if (chunk.done) break;
      bytes += chunk.value.byteLength;
      if (bytes > nativeResourceByteLimit) {
        await reader.cancel();
        throw new Error(
          'The resource response exceeds the 4 MiB console limit.'
        );
      }
      chunks.push(chunk.value);
    }
  } finally {
    reader.releaseLock();
  }
  const content = new Uint8Array(bytes);
  let offset = 0;
  for (const chunk of chunks) {
    content.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return content;
}

/** No retries, management cookies or persistence. File content is returned as
 * a bounded Blob for browser download rather than converted to base64/text. */
export async function runNativeResourceRequest(
  request: NativeResourceRequest
): Promise<NativeResourceResult> {
  const { path, body } = requestParts(request);
  const headers: Record<string, string> = {
    Authorization: `Bearer ${request.apiKey.trim()}`
  };
  if (request.route.trim()) headers['X-OLP-Route'] = request.route.trim();
  if (request.endpoint.method !== 'GET') {
    if (!request.submissionKey)
      throw new Error('A mutation identity is required.');
    headers['Idempotency-Key'] = request.submissionKey;
  }
  if (typeof body === 'string') headers['Content-Type'] = 'application/json';
  const response = await gatewayFetch(path, {
    method: request.endpoint.method,
    headers,
    body,
    signal: request.signal
  });
  const bytes = await readBoundedResource(response);
  const resourceId = response.headers.get('X-OLP-Resource-ID');
  if (!response.ok)
    throw new ResourceRequestError(
      `Resource request failed (HTTP ${response.status}). ${new TextDecoder().decode(bytes.slice(0, 2048))}`,
      resourceId
    );
  if (request.endpoint.action === 'content')
    return {
      text: '',
      resourceId,
      download: new Blob([bytes as BlobPart], {
        type: 'application/octet-stream'
      })
    };
  return { text: new TextDecoder().decode(bytes), resourceId };
}
