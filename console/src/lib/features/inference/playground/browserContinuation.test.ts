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

describe('browser continuation stream framing', () => {
  const encoder = new TextEncoder();
  const utf8Length = (text: string) => encoder.encode(text).length;
  const request = () => nativeChatRequest(source, 'route');
  const endings: Record<string, string> = { LF: '\n', CRLF: '\r\n', CR: '\r' };

  const wide = [
    chunk({}, { index: 0, type: 'text', phase: 'start', text: 'béfore' }),
    chunk({ content: 'béfore' }),
    chunk({ content: 'âfter' }),
    chunk({}, undefined, 'stop', true)
  ];

  function wire(frames: unknown[], ending = '\n'): string {
    return (
      frames
        .map((item) => `data: ${JSON.stringify(item)}${ending}${ending}`)
        .join('') + `data: [DONE]${ending}${ending}`
    );
  }

  function delivered(text: string, cuts: number[]): Response {
    const bytes = encoder.encode(text);
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        let offset = 0;
        for (const cut of cuts) {
          if (cut > offset) {
            controller.enqueue(bytes.subarray(offset, cut));
            offset = cut;
          }
        }
        controller.enqueue(bytes.subarray(offset));
        controller.close();
      }
    });
    return new Response(stream, {
      status: 200,
      headers: { 'Content-Type': 'text/event-stream' }
    });
  }

  function fragmented(text: string, size: number): Response {
    const cuts: number[] = [];
    for (let cut = size; cut < utf8Length(text); cut += size) cuts.push(cut);
    return delivered(text, cuts);
  }

  function contentEvent(wireBytes: number, ending: string): string {
    const pad =
      wireBytes -
      'data: '.length -
      2 * ending.length -
      utf8Length(JSON.stringify(chunk({ content: '' })));
    const content = 'é'.repeat(Math.floor(pad / 2)) + 'a'.repeat(pad % 2);
    return `data: ${JSON.stringify(chunk({ content }))}${ending}${ending}`;
  }

  it.each(Object.entries(endings))(
    'delivers the same ready continuation over %s line endings',
    async (_name, ending) => {
      for (const size of [1 << 20, 1, 7]) {
        vi.spyOn(globalThis, 'fetch').mockResolvedValueOnce(
          fragmented(wire(wide, ending), size)
        );
        const completed = await streamTurn('olp_secret', request(), submission);
        expect(completed.assistant.content).toBe('béforeâfter');
        expect(completed.assistant.tool_calls).toBeUndefined();
        expect(completed.observations.map((item) => item.type)).toEqual([
          'text'
        ]);
        expect(completed.handle).toBe(handle);
        expect(completed.finish).toBe('stop');
      }
    }
  );

  it('frames mixed endings and comment or control lines identically', async () => {
    const text =
      ': connective keepalive\n\n' +
      recorded
        .map(
          (item, index) =>
            `${index % 2 ? ':' : 'event: noted'}\r\ndata: ${JSON.stringify(item)}${
              index % 2 ? '\n\n' : '\r\n\r\n'
            }`
        )
        .join('') +
      'data: [DONE]\r\n\r\n';
    vi.spyOn(globalThis, 'fetch').mockResolvedValueOnce(fragmented(text, 3));
    const completed = await streamTurn('olp_secret', request(), submission);
    expect(completed.assistant.content).toBe('beforeafter');
    expect(completed.assistant.tool_calls?.map((call) => call.id)).toEqual([
      'call-weather',
      'call-clock'
    ]);
    expect(completed.handle).toBe(handle);
    expect(completed.finish).toBe('tool_calls');
  });

  it('keeps lossless native usage however the stream is fragmented', async () => {
    const terminal = JSON.stringify(recorded.at(-1)).replace(
      '"ready":true}',
      '"ready":true,"native_usage":{"cache_read_input_tokens":9007199254740993,"cache_write_input_tokens":-0}}'
    );
    const text =
      recorded
        .slice(0, -1)
        .map((item) => `data: ${JSON.stringify(item)}\r\n\r\n`)
        .join('') + `data: ${terminal}\r\n\r\ndata: [DONE]\r\n\r\n`;
    vi.spyOn(globalThis, 'fetch').mockResolvedValueOnce(fragmented(text, 1));
    const turn = await streamTurn('olp_secret', request(), submission);
    expect(turn.nativeUsageRaw).toBe(
      '{"cache_read_input_tokens":9007199254740993,"cache_write_input_tokens":-0}'
    );
  });

  it.each(Object.entries(endings))(
    'measures the wire event limit in bytes including %s framing',
    async (_name, ending) => {
      const terminal = `data: ${JSON.stringify(chunk({}, undefined, 'stop', true))}${ending}${ending}`;
      const marker = `data: [DONE]${ending}${ending}`;
      const transport = vi.spyOn(globalThis, 'fetch');
      const deliveries = (event: string) => [
        (text: string) => new Response(text, { status: 200 }),
        (text: string) => fragmented(text, 4099),
        (text: string) => delivered(text, [utf8Length(event) - 1])
      ];
      const exact = contentEvent(1 << 20, ending);
      for (const deliver of deliveries(exact)) {
        transport.mockResolvedValueOnce(deliver(exact + terminal + marker));
        const completed = await streamTurn('olp_secret', request(), submission);
        expect(completed.handle).toBe(handle);
      }
      const over = contentEvent((1 << 20) + 1, ending);
      for (const deliver of deliveries(over)) {
        transport.mockResolvedValueOnce(deliver(over + terminal + marker));
        await expect(
          streamTurn('olp_secret', request(), submission)
        ).rejects.toThrow('A continuation event exceeds the client limit.');
      }
    }
  );

  it('measures the stream limit in received bytes', async () => {
    const terminal = `data: ${JSON.stringify(chunk({}, undefined, 'stop', true))}\n\n`;
    const marker = 'data: [DONE]\n\n';
    const room = (8 << 20) - utf8Length(terminal + marker);
    const each = Math.floor(room / 8);
    const sizes = Array.from({ length: 8 }, (_, index) =>
      index === 7 ? room - each * 7 : each
    );
    const exact =
      sizes.map((size) => contentEvent(size, '\n')).join('') +
      terminal +
      marker;
    expect(utf8Length(exact)).toBe(8 << 20);
    vi.spyOn(globalThis, 'fetch').mockResolvedValueOnce(
      new Response(exact, { status: 200 })
    );
    const completed = await streamTurn('olp_secret', request(), submission);
    expect(completed.handle).toBe(handle);

    const over =
      contentEvent(each + 1, '\n') +
      sizes
        .slice(1)
        .map((size) => contentEvent(size, '\n'))
        .join('') +
      terminal +
      marker;
    vi.spyOn(globalThis, 'fetch').mockResolvedValueOnce(
      fragmented(over, 1 << 20)
    );
    await expect(
      streamTurn('olp_secret', request(), submission)
    ).rejects.toThrow('Continuation stream exceeds the client limit.');
  });

  it('measures retained tool arguments in UTF-8 bytes', async () => {
    const half = 'é'.repeat(300_000);
    const text = wire([
      chunk({
        tool_calls: [
          {
            index: 0,
            id: 'call-wide',
            type: 'function',
            function: { name: 'wide', arguments: `{"a":"${half}` }
          }
        ]
      }),
      chunk({
        tool_calls: [{ index: 0, function: { arguments: `${half}"}` } }]
      }),
      chunk({}, undefined, 'tool_calls', true)
    ]);
    vi.spyOn(globalThis, 'fetch').mockResolvedValueOnce(
      new Response(text, { status: 200 })
    );
    await expect(
      streamTurn('olp_secret', request(), submission)
    ).rejects.toThrow('Tool arguments exceed the client limit.');
  });

  it('measures unary and recovery result bodies in UTF-8 bytes', async () => {
    const body = JSON.stringify({
      choices: [
        {
          message: { role: 'assistant', content: 'é'.repeat(4_300_000) },
          finish_reason: 'stop'
        }
      ],
      olp: { version: 'chat-anthropic-tools-v1', handle, ready: true }
    });
    const transport = vi.spyOn(globalThis, 'fetch');
    transport.mockResolvedValueOnce(new Response(body, { status: 200 }));
    await expect(
      unaryTurn('olp_secret', request(), nextSubmission, handle)
    ).rejects.toThrow('The continuation result exceeds the client limit.');

    const recovery = JSON.stringify({
      version: 'chat-anthropic-tools-v1',
      state: 'ready',
      handle,
      assistant: { role: 'assistant', content: 'é'.repeat(4_300_000) },
      delivery: {
        stream: false,
        body: {
          choices: [
            {
              message: { role: 'assistant', content: 'é'.repeat(4_300_000) },
              finish_reason: 'stop'
            }
          ],
          olp: { version: 'chat-anthropic-tools-v1', handle, ready: true }
        }
      }
    });
    transport.mockResolvedValueOnce(new Response(recovery, { status: 200 }));
    await expect(
      recoverTurn('olp_secret', submission, request())
    ).rejects.toThrow('Recovery result exceeds the client limit.');
  });

  it('cancels the reader without masking a rejection', async () => {
    let cancelled = false;
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(encoder.encode(`data: ${'x'.repeat(2 << 20)}\n\n`));
      },
      cancel() {
        cancelled = true;
        return Promise.reject(new Error('teardown failed'));
      }
    });
    vi.spyOn(globalThis, 'fetch').mockResolvedValueOnce(
      new Response(stream, { status: 200 })
    );
    await expect(
      streamTurn('olp_secret', request(), submission)
    ).rejects.toThrow('A continuation event exceeds the client limit.');
    expect(cancelled).toBe(true);
  });

  it('rejects events after [DONE], mid-event endings, and missing markers', async () => {
    const framesText = recorded
      .map((item) => `data: ${JSON.stringify(item)}\n\n`)
      .join('');
    const transport = vi.spyOn(globalThis, 'fetch');
    transport.mockResolvedValueOnce(
      new Response(
        framesText +
          'data: [DONE]\n\n' +
          `data: ${JSON.stringify(recorded[0])}\n\n`,
        { status: 200 }
      )
    );
    await expect(
      streamTurn('olp_secret', request(), submission)
    ).rejects.toThrow('A continuation event followed [DONE].');

    transport.mockResolvedValueOnce(
      new Response(framesText + 'data: {"olp":', { status: 200 })
    );
    await expect(
      streamTurn('olp_secret', request(), submission)
    ).rejects.toThrow('The continuation stream ended mid-event.');

    transport.mockResolvedValueOnce(new Response(framesText, { status: 200 }));
    await expect(
      streamTurn('olp_secret', request(), submission)
    ).rejects.toThrow('The continuation stream has no terminal marker.');
  });
});
