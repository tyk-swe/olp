// Temporary baseline measurement for the console responsiveness plan. Each
// case records the observable behavior the plan asks about: requests issued
// during a typing burst, table visibility while a replacement request is in
// flight, cancellation on unmount, and the overview's request count.
import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { listProviderPage } from '$lib/features/providers/api';
import { listProviderModelInventoryPage } from '$lib/features/providers/models';
import { listRouteDraftPage, listRoutePage } from '$lib/features/routes/api';
import { captureRequests, jsonResponse } from '$lib/api/test/requestCapture';
import { provider, draft } from '$lib/forms/test/draftFixtures';
import type { ProviderModelInventory } from '$lib/features/providers/models';
import type { ActiveRoute } from '$lib/features/routes/api';
import ProviderListProbe from '$lib/features/providers/test/ProviderListProbe.svelte';
import ModelsPageProbe from '$lib/features/providers/models/test/ModelsPageProbe.svelte';
import RouteListProbe from '$lib/features/routes/test/RouteListProbe.svelte';
import OverviewProbe from '$lib/features/overview/test/OverviewProbe.svelte';

vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({ user: null, role: 'owner', can: () => true })
}));
vi.mock('$lib/features/providers/api', async (original) => ({
  ...(await original<typeof import('$lib/features/providers/api')>()),
  listProviderPage: vi.fn()
}));
vi.mock('$lib/features/providers/models', async (original) => ({
  ...(await original<typeof import('$lib/features/providers/models')>()),
  listProviderModelInventoryPage: vi.fn()
}));
vi.mock('$lib/features/routes/api', async (original) => ({
  ...(await original<typeof import('$lib/features/routes/api')>()),
  listRoutePage: vi.fn(),
  listRouteDraftPage: vi.fn()
}));
const SERVER_DELAY = 400;

const modelEntry: ProviderModelInventory = {
  available: true,
  metadata: {
    canonical_model: null,
    context_length: 128_000,
    data_collection: null,
    deployment: null,
    input_modalities: ['text'],
    max_output_tokens: 8_192,
    observed_at: null,
    output_modalities: ['text'],
    quantization: null,
    region: null,
    source: null,
    supported_parameters: null,
    zero_data_retention: null
  },
  model: {
    capabilities: [
      {
        operation: 'generation',
        surface: 'openai',
        mode: 'unary',
        source: 'certified'
      }
    ],
    display_name: 'Test model',
    enabled: true,
    id: 'model-a',
    upstream_model: 'test-model'
  },
  provider_id: 'provider-a',
  provider_kind: 'openai',
  provider_name: 'Original provider'
};

const activeRoute: ActiveRoute = {
  created_at: '2026-07-12T12:00:00Z',
  project_id: null,
  project_name: null,
  etag: 'route-etag-a',
  id: 'route-active-a',
  latest_revision: {
    activated_at: '2026-07-12T12:00:00Z',
    activated_by: 'user-a',
    content_policy: null,
    fidelity: { mode: 'strict' },
    id: 'revision-a',
    max_attempts: 1,
    operations: ['generation'],
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
    slug: 'active-route',
    source_draft_id: 'draft-a',
    targets: []
  },
  revision_count: 1,
  retired_at: null,
  retired_by: null,
  slug: 'active-route',
  state: 'active'
};

function deferred<T>(value: T, ms = SERVER_DELAY): Promise<T> {
  return new Promise((resolve) => setTimeout(() => resolve(value), ms));
}

function providerItem(index: number) {
  return {
    ...provider,
    id: `provider-${index}`,
    name: `Provider ${index}`,
    kind: 'openai' as const,
    active_revision: 1
  };
}

function routeItem(index: number): ActiveRoute {
  return { ...activeRoute, id: `route-${index}`, slug: `route-${index}` };
}

let host: HTMLElement;
let client: QueryClient;
const mounted: Array<ReturnType<typeof mount>> = [];

function queryClient() {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 0 } }
  });
}

beforeEach(() => {
  vi.useFakeTimers();
  host = document.createElement('div');
  document.body.append(host);
  client = queryClient();
});

afterEach(async () => {
  while (mounted.length) await unmount(mounted.pop()!);
  client.clear();
  host.remove();
  vi.useRealTimers();
});

function input(element: HTMLInputElement, value: string) {
  element.value = value;
  element.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
}

describe('baseline: provider list', () => {
  it('records requests and visibility during a typing burst', async () => {
    vi.mocked(listProviderPage).mockImplementation(() =>
      deferred({ items: [providerItem(0)], nextCursor: null })
    );
    mounted.push(
      mount(ProviderListProbe, {
        target: host,
        props: {
          client,
          initialState: {
            cursor: undefined,
            history: [],
            search: '',
            applied: ''
          }
        }
      })
    );
    flushSync();
    await vi.advanceTimersByTimeAsync(SERVER_DELAY + 10);
    expect(host.textContent).toContain('Provider 0');
    vi.mocked(listProviderPage).mockClear();

    const search = host.querySelector<HTMLInputElement>('#provider-search')!;
    for (const value of ['g', 'go', 'goo', 'goog']) {
      input(search, value);
      await vi.advanceTimersByTimeAsync(50);
    }
    const burstCalls = vi.mocked(listProviderPage).mock.calls.length;
    const blankedDuringSearch =
      host.querySelector('table') === null &&
      Boolean(host.textContent?.includes('Loading providers'));
    await vi.advanceTimersByTimeAsync(SERVER_DELAY + 500);
    console.log(
      `[baseline] provider search burst: ${burstCalls} requests, table blanked: ${blankedDuringSearch}`
    );
  });

  it('records the signal passed on unmount', async () => {
    const signals: Array<AbortSignal | undefined> = [];
    vi.mocked(listProviderPage).mockImplementation((_c, signal) => {
      signals.push(signal);
      return deferred({ items: [providerItem(0)], nextCursor: null });
    });
    const component = mount(ProviderListProbe, {
      target: host,
      props: {
        client,
        initialState: {
          cursor: undefined,
          history: [],
          search: '',
          applied: ''
        }
      }
    });
    flushSync();
    await unmount(component);
    console.log(
      `[baseline] provider list passes AbortSignal: ${signals.length > 0 && signals.every((s) => s instanceof AbortSignal)}, aborted on unmount: ${signals[0]?.aborted}`
    );
  });
});

describe('baseline: model inventory', () => {
  it('records requests and visibility during a typing burst', async () => {
    vi.mocked(listProviderModelInventoryPage).mockImplementation(() =>
      deferred({ items: [modelEntry], nextCursor: null })
    );
    mounted.push(mount(ModelsPageProbe, { target: host, props: { client } }));
    flushSync();
    await vi.advanceTimersByTimeAsync(SERVER_DELAY + 10);
    expect(host.textContent).toContain('Test model');
    vi.mocked(listProviderModelInventoryPage).mockClear();

    const search = host.querySelector<HTMLInputElement>(
      'input[type="search"]'
    )!;
    for (const value of ['g', 'go', 'goo', 'goog']) {
      input(search, value);
      await vi.advanceTimersByTimeAsync(50);
    }
    const burstCalls = vi.mocked(listProviderModelInventoryPage).mock.calls
      .length;
    const blankedDuringSearch =
      host.querySelector('table') === null &&
      Boolean(host.textContent?.includes('Loading certified models'));
    const signalPassed = vi
      .mocked(listProviderModelInventoryPage)
      .mock.calls.every((call) => call[2] instanceof AbortSignal);
    await vi.advanceTimersByTimeAsync(SERVER_DELAY + 500);
    console.log(
      `[baseline] model search burst: ${burstCalls} requests, table blanked: ${blankedDuringSearch}, AbortSignal passed: ${signalPassed}`
    );
  });
});

describe('baseline: route list', () => {
  it('records whether the queries pass an AbortSignal', async () => {
    vi.mocked(listRoutePage).mockImplementation(() =>
      deferred({ items: [routeItem(0)], nextCursor: null })
    );
    vi.mocked(listRouteDraftPage).mockImplementation(() =>
      deferred({ items: [draft], nextCursor: null })
    );
    mounted.push(
      mount(RouteListProbe, {
        target: host,
        props: {
          client,
          initialState: {
            draft: { cursor: undefined, history: [] },
            route: { cursor: undefined, history: [] }
          }
        }
      })
    );
    flushSync();
    await vi.advanceTimersByTimeAsync(SERVER_DELAY + 10);
    const routeSignal = vi.mocked(listRoutePage).mock.calls.at(-1)?.[1];
    const draftSignal = vi.mocked(listRouteDraftPage).mock.calls.at(-1)?.[1];
    console.log(
      `[baseline] route list AbortSignal: routes=${routeSignal instanceof AbortSignal}, drafts=${draftSignal instanceof AbortSignal}`
    );
  });
});

describe('baseline: overview', () => {
  it('counts list requests with 120 providers and 80 routes', async () => {
    // Fetch-level capture measures the real request count: the summary
    // endpoint answers the counts in one round-trip regardless of how many
    // providers or routes exist.
    const requests = captureRequests(async (request) => {
      const url = new URL(request.url);
      if (url.pathname === '/api/v1/overview') {
        await new Promise((resolve) => setTimeout(resolve, SERVER_DELAY));
        return jsonResponse({
          active_providers: 120,
          active_routes: 80,
          enabled_models: 300,
          usable_api_key: true
        });
      }
      if (url.pathname === '/api/v1/requests') {
        return jsonResponse({ items: [], next_cursor: null });
      }
      if (url.pathname === '/api/v1/auth/capabilities') {
        return jsonResponse({
          local_login_enabled: true,
          oidc_login_enabled: false,
          retention_enforced: true
        });
      }
      return jsonResponse({ title: 'Not found', status: 404 }, { status: 404 });
    });
    const component = mount(OverviewProbe, {
      target: host,
      props: { client }
    });
    mounted.push(component);
    flushSync();
    let settledAt = -1;
    for (let step = 0; step < 50; step += 1) {
      await vi.advanceTimersByTimeAsync(100);
      if (settledAt < 0 && host.textContent?.includes('120 active')) {
        settledAt = step * 100;
      }
    }
    const count = (path: string) =>
      requests.filter((request) => new URL(request.url).pathname === path)
        .length;
    console.log(
      `[baseline] overview: ${count('/api/v1/overview')} summary + ${count('/api/v1/providers')} provider + ${count('/api/v1/routes')} route + ${count('/api/v1/api-keys')} key + ${count('/api/v1/requests')} request calls, counts visible after ~${settledAt}ms`
    );
    // The full collections stay untouched: only the aggregate and the recent
    // requests feed the page.
    expect(count('/api/v1/providers')).toBe(0);
    expect(count('/api/v1/routes')).toBe(0);
    expect(count('/api/v1/api-keys')).toBe(0);
    expect(count('/api/v1/overview')).toBe(1);
  });
});
