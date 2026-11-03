import { beforeEach, describe, expect, it, vi } from 'vitest';
import { gatewayFetch } from '$lib/api/gateway';
import { stringifyNativeJSON } from '$lib/json/nativeJson';
import {
  continuationVersion,
  nativeChatRequest,
  nextTurn,
  recoverTurn,
  streamTurn,
  unaryTurn
} from './browserContinuation';

vi.mock('$lib/api/gateway', () => ({ gatewayFetch: vi.fn() }));

const submission = '123.00000000-0000-4000-8000-000000000001';
const handle = `continuation_${'a'.repeat(32)}`;
const source =
  '{"messages":[{"role":"user","content":"Weather?"}],' +
  '"tools":[{"type":"function","function":{"name":"lookup"}}],' +
  '"stream_options":{"include_usage":true},"seed":9007199254740993}';

beforeEach(() => {
  vi.mocked(gatewayFetch).mockReset();
});

describe('native chat requests', () => {
  it('binds the selected route without losing native number precision', () => {
    const request = nativeChatRequest(source, 'weather');

    expect(stringifyNativeJSON(request)).toContain('"model":"weather"');
    expect(stringifyNativeJSON(request)).toContain('"seed":9007199254740993');
  });

  it.each([
    ['[]', 'JSON object'],
    ['{"model":"other","messages":[{}],"tools":[{}]}', 'another route'],
    ['{"messages":[],"tools":[{}]}', 'ordered messages'],
    ['{"messages":[{}],"tools":[]}', 'nonempty tools']
  ])('rejects an invalid request before dispatch', (body, reason) => {
    expect(() => nativeChatRequest(body, 'weather')).toThrow(reason);
    expect(gatewayFetch).not.toHaveBeenCalled();
  });
});

describe('streamed continuation', () => {
  it('assembles fragmented events into a ready turn and submits ordered tool results', async () => {
    const request = nativeChatRequest(source, 'weather');
    const events = [
      {
        olp: {
          version: continuationVersion,
          observation: {
            index: 0,
            type: 'thinking',
            phase: 'start',
            opaque_state: true
          }
        },
        choices: []
      },
      {
        olp: { version: continuationVersion },
        choices: [
          {
            delta: {
              content: 'hé',
              tool_calls: [
                {
                  index: 0,
                  id: 'call_',
                  type: 'function',
                  function: { name: 'look', arguments: '{"q":"' }
                }
              ]
            },
            finish_reason: null
          }
        ]
      },
      {
        olp: { version: continuationVersion },
        choices: [
          {
            delta: {
              content: 'llo',
              tool_calls: [
                {
                  index: 0,
                  id: '123',
                  function: { name: 'up', arguments: 'rain"}' }
                }
              ]
            },
            finish_reason: null
          }
        ]
      },
      {
        olp: {
          version: continuationVersion,
          ready: true,
          handle,
          native_usage: { input_tokens: 42 }
        },
        choices: [{ delta: {}, finish_reason: 'tool_calls' }]
      }
    ];
    const body = `${events.map((event) => `data: ${JSON.stringify(event)}\r\n\r\n`).join('')}data: [DONE]\r\n\r\n`;
    const bytes = new TextEncoder().encode(body);
    const fragments = Array.from(
      { length: Math.ceil(bytes.length / 5) },
      (_, i) => bytes.slice(i * 5, i * 5 + 5)
    );
    vi.mocked(gatewayFetch).mockResolvedValueOnce(
      new Response(
        new ReadableStream({
          start(controller) {
            fragments.forEach((fragment) => controller.enqueue(fragment));
            controller.close();
          }
        })
      )
    );

    const turn = await streamTurn('olp_test', request, submission);

    expect(turn).toMatchObject({
      submission,
      handle,
      finish: 'tool_calls',
      nativeUsageRaw: '{"input_tokens":42}',
      observations: [
        { index: 0, type: 'thinking', phase: 'start', opaque_state: true }
      ],
      assistant: {
        role: 'assistant',
        content: 'héllo',
        tool_calls: [
          {
            id: 'call_123',
            type: 'function',
            function: { name: 'lookup', arguments: '{"q":"rain"}' }
          }
        ]
      }
    });
    const [path, options] = vi.mocked(gatewayFetch).mock.calls[0] ?? [];
    expect(path).toBe('/v1/chat/completions');
    expect(options?.method).toBe('POST');
    const headers = new Headers(options?.headers);
    expect(headers.get('Authorization')).toBe('Bearer olp_test');
    expect(headers.get('X-OLP-Continuation')).toBe(continuationVersion);
    expect(headers.get('X-OLP-Submission-ID')).toBe(submission);
    expect(options?.body).toContain('"stream":true');
    expect(options?.body).toContain('"seed":9007199254740993');

    expect(() => nextTurn(turn, [])).toThrow('one result');
    expect(() =>
      nextTurn(turn, [{ tool_call_id: 'wrong', content: 'rain' }])
    ).toThrow('call order and identity');
    const next = nextTurn(turn, [
      { tool_call_id: 'call_123', content: 'rain' }
    ]);
    expect(JSON.parse(stringifyNativeJSON(next))).toMatchObject({
      model: 'weather',
      messages: [
        { role: 'user', content: 'Weather?' },
        turn.assistant,
        { role: 'tool', tool_call_id: 'call_123', content: 'rain' }
      ]
    });
    expect(stringifyNativeJSON(next)).not.toContain('"stream_options"');
    expect(stringifyNativeJSON(next)).toContain('"seed":9007199254740993');
    expect(stringifyNativeJSON(request)).toContain('"stream_options"');
  });

  it.each([
    [
      `data: ${JSON.stringify({ olp: { version: continuationVersion }, choices: [] })}\n\n`,
      'no terminal marker'
    ],
    [
      `data: ${JSON.stringify({ olp: { version: continuationVersion }, choices: [] })}\n\ndata: [DONE]\n\n`,
      'before a ready continuation'
    ],
    [
      `data: ${JSON.stringify({ olp: { version: continuationVersion, ready: true, handle }, choices: [{ delta: {}, finish_reason: 'tool_calls' }] })}\n\ndata: [DONE]\n\n`,
      'does not match the observed calls'
    ],
    ['data: [DONE]\n\ndata: {}\n\n', 'followed [DONE]']
  ])('rejects an incomplete or inconsistent delivery', async (body, reason) => {
    vi.mocked(gatewayFetch).mockResolvedValueOnce(new Response(body));

    await expect(
      streamTurn('olp_test', nativeChatRequest(source, 'weather'), submission)
    ).rejects.toThrow(reason);
  });

  it('does not expose an upstream error body', async () => {
    vi.mocked(gatewayFetch).mockResolvedValueOnce(
      new Response('{"error":{"message":"secret credential"}}', { status: 502 })
    );

    await expect(
      streamTurn('olp_test', nativeChatRequest(source, 'weather'), submission)
    ).rejects.toThrow('Public inference request failed (502).');
  });
});

describe('follow-up and recovery', () => {
  const assistant = { role: 'assistant', content: 'It is raining.' };
  const delivery = {
    olp: {
      version: continuationVersion,
      ready: true,
      handle,
      observations: [{ index: 0, type: 'text', phase: 'end' }],
      native_usage: { output_tokens: 12 }
    },
    choices: [{ message: assistant, finish_reason: 'stop' }]
  };
  const recovery = {
    version: continuationVersion,
    state: 'ready',
    handle,
    assistant,
    delivery: { stream: false, body: delivery }
  };

  it('sends a follow-up under its parent handle and returns the committed result', async () => {
    const request = nativeChatRequest(source, 'weather');
    const signal = new AbortController().signal;
    vi.mocked(gatewayFetch).mockResolvedValueOnce(
      new Response(JSON.stringify(delivery))
    );

    const turn = await unaryTurn(
      'olp_test',
      request,
      submission,
      handle,
      signal
    );

    expect(turn).toEqual({
      submission,
      handle,
      request,
      assistant,
      observations: [{ index: 0, type: 'text', phase: 'end' }],
      finish: 'stop',
      nativeUsageRaw: '{"output_tokens":12}'
    });
    const [path, options] = vi.mocked(gatewayFetch).mock.calls[0] ?? [];
    expect(path).toBe('/v1/chat/completions');
    expect(options?.method).toBe('POST');
    expect(options?.signal).toBe(signal);
    expect(options?.body).toBe(stringifyNativeJSON(request));
    const headers = new Headers(options?.headers);
    expect(headers.get('X-OLP-Continuation-Handle')).toBe(handle);
    expect(headers.get('X-OLP-Submission-ID')).toBe(submission);
  });

  it('recovers an existing unary delivery without starting a new inference', async () => {
    const request = nativeChatRequest(source, 'weather');
    vi.mocked(gatewayFetch).mockResolvedValueOnce(
      new Response(JSON.stringify(recovery))
    );

    const turn = await recoverTurn('olp_test', submission, request);

    expect(turn).toEqual({
      submission,
      handle,
      request,
      assistant,
      observations: [{ index: 0, type: 'text', phase: 'end' }],
      finish: 'stop',
      nativeUsageRaw: '{"output_tokens":12}'
    });
    const [path, options] = vi.mocked(gatewayFetch).mock.calls[0] ?? [];
    expect(path).toBe(`/v1/continuation-submissions/${submission}`);
    expect(options?.method).toBe('GET');
    expect(options?.body).toBeUndefined();
    expect(gatewayFetch).toHaveBeenCalledOnce();
  });

  it('recovers a committed streamed delivery', async () => {
    const request = nativeChatRequest(source, 'weather');
    const streamed = {
      ...recovery,
      delivery: {
        stream: true,
        frames: [
          {
            olp: { version: continuationVersion, ready: true, handle },
            choices: [
              { delta: { content: assistant.content }, finish_reason: 'stop' }
            ]
          }
        ]
      }
    };
    vi.mocked(gatewayFetch).mockResolvedValueOnce(
      new Response(JSON.stringify(streamed))
    );

    await expect(recoverTurn('olp_test', submission, request)).resolves.toEqual(
      {
        submission,
        handle,
        request,
        assistant,
        observations: [],
        finish: 'stop',
        nativeUsageRaw: undefined
      }
    );
  });

  it.each([
    {
      saved: { ...recovery, state: 'pending' },
      reason: 'no ready delivery'
    },
    {
      saved: { ...recovery, assistant: { ...assistant, content: 'different' } },
      reason: 'assistant does not match'
    },
    {
      saved: { ...recovery, handle: `continuation_${'b'.repeat(32)}` },
      reason: 'handle does not match'
    }
  ])(
    'rejects a recovery that does not match committed work',
    async ({ saved, reason }) => {
      vi.mocked(gatewayFetch).mockResolvedValueOnce(
        new Response(JSON.stringify(saved))
      );

      await expect(
        recoverTurn(
          'olp_test',
          submission,
          nativeChatRequest(source, 'weather')
        )
      ).rejects.toThrow(reason);
      expect(vi.mocked(gatewayFetch).mock.calls[0]?.[1]?.method).toBe('GET');
    }
  );

  it('reports a key policy refusal without exposing the upstream body', async () => {
    vi.mocked(gatewayFetch).mockResolvedValueOnce(
      new Response(
        '{"error":{"code":"policy_conflict","message":"secret credential"}}',
        { status: 403 }
      )
    );

    await expect(
      unaryTurn(
        'olp_test',
        nativeChatRequest(source, 'weather'),
        submission,
        handle
      )
    ).rejects.toThrow('This key needs provider-state permission.');
  });
});
