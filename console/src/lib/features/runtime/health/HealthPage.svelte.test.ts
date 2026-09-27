import { mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import {
  getReadiness,
  listProviderHealth,
  listRequestMetadataGatewayEpochs
} from '$lib/features/runtime/health/api';
import { listRuntimeGenerations } from '$lib/features/runtime/api';
import { usageCompleteness } from '$lib/api/usage';
import HealthPageProbe from './test/HealthPageProbe.svelte';

const role = vi.hoisted(() => ({ globalScope: true }));

vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({
    globalScope: role.globalScope,
    allows: () => role.globalScope
  })
}));
vi.mock('$lib/features/runtime/health/api', () => ({
  getReadiness: vi.fn(),
  listProviderHealth: vi.fn(),
  listRequestMetadataGatewayEpochs: vi.fn()
}));
vi.mock('$lib/features/runtime/api', () => ({
  listRuntimeGenerations: vi.fn()
}));
vi.mock('$lib/api/usage', () => ({
  usageCompleteness: vi.fn()
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
  vi.mocked(getReadiness).mockRejectedValue(new Error('unavailable'));
  vi.mocked(listProviderHealth).mockResolvedValue({
    window_minutes: 15,
    items: []
  });
  vi.mocked(listRequestMetadataGatewayEpochs).mockResolvedValue({
    items: [],
    nextCursor: null
  });
  vi.mocked(listRuntimeGenerations).mockResolvedValue({
    items: [],
    nextCursor: null
  });
  vi.mocked(usageCompleteness).mockRejectedValue(new Error('unavailable'));
});

afterEach(async () => {
  if (component) await unmount(component);
  client.clear();
  host.remove();
});

it('reads installation-wide runtime generations for global members', async () => {
  role.globalScope = true;
  component = mount(HealthPageProbe, { target: host, props: { client } });
  await vi.waitFor(() => expect(listRuntimeGenerations).toHaveBeenCalled());
});

it('never requests installation-wide panels for assigned members', async () => {
  role.globalScope = false;
  component = mount(HealthPageProbe, { target: host, props: { client } });
  await vi.waitFor(() => expect(listProviderHealth).toHaveBeenCalled());
  expect(listRuntimeGenerations).not.toHaveBeenCalled();
  expect(listRequestMetadataGatewayEpochs).not.toHaveBeenCalled();
  expect(host.textContent).not.toContain('Runtime generations');
  expect(host.textContent).not.toContain('Unresolved gateway epochs');
});
