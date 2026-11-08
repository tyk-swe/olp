import { afterEach, describe, expect, it, vi } from 'vitest';
import { json, route, serve } from '#lib/management/test/transport.js';
import {
  inspectRouting,
  runPlayground,
  streamPlayground,
  type InspectRoutingInput,
  type PlaygroundRequest,
  type PlaygroundStreamDone,
  type PlaygroundStreamProblem
} from './api';

const request: PlaygroundRequest = {
  model: 'route-a',
  input: 'Hello',
  surface: 'openai'
};

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('playground requests', () => {
  it('disables caching and returns the playground response', async () => {
    const response = {
      id: 'result',
      model: 'route-a',
      output_text: 'Hello back',
      latency_ms: 1,
      routing: [],
      tool_calls: []
    };
    serve(async (received) => {
      route(received, 'POST', '/api/v1/playground');
      expect(received.cache).toBe('no-store');
      expect(received.headers.get('cache-control')).toBe('no-store');
      expect(await received.json()).toEqual(request);
      return json(response);
    });

    await expect(runPlayground(request)).resolves.toEqual(response);
  });

  it('decodes fragmented streamed frames, completion and error events', async () => {
    const payload = new TextEncoder().encode(
      'event: frame\ndata: {"data":"héllo"}\n\n' +
        'event: frame\ndata: {"data":\ndata: "next"}\n\n' +
        'event: frame\ndata: {bad json}\n\n' +
        'event: unknown\ndata: {}\n\n' +
        'event: done\ndata: {"id":"result","model":"route-a"}\n\n' +
        'event: error\ndata: {"code":"unavailable","message":"Try again","status":503}\n\n'
    );
    const split = payload.indexOf(0xc3) + 1;
    const fragments = [payload.slice(0, split), payload.slice(split)];
    const controller = new AbortController();
    const handlers = {
      frame: vi.fn<(data: string) => void>(),
      done: vi.fn<(meta: PlaygroundStreamDone) => void>(),
      error: vi.fn<(problem: PlaygroundStreamProblem) => void>()
    };
    serve(async (received) => {
      route(received, 'POST', '/api/v1/playground/stream');
      expect(received.cache).toBe('no-store');
      expect(received.headers.get('cache-control')).toBe('no-store');
      expect(await received.json()).toEqual(request);
      controller.abort();
      expect(received.signal.aborted).toBe(true);
      return new Response(
        new ReadableStream({
          start(stream) {
            fragments.forEach((fragment) => stream.enqueue(fragment));
            stream.close();
          }
        }),
        { headers: { 'Content-Type': 'text/event-stream' } }
      );
    });

    await streamPlayground(request, handlers, controller.signal);

    expect(handlers.frame.mock.calls).toEqual([['héllo'], ['next']]);
    expect(handlers.done).toHaveBeenCalledExactlyOnceWith({
      id: 'result',
      model: 'route-a'
    });
    expect(handlers.error).toHaveBeenCalledExactlyOnceWith({
      code: 'unavailable',
      message: 'Try again',
      status: 503
    });
  });

  it('reports a rejected stream without dispatching events', async () => {
    const handlers = {
      frame: vi.fn<(data: string) => void>(),
      done: vi.fn<(meta: PlaygroundStreamDone) => void>(),
      error: vi.fn<(problem: PlaygroundStreamProblem) => void>()
    };
    serve((received) => {
      route(received, 'POST', '/api/v1/playground/stream');
      return json({ title: 'Unavailable', status: 503 }, 503);
    });

    await expect(
      streamPlayground(request, handlers, new AbortController().signal)
    ).rejects.toMatchObject({ problem: { status: 503 } });
    expect(handlers.frame).not.toHaveBeenCalled();
    expect(handlers.done).not.toHaveBeenCalled();
    expect(handlers.error).not.toHaveBeenCalled();
  });

  it('sends routing inspection context and cancellation to the simulator', async () => {
    const preferences: NonNullable<InspectRoutingInput['preferences']> = {
      allow_fallbacks: true,
      deny_data_collection: false,
      ignore: [],
      max_price: null,
      only: null,
      order: null,
      preferred_max_latency_ms: null,
      preferred_min_throughput: null,
      quantizations: null,
      regions: null,
      require_parameters: true,
      require_zero_data_retention: false,
      strategy: null,
      max_cost: null
    };
    const controller = new AbortController();
    const decisions = [{ provider_id: 'provider', decision: 'eligible' }];
    serve(async (received) => {
      route(received, 'POST', '/api/v1/routing/simulate');
      expect(await received.json()).toEqual({
        operation: {
          operation: 'generation',
          route: 'route-a',
          request: { input: 'Hello' }
        },
        surface: 'openai',
        mode: 'unary',
        api_key_id: 'key',
        dialect: 'openai-responses',
        client_contract: 'contract',
        preferences,
        seed: 'repeatable'
      });
      controller.abort();
      expect(received.signal.aborted).toBe(true);
      return json(decisions);
    });

    await expect(
      inspectRouting(
        {
          route: 'route-a',
          operation: 'generation',
          surface: 'openai',
          mode: 'unary',
          request: { input: 'Hello' },
          apiKeyId: 'key',
          dialect: 'openai-responses',
          clientContract: 'contract',
          preferences,
          seed: 'repeatable'
        },
        controller.signal
      )
    ).resolves.toEqual(decisions);
  });
});
