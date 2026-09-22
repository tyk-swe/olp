import {
  nativeObject,
  parseNativeJSON,
  replaceNative,
  stringifyNativeJSON,
  type NativeObject,
  type NativeValue
} from '$lib/json/nativeJson';

export const continuationVersion = 'chat-anthropic-tools-v1';
const maxEventBytes = 1 << 20;
const maxStreamBytes = 8 << 20;
const maxObservations = 4096;
const handlePattern = /^continuation_[0-9a-f]{32}$/;

export type ToolCall = {
  id: string;
  type: 'function';
  function: { name: string; arguments: string };
};
export type Observation = {
  index: number;
  type: 'thinking' | 'redacted_thinking' | 'text' | 'tool_use';
  phase: 'start' | 'delta' | 'end';
  text?: string;
  name?: string;
  call_id?: string;
  opaque_state?: boolean;
};
export type Assistant = {
  role: 'assistant';
  content: string;
  tool_calls?: ToolCall[];
};
export type ReadyTurn = {
  submission: string;
  handle: string;
  request: NativeObject;
  assistant: Assistant;
  observations: Observation[];
  finish: string;
};

function record(value: unknown): Record<string, unknown> {
  if (!nativeObject(value))
    throw new Error('Incomplete continuation delivery.');
  return value;
}

function string(value: unknown): string {
  if (typeof value !== 'string')
    throw new Error('Incomplete continuation delivery.');
  return value;
}

function decodeDelivery(source: string): unknown {
  try {
    return JSON.parse(source);
  } catch {
    throw new Error('Malformed continuation delivery.');
  }
}

function validHandle(value: unknown): string {
  const handle = string(value);
  if (!handlePattern.test(handle))
    throw new Error('Incomplete continuation delivery.');
  return handle;
}

function observationsFrom(value: unknown): Observation[] {
  if (!Array.isArray(value) || value.length > maxObservations)
    throw new Error('Continuation observations exceed the client contract.');
  return value.map((source) => {
    const entry = record(source);
    const index = entry.index;
    if (!Number.isSafeInteger(index) || (index as number) < 0)
      throw new Error('Invalid continuation observation order.');
    if (
      !['thinking', 'redacted_thinking', 'text', 'tool_use'].includes(
        string(entry.type)
      )
    )
      throw new Error('Unknown continuation observation type.');
    if (!['start', 'delta', 'end'].includes(string(entry.phase)))
      throw new Error('Unknown continuation observation phase.');
    if (entry.text !== undefined && typeof entry.text !== 'string')
      throw new Error('Invalid continuation observation text.');
    if (entry.name !== undefined && typeof entry.name !== 'string')
      throw new Error('Invalid continuation tool name.');
    if (entry.call_id !== undefined && typeof entry.call_id !== 'string')
      throw new Error('Invalid continuation call identity.');
    if (
      entry.opaque_state !== undefined &&
      typeof entry.opaque_state !== 'boolean'
    )
      throw new Error('Invalid opaque state marker.');
    return entry as Observation;
  });
}

function assistantFrom(value: unknown): Assistant {
  const source = record(value);
  if (source.role !== 'assistant' || typeof source.content !== 'string')
    throw new Error('Incomplete assistant continuation.');
  const assistant: Assistant = { role: 'assistant', content: source.content };
  if (source.tool_calls !== undefined) {
    if (!Array.isArray(source.tool_calls) || source.tool_calls.length > 128)
      throw new Error('Incomplete assistant tool calls.');
    assistant.tool_calls = source.tool_calls.map((item) => {
      const call = record(item);
      const fn = record(call.function);
      if (call.type !== 'function')
        throw new Error('Unsupported assistant tool type.');
      return {
        id: string(call.id),
        type: 'function',
        function: { name: string(fn.name), arguments: string(fn.arguments) }
      };
    });
  }
  return assistant;
}

function validateRequest(request: NativeObject, route: string): NativeObject {
  const model = request.model;
  if (model !== undefined && model !== '' && model !== route)
    throw new Error('The native request names another route.');
  if (!Array.isArray(request.messages) || request.messages.length === 0)
    throw new Error('The native Chat request needs ordered messages.');
  if (!Array.isArray(request.tools) || request.tools.length === 0)
    throw new Error('The negotiated tool client needs a nonempty tools array.');
  return replaceNative(request, ['model'], route) as NativeObject;
}

export function nativeChatRequest(source: string, route: string): NativeObject {
  const parsed = parseNativeJSON(source);
  if (!nativeObject(parsed))
    throw new Error('The Chat request must be a JSON object.');
  return validateRequest(parsed, route);
}

export function submissionID(now = Date.now()): string {
  if (!Number.isSafeInteger(now) || now < 0)
    throw new Error('Invalid submission time.');
  return `${now}.${crypto.randomUUID()}`;
}

function headers(key: string, submission: string, handle?: string): Headers {
  if (!key.startsWith('olp_') || key.length > 512)
    throw new Error('Enter an inference API key for this route.');
  if (
    !/^\d{1,16}\.[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(
      submission
    )
  )
    throw new Error('Invalid submission identity.');
  const result = new Headers({
    Authorization: `Bearer ${key}`,
    'Content-Type': 'application/json',
    Accept: 'application/json, text/event-stream',
    'Cache-Control': 'no-store',
    'X-OLP-Continuation': continuationVersion,
    'X-OLP-Submission-ID': submission
  });
  if (handle) result.set('X-OLP-Continuation-Handle', validHandle(handle));
  return result;
}

async function checked(response: Response): Promise<Response> {
  if (response.ok) return response;
  let code = '';
  try {
    const body = record(JSON.parse((await response.text()).slice(0, 8192)));
    const error = record(body.error);
    if (typeof error.code === 'string') code = error.code;
  } catch {
    // The UI never echoes an upstream error body, URL or credential.
  }
  const named: Record<string, string> = {
    policy_conflict: 'This key needs provider-state permission.',
    route_forbidden: 'This key cannot use the selected route.',
    state_carrier: 'The route does not admit this continuation contract.',
    continuation_mismatch: 'The prior handle does not match this history.',
    continuation_unavailable: 'The prior continuation is unavailable.',
    continuation_outcome_unknown:
      'Accepted work has no recoverable result yet.',
    invalid_submission_identity:
      'The submission identity is invalid or expired.'
  };
  throw new Error(
    named[code] ?? `Public inference request failed (${response.status}).`
  );
}

type Chunk = {
  olp?: {
    version?: unknown;
    observation?: unknown;
    ready?: unknown;
    handle?: unknown;
  };
  choices?: unknown;
};

function parseChunk(source: string): Chunk {
  if (source.length > maxEventBytes)
    throw new Error('A continuation event exceeds the client limit.');
  const chunk = record(decodeDelivery(source));
  if (record(chunk.olp).version !== continuationVersion)
    throw new Error('The route returned another continuation version.');
  return chunk as Chunk;
}

function assemble(frames: Chunk[]): {
  assistant: Assistant;
  observations: Observation[];
  handle: string;
  finish: string;
} {
  const observations: Observation[] = [];
  const calls = new Map<number, ToolCall>();
  let text = '';
  let handle = '';
  let finish = '';
  let terminal = false;
  for (const chunk of frames) {
    const extension = record(chunk.olp);
    const choices = chunk.choices;
    if (!Array.isArray(choices) || choices.length > 1)
      throw new Error('Unsupported continuation candidate count.');
    if (extension.observation !== undefined) {
      if (terminal) throw new Error('Observation followed terminal delivery.');
      observations.push(...observationsFrom([extension.observation]));
      if (observations.length > maxObservations)
        throw new Error(
          'Continuation observations exceed the client contract.'
        );
    }
    const choice = choices[0];
    if (!choice) continue;
    const selected = record(choice);
    const delta = selected.delta === undefined ? {} : record(selected.delta);
    if (typeof delta.content === 'string') {
      text += delta.content;
      if (text.length > maxStreamBytes)
        throw new Error('Continuation text exceeds the client limit.');
    }
    if (Array.isArray(delta.tool_calls)) {
      for (const item of delta.tool_calls) {
        const call = record(item);
        const index = call.index;
        if (
          !Number.isSafeInteger(index) ||
          (index as number) < 0 ||
          (index as number) >= 128
        )
          throw new Error('Invalid tool call index.');
        const current = calls.get(index as number) ?? {
          id: '',
          type: 'function',
          function: { name: '', arguments: '' }
        };
        if (typeof call.id === 'string') current.id += call.id;
        if (call.type !== undefined && call.type !== 'function')
          throw new Error('Unsupported tool call type.');
        if (call.function !== undefined) {
          const fn = record(call.function);
          if (typeof fn.name === 'string') current.function.name += fn.name;
          if (typeof fn.arguments === 'string')
            current.function.arguments += fn.arguments;
        }
        if (current.function.arguments.length > maxEventBytes)
          throw new Error('Tool arguments exceed the client limit.');
        calls.set(index as number, current);
      }
    }
    if (
      selected.finish_reason !== null &&
      selected.finish_reason !== undefined
    ) {
      if (terminal || extension.ready !== true)
        throw new Error('Terminal delivery has no committed continuation.');
      handle = validHandle(extension.handle);
      finish = string(selected.finish_reason);
      terminal = true;
    }
  }
  if (!terminal)
    throw new Error('The stream ended before a ready continuation.');
  const toolCalls = [...calls]
    .sort(([a], [b]) => a - b)
    .map(([index, call], position) => {
      if (
        index !== position ||
        !call.id ||
        !call.function.name ||
        !call.function.arguments
      )
        throw new Error('Incomplete or reordered tool call.');
      return call;
    });
  if ((finish === 'tool_calls') !== toolCalls.length > 0)
    throw new Error('Tool terminal does not match the observed calls.');
  return {
    assistant: {
      role: 'assistant',
      content: text,
      ...(toolCalls.length ? { tool_calls: toolCalls } : {})
    },
    observations,
    handle,
    finish
  };
}

async function frames(response: Response): Promise<Chunk[]> {
  if (!response.body)
    throw new Error('The continuation stream is unavailable.');
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  const result: Chunk[] = [];
  let buffer = '';
  let total = 0;
  let doneMarker = false;
  const consume = (block: string) => {
    const data = block
      .split('\n')
      .filter((line) => line.startsWith('data:'))
      .map((line) => line.slice(5).trimStart())
      .join('\n');
    if (!data) return;
    if (data === '[DONE]') {
      doneMarker = true;
      return;
    }
    if (doneMarker) throw new Error('A continuation event followed [DONE].');
    result.push(parseChunk(data));
  };
  try {
    for (;;) {
      const item = await reader.read();
      const chunk = item.done
        ? decoder.decode()
        : decoder.decode(item.value, { stream: true });
      total += chunk.length;
      if (total > maxStreamBytes)
        throw new Error('Continuation stream exceeds the client limit.');
      buffer += chunk.replaceAll('\r\n', '\n');
      let boundary = buffer.indexOf('\n\n');
      while (boundary >= 0) {
        consume(buffer.slice(0, boundary));
        buffer = buffer.slice(boundary + 2);
        boundary = buffer.indexOf('\n\n');
      }
      if (buffer.length > maxEventBytes)
        throw new Error('A continuation event exceeds the client limit.');
      if (item.done) break;
    }
    if (buffer.trim())
      throw new Error('The continuation stream ended mid-event.');
    if (!doneMarker)
      throw new Error('The continuation stream has no terminal marker.');
    return result;
  } finally {
    reader.releaseLock();
  }
}

/** The browser client is a narrow negotiated contract. It exposes no tool
 * action until every streamed event and the encrypted ready marker arrive. */
export async function streamTurn(
  key: string,
  request: NativeObject,
  submission: string,
  signal?: AbortSignal
): Promise<ReadyTurn> {
  const wire = replaceNative(request, ['stream'], true);
  const body = stringifyNativeJSON(wire);
  if (new TextEncoder().encode(body).byteLength > maxEventBytes)
    throw new Error('The native request exceeds the public body limit.');
  const response = await checked(
    await fetch('/v1/chat/completions', {
      method: 'POST',
      headers: headers(key, submission),
      body,
      cache: 'no-store',
      credentials: 'omit',
      redirect: 'error',
      signal
    })
  );
  const completed = assemble(await frames(response));
  return { submission, request, ...completed };
}

export function nextTurn(
  completed: ReadyTurn,
  results: { tool_call_id: string; content: string }[]
): NativeObject {
  const calls = completed.assistant.tool_calls;
  if (!calls?.length || results.length !== calls.length)
    throw new Error('Provide one result for every ready tool call.');
  const messages = completed.request.messages;
  if (!Array.isArray(messages))
    throw new Error('The original messages are unavailable.');
  const tools = results.map((result, index) => {
    if (
      result.tool_call_id !== calls[index]!.id ||
      typeof result.content !== 'string'
    )
      throw new Error('Tool results must match call order and identity.');
    return {
      role: 'tool',
      tool_call_id: result.tool_call_id,
      content: result.content
    };
  });
  let request: NativeValue = replaceNative(
    completed.request,
    ['messages'],
    [...messages, completed.assistant, ...tools]
  );
  request = replaceNative(request, ['stream'], undefined);
  request = replaceNative(request, ['stream_options'], undefined);
  return request as NativeObject;
}

function completedUnary(
  source: unknown,
  request: NativeObject,
  submission: string
): ReadyTurn {
  const payload = record(source);
  const extension = record(payload.olp);
  if (extension.version !== continuationVersion || extension.ready !== true)
    throw new Error('The result has no ready continuation.');
  const choices = payload.choices;
  if (!Array.isArray(choices) || choices.length !== 1)
    throw new Error('The result has an unsupported candidate count.');
  const selected = record(choices[0]);
  const assistant = assistantFrom(selected.message);
  const observations =
    extension.observations === undefined
      ? []
      : observationsFrom(extension.observations);
  return {
    submission,
    handle: validHandle(extension.handle),
    request,
    assistant,
    observations,
    finish: string(selected.finish_reason)
  };
}

export async function unaryTurn(
  key: string,
  request: NativeObject,
  submission: string,
  parentHandle: string,
  signal?: AbortSignal
): Promise<ReadyTurn> {
  const body = stringifyNativeJSON(request);
  if (new TextEncoder().encode(body).byteLength > maxEventBytes)
    throw new Error('The next native request exceeds the public body limit.');
  const response = await checked(
    await fetch('/v1/chat/completions', {
      method: 'POST',
      headers: headers(key, submission, parentHandle),
      body,
      cache: 'no-store',
      credentials: 'omit',
      redirect: 'error',
      signal
    })
  );
  const source = await response.text();
  if (source.length > maxStreamBytes)
    throw new Error('The continuation result exceeds the client limit.');
  return completedUnary(decodeDelivery(source), request, submission);
}

/** Recovery reads committed delivery only. It never creates a fresh attempt. */
export async function recoverTurn(
  key: string,
  submission: string,
  request: NativeObject,
  signal?: AbortSignal
): Promise<ReadyTurn> {
  const response = await checked(
    await fetch(
      `/v1/continuation-submissions/${encodeURIComponent(submission)}`,
      {
        method: 'GET',
        headers: headers(key, submission),
        cache: 'no-store',
        credentials: 'omit',
        redirect: 'error',
        signal
      }
    )
  );
  const source = await response.text();
  if (source.length > maxStreamBytes)
    throw new Error('Recovery result exceeds the client limit.');
  const recovery = record(decodeDelivery(source));
  if (recovery.version !== continuationVersion || recovery.state !== 'ready')
    throw new Error('The submission has no ready delivery.');
  const delivery = record(recovery.delivery);
  const completed =
    delivery.stream === true && Array.isArray(delivery.frames)
      ? {
          submission,
          request,
          ...assemble(
            delivery.frames.map((frame) => parseChunk(JSON.stringify(frame)))
          )
        }
      : delivery.stream === false && delivery.body !== undefined
        ? completedUnary(delivery.body, request, submission)
        : null;
  if (!completed)
    throw new Error('This submission has another delivery shape.');
  const savedAssistant = assistantFrom(recovery.assistant);
  if (
    stringifyNativeJSON(savedAssistant) !==
    stringifyNativeJSON(completed.assistant)
  )
    throw new Error('Recovered assistant does not match the committed stream.');
  if (completed.handle !== validHandle(recovery.handle))
    throw new Error('Recovered handle does not match the committed stream.');
  return completed;
}
