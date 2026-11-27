import { describe, expect, it, vi } from 'vitest';
import {
  assembleContinuation,
  continuationVersion,
  extractNativeUsage,
  isValidHandle,
  parseAssistant,
  parseContinuationChunk,
  readContinuationFrames,
  validHandle
} from './continuationStream';
import { MAX_PUBLIC_REQUEST_BYTES } from './publicGateway';

const VALID_HANDLE = 'continuation_0123456789abcdef0123456789abcdef';

function streamResponse(chunks: readonly Uint8Array[]): Response {
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      for (const chunk of chunks) controller.enqueue(chunk);
      controller.close();
    }
  });
  return new Response(body, {
    status: 200,
    headers: { 'content-type': 'text/event-stream' }
  });
}

function encode(text: string): Uint8Array {
  return new TextEncoder().encode(text);
}

function frame(olp: Record<string, unknown>, choice?: Record<string, unknown>) {
  return JSON.stringify({
    olp: { version: continuationVersion, ...olp },
    choices: choice ? [choice] : []
  });
}

describe('handles and assistant parsing', () => {
  it('accepts canonical continuation handles and rejects malformed ones', () => {
    expect(isValidHandle(VALID_HANDLE)).toBe(true);
    expect(validHandle(VALID_HANDLE)).toBe(VALID_HANDLE);
    expect(isValidHandle('continuation_0123')).toBe(false);
    expect(isValidHandle('continuation_0123456789ABCDEF0123456789abcdef')).toBe(
      false
    );
    expect(() => validHandle('bad')).toThrow('Incomplete continuation');
    expect(() => validHandle(123)).toThrow('Incomplete continuation');
  });

  it('parses assistant messages with and without function tool calls', () => {
    expect(parseAssistant({ role: 'assistant', content: 'hi' })).toEqual({
      role: 'assistant',
      content: 'hi'
    });
    expect(
      parseAssistant({
        role: 'assistant',
        content: '',
        tool_calls: [
          {
            id: 'call_1',
            type: 'function',
            function: { name: 'lookup', arguments: '{"q":"a"}' }
          }
        ]
      })
    ).toEqual({
      role: 'assistant',
      content: '',
      tool_calls: [
        {
          id: 'call_1',
          type: 'function',
          function: { name: 'lookup', arguments: '{"q":"a"}' }
        }
      ]
    });
    expect(() => parseAssistant({ role: 'user', content: 'hi' })).toThrow(
      'Incomplete assistant continuation.'
    );
    expect(() =>
      parseAssistant({
        role: 'assistant',
        content: '',
        tool_calls: [
          {
            id: 'c1',
            type: 'custom',
            function: { name: 'f', arguments: '{}' }
          }
        ]
      })
    ).toThrow('Unsupported assistant tool type.');
  });

  it('extracts native usage objects and rejects non-object native usage', () => {
    expect(
      extractNativeUsage({ olp: { native_usage: { input_tokens: 12 } } })
    ).toBe('{"input_tokens":12}');
    expect(extractNativeUsage({ olp: {} })).toBeUndefined();
    expect(() =>
      extractNativeUsage({ olp: { native_usage: 'invalid' } })
    ).toThrow('Invalid native usage categories');
  });
});

describe('readContinuationFrames SSE framing', () => {
  it('reassembles split CRLF boundaries, multi-line data, UTF-8, and keepalives', async () => {
    const first = frame({}, { delta: { content: 'hé' } });
    const splitAt = first.indexOf(',"choices":');
    const second = frame(
      { ready: true, handle: VALID_HANDLE },
      { delta: { content: 'llo' }, finish_reason: 'stop' }
    );
    const full = encode(
      `: keepalive\r\n\r\ndata: ${first.slice(0, splitAt)}\r\ndata: ${first.slice(splitAt)}\r\n\r\ndata: ${second}\r\r\ndata: [DONE]\n\n`
    );
    const chunks: Uint8Array[] = [];
    for (let i = 0; i < full.length; i += 3) {
      chunks.push(full.subarray(i, i + 3));
    }
    const frames = await readContinuationFrames(streamResponse(chunks));
    expect(frames).toHaveLength(2);
    const assembled = assembleContinuation(frames);
    expect(assembled.assistant).toEqual({
      role: 'assistant',
      content: 'héllo'
    });
    expect(assembled.handle).toBe(VALID_HANDLE);
    expect(assembled.finish).toBe('stop');
  });

  it('rejects missing [DONE], mid-event EOF, events after [DONE], and cancels reader on error', async () => {
    const one = frame(
      { ready: true, handle: VALID_HANDLE },
      { delta: { content: 'hi' }, finish_reason: 'stop' }
    );
    await expect(
      readContinuationFrames(streamResponse([encode(`data: ${one}\n\n`)]))
    ).rejects.toThrow('no terminal marker');

    await expect(
      readContinuationFrames(streamResponse([encode(`data: ${one}`)]))
    ).rejects.toThrow('ended mid-event');

    await expect(
      readContinuationFrames(
        streamResponse([encode(`data: [DONE]\n\ndata: ${one}\n\n`)])
      )
    ).rejects.toThrow('followed [DONE]');

    await expect(readContinuationFrames(new Response(null))).rejects.toThrow(
      'stream is unavailable'
    );

    const cancel = vi.fn().mockResolvedValue(undefined);
    const oversized = new Uint8Array(MAX_PUBLIC_REQUEST_BYTES + 16).fill(0x61);
    const body = new ReadableStream<Uint8Array>({
      pull(controller) {
        controller.enqueue(oversized);
      },
      cancel
    });
    await expect(readContinuationFrames(new Response(body))).rejects.toThrow(
      'exceeds the client limit'
    );
    expect(cancel).toHaveBeenCalledOnce();
  });

  it('rejects continuation chunks with another version', () => {
    expect(() =>
      parseContinuationChunk(
        JSON.stringify({ olp: { version: 'other-v2' }, choices: [] })
      )
    ).toThrow('another continuation version');
  });
});

describe('assembleContinuation', () => {
  it('assembles interleaved tool call deltas, observations, and latest native usage', () => {
    const frames = [
      parseContinuationChunk(
        frame(
          {
            observation: {
              index: 0,
              type: 'thinking',
              phase: 'delta',
              text: 'plan'
            },
            native_usage: { input_tokens: 10 }
          },
          {
            delta: {
              tool_calls: [
                {
                  index: 1,
                  id: 'call_2',
                  type: 'function',
                  function: { name: 'second', arguments: '{"b":' }
                },
                {
                  index: 0,
                  id: 'call_1',
                  type: 'function',
                  function: { name: 'first', arguments: '{"a":1}' }
                }
              ]
            }
          }
        )
      ),
      parseContinuationChunk(
        frame(
          {
            ready: true,
            handle: VALID_HANDLE,
            native_usage: { input_tokens: 10, output_tokens: 5 }
          },
          {
            delta: {
              tool_calls: [{ index: 1, function: { arguments: '2}' } }]
            },
            finish_reason: 'tool_calls'
          }
        )
      )
    ];

    const got = assembleContinuation(frames);
    expect(got.handle).toBe(VALID_HANDLE);
    expect(got.finish).toBe('tool_calls');
    expect(got.observations).toEqual([
      { index: 0, type: 'thinking', phase: 'delta', text: 'plan' }
    ]);
    expect(got.nativeUsageRaw).toBe('{"input_tokens":10,"output_tokens":5}');
    expect(got.assistant.tool_calls).toEqual([
      {
        id: 'call_1',
        type: 'function',
        function: { name: 'first', arguments: '{"a":1}' }
      },
      {
        id: 'call_2',
        type: 'function',
        function: { name: 'second', arguments: '{"b":2}' }
      }
    ]);
  });

  it('rejects tool/finish mismatches, non-contiguous tool indices, observations after terminal, and missing ready terminal', () => {
    expect(() =>
      assembleContinuation([
        parseContinuationChunk(
          frame(
            { ready: true, handle: VALID_HANDLE },
            { delta: { content: 'no tools' }, finish_reason: 'tool_calls' }
          )
        )
      ])
    ).toThrow('Tool terminal does not match');

    expect(() =>
      assembleContinuation([
        parseContinuationChunk(
          frame(
            { ready: true, handle: VALID_HANDLE },
            {
              delta: {
                tool_calls: [
                  {
                    index: 1,
                    id: 'c1',
                    type: 'function',
                    function: { name: 'f', arguments: '{}' }
                  }
                ]
              },
              finish_reason: 'tool_calls'
            }
          )
        )
      ])
    ).toThrow('Incomplete or reordered tool call.');

    expect(() =>
      assembleContinuation([
        parseContinuationChunk(
          frame(
            { ready: true, handle: VALID_HANDLE },
            { delta: { content: 'done' }, finish_reason: 'stop' }
          )
        ),
        parseContinuationChunk(
          frame({
            observation: { index: 0, type: 'text', phase: 'end', text: 'late' }
          })
        )
      ])
    ).toThrow('Observation followed terminal delivery.');

    expect(() =>
      assembleContinuation([
        parseContinuationChunk(
          frame(
            { ready: false, handle: VALID_HANDLE },
            { delta: { content: 'done' }, finish_reason: 'stop' }
          )
        )
      ])
    ).toThrow('no committed continuation');

    expect(() =>
      assembleContinuation([
        parseContinuationChunk(frame({}, { delta: { content: 'partial' } }))
      ])
    ).toThrow('ended before a ready continuation');
  });
});
