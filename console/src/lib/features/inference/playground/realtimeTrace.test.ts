import { describe, expect, it } from 'vitest';
import { parseRealtimeTrace } from './realtimeTrace';

describe('realtime trace presentation', () => {
  it('presents native events in order with their roles and known phases', () => {
    const events = [
      { type: 'session.created', role: 'system' },
      { type: 'input_audio_buffer.speech_started', role: 'user' },
      { type: 'response.function_call_arguments.done', role: 'assistant' },
      { type: 'response.cancelled', role: 'tool' }
    ];
    const wrapped = parseRealtimeTrace(JSON.stringify({ events }));
    expect(wrapped).toEqual({
      ok: true,
      value: [
        { position: 0, phase: 'Session created', role: 'system', timing: [] },
        { position: 1, phase: 'VAD speech start', role: 'user', timing: [] },
        {
          position: 2,
          phase: 'Tool arguments ready',
          role: 'assistant',
          timing: []
        },
        { position: 3, phase: 'Response interrupted', role: 'tool', timing: [] }
      ]
    });
    expect(parseRealtimeTrace(JSON.stringify(events))).toEqual(wrapped);
  });

  it('preserves exact decimal timing values and ignores unsupported values', () => {
    const source = `[{"type":"response.audio.delta","role":"developer",
      "audio_start_ms":9007199254740993.0123456789,"audio_end_ms":-12.5,
      "offset_ms":"42","timestamp_ms":null,"sample_rate_hz":48000}]`;
    expect(parseRealtimeTrace(source)).toEqual({
      ok: true,
      value: [
        {
          position: 0,
          phase: 'Output audio chunk',
          role: 'developer',
          timing: [
            'audio_start_ms: 9007199254740993.0123456789',
            'audio_end_ms: -12.5',
            'sample_rate_hz: 48000'
          ]
        }
      ]
    });

    const filtered = parseRealtimeTrace(
      `[{"offset_ms":1e3,"timestamp_ms":${'9'.repeat(33)}}]`
    );
    expect(filtered).toEqual({
      ok: true,
      value: [
        {
          position: 0,
          phase: 'Unknown native event',
          role: 'Not declared',
          timing: []
        }
      ]
    });
  });

  it('shows unknown types and unsupported roles without inventing labels', () => {
    expect(
      parseRealtimeTrace(
        JSON.stringify([
          { type: 'new.vendor.event', role: 'client' },
          { type: 'toString' },
          {}
        ])
      )
    ).toEqual({
      ok: true,
      value: [
        {
          position: 0,
          phase: 'Unknown native event',
          role: 'Not declared',
          timing: []
        },
        {
          position: 1,
          phase: 'Unknown native event',
          role: 'Not declared',
          timing: []
        },
        {
          position: 2,
          phase: 'Unknown native event',
          role: 'Not declared',
          timing: []
        }
      ]
    });
  });

  it.each(['{}', '{"events":{}}', 'null', '1'])(
    'requires an event array in %s',
    (source) => {
      expect(parseRealtimeTrace(source)).toEqual({
        ok: false,
        error: 'Paste a JSON array with at most 1024 native events.'
      });
    }
  );

  it('returns an error value for invalid JSON', () => {
    const result = parseRealtimeTrace('{broken');
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.error).toBeTruthy();
  });

  it('identifies a non-object event by its position', () => {
    expect(parseRealtimeTrace('[{},null]')).toEqual({
      ok: false,
      error: 'Event 2 must be a JSON object.'
    });
  });

  it('limits the number of events', () => {
    const allowed = parseRealtimeTrace(
      JSON.stringify(Array.from({ length: 1024 }, () => ({})))
    );
    expect(allowed.ok).toBe(true);
    if (allowed.ok) expect(allowed.value).toHaveLength(1024);

    expect(
      parseRealtimeTrace(
        JSON.stringify(Array.from({ length: 1025 }, () => ({})))
      )
    ).toEqual({
      ok: false,
      error: 'Paste a JSON array with at most 1024 native events.'
    });
  });
});
