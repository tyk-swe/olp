import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { listApiKeys } from '$lib/features/access/api-keys/api';
import {
  listRoutes,
  simulateRouting,
  type ActiveRoute,
  type RoutingDecision
} from '$lib/features/routes/api';
import {
  runPlayground,
  streamPlayground,
  type PlaygroundResponse,
  type PlaygroundStreamHandlers
} from './api';
import PlaygroundProbe from './test/PlaygroundProbe.svelte';

vi.mock('$lib/features/routes/api', async (original) => ({
  ...(await original<typeof import('$lib/features/routes/api')>()),
  listRoutes: vi.fn(),
  simulateRouting: vi.fn()
}));
vi.mock('$lib/features/access/api-keys/api', async (original) => ({
  ...(await original<typeof import('$lib/features/access/api-keys/api')>()),
  listApiKeys: vi.fn()
}));
vi.mock('./api', async (original) => ({
  ...(await original<typeof import('./api')>()),
  runPlayground: vi.fn(),
  streamPlayground: vi.fn()
}));

const SERVER_DELAY = 40;

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;

const route: ActiveRoute = {
  created_at: '2026-07-12T12:00:00Z',
  project_id: null,
  project_name: null,
  etag: 'route-etag-a',
  id: 'route-active-a',
  latest_revision: {
    activated_at: '2026-07-12T12:00:00Z',
    activated_by: 'user-a',
    content_policy: null,
    id: 'revision-a',
    max_attempts: 1,
    operations: ['generation', 'embeddings'],
    overall_timeout_ms: 120_000,
    revision: 1,
    route_id: 'route-active-a',
    routing_policy: {
      allowed_strategies: null,
      constraints: {
        deny_data_collection: false,
        ignore: [],
        max_price: null,
        only: null,
        quantizations: null,
        regions: null,
        require_parameters: false,
        require_zero_data_retention: false
      },
      defaults: {
        allow_fallbacks: null,
        deny_data_collection: false,
        ignore: [],
        max_price: null,
        only: null,
        order: null,
        preferred_max_latency_ms: null,
        preferred_min_throughput: null,
        quantizations: null,
        regions: null,
        require_parameters: false,
        require_zero_data_retention: false,
        strategy: null
      }
    },
    slug: 'chat-route',
    source_draft_id: 'draft-a',
    targets: []
  },
  revision_count: 1,
  retired_at: null,
  retired_by: null,
  slug: 'chat-route',
  state: 'active'
};

const unaryReply: PlaygroundResponse = {
  id: '01980000-0000-7000-8000-000000000777',
  model: 'chat-route',
  output_text: 'the answer',
  tool_calls: [],
  latency_ms: 7,
  routing: [],
  usage: { input_tokens: 3, output_tokens: 4, total_tokens: 7 }
};

const eligibleDecision: RoutingDecision = {
  target_id: '01980000-0000-7000-8000-000000000601',
  provider_id: '01980000-0000-7000-8000-000000000602',
  upstream_model: 'vendor-model',
  eligible: true,
  priority: 0,
  strategy: 'weighted',
  attempt: 1,
  credential_slot_id: null,
  context_length: null,
  estimated_input_tokens: null,
  max_output_tokens: null,
  metadata_observed_at: null,
  performance: null,
  price: null,
  reason: null,
  requested_output_tokens: null,
  vendor_id: null
};

function deferred<T>(value: T, ms = SERVER_DELAY): Promise<T> {
  return new Promise((resolve) => setTimeout(() => resolve(value), ms));
}

function settle(ms = SERVER_DELAY + 20) {
  return vi.advanceTimersByTimeAsync(ms);
}

async function establish() {
  vi.mocked(listRoutes).mockImplementation(() => deferred([route]));
  vi.mocked(listApiKeys).mockImplementation(() => deferred([]));
  component = mount(PlaygroundProbe, {
    target: host,
    props: { client }
  });
  flushSync();
  await settle();
}

function input(selector: string) {
  return host.querySelector<HTMLInputElement>(selector)!;
}

function fill(selector: string, value: string) {
  const field = input(selector);
  field.value = value;
  field.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
}

function submit() {
  host
    .querySelector('form')!
    .dispatchEvent(
      new SubmitEvent('submit', { bubbles: true, cancelable: true })
    );
  flushSync();
}

function streamToggle() {
  return host.querySelector<HTMLInputElement>(
    '.stream-toggle input[type="checkbox"]'
  )!;
}

beforeEach(() => {
  vi.useFakeTimers();
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
});

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  client.clear();
  host.remove();
  vi.useRealTimers();
});

describe('basic composer', () => {
  it('submits the legacy input form and renders the reply', async () => {
    vi.mocked(runPlayground).mockResolvedValue(unaryReply);
    await establish();
    fill('#playground-model', 'chat-route');
    fill('#playground-input', 'hello');
    submit();
    await settle(0);
    expect(runPlayground).toHaveBeenCalledWith(
      expect.objectContaining({
        model: 'chat-route',
        input: 'hello',
        surface: 'openai'
      }),
      expect.anything()
    );
    flushSync();
    await settle(0);
    expect(host.textContent).toContain('the answer');
    expect(host.textContent).toContain('Output tokens');
  });
});

describe('advanced composer', () => {
  it('applies a template and submits the raw request document', async () => {
    vi.mocked(runPlayground).mockResolvedValue({
      ...unaryReply,
      output_text: '',
      response: { data: [{ embedding: [0.25] }] }
    });
    await establish();
    const advanced = [
      ...host.querySelectorAll<HTMLInputElement>('input[type="radio"]')
    ].find((radio) => radio.value === 'advanced')!;
    advanced.click();
    flushSync();
    const template = host.querySelector<HTMLSelectElement>(
      '#playground-template'
    )!;
    template.value = 'embeddings';
    template.dispatchEvent(new Event('change', { bubbles: true }));
    flushSync();
    fill('#playground-model', 'chat-route');
    submit();
    await settle(0);
    expect(runPlayground).toHaveBeenCalledWith(
      expect.objectContaining({
        model: 'chat-route',
        operation: 'embeddings',
        request: expect.objectContaining({
          input: ['First document to embed', 'Second document to embed']
        })
      }),
      expect.anything()
    );
    flushSync();
    await settle(0);
    expect(host.textContent).toContain('Operation result');
  });
});

describe('streaming', () => {
  async function enableStream() {
    const toggle = streamToggle();
    toggle.checked = true;
    toggle.dispatchEvent(new Event('change', { bubbles: true }));
    flushSync();
    await settle();
  }

  it('verifies eligibility then renders incremental frames and done metadata', async () => {
    vi.mocked(simulateRouting).mockResolvedValue([eligibleDecision]);
    vi.mocked(streamPlayground).mockImplementation(
      (_request, handlers: PlaygroundStreamHandlers) => {
        handlers.frame('chunk-one ');
        handlers.frame('chunk-two');
        handlers.done({
          id: 'resp-1',
          model: 'chat-route',
          usage: { input_tokens: 1, output_tokens: 2, total_tokens: 3 }
        });
        return Promise.resolve();
      }
    );
    await establish();
    fill('#playground-model', 'chat-route');
    fill('#playground-input', 'hi');
    await enableStream();
    expect(simulateRouting).toHaveBeenCalledWith(
      expect.objectContaining({ route: 'chat-route', mode: 'streaming' })
    );
    submit();
    await settle(0);
    expect(streamPlayground).toHaveBeenCalled();
    flushSync();
    const frames = host.querySelector('[data-testid="stream-frames"]');
    expect(frames?.textContent).toBe('chunk-one chunk-two');
    expect(host.textContent).toContain('Output tokens');
  });

  it('cancel aborts the in-flight stream', async () => {
    vi.mocked(simulateRouting).mockResolvedValue([eligibleDecision]);
    let captured: AbortSignal | null = null;
    vi.mocked(streamPlayground).mockImplementation(
      (_request, _handlers, signal) => {
        captured = signal;
        return new Promise(() => {});
      }
    );
    await establish();
    fill('#playground-model', 'chat-route');
    fill('#playground-input', 'hi');
    await enableStream();
    submit();
    await settle(0);
    const cancel = [...host.querySelectorAll('button')].find(
      (item) => item.textContent?.trim() === 'Cancel'
    )!;
    expect(cancel).toBeDefined();
    cancel.click();
    flushSync();
    expect(captured!.aborted).toBe(true);
  });

  it('aborts the stream when the page unmounts', async () => {
    vi.mocked(simulateRouting).mockResolvedValue([eligibleDecision]);
    let captured: AbortSignal | null = null;
    vi.mocked(streamPlayground).mockImplementation(
      (_request, _handlers, signal) => {
        captured = signal;
        return new Promise(() => {});
      }
    );
    await establish();
    fill('#playground-model', 'chat-route');
    fill('#playground-input', 'hi');
    await enableStream();
    submit();
    await settle(0);
    await unmount(component!);
    component = undefined;
    expect(captured!.aborted).toBe(true);
  });

  it('warns instead of streaming when the route has an output policy', async () => {
    const policyRoute: ActiveRoute = {
      ...route,
      latest_revision: {
        ...route.latest_revision,
        content_policy: {
          rules: [
            {
              id: 'no-secrets',
              phase: 'output',
              pattern: 'secret',
              action: 'block'
            }
          ]
        }
      }
    };
    vi.mocked(listRoutes).mockImplementation(() => deferred([policyRoute]));
    vi.mocked(listApiKeys).mockImplementation(() => deferred([]));
    component = mount(PlaygroundProbe, {
      target: host,
      props: { client }
    });
    flushSync();
    await settle();
    fill('#playground-model', 'chat-route');
    expect(streamToggle().disabled).toBe(true);
    expect(host.textContent).toMatch(/streaming is\s+disabled/);
  });

  it('warns that streaming is unverified for an unknown route', async () => {
    await establish();
    fill('#playground-model', 'not-a-route');
    const toggle = streamToggle();
    toggle.checked = true;
    toggle.dispatchEvent(new Event('change', { bubbles: true }));
    flushSync();
    await settle();
    expect(host.textContent).toContain('cannot be verified');
  });
});
