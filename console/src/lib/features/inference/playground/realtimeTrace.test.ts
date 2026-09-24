import { describe, expect, it } from 'vitest';
import { parseRealtimeTrace } from './realtimeTrace';

describe('local realtime trace presentation', () => {
  it('retains event order and exact timing while hiding content and unknown types', () => {
    const rows = parseRealtimeTrace(
      '[{"type":"input_audio_buffer.speech_started","audio_start_ms":9007199254740993,"audio":"private-audio"},{"type":"response.created","role":"assistant","text":"private-text"},{"type":"conversation.item.truncated","offset_ms":-0,"item_id":"private-state"},{"type":"private-type","role":"private-role","message":"private-message"}]'
    );
    expect(rows.map((row) => row.phase)).toEqual([
      'VAD speech start',
      'Response turn start',
      'Interruption / truncation',
      'Unknown native event'
    ]);
    expect(rows[0].timing).toEqual(['audio_start_ms: 9007199254740993']);
    expect(rows[2].timing).toEqual(['offset_ms: -0']);
    expect(JSON.stringify(rows)).not.toContain('private-');
  });

  it('rejects non-array, malformed and excessive traces before presentation', () => {
    expect(() => parseRealtimeTrace('{}')).toThrow(/JSON array/);
    expect(() => parseRealtimeTrace('["text"]')).toThrow(/JSON object/);
    expect(() =>
      parseRealtimeTrace(
        '{"events":[' + Array(1025).fill('{}').join(',') + ']}'
      )
    ).toThrow(/at most 1024/);
  });
});
