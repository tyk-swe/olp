import {
  nativeObject,
  parseNativeJSON,
  stringifyNativeJSON,
  type NativeValue
} from '$lib/json/nativeJson';

export type RealtimeEventRow = {
  position: number;
  phase: string;
  role: string;
  timing: string[];
};

const knownTypes: Record<string, string> = {
  'session.created': 'Session created',
  'session.updated': 'Session updated',
  'conversation.item.created': 'Conversation item',
  'conversation.item.truncated': 'Interruption / truncation',
  'input_audio_buffer.append': 'Incoming audio',
  'input_audio_buffer.committed': 'Input turn committed',
  'input_audio_buffer.speech_started': 'VAD speech start',
  'input_audio_buffer.speech_stopped': 'VAD speech stop',
  'response.created': 'Response turn start',
  'response.output_item.added': 'Output item start',
  'response.output_item.done': 'Output item end',
  'response.audio.delta': 'Output audio chunk',
  'response.audio.done': 'Output audio end',
  'response.text.delta': 'Output text chunk',
  'response.text.done': 'Output text end',
  'response.function_call_arguments.delta': 'Tool argument chunk',
  'response.function_call_arguments.done': 'Tool arguments ready',
  'response.done': 'Response turn end',
  'response.cancelled': 'Response interrupted',
  error: 'Error event'
};
const timingFields = [
  'audio_start_ms',
  'audio_end_ms',
  'offset_ms',
  'timestamp_ms',
  'sample_rate_hz'
];

function member(value: NativeValue, name: string): NativeValue | undefined {
  return nativeObject(value) && Object.hasOwn(value, name)
    ? value[name]
    : undefined;
}

export function parseRealtimeTrace(source: string): RealtimeEventRow[] {
  const root = parseNativeJSON(source);
  const events = Array.isArray(root) ? root : member(root, 'events');
  if (!Array.isArray(events) || events.length > 1024)
    throw new Error('Paste a JSON array with at most 1024 native events.');
  return events.map((event, position) => {
    if (!nativeObject(event))
      throw new Error(`Event ${position + 1} must be a JSON object.`);
    const nativeType = member(event, 'type');
    const phase =
      typeof nativeType === 'string'
        ? (knownTypes[nativeType] ?? 'Unknown native event')
        : 'Unknown native event';
    const actor = member(event, 'role');
    const role =
      typeof actor === 'string' &&
      ['system', 'developer', 'user', 'assistant', 'tool'].includes(actor)
        ? actor
        : 'Not declared';
    const timing = timingFields.flatMap((name) => {
      const value = member(event, name);
      if (value === undefined || value === null) return [];
      const exact = stringifyNativeJSON(value);
      if (!/^-?\d+(?:\.\d+)?$/.test(exact) || exact.length > 32) return [];
      return [`${name}: ${exact}`];
    });
    return { position, phase, role, timing };
  });
}
