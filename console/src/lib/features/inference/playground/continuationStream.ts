import {
  NativeNumber,
  nativeAt,
  nativeObject,
  stringifyNativeJSON,
  type NativeObject,
  type NativeValue
} from '#lib/nativeJson.js';
import {
  MAX_PUBLIC_REQUEST_BYTES,
  MAX_PUBLIC_RESPONSE_BYTES,
  parseNativeValue,
  utf8ByteLength
} from './publicGateway';

export const continuationVersion = 'chat-anthropic-tools-v1';

const maxObservations = 4096;
const maxToolCalls = 128;
const handlePattern = /^continuation_[0-9a-f]{32}$/;
const observationTypes = [
  'thinking',
  'redacted_thinking',
  'text',
  'tool_use'
] as const;
const observationPhases = ['start', 'delta', 'end'] as const;

export type ToolCall = {
  id: string;
  type: 'function';
  function: { name: string; arguments: string };
};
export type Observation = {
  index: number;
  type: (typeof observationTypes)[number];
  phase: (typeof observationPhases)[number];
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
export type ContinuationChunk = {
  readonly olp: NativeObject;
  readonly choices: NativeValue | undefined;
  readonly nativeUsageRaw: string | undefined;
};
export type ContinuationAssembly = {
  assistant: Assistant;
  observations: Observation[];
  handle: string;
  finish: string;
  nativeUsageRaw?: string;
};

export function requireObject(value: unknown): NativeObject {
  if (!nativeObject(value))
    throw new Error('Incomplete continuation delivery.');
  return value;
}

export function requireString(value: unknown): string {
  if (typeof value !== 'string')
    throw new Error('Incomplete continuation delivery.');
  return value;
}

export const isValidHandle = (value: string): boolean =>
  handlePattern.test(value);

export function validHandle(value: unknown): string {
  const handle = requireString(value);
  if (!isValidHandle(handle))
    throw new Error('Incomplete continuation delivery.');
  return handle;
}

export function parseNativeDelivery(
  source: string,
  maxBytes: number
): NativeObject {
  const parsed = parseNativeValue(
    source,
    'Malformed continuation delivery.',
    maxBytes
  );
  if (!parsed.ok) throw new Error('Malformed continuation delivery.');
  return requireObject(parsed.value);
}

export function extractNativeUsage(value: NativeValue): string | undefined {
  const usage = nativeAt(value, ['olp', 'native_usage']);
  if (usage === undefined) return undefined;
  if (!nativeObject(usage))
    throw new Error(
      'Invalid native usage categories in continuation delivery.'
    );
  return stringifyNativeJSON(usage);
}

export function parseAssistant(value: unknown): Assistant {
  const src = requireObject(value);
  if (src.role !== 'assistant' || typeof src.content !== 'string')
    throw new Error('Incomplete assistant continuation.');
  if (src.tool_calls === undefined)
    return { role: 'assistant', content: src.content };
  if (!Array.isArray(src.tool_calls) || src.tool_calls.length > maxToolCalls)
    throw new Error('Incomplete assistant tool calls.');
  const tool_calls: ToolCall[] = src.tool_calls.map((item) => {
    const call = requireObject(item);
    const fn = requireObject(call.function);
    if (call.type !== 'function')
      throw new Error('Unsupported assistant tool type.');
    return {
      id: requireString(call.id),
      type: 'function',
      function: {
        name: requireString(fn.name),
        arguments: requireString(fn.arguments)
      }
    };
  });
  return { role: 'assistant', content: src.content, tool_calls };
}

function safeInt(value: unknown): number | null {
  const n =
    typeof value === 'number'
      ? value
      : value instanceof NativeNumber
        ? Number(value.source)
        : NaN;
  return Number.isSafeInteger(n) && n >= 0 ? n : null;
}

function optString(value: unknown, msg: string): string | undefined {
  if (value === undefined) return undefined;
  if (typeof value !== 'string') throw new Error(msg);
  return value;
}

function parseObservation(source: unknown): Observation {
  const e = requireObject(source);
  const index = safeInt(e.index);
  if (index === null)
    throw new Error('Invalid continuation observation order.');
  const type = observationTypes.find((t) => t === requireString(e.type));
  if (!type) throw new Error('Unknown continuation observation type.');
  const phase = observationPhases.find((p) => p === requireString(e.phase));
  if (!phase) throw new Error('Unknown continuation observation phase.');
  const text = optString(e.text, 'Invalid continuation observation text.');
  const name = optString(e.name, 'Invalid continuation tool name.');
  const call_id = optString(e.call_id, 'Invalid continuation call identity.');
  if (e.opaque_state !== undefined && typeof e.opaque_state !== 'boolean')
    throw new Error('Invalid opaque state marker.');
  return {
    index,
    type,
    phase,
    ...(text !== undefined && { text }),
    ...(name !== undefined && { name }),
    ...(call_id !== undefined && { call_id }),
    ...(e.opaque_state !== undefined && { opaque_state: e.opaque_state })
  };
}

export function observationsFrom(value: unknown): Observation[] {
  if (!Array.isArray(value) || value.length > maxObservations)
    throw new Error('Continuation observations exceed the client contract.');
  return value.map(parseObservation);
}

export function concatBytes(
  parts: readonly Uint8Array[],
  totalLength = parts.reduce((sum, part) => sum + part.length, 0)
): Uint8Array {
  const bytes = new Uint8Array(totalLength);
  parts.reduce((offset, part) => {
    bytes.set(part, offset);
    return offset + part.length;
  }, 0);
  return bytes;
}

export function parseContinuationChunkValue(
  value: NativeValue
): ContinuationChunk {
  const chunk = requireObject(value);
  const olp = requireObject(chunk.olp);
  if (olp.version !== continuationVersion)
    throw new Error('The route returned another continuation version.');
  return {
    olp,
    choices: chunk.choices,
    nativeUsageRaw: extractNativeUsage(chunk)
  };
}

export function parseContinuationChunk(source: string): ContinuationChunk {
  if (utf8ByteLength(source) > MAX_PUBLIC_REQUEST_BYTES)
    throw new Error('A continuation event exceeds the client limit.');
  return parseContinuationChunkValue(
    parseNativeDelivery(source, MAX_PUBLIC_REQUEST_BYTES)
  );
}

function extractTerminal(
  frames: readonly ContinuationChunk[],
  choices: readonly (NativeObject | null)[]
): { index: number; handle: string; finish: string } | null {
  const hits = choices.flatMap((c, index) =>
    c && c.finish_reason !== null && c.finish_reason !== undefined
      ? [{ index, choice: c, olp: frames[index]!.olp }]
      : []
  );
  if (hits.length === 0) return null;
  const first = hits[0]!;
  if (hits.length > 1 || first.olp.ready !== true)
    throw new Error('Terminal delivery has no committed continuation.');
  return {
    index: first.index,
    handle: validHandle(first.olp.handle),
    finish: requireString(first.choice.finish_reason)
  };
}

function assembleToolCalls(deltas: readonly NativeObject[]): ToolCall[] {
  // Local Map mutation keeps token-delta assembly O(N).
  const acc = new Map<number, { call: ToolCall; argBytes: number }>();
  for (const delta of deltas) {
    if (!Array.isArray(delta.tool_calls)) continue;
    for (const item of delta.tool_calls) {
      const call = requireObject(item);
      const index = safeInt(call.index);
      if (index === null || index >= maxToolCalls)
        throw new Error('Invalid tool call index.');
      if (call.type !== undefined && call.type !== 'function')
        throw new Error('Unsupported tool call type.');
      let entry = acc.get(index);
      if (!entry) {
        entry = {
          call: {
            id: '',
            type: 'function',
            function: { name: '', arguments: '' }
          },
          argBytes: 0
        };
        acc.set(index, entry);
      }
      if (typeof call.id === 'string') entry.call.id += call.id;
      if (call.function !== undefined) {
        const fn = requireObject(call.function);
        if (typeof fn.name === 'string') entry.call.function.name += fn.name;
        if (typeof fn.arguments === 'string') {
          entry.call.function.arguments += fn.arguments;
          entry.argBytes += utf8ByteLength(fn.arguments);
        }
      }
      if (entry.argBytes > MAX_PUBLIC_REQUEST_BYTES)
        throw new Error('Tool arguments exceed the client limit.');
    }
  }
  return [...acc]
    .sort(([a], [b]) => a - b)
    .map(([index, { call }], pos) => {
      if (
        index !== pos ||
        !call.id ||
        !call.function.name ||
        !call.function.arguments
      )
        throw new Error('Incomplete or reordered tool call.');
      return call;
    });
}

export function assembleContinuation(
  frames: readonly ContinuationChunk[]
): ContinuationAssembly {
  const choices = frames.map((chunk) => {
    if (!Array.isArray(chunk.choices) || chunk.choices.length > 1)
      throw new Error('Unsupported continuation candidate count.');
    return chunk.choices[0] ? requireObject(chunk.choices[0]) : null;
  });
  const terminal = extractTerminal(frames, choices);
  const observations = frames.flatMap((chunk, index) => {
    if (chunk.olp.observation === undefined) return [];
    if (terminal && index > terminal.index)
      throw new Error('Observation followed terminal delivery.');
    return [parseObservation(chunk.olp.observation)];
  });
  if (observations.length > maxObservations)
    throw new Error('Continuation observations exceed the client contract.');
  const deltas = choices.flatMap((c) =>
    c && c.delta !== undefined ? [requireObject(c.delta)] : []
  );
  const parts = deltas.flatMap((d) =>
    typeof d.content === 'string' ? [d.content] : []
  );
  if (
    parts.reduce((sum, p) => sum + utf8ByteLength(p), 0) >
    MAX_PUBLIC_RESPONSE_BYTES
  )
    throw new Error('Continuation text exceeds the client limit.');
  const toolCalls = assembleToolCalls(deltas);
  if (!terminal)
    throw new Error('The stream ended before a ready continuation.');
  const expectsTools = terminal.finish === 'tool_calls';
  const hasTools = toolCalls.length > 0;
  if (expectsTools !== hasTools)
    throw new Error('Tool terminal does not match the observed calls.');
  return {
    assistant: {
      role: 'assistant',
      content: parts.join(''),
      ...(hasTools ? { tool_calls: toolCalls } : {})
    },
    observations,
    handle: terminal.handle,
    finish: terminal.finish,
    nativeUsageRaw: frames.reduce<string | undefined>(
      (latest, c) => c.nativeUsageRaw ?? latest,
      undefined
    )
  };
}

// Stateful SSE decoder keeps multi-MiB byte framing O(N) without per-byte copies.
class SseDecoder {
  private readonly decoder = new TextDecoder();
  private readonly frames: ContinuationChunk[] = [];
  private totalBytes = 0;
  private eventBytes = 0;
  private doneMarker = false;
  private awaitingLF = false;
  private heldDispatch = false;
  private lineParts: Uint8Array[] = [];
  private lineLength = 0;
  private lines: string[] = [];

  private assertEventLimit(): void {
    if (this.eventBytes > MAX_PUBLIC_REQUEST_BYTES)
      throw new Error('A continuation event exceeds the client limit.');
  }

  private dispatch(): void {
    this.assertEventLimit();
    const data = this.lines
      .filter((line) => line.startsWith('data:'))
      .map((line) => line.slice(5).trimStart())
      .join('\n');
    this.lines = [];
    this.eventBytes = 0;
    this.heldDispatch = false;
    if (!data) return;
    if (data === '[DONE]') {
      this.doneMarker = true;
      return;
    }
    if (this.doneMarker)
      throw new Error('A continuation event followed [DONE].');
    this.frames.push(parseContinuationChunk(data));
  }

  private endLine(carriage: boolean): void {
    const bytes = concatBytes(this.lineParts, this.lineLength);
    this.lineParts = [];
    this.lineLength = 0;
    this.assertEventLimit();
    if (bytes.length === 0) {
      if (carriage) this.heldDispatch = true;
      else this.dispatch();
      return;
    }
    this.lines.push(this.decoder.decode(bytes));
  }

  feed(chunk: Uint8Array): void {
    this.totalBytes += chunk.length;
    if (this.totalBytes > MAX_PUBLIC_RESPONSE_BYTES)
      throw new Error('Continuation stream exceeds the client limit.');
    let start = 0;
    for (let i = 0; i < chunk.length; i++) {
      const byte = chunk[i];
      if (this.awaitingLF) {
        this.awaitingLF = false;
        if (byte === 0x0a) {
          this.eventBytes += 1;
          start = i + 1;
          if (this.heldDispatch) this.dispatch();
          continue;
        }
        if (this.heldDispatch) this.dispatch();
      }
      if (byte === 0x0d || byte === 0x0a) {
        if (i > start) {
          this.lineParts.push(chunk.subarray(start, i));
          this.lineLength += i - start;
        }
        this.eventBytes += i - start + 1;
        start = i + 1;
        if (byte === 0x0d) this.awaitingLF = true;
        this.endLine(byte === 0x0d);
      }
    }
    if (start < chunk.length) {
      this.lineParts.push(chunk.subarray(start));
      this.lineLength += chunk.length - start;
      this.eventBytes += chunk.length - start;
    }
    this.assertEventLimit();
  }

  finish(): ContinuationChunk[] {
    if (this.heldDispatch) this.dispatch();
    const trailing = concatBytes(this.lineParts, this.lineLength);
    if ([...this.lines, this.decoder.decode(trailing)].join('\n').trim())
      throw new Error('The continuation stream ended mid-event.');
    if (!this.doneMarker)
      throw new Error('The continuation stream has no terminal marker.');
    return this.frames;
  }
}

export async function readContinuationFrames(
  response: Response
): Promise<ContinuationChunk[]> {
  if (!response.body)
    throw new Error('The continuation stream is unavailable.');
  const reader = response.body.getReader();
  const stream = new SseDecoder();
  try {
    for (;;) {
      const item = await reader.read();
      if (item.value !== undefined && item.value.length > 0)
        stream.feed(item.value);
      if (item.done) break;
    }
    return stream.finish();
  } catch (error) {
    await reader.cancel().catch(() => undefined);
    throw error;
  } finally {
    reader.releaseLock();
  }
}
