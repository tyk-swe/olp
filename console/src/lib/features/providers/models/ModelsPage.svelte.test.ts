import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { listProviderModelInventoryPage } from '../models';
import { SEARCH_DEBOUNCE_MS } from '$lib/lists/search.svelte';
import type { ProviderModelInventory } from '../models';
import ModelsPageProbe from './test/ModelsPageProbe.svelte';

vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({ can: () => true })
}));
vi.mock('../models', async (original) => ({
  ...(await original<typeof import('../models')>()),
  listProviderModelInventoryPage: vi.fn()
}));

const SERVER_DELAY = 400;

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount>;

function modelEntry(index: number): ProviderModelInventory {
  return {
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
      display_name: `Test model ${index}`,
      enabled: true,
      id: `model-${index}`,
      upstream_model: `test-model-${index}`
    },
    provider_id: 'provider-a',
    provider_kind: 'openai',
    provider_name: 'Original provider'
  };
}

function deferred<T>(value: T, ms = SERVER_DELAY): Promise<T> {
  return new Promise((resolve) => setTimeout(() => resolve(value), ms));
}

function settle() {
  return vi.advanceTimersByTimeAsync(SERVER_DELAY + 20);
}

async function establish(nextCursor: string | null = null) {
  vi.mocked(listProviderModelInventoryPage).mockImplementation(() =>
    deferred({ items: [modelEntry(0)], nextCursor })
  );
  component = mount(ModelsPageProbe, { target: host, props: { client } });
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
  return host.querySelector<HTMLInputElement>('input[type="search"]')!;
}

function typeIn(box: HTMLInputElement, value: string) {
  box.value = value;
  box.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
}

function select(label: string, value: string) {
  const element = [...host.querySelectorAll('label')]
    .find((item) => item.textContent?.includes(label))!
    .querySelector('select')!;
  element.value = value;
  element.dispatchEvent(new Event('change', { bubbles: true }));
  flushSync();
}

/// Requests still in flight count toward the burst; the superseded fetch a
/// key change leaves behind is already cancelled.
function liveCalls() {
  return vi
    .mocked(listProviderModelInventoryPage)
    .mock.calls.filter((call) => !(call[2] as AbortSignal).aborted);
}

describe('debounced search', () => {
  it('applies a typing burst as one search after the pause', async () => {
    await establish();
    vi.mocked(listProviderModelInventoryPage).mockClear();
    for (const value of ['l', 'll', 'lla', 'llam']) {
      typeIn(searchBox(), value);
      await vi.advanceTimersByTimeAsync(50);
    }
    expect(liveCalls()).toHaveLength(0);
    await vi.advanceTimersByTimeAsync(SEARCH_DEBOUNCE_MS);
    expect(liveCalls()).toEqual([
      [undefined, undefined, expect.any(AbortSignal), 'llam', undefined]
    ]);
    await settle();
  });

  it('applies filter and cursor changes atomically', async () => {
    vi.mocked(listProviderModelInventoryPage).mockImplementation((cursor) =>
      deferred({
        items: [modelEntry(cursor === 'page-two' ? 1 : 0)],
        nextCursor: cursor === 'page-two' ? null : 'page-two'
      })
    );
    component = mount(ModelsPageProbe, { target: host, props: { client } });
    flushSync();
    await settle();
    button('Next').click();
    flushSync();
    await settle();
    expect(host.textContent).toContain('Page 2');
    // A new search on page two must not reuse the page-two cursor.
    typeIn(searchBox(), 'mistral');
    await vi.advanceTimersByTimeAsync(SEARCH_DEBOUNCE_MS + 10);
    for (const call of vi.mocked(listProviderModelInventoryPage).mock.calls) {
      if (call[3] === 'mistral') expect(call[0]).toBeUndefined();
    }
    expect(liveCalls().some((call) => call[3] === 'mistral')).toBe(true);
    await settle();
  });

  it('applies dropdown changes immediately with the current search text', async () => {
    await establish();
    vi.mocked(listProviderModelInventoryPage).mockClear();
    typeIn(searchBox(), 'vision');
    await vi.advanceTimersByTimeAsync(50);
    // The pending debounce is cancelled; the select applies everything now.
    select('Client surface', 'anthropic');
    await vi.advanceTimersByTimeAsync(10);
    expect(liveCalls()).toEqual([
      [undefined, undefined, expect.any(AbortSignal), 'vision', 'anthropic']
    ]);
    await vi.advanceTimersByTimeAsync(SEARCH_DEBOUNCE_MS + SERVER_DELAY + 100);
    expect(liveCalls()).toHaveLength(1);
  });

  it('clears the search immediately without waiting out the pause', async () => {
    await establish();
    typeIn(searchBox(), 'pending');
    await vi.advanceTimersByTimeAsync(50);
    typeIn(searchBox(), '');
    await vi.advanceTimersByTimeAsync(10);
    expect(liveCalls()).toEqual([
      [undefined, undefined, expect.any(AbortSignal), '', undefined]
    ]);
    await settle();
  });

  it('drops a pending search when the page unmounts', async () => {
    await establish();
    vi.mocked(listProviderModelInventoryPage).mockClear();
    typeIn(searchBox(), 'never');
    await unmount(component);
    component = undefined as unknown as ReturnType<typeof mount>;
    await vi.advanceTimersByTimeAsync(SEARCH_DEBOUNCE_MS + SERVER_DELAY + 100);
    expect(listProviderModelInventoryPage).not.toHaveBeenCalled();
  });
});

describe('updating state', () => {
  it('keeps the current rows visible while a replacement filter loads', async () => {
    await establish();
    typeIn(searchBox(), 'embedding');
    await vi.advanceTimersByTimeAsync(SEARCH_DEBOUNCE_MS + 10);
    expect(host.textContent).toContain('Test model 0');
    expect(host.textContent).toContain('Updating…');
    expect(host.querySelector('table')).not.toBeNull();
    expect(
      host.querySelector('.table-shell[aria-busy]')?.getAttribute('aria-busy')
    ).toBe('true');
    await settle();
    expect(host.textContent).not.toContain('Updating…');
  });

  it('disables stale-result controls while a replacement page loads', async () => {
    await establish('page-two');
    button('Next').click();
    flushSync();
    await vi.advanceTimersByTimeAsync(10);
    // The previous page's cursor cannot page the replacement result set, and
    // the eligibility mutation is disabled on outdated rows.
    expect(button('Next').disabled).toBe(true);
    expect(
      host.querySelector<HTMLInputElement>('.eligibility input')!.disabled
    ).toBe(true);
    await settle();
    expect(button('Next').disabled).toBe(false);
    expect(
      host.querySelector<HTMLInputElement>('.eligibility input')!.disabled
    ).toBe(false);
  });
});

describe('cancellation', () => {
  it('passes an AbortSignal and aborts in-flight requests on unmount', async () => {
    const signals: AbortSignal[] = [];
    vi.mocked(listProviderModelInventoryPage).mockImplementation(
      (_c, _e, signal) => {
        if (signal) signals.push(signal);
        return deferred({ items: [modelEntry(0)], nextCursor: null });
      }
    );
    component = mount(ModelsPageProbe, { target: host, props: { client } });
    flushSync();
    await vi.advanceTimersByTimeAsync(10);
    await unmount(component);
    component = undefined as unknown as ReturnType<typeof mount>;
    expect(signals.length).toBeGreaterThan(0);
    expect(signals.every((signal) => signal.aborted)).toBe(true);
  });
});
