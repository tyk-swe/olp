import { afterEach, describe, expect, it, onTestFinished, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import { json, route, serve } from '#lib/management/test/transport.js';
import StateHarness from '../../../../../tests/unit/support/StateHarness.svelte';
import { PlaygroundState } from './playgroundState.svelte';
import { playgroundTemplates } from './templates';

vi.mock('#lib/features/routes/api.js', () => ({
  listRoutes: vi.fn().mockResolvedValue([]),
  simulateRouting: vi.fn().mockResolvedValue([])
}));
vi.mock('#lib/features/access/api-keys/api.js', () => ({
  listApiKeys: vi.fn().mockResolvedValue([])
}));

afterEach(() => {
  vi.unstubAllGlobals();
});

function playground(): PlaygroundState {
  const ready = vi.fn((state: PlaygroundState) => state);
  const component = mount(StateHarness, {
    target: document.createElement('div'),
    props: { setup: () => ready(new PlaygroundState()) }
  });
  onTestFinished(() => unmount(component));
  flushSync();
  return ready.mock.results[0].value;
}

function submitEvent(): SubmitEvent {
  return new SubmitEvent('submit', { cancelable: true });
}

describe('playground composer', () => {
  it('applies a selected example to its operation, dialect and request', () => {
    const state = playground();
    const template = playgroundTemplates.find((item) => item.nativeDialect);
    expect(template).toBeDefined();
    if (!template) return;

    state.applyTemplate(template.key);

    expect(state.templateKey).toBe(template.key);
    expect(state.operation).toBe(template.operation);
    expect(state.nativeDialect).toBe(template.nativeDialect);
    expect(state.inspectDialect).toBe(template.inspectDialect ?? '');
    expect(JSON.parse(state.rawJson)).toEqual(template.request);
  });

  it('rejects a submission without a route or prompt before dispatch', async () => {
    const handler = vi.fn(() => json({}));
    serve(handler);
    const state = playground();
    const event = submitEvent();

    await state.submit(event);
    expect(event.defaultPrevented).toBe(true);
    expect(state.validationError).toBe('Enter an active route slug.');
    state.model = 'route-a';
    await state.submit(submitEvent());
    expect(state.validationError).toBe('Enter a prompt.');
    expect(handler).not.toHaveBeenCalled();
  });

  it('sends the composed request and displays its response', async () => {
    const handler = vi.fn(async (request: Request) => {
      route(request, 'POST', '/api/v1/playground');
      expect(await request.json()).toMatchObject({
        model: 'route-a',
        input: 'Hello',
        surface: 'openai',
        routing: {},
        temperature: 0.5,
        max_output_tokens: 100
      });
      return json({
        id: 'response',
        model: 'route-a',
        output_text: 'Hello back',
        latency_ms: 1,
        routing: [],
        tool_calls: []
      });
    });
    serve(handler);
    const state = playground();
    state.model = ' route-a ';
    state.input = 'Hello';
    state.temperature = '0.5';
    state.maxOutputTokens = '100';

    await state.submit(submitEvent());

    expect(handler).toHaveBeenCalledOnce();
    expect(state.completedRequest?.model).toBe('route-a');
    await vi.waitFor(() =>
      expect(state.mutation.data?.output_text).toBe('Hello back')
    );
  });

  it('sends an advanced request and reports invalid JSON before dispatch', async () => {
    const handler = vi.fn(async (request: Request) => {
      route(request, 'POST', '/api/v1/playground');
      expect(await request.json()).toMatchObject({
        model: 'route-a',
        operation: 'generation',
        request: { messages: [{ role: 'user', content: 'Hello' }] }
      });
      return json({
        id: 'response',
        model: 'route-a',
        output_text: 'Done',
        latency_ms: 1,
        routing: [],
        tool_calls: []
      });
    });
    serve(handler);
    const state = playground();
    state.model = 'route-a';
    state.composer = 'advanced';
    state.rawJson = '{broken';

    await state.submit(submitEvent());
    expect(state.validationError).toBeTruthy();
    expect(handler).not.toHaveBeenCalled();
    state.rawJson = JSON.stringify({
      messages: [{ role: 'user', content: 'Hello' }]
    });
    await state.submit(submitEvent());
    expect(state.validationError).toBe('');
    expect(handler).toHaveBeenCalledOnce();
    await vi.waitFor(() =>
      expect(state.mutation.data?.output_text).toBe('Done')
    );
  });

  it.each([
    ['translation', 'Use the audio upload form below.'],
    [
      'realtime',
      'Use the local realtime event viewer below; it does not open a provider session.'
    ],
    [
      'search',
      'Use the public native client below with a route certified for this operation.'
    ]
  ] as const)(
    'directs %s requests to their dedicated client',
    async (operation, error) => {
      const handler = vi.fn(() => json({}));
      serve(handler);
      const state = playground();
      state.model = 'route-a';
      state.composer = 'advanced';
      state.operation = operation;

      await state.submit(submitEvent());

      expect(state.validationError).toBe(error);
      expect(handler).not.toHaveBeenCalled();
    }
  );

  it('does not run an unverified stream and clears a disabled capability check', async () => {
    const handler = vi.fn(() => json({}));
    serve(handler);
    const state = playground();
    state.model = 'unknown-route';
    state.input = 'Hello';
    flushSync();

    state.toggleStream(true);
    expect(state.streamCheck).toBe('unknown');
    await state.submit(submitEvent());
    expect(state.validationError).toBe(
      'Streaming has not been verified as available for this route.'
    );
    expect(handler).not.toHaveBeenCalled();
    state.toggleStream(false);
    expect(state.streamCheck).toBe('idle');
    expect(state.streamCheckMessage).toBe('');
  });
});
