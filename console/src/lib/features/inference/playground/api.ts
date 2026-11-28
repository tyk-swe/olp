import { parseJsonObject } from '#lib/editing/json.js';
import { unwrap } from '#lib/management/http.js';
import type { components } from '#lib/management/schema.js';
import { requestNative } from '#lib/management/transport.js';
import { apiClient } from '#lib/session/client.js';

export type PlaygroundRequest = Omit<
  components['schemas']['PlaygroundRequest'],
  'surface'
> & {
  surface?: 'openai' | 'anthropic' | 'gemini';
};

type PlaygroundResponse = components['schemas']['PlaygroundResponse'];

const maxPlaygroundResponseBytes = 32 * 1024 * 1024;

export type PlaygroundStreamDone = {
  id?: string;
  model?: string;
  routing?: PlaygroundResponse['routing'];
  usage?: PlaygroundResponse['usage'];
};

export type PlaygroundStreamProblem = {
  code?: string;
  message?: string;
  status?: number;
};

type PlaygroundStreamHandlers = {
  frame: (data: string) => void;
  done: (meta: PlaygroundStreamDone) => void;
  error: (problem: PlaygroundStreamProblem) => void;
};

export async function runPlayground(
  input: PlaygroundRequest
): Promise<PlaygroundResponse> {
  return requestNative(
    apiClient,
    'post',
    '/api/v1/playground',
    {
      cache: 'no-store',
      headers: { 'cache-control': 'no-store' },
      body: input
    },
    [['response'], ['structured_output']],
    maxPlaygroundResponseBytes
  );
}

function parseStreamProblem(
  payload: Record<string, unknown>
): PlaygroundStreamProblem {
  return {
    ...(typeof payload.code === 'string' ? { code: payload.code } : {}),
    ...(typeof payload.message === 'string'
      ? { message: payload.message }
      : {}),
    ...(typeof payload.status === 'number' ? { status: payload.status } : {})
  };
}

function dispatchEvent(
  block: string,
  handlers: PlaygroundStreamHandlers
): void {
  const lines = block.split('\n');
  const event =
    lines
      .filter((line) => line.startsWith('event:'))
      .map((line) => line.slice(6).trim())
      .at(-1) ?? '';
  const dataLines = lines
    .filter((line) => line.startsWith('data:'))
    .map((line) => line.slice(5).trim());
  if (!event || dataLines.length === 0) return;
  const payload = parseJsonObject(dataLines.join('\n'));
  if (!payload) return;
  if (event === 'frame' && typeof payload.data === 'string') {
    handlers.frame(payload.data);
  } else if (event === 'done') {
    handlers.done(payload as PlaygroundStreamDone);
  } else if (event === 'error') {
    handlers.error(parseStreamProblem(payload));
  }
}

export async function streamPlayground(
  input: PlaygroundRequest,
  handlers: PlaygroundStreamHandlers,
  signal: AbortSignal
): Promise<void> {
  const { data, error, response } = await apiClient.POST(
    '/api/v1/playground/stream',
    {
      cache: 'no-store',
      headers: { 'cache-control': 'no-store' },
      body: input,
      parseAs: 'stream',
      signal
    }
  );
  if (error || !(data instanceof ReadableStream)) {
    unwrap({ error: error ?? {}, response });
    return;
  }
  const reader = data.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  try {
    for (;;) {
      const { done, value } = await reader.read();
      buffer += done
        ? decoder.decode()
        : decoder.decode(value, { stream: true });
      let boundary = buffer.indexOf('\n\n');
      while (boundary >= 0) {
        dispatchEvent(buffer.slice(0, boundary), handlers);
        buffer = buffer.slice(boundary + 2);
        boundary = buffer.indexOf('\n\n');
      }
      if (done) return;
    }
  } finally {
    reader.releaseLock();
  }
}

type Schemas = components['schemas'];
export type InspectRoutingInput = {
  route: string;
  operation: string;
  surface: Schemas['Surface'];
  mode: Schemas['TransportMode'];
  request?: Record<string, unknown>;
  dialect?: Schemas['SimulationDialect'];
  clientContract?: string;
  preferences?: Schemas['RoutingPreferences'];
  apiKeyId?: string | null;
  seed?: string;
};

export async function inspectRouting(
  input: InspectRoutingInput,
  signal?: AbortSignal
): Promise<Schemas['RoutingDecision'][]> {
  return unwrap(
    await apiClient.POST('/api/v1/routing/simulate', {
      body: {
        operation: {
          operation: input.operation,
          route: input.route,
          ...(input.request === undefined ? {} : { request: input.request })
        },
        surface: input.surface,
        mode: input.mode,
        preferences: input.preferences,
        api_key_id: input.apiKeyId ?? null,
        dialect: input.dialect,
        client_contract: input.clientContract,
        seed: input.seed ?? ''
      },
      signal
    })
  );
}
