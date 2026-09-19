import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { listRouteDraftPage, listRoutePage } from './api';
import { draft } from '$lib/forms/test/draftFixtures';
import type { ActiveRoute, RouteDraft } from './api';
import RouteListProbe from './test/RouteListProbe.svelte';

vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({ can: () => true })
}));
vi.mock('./api', async (original) => ({
  ...(await original<typeof import('./api')>()),
  listRoutePage: vi.fn(),
  listRouteDraftPage: vi.fn()
}));

const SERVER_DELAY = 400;

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount>;

const activeRoute: ActiveRoute = {
  created_at: '2026-07-12T12:00:00Z',
  id: 'route-active-a',
  latest_revision: {
    activated_at: '2026-07-12T12:00:00Z',
    activated_by: 'user-a',
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
  slug: 'active-route'
};

function deferred<T>(value: T, ms = SERVER_DELAY): Promise<T> {
  return new Promise((resolve) => setTimeout(() => resolve(value), ms));
}

function settle() {
  return vi.advanceTimersByTimeAsync(SERVER_DELAY + 20);
}

function routeItem(index: number): ActiveRoute {
  return { ...activeRoute, id: `route-${index}`, slug: `route-${index}` };
}

function draftItem(index: number): RouteDraft {
  return { ...draft, id: `draft-${index}`, slug: `draft-${index}` };
}

async function establish(
  routes: ActiveRoute[] = [routeItem(0)],
  routeCursor: string | null = null,
  drafts: RouteDraft[] = [draftItem(0)],
  draftCursor: string | null = null
) {
  vi.mocked(listRoutePage).mockImplementation(() =>
    deferred({ items: routes, nextCursor: routeCursor })
  );
  vi.mocked(listRouteDraftPage).mockImplementation(() =>
    deferred({ items: drafts, nextCursor: draftCursor })
  );
  component = mount(RouteListProbe, {
    target: host,
    props: {
      client,
      initialState: {
        draft: { cursor: undefined, history: [] },
        route: { cursor: undefined, history: [] }
      }
    }
  });
  flushSync();
  await settle();
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
  client.clear();
  host.remove();
  vi.useRealTimers();
});

function button(label: string, section?: string) {
  const scope = section
    ? host.querySelector<HTMLElement>(`[aria-labelledby="${section}"]`)!
    : host;
  return [...scope.querySelectorAll('button')].find(
    (button) => button.textContent?.trim() === label
  )!;
}

describe('updating state', () => {
  it('keeps the active routes visible while a replacement page loads', async () => {
    await establish([routeItem(0)], 'route-page-two');
    button('Next', 'active-routes-heading').click();
    flushSync();
    await vi.advanceTimersByTimeAsync(10);
    const section = host.querySelector<HTMLElement>(
      '[aria-labelledby="active-routes-heading"]'
    )!;
    expect(section.textContent).toContain('route-0');
    expect(section.textContent).toContain('Updating…');
    expect(
      section
        .querySelector('[aria-label="Active routes table"]')
        ?.getAttribute('aria-busy')
    ).toBe('true');
    expect(button('Next', 'active-routes-heading').disabled).toBe(true);
    // The draft section is unaffected by the active-route page change.
    const draftsSection = host.querySelector<HTMLElement>(
      '[aria-labelledby="draft-routes-heading"]'
    )!;
    expect(draftsSection.textContent).toContain('draft-0');
    expect(draftsSection.textContent).not.toContain('Updating…');
    await settle();
    expect(section.textContent).not.toContain('Updating…');
  });

  it('keeps drafts visible while a replacement page loads', async () => {
    await establish([routeItem(0)], null, [draftItem(0)], 'draft-page-two');
    button('Next', 'draft-routes-heading').click();
    flushSync();
    await vi.advanceTimersByTimeAsync(10);
    const section = host.querySelector<HTMLElement>(
      '[aria-labelledby="draft-routes-heading"]'
    )!;
    expect(section.textContent).toContain('draft-0');
    expect(section.textContent).toContain('Updating…');
    expect(
      section
        .querySelector('[aria-label="Route drafts table"]')
        ?.getAttribute('aria-busy')
    ).toBe('true');
    expect(button('Next', 'draft-routes-heading').disabled).toBe(true);
    await settle();
  });
});

describe('independent sections', () => {
  it('shows drafts when the active-route request fails', async () => {
    vi.mocked(listRoutePage).mockImplementation(() =>
      Promise.reject(new Error('routes down'))
    );
    vi.mocked(listRouteDraftPage).mockImplementation(() =>
      deferred({ items: [draftItem(0)], nextCursor: null })
    );
    component = mount(RouteListProbe, {
      target: host,
      props: {
        client,
        initialState: {
          draft: { cursor: undefined, history: [] },
          route: { cursor: undefined, history: [] }
        }
      }
    });
    flushSync();
    await settle();
    const draftsSection = host.querySelector<HTMLElement>(
      '[aria-labelledby="draft-routes-heading"]'
    )!;
    expect(draftsSection.textContent).toContain('draft-0');
    const routesSection = host.querySelector<HTMLElement>(
      '[aria-labelledby="active-routes-heading"]'
    )!;
    expect(routesSection.textContent).toContain('routes down');
    expect(routesSection.textContent).toContain('Retry');
  });
});

describe('cancellation', () => {
  it('passes AbortSignals and aborts in-flight requests on unmount', async () => {
    const signals: AbortSignal[] = [];
    vi.mocked(listRoutePage).mockImplementation((_c, signal) => {
      if (signal) signals.push(signal);
      return deferred({ items: [routeItem(0)], nextCursor: null });
    });
    vi.mocked(listRouteDraftPage).mockImplementation((_c, signal) => {
      if (signal) signals.push(signal);
      return deferred({ items: [draftItem(0)], nextCursor: null });
    });
    component = mount(RouteListProbe, {
      target: host,
      props: {
        client,
        initialState: {
          draft: { cursor: undefined, history: [] },
          route: { cursor: undefined, history: [] }
        }
      }
    });
    flushSync();
    await vi.advanceTimersByTimeAsync(10);
    await unmount(component);
    component = undefined as unknown as ReturnType<typeof mount>;
    expect(signals).toHaveLength(2);
    expect(signals.every((signal) => signal.aborted)).toBe(true);
  });

  it('aborts the superseded request when a page changes mid-flight', async () => {
    const calls: Array<{ cursor: string | undefined; signal: AbortSignal }> =
      [];
    vi.mocked(listRoutePage).mockImplementation((cursor, signal) => {
      calls.push({ cursor, signal: signal! });
      return deferred({ items: [routeItem(0)], nextCursor: 'route-page-two' });
    });
    vi.mocked(listRouteDraftPage).mockImplementation(() =>
      deferred({ items: [draftItem(0)], nextCursor: null })
    );
    component = mount(RouteListProbe, {
      target: host,
      props: {
        client,
        initialState: {
          draft: { cursor: undefined, history: [] },
          route: { cursor: undefined, history: [] }
        }
      }
    });
    flushSync();
    await settle();
    // Move to the second page, then return while that request is still in
    // flight; the superseded request must abort.
    button('Next', 'active-routes-heading').click();
    flushSync();
    await vi.advanceTimersByTimeAsync(10);
    button('Previous', 'active-routes-heading').click();
    flushSync();
    await vi.advanceTimersByTimeAsync(10);
    const pageTwo = calls.find((call) => call.cursor === 'route-page-two');
    expect(pageTwo).toBeDefined();
    expect(pageTwo!.signal.aborted).toBe(true);
    await settle();
  });
});
