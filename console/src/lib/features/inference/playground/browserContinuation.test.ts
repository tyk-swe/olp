import { afterEach, describe, expect, it, vi } from 'vitest';
import { stringifyNativeJSON } from '$lib/json/nativeJson';
import {
  nativeChatRequest,
  nextTurn,
  recoverTurn,
  streamTurn,
  unaryTurn
} from './browserContinuation';

const submission = '1780000000000.11111111-2222-4333-8444-555555555555';
const nextSubmission = '1780000000001.11111111-2222-4333-8444-555555555556';
const handle = 'continuation_11111111222243338444555555555555';
const finalHandle = 'continuation_11111111222243338444555555555556';
const source =
  '{"model":"route","seed":9007199254740993,"messages":[{"role":"user","content":"Weather and time?"}],"tools":[{"type":"function","function":{"name":"weather","parameters":{"type":"object"}}},{"type":"function","function":{"name":"clock","parameters":{"type":"object"}}}]}';

function chunk(
  delta: Record<string, unknown>,
  observation?: Record<string, unknown>,
  finish: string | null = null,
  ready = false
) {
  return {
    choices: [{ index: 0, delta, finish_reason: finish }],
    olp: {
      version: 'chat-anthropic-tools-v1',
      ...(observation ? { observation } : {}),
      ...(ready ? { handle, ready: true } : {})
    }
  };
}

const recorded = [
  chunk({}, { index: 0, type: 'thinking', phase: 'start', opaque_state: true }),
  chunk(
    { content: 'before' },
    { index: 1, type: 'text', phase: 'start', text: 'before' }
  ),
  chunk(
    {
      tool_calls: [
        {
          index: 0,
          id: 'call-weather',
          type: 'function',
          function: { name: 'weather', arguments: '{"city":"Paris"}' }
        }
      ]
    },
    {
      index: 2,
      type: 'tool_use',
      phase: 'start',
      call_id: 'call-weather',
      name: 'weather'
    }
  ),
  chunk(
    {
      tool_calls: [
        {
          index: 1,
          id: 'call-clock',
          type: 'function',
          function: { name: 'clock', arguments: '{"zone":"Europe/Paris"}' }
        }
      ]
    },
    {
      index: 3,
      type: 'tool_use',
      phase: 'start',
      call_id: 'call-clock',
      name: 'clock'
    }
  ),
  chunk(
    { content: 'after' },
    { index: 4, type: 'text', phase: 'start', text: 'after' }
  ),
  chunk({}, undefined, 'tool_calls', true)
];

function sse(frames = recorded, terminal = true): Response {
  const text =
    frames.map((item) => `data: ${JSON.stringify(item)}\n\n`).join('') +
    (terminal ? 'data: [DONE]\n\n' : '');
  return new Response(text, {
    status: 200,
    headers: { 'Content-Type': 'text/event-stream' }
  });
}

afterEach(() => vi.restoreAllMocks());

describe('browser negotiated tool client', () => {
  it('withholds actions until ready and reconstructs exact ordered next-turn history', async () => {
    let release!: () => void;
    const pending = new Promise<void>((resolve) => (release = resolve));
    const encoder = new TextEncoder();
    const stream = new ReadableStream<Uint8Array>({
      async start(controller) {
        for (const item of recorded.slice(0, -1))
          controller.enqueue(
            encoder.encode(`data: ${JSON.stringify(item)}\n\n`)
          );
        await pending;
        controller.enqueue(
          encoder.encode(
            `data: ${JSON.stringify(recorded.at(-1))}\n\ndata: [DONE]\n\n`
          )
        );
        controller.close();
      }
    });
    const transport = vi
      .spyOn(globalThis, 'fetch')
      .mockResolvedValueOnce(new Response(stream, { status: 200 }));
    const request = nativeChatRequest(source, 'route');
    let ready = false;
    const inFlight = streamTurn('olp_secret', request, submission).then(
      (value) => {
        ready = true;
        return value;
      }
    );
    await Promise.resolve();
    expect(ready).toBe(false);
    release();
    const completed = await inFlight;
    expect(completed.assistant.content).toBe('beforeafter');
    expect(completed.assistant.tool_calls?.map((call) => call.id)).toEqual([
      'call-weather',
      'call-clock'
    ]);
    expect(completed.observations.map((item) => item.type)).toEqual([
      'thinking',
      'text',
      'tool_use',
      'tool_use',
      'text'
    ]);
    const sent = transport.mock.calls[0]![1]!;
    expect((sent.headers as Headers).get('Authorization')).toBe(
      'Bearer olp_secret'
    );
    expect((sent.headers as Headers).get('X-OLP-Submission-ID')).toBe(
      submission
    );
    expect(sent.credentials).toBe('omit');
    expect(sent.body).toContain('"seed":9007199254740993');
    const next = nextTurn(completed, [
      { tool_call_id: 'call-weather', content: 'sunny' },
      { tool_call_id: 'call-clock', content: '14:00' }
    ]);
    expect(stringifyNativeJSON(next)).toBe(
      '{"model":"route","seed":9007199254740993,"messages":[{"role":"user","content":"Weather and time?"},{"role":"assistant","content":"beforeafter","tool_calls":[{"id":"call-weather","type":"function","function":{"name":"weather","arguments":"{\\"city\\":\\"Paris\\"}"}},{"id":"call-clock","type":"function","function":{"name":"clock","arguments":"{\\"zone\\":\\"Europe/Paris\\"}"}}]},{"role":"tool","tool_call_id":"call-weather","content":"sunny"},{"role":"tool","tool_call_id":"call-clock","content":"14:00"}],"tools":[{"type":"function","function":{"name":"weather","parameters":{"type":"object"}}},{"type":"function","function":{"name":"clock","parameters":{"type":"object"}}}]}'
    );
    transport.mockResolvedValueOnce(
      Response.json({
        choices: [
          {
            message: { role: 'assistant', content: 'Both tools completed.' },
            finish_reason: 'stop'
          }
        ],
        olp: {
          version: 'chat-anthropic-tools-v1',
          handle: finalHandle,
          ready: true
        }
      })
    );
    const final = await unaryTurn('olp_secret', next, nextSubmission, handle);
    expect(final.assistant.content).toBe('Both tools completed.');
    expect(
      (transport.mock.calls[1]![1]!.headers as Headers).get(
        'X-OLP-Continuation-Handle'
      )
    ).toBe(handle);
    expect(transport.mock.calls[1]![1]!.body).toBe(stringifyNativeJSON(next));
  });

  it('rejects incomplete delivery and never exposes a tool as ready', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(sse(recorded.slice(0, -1)));
    await expect(
      streamTurn('olp_secret', nativeChatRequest(source, 'route'), submission)
    ).rejects.toThrow(/ready continuation/);
  });

  it('retains exact native cache-usage categories on a ready stream', async () => {
    const terminal = JSON.stringify(recorded.at(-1)).replace(
      '"ready":true}',
      '"ready":true,"native_usage":{"cache_read_input_tokens":9007199254740993,"cache_write_input_tokens":-0}}'
    );
    const streamSource =
      recorded
        .slice(0, -1)
        .map((item) => `data: ${JSON.stringify(item)}\n\n`)
        .join('') + `data: ${terminal}\n\ndata: [DONE]\n\n`;
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(streamSource, { status: 200 })
    );
    const turn = await streamTurn(
      'olp_secret',
      nativeChatRequest(source, 'route'),
      submission
    );
    expect(turn.nativeUsageRaw).toBe(
      '{"cache_read_input_tokens":9007199254740993,"cache_write_input_tokens":-0}'
    );
  });

  it('recovers only the committed delivery under the same key and submission', async () => {
    const assistant = {
      role: 'assistant',
      content: 'beforeafter',
      tool_calls: [
        {
          id: 'call-weather',
          type: 'function',
          function: { name: 'weather', arguments: '{"city":"Paris"}' }
        },
        {
          id: 'call-clock',
          type: 'function',
          function: { name: 'clock', arguments: '{"zone":"Europe/Paris"}' }
        }
      ]
    };
    const transport = vi.spyOn(globalThis, 'fetch').mockResolvedValueOnce(
      Response.json({
        version: 'chat-anthropic-tools-v1',
        state: 'ready',
        handle,
        assistant,
        delivery: { stream: true, frames: recorded }
      })
    );
    const recovered = await recoverTurn(
      'olp_secret',
      submission,
      nativeChatRequest(source, 'route')
    );
    expect(recovered.assistant).toEqual(assistant);
    expect(transport.mock.calls[0]![0]).toBe(
      `/v1/continuation-submissions/${submission}`
    );
    expect(transport.mock.calls[0]![1]!.method).toBe('GET');
  });
});
