import { captureResult } from '#lib/editing/result.js';
import { gatewayFetch } from '#lib/management/gateway.js';
import { createHeaders, errorMessage } from '#lib/management/http.js';
import {
  nativeObject,
  parseNativeJSON,
  stringifyNativeJSON,
  type NativeValue
} from '#lib/nativeJson.js';
import type {
  ProviderResource,
  RetrievalBindingFields,
  RetrievalBindingInput
} from './api';

export const resourceByteLimit = 4 * 1024 * 1024;
export const resourceKindPattern = /^[a-z][a-z0-9_]{0,63}$/;

export type ValidationResult<T> =
  { ok: true; value: T } | { ok: false; message: string };
export type ExtractedResourceMetadata = {
  id: string | null;
  nextPageToken: string | null;
};
export type AgentAction = 'create' | 'list' | 'get' | 'delete';
export type AgentRequest = {
  action: AgentAction;
  apiKey: string;
  route?: string;
  id?: string;
  body?: string;
  submissionKey?: string;
  pageToken?: string;
  signal?: AbortSignal;
};
export type RetrievalBindingForm = {
  project: string;
  name: string;
  route: string;
  target: string;
  backend: RetrievalBindingInput['backend'];
  resource: string;
  enabled: boolean;
  fields: string;
  grants: string;
};

export class ResourceRequestError extends Error {
  constructor(
    message: string,
    readonly resourceId: string | null,
    readonly definitiveMutationRejection = false
  ) {
    super(message);
  }
}

export function isGatewayResourceId(kind: string, value: string): boolean {
  return (
    resourceKindPattern.test(kind) &&
    new RegExp(`^${kind}_[a-f0-9]{32}$`).test(value)
  );
}

export async function readBoundedResponseBytes(
  response: Response,
  limit = resourceByteLimit
): Promise<Uint8Array<ArrayBuffer>> {
  const reader = response.body?.getReader();
  if (!reader) return new Uint8Array();
  const chunks: Uint8Array[] = [];
  let bytes = 0;
  try {
    for (;;) {
      const chunk = await reader.read();
      if (chunk.done) break;
      bytes += chunk.value.byteLength;
      if (bytes > limit) {
        await reader.cancel();
        throw new Error('The resource response is too large.');
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

const nonEmptyString = (value: unknown): string | null =>
  typeof value === 'string' && value.length > 0 ? value : null;

export function extractResourceResponseMetadata(
  text: string,
  kind?: string
): ExtractedResourceMetadata {
  if (!text.trim()) return { id: null, nextPageToken: null };
  const parsed = captureResult(() => parseNativeJSON(text));
  if (!parsed.ok || !nativeObject(parsed.value))
    return { id: null, nextPageToken: null };
  const rawId = nonEmptyString(parsed.value.id);
  const id = kind
    ? ([rawId, nonEmptyString(parsed.value.name)].find(
        (value): value is string =>
          value !== null && isGatewayResourceId(kind, value)
      ) ?? null)
    : rawId;
  return { id, nextPageToken: nonEmptyString(parsed.value.next_page_token) };
}

export function extractResourceJsonId(
  text: string,
  kind?: string
): string | null {
  return extractResourceResponseMetadata(text, kind).id;
}

export function agentSubmissionFingerprint(
  route: string,
  body: string
): string {
  const canonical = (value: NativeValue): string => {
    if (Array.isArray(value)) return `[${value.map(canonical).join(',')}]`;
    if (nativeObject(value))
      return `{${Object.keys(value)
        .sort()
        .map((key) => `${JSON.stringify(key)}:${canonical(value[key]!)}`)
        .join(',')}}`;
    return stringifyNativeJSON(value);
  };
  return `${route.trim()}\n${canonical(parseNativeJSON(body))}`;
}

function buildAgentRequestSpec(request: AgentRequest) {
  const apiKey = request.apiKey.trim();
  if (!apiKey) throw new Error('Enter an inference API key.');
  const authHeaders: Record<string, string> = {
    Authorization: `Bearer ${apiKey}`
  };
  if (request.action === 'create') {
    const route = request.route?.trim();
    if (!route) throw new Error('Choose a route.');
    if (!request.submissionKey)
      throw new Error('A submission identity is required.');
    const input = parseNativeJSON(request.body ?? '{}');
    if (!nativeObject(input))
      throw new Error('The agent definition must be a JSON object.');
    return {
      method: 'POST',
      path: '/gemini/v1beta/agents',
      headers: {
        ...authHeaders,
        'Content-Type': 'application/json',
        'X-OLP-Route': route,
        ...createHeaders(request.submissionKey)
      },
      body: stringifyNativeJSON(input)
    };
  }
  if (request.action === 'list') {
    const path = request.pageToken
      ? `/gemini/v1beta/agents?page_token=${encodeURIComponent(request.pageToken)}`
      : '/gemini/v1beta/agents';
    return { method: 'GET', path, headers: authHeaders, body: undefined };
  }
  const id = request.id ?? '';
  if (!isGatewayResourceId('agent_resource', id))
    throw new Error('Enter a gateway agent resource ID.');
  return {
    method: request.action === 'delete' ? 'DELETE' : 'GET',
    path: `/gemini/v1beta/agents/${encodeURIComponent(id)}`,
    headers: authHeaders,
    body: undefined
  };
}

export async function runAgentRequest(request: AgentRequest) {
  const spec = buildAgentRequestSpec(request);
  const response = await gatewayFetch(spec.path, {
    method: spec.method,
    headers: spec.headers,
    body: spec.body,
    signal: request.signal
  });
  const text = new TextDecoder().decode(
    await readBoundedResponseBytes(response)
  );
  if (!response.ok)
    throw new ResourceRequestError(
      `Resource request failed (HTTP ${response.status}). ${text.slice(0, 2048)}`,
      response.headers.get('X-OLP-Resource-ID')
    );
  const creating = request.action === 'create';
  const metadata = extractResourceResponseMetadata(
    text,
    creating ? 'agent_resource' : undefined
  );
  if (creating && !metadata.id)
    throw new ResourceRequestError(
      'The gateway returned an invalid agent creation response.',
      response.headers.get('X-OLP-Resource-ID')
    );
  return { text, ...metadata };
}

function evidenceQuantityTemplate(kind: ProviderResource['kind']) {
  if (kind === 'fine_tuning_job' || kind === 'vertex_tuning_job')
    return { training_tokens: 0 };
  if (kind === 'context_cache') return { cache_token_seconds: '0' };
  if (kind === 'container') return { container_sessions: { '1g': 0 } };
  if (kind === 'eval_run')
    return {
      input_tokens: 0,
      output_tokens: 0,
      cached_input_tokens: 0,
      invocation_count: 0
    };
  return { storage_byte_seconds: '0' };
}

export function draftRetainedEvidence(
  resource: Pick<
    ProviderResource,
    'kind' | 'created_at' | 'upstream_model' | 'retained_settlement'
  >,
  options: { idempotencyKey?: string; now?: string } = {}
): string {
  return JSON.stringify(
    {
      idempotency_key: options.idempotencyKey ?? crypto.randomUUID(),
      evidence_digest: '',
      start: resource.created_at,
      end: options.now ?? new Date().toISOString(),
      model:
        resource.retained_settlement?.models?.[0] ?? resource.upstream_model,
      ...evidenceQuantityTemplate(resource.kind),
      terminal: false
    },
    null,
    2
  );
}

export function parseRetainedEvidence(raw: string): ValidationResult<string> {
  if (!raw.trim())
    return { ok: false, message: 'Enter one usage evidence object.' };
  const parsed = captureResult(() => parseNativeJSON(raw));
  if (!parsed.ok)
    return {
      ok: false,
      message: errorMessage(
        parsed.error,
        'Enter valid JSON for usage evidence.'
      )
    };
  if (!nativeObject(parsed.value))
    return { ok: false, message: 'Enter one usage evidence object.' };
  return { ok: true, value: stringifyNativeJSON(parsed.value) };
}

const stringFieldKeys = ['text', 'id', 'title', 'url'] as const;
const fieldMappingKeys = new Set([...stringFieldKeys, 'search', 'metadata']);

function parseRetrievalBindingFields(
  raw: string
): ValidationResult<RetrievalBindingFields> {
  const parsed = captureResult(() => parseNativeJSON(raw));
  if (!parsed.ok || !nativeObject(parsed.value))
    return {
      ok: false,
      message: parsed.ok
        ? 'Field mappings must be a JSON object.'
        : errorMessage(parsed.error, 'Field mappings must be a JSON object.')
    };
  const source = parsed.value;
  const unknownKey = Object.keys(source).find((k) => !fieldMappingKeys.has(k));
  if (unknownKey !== undefined)
    return { ok: false, message: `Unknown field mapping key "${unknownKey}".` };
  const badString = stringFieldKeys.find(
    (k) => Object.hasOwn(source, k) && typeof source[k] !== 'string'
  );
  if (badString)
    return {
      ok: false,
      message: `Field mapping "${badString}" must be a string.`
    };
  const search = Object.hasOwn(source, 'search') ? source.search : undefined;
  if (
    search !== undefined &&
    (!Array.isArray(search) ||
      !search.every((item): item is string => typeof item === 'string'))
  )
    return {
      ok: false,
      message: 'Field mapping "search" must be an array of strings.'
    };
  const metaEntries = Object.hasOwn(source, 'metadata')
    ? nativeObject(source.metadata)
      ? Object.entries(source.metadata)
      : null
    : undefined;
  if (
    metaEntries === null ||
    (metaEntries !== undefined &&
      !metaEntries.every(
        (e): e is [string, string] => typeof e[1] === 'string'
      ))
  )
    return {
      ok: false,
      message: 'Field mapping "metadata" must be an object of string values.'
    };
  return {
    ok: true,
    value: {
      ...(typeof source.text === 'string' ? { text: source.text } : {}),
      ...(typeof source.id === 'string' ? { id: source.id } : {}),
      ...(typeof source.title === 'string' ? { title: source.title } : {}),
      ...(typeof source.url === 'string' ? { url: source.url } : {}),
      ...(search !== undefined ? { search } : {}),
      ...(metaEntries !== undefined
        ? { metadata: Object.fromEntries(metaEntries) }
        : {})
    }
  };
}

export function buildRetrievalBindingInput(
  form: RetrievalBindingForm
): ValidationResult<RetrievalBindingInput> {
  const fields = parseRetrievalBindingFields(form.fields);
  if (!fields.ok) return fields;
  const keys = form.grants.split(/[\s,]+/).filter(Boolean);
  if (new Set(keys).size !== keys.length)
    return { ok: false, message: 'Each API-key grant must appear once.' };
  return {
    ok: true,
    value: {
      project_id: form.project.trim(),
      name: form.name.trim(),
      route: form.route.trim(),
      target_id: form.target.trim(),
      backend: form.backend,
      backend_resource: form.resource.trim(),
      enabled: form.enabled,
      fields: fields.value,
      key_grants: keys
    }
  };
}
