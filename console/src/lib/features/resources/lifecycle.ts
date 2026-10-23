import { gatewayFetch } from '$lib/api/gateway';
import {
  parseNativeJSON,
  stringifyNativeJSON,
  nativeObject
} from '$lib/json/nativeJson';

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

export class ResourceRequestError extends Error {
  constructor(
    message: string,
    readonly resourceId: string | null
  ) {
    super(message);
  }
}

/** Credentials and provider content stay in the calling component's memory. */
export async function runAgentRequest(request: AgentRequest): Promise<string> {
  if (!request.apiKey.trim()) throw new Error('Enter an inference API key.');
  let path = '/gemini/v1beta/agents';
  const headers: Record<string, string> = {
    Authorization: `Bearer ${request.apiKey.trim()}`
  };
  let body: string | undefined;
  if (request.action === 'create') {
    if (!request.route?.trim()) throw new Error('Choose a route.');
    if (!request.submissionKey)
      throw new Error('A submission identity is required.');
    const input = parseNativeJSON(request.body ?? '{}');
    if (!nativeObject(input))
      throw new Error('The agent definition must be a JSON object.');
    body = stringifyNativeJSON(input);
    headers['Content-Type'] = 'application/json';
    headers['X-OLP-Route'] = request.route.trim();
    headers['Idempotency-Key'] = request.submissionKey;
  } else if (request.action !== 'list') {
    if (!/^agent_resource_[a-f0-9]{32}$/.test(request.id ?? ''))
      throw new Error('Enter a gateway agent resource ID.');
    path += `/${encodeURIComponent(request.id!)}`;
  } else if (request.pageToken) {
    path += `?page_token=${encodeURIComponent(request.pageToken)}`;
  }
  const response = await gatewayFetch(path, {
    method:
      request.action === 'create'
        ? 'POST'
        : request.action === 'delete'
          ? 'DELETE'
          : 'GET',
    headers,
    body,
    signal: request.signal
  });
  const reader = response.body?.getReader();
  const decoder = new TextDecoder();
  let result = '';
  let bytes = 0;
  if (reader) {
    try {
      for (;;) {
        const chunk = await reader.read();
        if (chunk.done) break;
        bytes += chunk.value.byteLength;
        if (bytes > 4 * 1024 * 1024) {
          await reader.cancel();
          throw new Error('The resource response is too large.');
        }
        result += decoder.decode(chunk.value, { stream: true });
      }
      result += decoder.decode();
    } finally {
      reader.releaseLock();
    }
  }
  if (!response.ok) {
    throw new ResourceRequestError(
      `Resource request failed (HTTP ${response.status}). ${result.slice(0, 2048)}`,
      response.headers.get('X-OLP-Resource-ID')
    );
  }
  return result;
}
