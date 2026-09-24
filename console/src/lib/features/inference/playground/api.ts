import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { result } from '$lib/api/http';

export type PlaygroundRequest = Omit<
  components['schemas']['PlaygroundRequest'],
  'surface'
> & {
  surface?: 'openai' | 'anthropic' | 'gemini';
};
export type PlaygroundResponse = components['schemas']['PlaygroundResponse'];

export type PlaygroundOperation =
  | 'generation'
  | 'token_count'
  | 'embeddings'
  | 'moderation'
  | 'rerank'
  | 'classification'
  | 'scoring'
  | 'translation'
  | 'realtime';

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

export type PlaygroundStreamHandlers = {
  frame: (data: string) => void;
  done: (meta: PlaygroundStreamDone) => void;
  error: (problem: PlaygroundStreamProblem) => void;
};

export async function runPlayground(
  input: PlaygroundRequest
): Promise<PlaygroundResponse> {
  const { data, error, response } = await apiClient.POST('/api/v3/playground', {
    cache: 'no-store',
    headers: { 'cache-control': 'no-store' },
    body: input
  });
  return result(data, error, response);
}

function dispatchEvent(block: string, handlers: PlaygroundStreamHandlers) {
  let event = '';
  const data: string[] = [];
  for (const line of block.split('\n')) {
    if (line.startsWith('event:')) event = line.slice(6).trim();
    else if (line.startsWith('data:')) data.push(line.slice(5).trim());
  }
  if (!event || data.length === 0) return;
  let payload: Record<string, unknown>;
  try {
    payload = JSON.parse(data.join('\n'));
  } catch {
    return;
  }
  if (event === 'frame' && typeof payload.data === 'string') {
    handlers.frame(payload.data);
  } else if (event === 'done') {
    handlers.done(payload as PlaygroundStreamDone);
  } else if (event === 'error') {
    handlers.error(payload as PlaygroundStreamProblem);
  }
}

export async function streamPlayground(
  input: PlaygroundRequest,
  handlers: PlaygroundStreamHandlers,
  signal: AbortSignal
): Promise<void> {
  const { data, error, response } = await apiClient.POST(
    '/api/v3/playground/stream',
    {
      cache: 'no-store',
      headers: { 'cache-control': 'no-store' },
      body: input,
      parseAs: 'stream',
      signal
    }
  );
  if (error || !(data instanceof ReadableStream)) {
    result(undefined, error ?? {}, response);
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
