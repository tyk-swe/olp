import { captureResult } from '#lib/editing/result.js';
import {
  nativeAt,
  nativeObject,
  parseNativeJSON,
  stringifyNativeJSON,
  type NativeValue
} from '#lib/nativeJson.js';

export type RealtimeEventRow = {
  position: number;
  phase: string;
  role: string;
  timing: string[];
};

export type RealtimeTraceResult =
  { ok: true; value: RealtimeEventRow[] } | { ok: false; error: string };

const maxEvents = 1024;

const knownTypes: ReadonlyMap<string, string> = new Map([
  ['session.created', 'Session created'],
  ['session.updated', 'Session updated'],
  ['conversation.item.created', 'Conversation item'],
  ['conversation.item.truncated', 'Interruption / truncation'],
  ['input_audio_buffer.append', 'Incoming audio'],
  ['input_audio_buffer.committed', 'Input turn committed'],
  ['input_audio_buffer.speech_started', 'VAD speech start'],
  ['input_audio_buffer.speech_stopped', 'VAD speech stop'],
  ['response.created', 'Response turn start'],
  ['response.output_item.added', 'Output item start'],
  ['response.output_item.done', 'Output item end'],
  ['response.audio.delta', 'Output audio chunk'],
  ['response.audio.done', 'Output audio end'],
  ['response.text.delta', 'Output text chunk'],
  ['response.text.done', 'Output text end'],
  ['response.function_call_arguments.delta', 'Tool argument chunk'],
  ['response.function_call_arguments.done', 'Tool arguments ready'],
  ['response.done', 'Response turn end'],
  ['response.cancelled', 'Response interrupted'],
  ['error', 'Error event']
]);

const knownRoles: ReadonlySet<string> = new Set([
  'system',
  'developer',
  'user',
  'assistant',
  'tool'
]);

const timingFields = [
  'audio_start_ms',
  'audio_end_ms',
  'offset_ms',
  'timestamp_ms',
  'sample_rate_hz'
] as const;

function toEventRow(event: NativeValue, position: number): RealtimeEventRow {
  const nativeType = nativeAt(event, ['type']);
  const phase =
    typeof nativeType === 'string'
      ? (knownTypes.get(nativeType) ?? 'Unknown native event')
      : 'Unknown native event';
  const actor = nativeAt(event, ['role']);
  const role =
    typeof actor === 'string' && knownRoles.has(actor) ? actor : 'Not declared';
  const timing = timingFields.flatMap((name) => {
    const value = nativeAt(event, [name]);
    if (value === undefined || value === null) return [];
    const exact = stringifyNativeJSON(value);
    if (!/^-?\d+(?:\.\d+)?$/.test(exact) || exact.length > 32) return [];
    return [`${name}: ${exact}`];
  });
  return { position, phase, role, timing };
}

export function parseRealtimeTrace(source: string): RealtimeTraceResult {
  const parsed = captureResult(() => parseNativeJSON(source));
  if (!parsed.ok) {
    return {
      ok: false,
      error:
        parsed.error instanceof Error
          ? parsed.error.message
          : 'Invalid event trace.'
    };
  }
  const events = Array.isArray(parsed.value)
    ? parsed.value
    : nativeAt(parsed.value, ['events']);
  if (!Array.isArray(events) || events.length > maxEvents) {
    return {
      ok: false,
      error: 'Paste a JSON array with at most 1024 native events.'
    };
  }
  const invalidIndex = events.findIndex((event) => !nativeObject(event));
  if (invalidIndex !== -1) {
    return {
      ok: false,
      error: `Event ${invalidIndex + 1} must be a JSON object.`
    };
  }
  return { ok: true, value: events.map(toEventRow) };
}
