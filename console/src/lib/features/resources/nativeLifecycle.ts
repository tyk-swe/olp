import { isJsonObject } from '#lib/editing/json.js';
import { gatewayFetch } from '#lib/management/gateway.js';
import { createHeaders } from '#lib/management/http.js';
import {
  nativeObject,
  parseNativeJSON,
  stringifyNativeJSON
} from '#lib/nativeJson.js';
import { getProviderResourceContracts } from './api';
import {
  agentSubmissionFingerprint,
  isGatewayResourceId,
  readBoundedResponseBytes,
  resourceByteLimit,
  resourceKindPattern,
  ResourceRequestError
} from './lifecycle';

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

export const nativeResourceByteLimit = resourceByteLimit;

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
const segmentPattern = /^(?:[A-Za-z0-9_-]+|\{(?:id|parent[01])\})$/;

function validEndpoint(endpoint: NativeEndpoint): boolean {
  const root = roots[`${endpoint.operation}/${endpoint.surface}`];
  if (
    !root ||
    actionMethods[endpoint.action] !== endpoint.method ||
    !resourceKindPattern.test(endpoint.kind) ||
    endpoint.ancestors.length > 2 ||
    !endpoint.ancestors.every((kind) => resourceKindPattern.test(kind)) ||
    !(endpoint.pattern === root || endpoint.pattern.startsWith(`${root}/`))
  )
    return false;
  const segments = endpoint.pattern.split('/').slice(1);
  if (!segments.every((segment) => segmentPattern.test(segment))) return false;
  const placeholders = segments.filter((segment) => segment.startsWith('{'));
  const expected = [
    ...endpoint.ancestors.map((_, index) => `{parent${index}}`),
    ...(endpoint.action !== 'create' && endpoint.action !== 'list'
      ? ['{id}']
      : [])
  ];
  return (
    placeholders.length === expected.length &&
    expected.every(
      (name) => placeholders.filter((value) => value === name).length === 1
    )
  );
}

function parseNativeEndpoint(value: unknown): NativeEndpoint | null {
  if (!isJsonObject(value)) return null;
  const { operation, surface, kind, action, method, pattern, metered } = value;
  const ancestors = value.ancestors ?? [];
  if (
    typeof operation !== 'string' ||
    typeof surface !== 'string' ||
    typeof kind !== 'string' ||
    typeof action !== 'string' ||
    typeof pattern !== 'string' ||
    typeof metered !== 'boolean' ||
    !Array.isArray(ancestors) ||
    !ancestors.every((item): item is string => typeof item === 'string')
  )
    return null;
  const expectedMethod = Object.hasOwn(actionMethods, action)
    ? actionMethods[action]
    : undefined;
  if (!expectedMethod || method !== expectedMethod) return null;
  const endpoint: NativeEndpoint = {
    operation,
    surface,
    kind,
    action,
    method: expectedMethod,
    pattern,
    ancestors,
    metered
  };
  return validEndpoint(endpoint) ? endpoint : null;
}

export async function listNativeEndpoints(
  signal?: AbortSignal
): Promise<NativeEndpoint[]> {
  const contracts = await getProviderResourceContracts(signal);
  if (!Array.isArray(contracts.endpoints)) return [];
  return contracts.endpoints
    .map(parseNativeEndpoint)
    .filter((entry): entry is NativeEndpoint => entry !== null);
}

function gatewayId(kind: string, value: string): string {
  const id = value.trim();
  if (!isGatewayResourceId(kind, id))
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
  const withParents = endpoint.ancestors.reduce(
    (current, kind, index) =>
      current.replace(
        `{parent${index}}`,
        encodeURIComponent(gatewayId(kind, parents[index] ?? ''))
      ),
    endpoint.pattern
  );
  return withParents.includes('{id}')
    ? withParents.replace(
        '{id}',
        encodeURIComponent(gatewayId(endpoint.kind, id))
      )
    : withParents;
}

function queryParameters(raw: string): URLSearchParams {
  const value = parseNativeJSON(raw.trim() || '{}');
  if (!nativeObject(value))
    throw new Error('Query parameters must be a JSON object of string values.');
  const entries: [string, string][] = Object.keys(value)
    .sort()
    .flatMap((name) => {
      if (!name || /[\r\n]/.test(name) || name.includes('\0'))
        throw new Error('Enter a valid query parameter name.');
      const item = value[name];
      return (Array.isArray(item) ? item : [item]).map(
        (part): [string, string] => {
          if (typeof part !== 'string')
            throw new Error(
              'Query parameter values must be strings or arrays of strings.'
            );
          return [name, part];
        }
      );
    });
  const params = new URLSearchParams(entries);
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
  const encoder = new TextEncoder();
  const rawBytes = encoder.encode(raw).byteLength;
  if (rawBytes > nativeResourceByteLimit)
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
    (request.files.length !== 1 || Object.keys(value).length > 0)
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
    return size + file.size + encoder.encode(name).byteLength + 512;
  }, rawBytes);
  if (total > nativeResourceByteLimit)
    throw new Error('Uploads are limited to 4 MiB in this console.');
  const form = new FormData();
  for (const name of Object.keys(value).sort()) {
    const field = value[name];
    if (name === 'file' || name === 'files[]' || typeof field !== 'string')
      throw new Error(
        'Multipart metadata must contain string fields; choose files using the file picker.'
      );
    form.append(name, field);
  }
  const fieldName = endpoint.kind === 'container_file' ? 'file' : 'files[]';
  for (const file of request.files)
    form.append(fieldName, file, file.webkitRelativePath || file.name);
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

export async function nativeResourceFingerprint(
  request: NativeResourceRequest
): Promise<string> {
  const { path, body } = requestParts(request);
  const files =
    body instanceof FormData
      ? await Promise.all(
          request.files.map(async (file) => {
            const digest = await crypto.subtle.digest(
              'SHA-256',
              await file.arrayBuffer()
            );
            return [
              file.webkitRelativePath || file.name,
              file.type,
              file.size,
              Array.from(new Uint8Array(digest), (byte) =>
                byte.toString(16).padStart(2, '0')
              ).join('')
            ] as const;
          })
        )
      : [];
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

export async function runNativeResourceRequest(
  request: NativeResourceRequest
): Promise<NativeResourceResult> {
  const { path, body } = requestParts(request);
  if (request.endpoint.method !== 'GET' && !request.submissionKey)
    throw new Error('A mutation identity is required.');
  const route = request.route.trim();
  const headers: Record<string, string> = {
    Authorization: `Bearer ${request.apiKey.trim()}`,
    ...(route ? { 'X-OLP-Route': route } : {}),
    ...(request.endpoint.method !== 'GET' && request.submissionKey
      ? createHeaders(request.submissionKey)
      : {}),
    ...(typeof body === 'string' ? { 'Content-Type': 'application/json' } : {})
  };
  const response = await gatewayFetch(path, {
    method: request.endpoint.method,
    headers,
    body,
    signal: request.signal
  });
  const bytes = await readBoundedResponseBytes(response);
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
      download: new Blob([bytes], { type: 'application/octet-stream' })
    };
  return { text: new TextDecoder().decode(bytes), resourceId };
}
