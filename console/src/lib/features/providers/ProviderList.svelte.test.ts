import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { listProviderPage } from './api';
import { provider } from '$lib/forms/test/draftFixtures';
import { SEARCH_DEBOUNCE_MS } from '$lib/lists/search.svelte';
import ProviderListProbe from './test/ProviderListProbe.svelte';

vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({ can: () => true })
}));
vi.mock('./api', async (original) => ({
  ...(await original<typeof import('./api')>()),
  listProviderPage: vi.fn()
}));

const SERVER_DELAY = 400;

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount>;

function providerItem(index: number, vendor = 'google') {
  return {
    ...provider,
    id: `provider-${index}`,
    name: `Provider ${index}`,
    kind: 'gemini' as const,
    vendor_id: vendor
  };
}

function deferred<T>(value: T, ms = SERVER_DELAY): Promise<T> {
  return new Promise((resolve) => setTimeout(() => resolve(value), ms));
}

function page(
  items: ReturnType<typeof providerItem>[],
  nextCursor: string | null = null
) {
  return { items, nextCursor };
}

function settle() {
  return vi.advanceTimersByTimeAsync(SERVER_DELAY + 20);
}

async function establish(
  initialState = {
    cursor: 'page-two',
    history: [undefined] as Array<string | undefined>,
    search: 'google',
    applied: 'google'
  }
) {
  vi.mocked(listProviderPage).mockImplementation(() =>
    deferred(page([providerItem(0)], 'page-three'))
  );
  component = mount(ProviderListProbe, {
    target: host,
    props: { client, initialState }
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

function button(label: string) {
  return [...host.querySelectorAll('button')].find(
    (button) => button.textContent?.trim() === label
  )!;
}

function searchBox() {
  return host.querySelector<HTMLInputElement>('#provider-search')!;
}

function typeIn(box: HTMLInputElement, value: string) {
  box.value = value;
  box.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
}

/// Calls still in flight count toward the burst; the superseded fetch each
/// key change leaves behind is already cancelled.
function liveCalls() {
  return vi
    .mocked(listProviderPage)
    .mock.calls.filter((call) => !(call[1] as AbortSignal).aborted);
}

describe('debounced search', () => {
  it('applies a typing burst as one search after the pause', async () => {
    await establish();
    vi.mocked(listProviderPage).mockClear();
    for (const value of ['a', 'an', 'ant', 'anth']) {
      typeIn(searchBox(), value);
      await vi.advanceTimersByTimeAsync(50);
    }
    expect(liveCalls()).toHaveLength(0);
    await vi.advanceTimersByTimeAsync(SEARCH_DEBOUNCE_MS);
    expect(liveCalls()).toEqual([[undefined, expect.any(AbortSignal), 'anth']]);
    await settle();
    expect(host.textContent).toContain('Page 1');
  });

  it('applies and resets the cursor atomically', async () => {
    await establish();
    typeIn(searchBox(), 'anthropic');
    await vi.advanceTimersByTimeAsync(SEARCH_DEBOUNCE_MS + 10);
    // No request may pair the new search with the previous page's cursor.
    for (const call of vi.mocked(listProviderPage).mock.calls) {
      if (call[2] === 'anthropic') expect(call[0]).toBeUndefined();
    }
    expect(liveCalls().some((call) => call[2] === 'anthropic')).toBe(true);
    await settle();
  });

  it('clears immediately without waiting out the pause', async () => {
    await establish();
    vi.mocked(listProviderPage).mockClear();
    typeIn(searchBox(), 'zoom');
    await vi.advanceTimersByTimeAsync(50);
    typeIn(searchBox(), '');
    // The pending 'zoom' search is cancelled; the empty search runs now.
    await vi.advanceTimersByTimeAsync(10);
    expect(liveCalls()).toEqual([[undefined, expect.any(AbortSignal), '']]);
    await settle();
  });

  it('drops a pending search when the component unmounts', async () => {
    await establish();
    vi.mocked(listProviderPage).mockClear();
    typeIn(searchBox(), 'never');
    await unmount(component);
    component = undefined as unknown as ReturnType<typeof mount>;
    await vi.advanceTimersByTimeAsync(SEARCH_DEBOUNCE_MS + SERVER_DELAY + 100);
    expect(listProviderPage).not.toHaveBeenCalled();
  });
});

describe('navigation', () => {
  it('retains the search and page when returning from a provider detail', async () => {
    await establish();
    expect(host.textContent).toContain('Page 2');
    expect(listProviderPage).toHaveBeenCalledWith(
      'page-two',
      expect.any(AbortSignal),
      'google'
    );
    button('Open detail').click();
    flushSync();
    expect(searchBox()).toBeNull();
    button('Return to list').click();
    flushSync();
    await vi.advanceTimersByTimeAsync(SEARCH_DEBOUNCE_MS + SERVER_DELAY + 20);
    expect(host.textContent).toContain('Page 2');
    expect(searchBox().value).toBe('google');
    expect(listProviderPage).not.toHaveBeenCalledWith(
      undefined,
      expect.anything(),
      expect.anything()
    );
  });

  it('resumes a pending search after a detail visit during the pause', async () => {
    await establish();
    vi.mocked(listProviderPage).mockClear();
    typeIn(searchBox(), 'vertex');
    await vi.advanceTimersByTimeAsync(50);
    button('Open detail').click();
    flushSync();
    button('Return to list').click();
    flushSync();
    expect(searchBox().value).toBe('vertex');
    await vi.advanceTimersByTimeAsync(SEARCH_DEBOUNCE_MS + 10);
    expect(liveCalls()).toEqual([
      [undefined, expect.any(AbortSignal), 'vertex']
    ]);
    await settle();
    expect(host.textContent).toContain('Page 1');
  });
});

describe('updating state', () => {
  it('keeps the current rows visible while a replacement search loads', async () => {
    await establish();
    typeIn(searchBox(), 'mistral');
    await vi.advanceTimersByTimeAsync(SEARCH_DEBOUNCE_MS + 10);
    // The replacement request is in flight; the previous page stays rendered
    // and is marked busy instead of collapsing into a loading spinner.
    const region = host.querySelector(
      '[role="region"][aria-label="Providers"]'
    );
    expect(host.textContent).toContain('Provider 0');
    expect(host.textContent).toContain('Updating…');
    expect(region?.getAttribute('aria-busy')).toBe('true');
    expect(host.querySelector('table')).not.toBeNull();
    await settle();
    expect(host.textContent).not.toContain('Updating…');
    expect(region?.getAttribute('aria-busy')).toBe('false');
  });

  it('disables stale-result controls while the replacement page loads', async () => {
    await establish();
    // Select a row, then page forward with a delayed response: the cursor
    // from the previous result set cannot be reused, and the selection is
    // cleared when the page changes.
    host
      .querySelector<HTMLInputElement>('input[aria-label="Select Provider 0"]')!
      .click();
    flushSync();
    expect(host.textContent).toContain('connections selected');
    button('Next').click();
    flushSync();
    await vi.advanceTimersByTimeAsync(10);
    expect(host.textContent).toContain('Provider 0');
    expect(button('Next').disabled).toBe(true);
    expect(host.textContent).not.toContain('connections selected');
    await settle();
    expect(button('Next').disabled).toBe(false);
  });
});

describe('immediate behaviors', () => {
  it('requests the next page without a debounce delay', async () => {
    vi.mocked(listProviderPage).mockImplementation((cursor) =>
      deferred(
        page(
          [providerItem(cursor === 'page-two' ? 1 : 0)],
          cursor === 'page-two' ? null : 'page-two'
        )
      )
    );
    component = mount(ProviderListProbe, {
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
    await settle();
    vi.mocked(listProviderPage).mockClear();
    button('Next').click();
    flushSync();
    await vi.advanceTimersByTimeAsync(10);
    expect(liveCalls()).toEqual([['page-two', expect.any(AbortSignal), '']]);
    await settle();
    expect(host.textContent).toContain('Page 2');
  });
});
