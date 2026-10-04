import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { ApiProblem } from '$lib/api/http';
import { useRole } from '$lib/features/access/session/useRole.svelte';
import {
  listPricingSources,
  updatePricingSource,
  type PricingSource
} from './api/pricingSources';
import { pricingKeys } from './pricingKeys';
import PricingSourcesProbe from './test/PricingSourcesProbe.svelte';

vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: vi.fn()
}));
vi.mock('./api/pricingSources', () => ({
  createPricingSource: vi.fn(),
  listPricingSourceSnapshots: vi.fn(),
  listPricingSources: vi.fn(),
  publishPricingSnapshot: vi.fn(),
  refreshPricingSource: vi.fn(),
  updatePricingSource: vi.fn()
}));

const source: PricingSource = {
  id: 'source-1',
  name: 'Vendor prices',
  url: 'https://prices.example/models.json',
  enabled: true,
  etag: 'e1',
  created_at: '2026-09-15T12:00:00Z',
  created_by: 'owner',
  updated_at: '2026-09-15T12:00:00Z'
};
const changed: PricingSource = {
  ...source,
  name: 'Vendor prices (renamed)',
  url: 'https://prices.example/current.json',
  enabled: false,
  etag: 'e2'
};
const mismatch = new ApiProblem({
  status: 412,
  type: 'https://openllmproxy.dev/problems/etag_mismatch',
  title: 'Precondition Failed'
});

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;

beforeEach(() => {
  vi.resetAllMocks();
  vi.mocked(useRole).mockReturnValue({
    can: () => true,
    allows: () => true,
    role: 'owner',
    globalScope: true,
    user: null
  });
  vi.mocked(listPricingSources)
    .mockResolvedValueOnce([source])
    .mockResolvedValue([changed]);
  vi.mocked(updatePricingSource)
    .mockRejectedValueOnce(mismatch)
    .mockResolvedValue(changed as never);
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
  component = mount(PricingSourcesProbe, { target: host, props: { client } });
  flushSync();
});

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  client.clear();
  host.remove();
});

function button(label: string) {
  return [...host.querySelectorAll<HTMLButtonElement>('button')].find(
    (candidate) => candidate.textContent?.trim() === label
  )!;
}

async function refused() {
  await vi.waitFor(() =>
    expect(host.querySelector('[role="alert"]')?.textContent).toContain(
      'changed meanwhile'
    )
  );
  await vi.waitFor(() => expect(listPricingSources).toHaveBeenCalledTimes(2));
  await vi.waitFor(() =>
    expect(
      client.getQueryData<PricingSource[]>(pricingKeys.sources())?.[0]?.etag
    ).toBe('e2')
  );
  flushSync();
}

it('refreshes the source after a refused toggle so a retry sends its ETag', async () => {
  await vi.waitFor(() => expect(button('Disable')).toBeDefined());
  button('Disable').click();
  await refused();
  button('Enable').click();
  await vi.waitFor(() => expect(updatePricingSource).toHaveBeenCalledTimes(2));
  expect(vi.mocked(updatePricingSource).mock.calls[1]![0].etag).toBe('e2');
});

it('requires reopening a conflicted edit with current values and ETag', async () => {
  await vi.waitFor(() => expect(button('Edit')).toBeDefined());
  button('Edit').click();
  flushSync();
  button('Save').click();
  await refused();
  expect(button('Save')).toBeUndefined();
  button('Edit').click();
  flushSync();
  button('Save').click();
  await vi.waitFor(() => expect(updatePricingSource).toHaveBeenCalledTimes(2));
  expect(vi.mocked(updatePricingSource).mock.calls[1]).toEqual([
    changed,
    { name: changed.name, url: changed.url, enabled: changed.enabled }
  ]);
});
