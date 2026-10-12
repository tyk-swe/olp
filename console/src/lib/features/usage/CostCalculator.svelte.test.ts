import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { sessionKeys } from '$lib/features/access/session/sessionKeys';
import { useRole } from '$lib/features/access/session/useRole.svelte';
import { listProviders } from '$lib/features/providers/api/providers';
import { listProjects } from '$lib/features/access/projects/api';
import { listRoutes, simulateRouting } from '$lib/features/routes/api';
import { quoteUsage } from '$lib/features/usage/api/quote';
import CostCalculatorProbe from './test/CostCalculatorProbe.svelte';

vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: vi.fn()
}));
vi.mock('$lib/features/providers/api/providers', () => ({
  listProviders: vi.fn()
}));
vi.mock('$lib/features/access/projects/api', () => ({ listProjects: vi.fn() }));
vi.mock('$lib/features/routes/api', async (original) => ({
  ...(await original<typeof import('$lib/features/routes/api')>()),
  listRoutes: vi.fn(),
  simulateRouting: vi.fn()
}));
vi.mock('$lib/features/usage/api/quote', () => ({ quoteUsage: vi.fn() }));

const capabilities = {
  local_login_enabled: true,
  oidc_login_enabled: false,
  gateway_available: true,
  limits_enforced: true,
  notifications_active: false,
  payload_capture_active: false,
  retention_enforced: true
};

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
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
  client.setQueryData(sessionKeys.serviceCapabilities, capabilities);
  vi.mocked(listRoutes).mockResolvedValue([
    {
      id: 'route-1',
      slug: 'chat',
      name: undefined
    } as never
  ]);
  vi.mocked(listProviders).mockResolvedValue([
    { id: 'provider-1', name: 'Quote provider' } as never
  ]);
  vi.mocked(listProjects).mockResolvedValue([
    { id: 'project-1', name: 'Billing' } as never
  ]);
  vi.mocked(simulateRouting).mockResolvedValue([
    {
      upstream_model: 'fixture-model',
      eligible: true,
      reason: null,
      estimated_cost: { minimum: '0.001', maximum: '0.005', currency: 'USD' }
    } as never
  ]);
  vi.mocked(quoteUsage).mockResolvedValue({
    currency: 'USD',
    pricing_revision_id: 'rev-1',
    discount_id: null,
    rate_card_revision_id: 'card-1',
    complete: true,
    list_cost: '0.011',
    cost: '0.011',
    charge: '0.022',
    components: ['input', 'output'],
    commitment: null
  });
});

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  client.clear();
  host.remove();
});

function establish() {
  component = mount(CostCalculatorProbe, { target: host, props: { client } });
  flushSync();
}

async function settle() {
  await vi.waitFor(() => {
    expect(host.querySelector('select')).not.toBeNull();
  });
  flushSync();
}

it('simulates a route and lists each target bound', async () => {
  establish();
  await settle();
  const select = host.querySelector<HTMLSelectElement>('#calc-route')!;
  select.value = 'chat';
  select.dispatchEvent(new Event('change', { bubbles: true }));
  flushSync();
  const button = Array.from(
    host.querySelectorAll<HTMLButtonElement>('button')
  ).find((b) => b.textContent === 'Simulate')!;
  button.click();
  flushSync();
  await vi.waitFor(() => {
    expect(host.textContent).toContain('0.005 USD');
  });
  expect(host.textContent).toContain('0.001 USD');
});

it('quotes usage against a provider and shows cost and charge', async () => {
  establish();
  await settle();
  const usageTab = host.querySelectorAll<HTMLButtonElement>('[role="tab"]')[1];
  usageTab.click();
  flushSync();
  host.querySelector<HTMLInputElement>('#calc-model')!.value = 'fixture-model';
  host
    .querySelector<HTMLInputElement>('#calc-model')!
    .dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  const provider = host.querySelector<HTMLSelectElement>('#calc-provider')!;
  provider.value = 'provider-1';
  provider.dispatchEvent(new Event('change', { bubbles: true }));
  flushSync();
  host.querySelector<HTMLInputElement>('#calc-input')!.value = '1000';
  host
    .querySelector<HTMLInputElement>('#calc-input')!
    .dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  const button = Array.from(
    host.querySelectorAll<HTMLButtonElement>('button')
  ).find((b) => b.textContent === 'Quote')!;
  button.click();
  flushSync();
  await vi.waitFor(() => {
    expect(host.textContent).toContain('0.022');
  });
  expect(quoteUsage).toHaveBeenCalledWith(
    expect.objectContaining({
      provider_id: 'provider-1',
      model: 'fixture-model',
      operation: 'generation'
    })
  );
});
