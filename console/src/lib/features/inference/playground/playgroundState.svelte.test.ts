import { afterEach, describe, expect, it, onTestFinished, vi } from 'vitest';
import { flushSync, getAllContexts, mount, unmount } from 'svelte';
import { json, route, serve } from '#lib/management/test/transport.js';
import StateHarness from '../../../../../tests/unit/support/StateHarness.svelte';
import { listRoutes, simulateRouting } from '#lib/features/routes/api.js';
import AgentResources from '#lib/features/resources/AgentResources.svelte';
import NativeResources from '#lib/features/resources/NativeResources.svelte';
import CostCalculator from '#lib/features/usage/CostCalculator.svelte';
import PlaygroundPage from './PlaygroundPage.svelte';
import {
  PlaygroundState,
  resolvePlaygroundPlan
} from './playgroundState.svelte';
import { playgroundTemplates } from './templates';

vi.mock(import('#lib/features/routes/api.js'), async (importOriginal) => ({
  ...(await importOriginal()),
  listRoutes: vi.fn().mockResolvedValue([]),
  simulateRouting: vi.fn().mockResolvedValue([])
}));
vi.mock('#lib/features/access/api-keys/api.js', () => ({
  listApiKeys: vi.fn().mockResolvedValue([])
}));

afterEach(() => {
  vi.unstubAllGlobals();
  vi.mocked(listRoutes).mockResolvedValue([]);
  vi.mocked(simulateRouting).mockResolvedValue([]);
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

const publishedRoute = {
  id: 'active-id',
  slug: 'route-a',
  state: 'active' as const,
  latest_revision: {
    operations: ['generation', 'vector_store', 'agent_resource']
  }
};

function listPublishedRoutes() {
  vi.mocked(listRoutes, { partial: true, deep: true }).mockResolvedValue([
    {
      ...publishedRoute,
      id: 'retired-id',
      slug: 'old-route',
      state: 'retired'
    },
    publishedRoute
  ]);
}

describe('playground composer', () => {
  it('offers retired routes for retained lists and only serving routes for new requests and estimates', async () => {
    listPublishedRoutes();
    serve(
      (request) => {
        const path = new URL(request.url).pathname;
        if (path === '/api/v1/auth/capabilities')
          return json({
            local_login_enabled: false,
            oidc_login_enabled: true,
            gateway_available: true
          });
        if (path === '/api/v1/provider-resource-contracts')
          return json({
            endpoints: [
              {
                operation: 'vector_store',
                surface: 'openai',
                kind: 'vector_store',
                action: 'create',
                method: 'POST',
                pattern: '/v1/vector_stores',
                ancestors: [],
                metered: true
              },
              {
                operation: 'vector_store',
                surface: 'openai',
                kind: 'vector_store',
                action: 'list',
                method: 'GET',
                pattern: '/v1/vector_stores',
                ancestors: [],
                metered: true
              }
            ]
          });
        return json({ items: [] });
      },
      ['read']
    );
    const target = document.createElement('div');
    const setup = vi.fn(() => {
      const options = { target, context: getAllContexts() };
      return [
        mount(PlaygroundPage, options),
        mount(AgentResources, options),
        mount(NativeResources, options),
        mount(CostCalculator, options)
      ];
    });
    const harness = mount(StateHarness, { target, props: { setup } });
    onTestFinished(async () => {
      await Promise.all(setup.mock.results[0].value.map(unmount));
      await unmount(harness);
    });
    const values = (selector: string) =>
      Array.from(
        target.querySelectorAll<HTMLOptionElement>(selector),
        (option) => option.value
      );

    await vi.waitFor(() => {
      expect(values('#playground-routes option')).toEqual(['route-a']);
      expect(
        values('select option').filter((value) => value === 'route-a')
      ).toHaveLength(3);
      expect(values('select option')).not.toContain('old-route');
      expect(target.querySelector('#calc-route')).toHaveProperty(
        'value',
        'route-a'
      );
    });

    const action = target.querySelector<HTMLSelectElement>(
      'section[aria-labelledby="native-resources-title"] select:has(option[value="GET /v1/vector_stores"])'
    );
    if (!action) throw new Error('Missing native resource action');
    const choose = (select: HTMLSelectElement, value: string) => {
      select.value = value;
      select.dispatchEvent(new Event('change', { bubbles: true }));
      flushSync();
    };
    choose(action, 'GET /v1/vector_stores');
    const nativeRoutes =
      'section[aria-labelledby="native-resources-title"] select:has(option[value="route-a"])';
    expect(values(`${nativeRoutes} option`)).toEqual([
      '',
      'old-route',
      'route-a'
    ]);
    const route = target.querySelector<HTMLSelectElement>(nativeRoutes);
    if (!route) throw new Error('Missing native resource route');
    choose(route, 'old-route');
    choose(action, 'POST /v1/vector_stores');
    expect(values(`${nativeRoutes} option`)).toEqual(['', 'route-a']);
    expect(target.querySelector(nativeRoutes)).toHaveProperty('value', '');
  });

  it('shows the surface of the selected Decisions dialect', () => {
    serve(() => json({}));
    const target = document.createElement('div');
    document.body.append(target);
    const setup = vi.fn(() =>
      mount(PlaygroundPage, { target, context: getAllContexts() })
    );
    const harness = mount(StateHarness, { target, props: { setup } });
    onTestFinished(async () => {
      await unmount(setup.mock.results[0].value);
      await unmount(harness);
      target.remove();
    });
    const control = (name: string) =>
      Array.from(target.querySelectorAll('label')).find(
        (label) => label.textContent?.trim() === name
      )?.control;
    const select = (label: string, value: string) => {
      const element = control(label);
      if (!(element instanceof HTMLSelectElement))
        throw new Error(`Missing ${label} select`);
      element.value = value;
      element.dispatchEvent(new Event('change', { bubbles: true }));
      flushSync();
    };
    flushSync();
    control('Advanced')?.click();
    flushSync();
    select('Operation', 'decision');
    const surface = control('Client surface');
    expect(surface).toHaveProperty('value', 'openai');
    select('Registered native dialect', 'gemini-decisions');
    expect(surface).toHaveProperty('value', 'gemini');
    select('Registered native dialect', 'systemone');
    expect(surface).toHaveProperty('value', 'openai');
  });

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

  it.each([
    ['search', 'tavily-search', 'native'],
    ['search', 'exa-search', 'native'],
    ['search', 'parallel-search', 'native'],
    ['search', 'brave-search', 'native'],
    ['search', 'perplexity-search', 'openai'],
    ['ocr', 'azure-document-intelligence-ocr', 'native'],
    ['ocr', 'vertex-document-processing-ocr', 'native'],
    ['ocr', 'mistral-ocr', 'openai']
  ])('inspects %s with %s on %s', async (operation, dialect, surface) => {
    const handler = vi.fn(async (request: Request) => {
      route(request, 'POST', '/api/v1/routing/simulate');
      expect(await request.json()).toMatchObject({
        operation: { operation, route: 'route-a', request: expect.any(Object) },
        dialect,
        surface,
        mode: 'unary'
      });
      return json([]);
    });
    serve(handler);
    const state = playground();
    state.model = 'route-a';
    state.composer = 'advanced';
    state.applyTemplate(dialect);

    await state.explain();

    expect(handler).toHaveBeenCalledOnce();
    expect(state.simulationError).toBe('');
  });

  it('discloses request metadata and encrypted tool recovery retention', () => {
    serve(() => json({}));
    const target = document.createElement('div');
    const setup = vi.fn(() =>
      mount(PlaygroundPage, { target, context: getAllContexts() })
    );
    const harness = mount(StateHarness, { target, props: { setup } });
    onTestFinished(async () => {
      await unmount(setup.mock.results[0].value);
      await unmount(harness);
    });
    flushSync();

    const content = target.textContent?.replaceAll(/\s+/g, ' ');
    expect(content).toMatch(/metadata retention is enabled.*Request Explorer/s);
    expect(content).toMatch(
      /Negotiated tool recovery.*encrypted for up to 24 hours/s
    );
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

  it('blocks and re-enables streaming as routing preferences change', async () => {
    listPublishedRoutes();
    vi.mocked(simulateRouting).mockImplementation(async ({ preferences }) => [
      {
        eligible: !preferences?.ignore?.includes('provider-a'),
        priority: 1,
        provider_id: 'provider-a',
        strategy: 'weighted',
        target_id: 'target-a',
        upstream_model: 'model-a'
      }
    ]);
    const handler = vi.fn(async (request: Request) => {
      route(request, 'POST', '/api/v1/playground/stream');
      expect(await request.json()).toMatchObject({
        model: 'route-a',
        stream: true,
        routing: { ignore: [] }
      });
      return new Response('event: done\ndata: {}\n\n', {
        headers: { 'Content-Type': 'text/event-stream' }
      });
    });
    serve(handler);
    const state = playground();
    state.model = publishedRoute.slug;
    state.input = 'Hello';
    await vi.waitFor(() =>
      expect(state.selectedRoute?.slug).toBe(publishedRoute.slug)
    );
    flushSync();
    state.toggleStream(true);
    await vi.waitFor(() => expect(state.streamCheck).toBe('ok'));

    state.routing = '{"ignore":["provider-a"]}';
    await state.submit(submitEvent());
    expect(handler).not.toHaveBeenCalled();
    await vi.waitFor(() => expect(state.streamCheck).toBe('unsupported'));

    state.routing = '{"ignore":[]}';
    await vi.waitFor(() => expect(state.streamCheck).toBe('ok'));
    await state.submit(submitEvent());
    expect(handler).toHaveBeenCalledOnce();
  });
});

describe('playground execution plans', () => {
  const input = {
    composer: 'advanced' as const,
    operation: 'embeddings' as const,
    surface: 'openai' as const,
    strict: true,
    template: '',
    nativeDialect: 'openai-embeddings',
    inspectDialect: '',
    stream: true,
    operationKnown: true
  };

  it('keeps a strict native embedding client and its dry run on the same unary contract', () => {
    const plan = resolvePlaygroundPlan(input);
    expect(plan).toMatchObject({
      kind: 'native',
      operation: 'embeddings',
      dialect: 'openai-embeddings',
      inspection: {
        mode: 'unary',
        surface: 'native',
        dialect: 'openai-embeddings',
        registeredNative: true,
        clientContract: 'raw-vector-storage/1'
      }
    });
    expect(plan.submissionError).toBeTruthy();
  });

  it('inspects Gemini Decisions on the surface used by its public client', () => {
    expect(
      resolvePlaygroundPlan({
        ...input,
        operation: 'decision',
        nativeDialect: 'gemini-decisions'
      })
    ).toMatchObject({
      kind: 'native',
      dialect: 'gemini-decisions',
      inspection: {
        mode: 'unary',
        surface: 'gemini',
        dialect: 'gemini-decisions'
      }
    });
  });

  it('uses the basic generation composer after leaving a dedicated advanced operation', () => {
    const plan = resolvePlaygroundPlan({
      ...input,
      composer: 'basic',
      strict: false
    });
    expect(plan.kind).toBe('composer');
    expect(plan.activeOperation).toBe('generation');
    expect(plan.submissionError).toBeNull();
    expect(plan.inspection.registeredNative).toBe(false);
  });

  it.each(['classification', 'scoring'] as const)(
    'preserves the client visibility and submission restriction for non-strict %s',
    (operation) => {
      const plan = resolvePlaygroundPlan({
        ...input,
        operation,
        strict: false
      });
      expect(plan.kind).toBe('composer');
      expect(plan.inspection.registeredNative).toBe(false);
      expect(plan.submissionError).toContain('Use the public native client');
    }
  );

  it('keeps strict realtime requests local without opening a native provider session', () => {
    const plan = resolvePlaygroundPlan({ ...input, operation: 'realtime' });
    expect(plan.kind).toBe('realtime');
    expect(plan.inspection.mode).toBe('realtime');
    expect(plan.submissionError).toContain('local realtime event viewer');
  });
});
