import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { listProviderPage } from './api';
import { provider } from '$lib/forms/test/draftFixtures';
import ProviderListProbe from './test/ProviderListProbe.svelte';

vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({ can: () => true })
}));
vi.mock('./api', async (original) => ({
  ...(await original<typeof import('./api')>()),
  listProviderPage: vi.fn()
}));

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount>;

beforeEach(() => {
  vi.resetAllMocks();
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
  vi.mocked(listProviderPage).mockResolvedValue({
    items: [{ ...provider, kind: 'gemini', vendor_id: 'google' }],
    nextCursor: 'page-three'
  });
  component = mount(ProviderListProbe, {
    target: host,
    props: {
      client,
      initialState: {
        cursor: 'page-two',
        history: [undefined],
        search: 'google'
      }
    }
  });
  flushSync();
});

afterEach(async () => {
  await unmount(component);
  client.clear();
  host.remove();
});

function button(label: string) {
  return [...host.querySelectorAll('button')].find(
    (button) => button.textContent?.trim() === label
  )!;
}

it('retains the search and page when returning from a provider detail', async () => {
  await vi.waitFor(() => expect(host.textContent).toContain('Page 2'));
  expect(listProviderPage).toHaveBeenCalledWith(
    'page-two',
    expect.any(AbortSignal),
    'google'
  );
  button('Open detail').click();
  flushSync();
  expect(host.querySelector('#provider-search')).toBeNull();
  button('Return to list').click();
  flushSync();
  await vi.waitFor(() => expect(host.textContent).toContain('Page 2'));
  expect(host.querySelector<HTMLInputElement>('#provider-search')!.value).toBe(
    'google'
  );
  expect(listProviderPage).not.toHaveBeenCalledWith(
    undefined,
    expect.anything(),
    expect.anything()
  );
});

it('resets pagination only when the operator changes the search', async () => {
  await vi.waitFor(() => expect(host.textContent).toContain('Page 2'));
  const search = host.querySelector<HTMLInputElement>('#provider-search')!;
  search.value = 'anthropic';
  search.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  await vi.waitFor(() => {
    expect(listProviderPage).toHaveBeenLastCalledWith(
      undefined,
      expect.any(AbortSignal),
      'anthropic'
    );
    expect(host.textContent).toContain('Page 1');
  });
  expect(button('Previous').disabled).toBe(true);
  button('Open detail').click();
  flushSync();
  button('Return to list').click();
  flushSync();
  await vi.waitFor(() => expect(host.textContent).toContain('Page 1'));
  expect(host.querySelector<HTMLInputElement>('#provider-search')!.value).toBe(
    'anthropic'
  );
});
