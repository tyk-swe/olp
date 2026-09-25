import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  createRouteMigrationDraft,
  listRouteDraftPage,
  listRoutePage,
  retireRoute
} from './api';
import { draft } from '$lib/forms/test/draftFixtures';
import type { ActiveRoute, RouteDraft } from './api';
import RouteListProbe from './test/RouteListProbe.svelte';
import { validateRouteEditor, type RouteEditorValues } from './routeEditor';

vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({ can: () => true })
}));
vi.mock('$app/navigation', () => ({
  goto: vi.fn().mockResolvedValue(undefined)
}));
vi.mock('./api', async (original) => ({
  ...(await original<typeof import('./api')>()),
  listRoutePage: vi.fn(),
  listRouteDraftPage: vi.fn(),
  createRouteMigrationDraft: vi.fn(),
  retireRoute: vi.fn()
}));

const SERVER_DELAY = 400;

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount>;

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

const retiredRoute: ActiveRoute = {
  ...activeRoute,
  etag: 'route-etag-b',
  id: 'route-retired-b',
  retired_at: '2026-07-13T12:00:00Z',
  retired_by: 'user-a',
  slug: 'retired-route',
  state: 'retired'
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

describe('strict route migration', () => {
  it('creates a review draft under a new slug with the selected published route ETag', async () => {
    const published = {
      ...activeRoute,
      latest_revision: { ...activeRoute.latest_revision, fidelity: null }
    };
    vi.mocked(createRouteMigrationDraft).mockResolvedValue({
      ...draft,
      id: 'migrated-draft'
    });
    await establish([published], null, []);
    button('Create strict migration draft').click();
    flushSync();
    const input = host.querySelector<HTMLInputElement>('#migration-slug')!;
    expect(input.value).toBe('active-route-strict');
    input.value = 'reviewed-strict';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    flushSync();
    const form = host.querySelector<HTMLFormElement>('.migration-form')!;
    form.dispatchEvent(
      new Event('submit', { bubbles: true, cancelable: true })
    );
    await vi.waitFor(() => {
      expect(createRouteMigrationDraft).toHaveBeenCalledWith(
        expect.objectContaining({ id: published.id, etag: published.etag }),
        'reviewed-strict'
      );
    });
  });

  it('proposes and accepts only slugs the route editor can save', async () => {
    const editor: RouteEditorValues = {
      slug: '',
      operations: ['generation'],
      overallTimeoutMs: 120_000,
      maxAttempts: 1,
      targets: [
        {
          providerModelId: 'model-a',
          priority: 0,
          weight: 1,
          timeoutMs: 60_000
        }
      ],
      contentPolicyRules: []
    };
    const saveable = (slug: string) =>
      validateRouteEditor({ ...editor, slug }) === null;
    // Legacy slugs published through the API may be up to 100 characters and
    // contain dots or underscores.
    const legacySlugs = [
      `a${'b'.repeat(59)}`,
      `a-${'b'.repeat(53)}-c`,
      'gpt-4.1_mini'
    ];
    await establish(
      legacySlugs.map((slug, index) => ({
        ...activeRoute,
        id: `legacy-${index}`,
        slug,
        latest_revision: { ...activeRoute.latest_revision, fidelity: null }
      })),
      null,
      []
    );
    const proposals = [...host.querySelectorAll('button')]
      .filter(
        (candidate) =>
          candidate.textContent?.trim() === 'Create strict migration draft'
      )
      .map((review) => {
        review.click();
        flushSync();
        return host.querySelector<HTMLInputElement>('#migration-slug')!.value;
      });
    expect(proposals).toEqual([
      `a${'b'.repeat(55)}-strict`,
      `a-${'b'.repeat(53)}-strict`,
      'gpt-4-1-mini-strict'
    ]);
    expect(proposals.every(saveable)).toBe(true);

    const input = host.querySelector<HTMLInputElement>('#migration-slug')!;
    const accepted = (slug: string) =>
      new RegExp(`^(?:${input.pattern})$`, 'u').test(slug) &&
      slug.length <= input.maxLength;
    for (const slug of [
      'reviewed-strict',
      'gpt-4.1-strict',
      'under_score',
      'double--hyphen',
      `a${'b'.repeat(62)}`,
      `a${'b'.repeat(63)}`
    ])
      expect(accepted(slug), slug).toBe(saveable(slug));
  });
});

describe('updating state', () => {
  it('keeps the active routes visible while a replacement page loads', async () => {
    await establish([routeItem(0)], 'route-page-two');
    button('Next', 'published-routes-heading').click();
    flushSync();
    await vi.advanceTimersByTimeAsync(10);
    const section = host.querySelector<HTMLElement>(
      '[aria-labelledby="published-routes-heading"]'
    )!;
    expect(section.textContent).toContain('route-0');
    expect(section.textContent).toContain('Updating…');
    expect(
      section
        .querySelector('[aria-label="Published routes table"]')
        ?.getAttribute('aria-busy')
    ).toBe('true');
    expect(button('Next', 'published-routes-heading').disabled).toBe(true);
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
      '[aria-labelledby="published-routes-heading"]'
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
    button('Next', 'published-routes-heading').click();
    flushSync();
    await vi.advanceTimersByTimeAsync(10);
    button('Previous', 'published-routes-heading').click();
    flushSync();
    await vi.advanceTimersByTimeAsync(10);
    const pageTwo = calls.find((call) => call.cursor === 'route-page-two');
    expect(pageTwo).toBeDefined();
    expect(pageTwo!.signal.aborted).toBe(true);
    await settle();
  });

  it('marks retired routes and only offers retirement for active ones', async () => {
    await establish([routeItem(0), retiredRoute]);
    const rows = [
      ...host.querySelectorAll<HTMLElement>(
        '[aria-labelledby="published-routes-heading"] tbody tr'
      )
    ];
    expect(rows).toHaveLength(2);
    expect(rows[0].textContent).toContain('active');
    expect(rows[1].textContent).toContain('retired');
    const retireButtons = [...host.querySelectorAll('button')].filter(
      (item) => item.textContent?.trim() === 'Retire'
    );
    expect(retireButtons).toHaveLength(1);
  });

  it('retires an active route with its etag and refreshes the lists', async () => {
    await establish([routeItem(0)]);
    vi.mocked(retireRoute).mockResolvedValue({
      etag: 'route-etag-a2',
      runtime_generation: { id: 'gen-2', sequence: 2 }
    });
    button('Retire', 'published-routes-heading').click();
    flushSync();
    await vi.advanceTimersByTimeAsync(10);
    expect(retireRoute).toHaveBeenCalledWith('route-0', 'route-etag-a');
    await settle();
    expect(vi.mocked(listRoutePage).mock.calls.length).toBeGreaterThan(1);
  });

  it('reports a failed retirement without hiding the list', async () => {
    await establish([routeItem(0)]);
    vi.mocked(retireRoute).mockRejectedValue(new Error('etag mismatch'));
    button('Retire', 'published-routes-heading').click();
    flushSync();
    await vi.advanceTimersByTimeAsync(10);
    expect(host.querySelector('[role="alert"]')?.textContent).toContain(
      'etag mismatch'
    );
    expect(host.textContent).toContain('route-0');
  });
});
