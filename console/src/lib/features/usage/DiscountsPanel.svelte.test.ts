import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { sessionKeys } from '$lib/features/access/session/sessionKeys';
import { useRole } from '$lib/features/access/session/useRole.svelte';
import { listProviderKinds } from '$lib/features/providers/api/models';
import { listProviderVendors } from '$lib/features/providers/api/providers';
import {
  createDiscount,
  listDiscounts,
  type PricingDiscount
} from './api/discounts';
import DiscountsProbe from './test/DiscountsProbe.svelte';

vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: vi.fn()
}));
vi.mock('$lib/features/providers/api/models', () => ({
  listProviderKinds: vi.fn()
}));
vi.mock('$lib/features/providers/api/providers', () => ({
  listProviderVendors: vi.fn()
}));
vi.mock('./api/discounts', () => ({
  createDiscount: vi.fn(),
  listDiscounts: vi.fn()
}));

const capabilities = {
  local_login_enabled: true,
  oidc_login_enabled: false,
  gateway_available: true,
  limits_enforced: true,
  notifications_active: false,
  payload_capture_active: false,
  retention_enforced: true
};
const discount: PricingDiscount = {
  id: 'discount-1',
  provider_kind: 'openai',
  vendor_id: null,
  provider_id: 'provider-1',
  provider_name: 'primary',
  multiplier: '0.9',
  effective_at: '2026-10-01T00:00:00Z',
  created_at: '2026-09-30T12:00:00Z',
  created_by: 'owner'
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
  vi.mocked(listProviderKinds).mockResolvedValue([
    {
      kind: 'openai',
      label: 'OpenAI',
      description: '',
      auth_modes: [],
      default_auth_mode: 'api_key',
      fields: [],
      presets: []
    }
  ]);
  vi.mocked(listProviderVendors).mockResolvedValue([]);
  vi.mocked(listDiscounts).mockResolvedValue({
    items: [discount],
    nextCursor: null
  });
  vi.mocked(createDiscount).mockResolvedValue(discount);
});

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  client.clear();
  host.remove();
});

function establish() {
  component = mount(DiscountsProbe, { target: host, props: { client } });
  flushSync();
}

function field(id: string) {
  const element = host.querySelector<HTMLInputElement | HTMLSelectElement>(
    `#${id}`
  );
  if (!element) throw new Error(`Missing field ${id}`);
  return element;
}

function edit(id: string, value: string) {
  const element = field(id);
  element.value = value;
  element.dispatchEvent(
    new Event(element instanceof HTMLSelectElement ? 'change' : 'input', {
      bubbles: true
    })
  );
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

it('lists discounts and submits a scoped multiplier', async () => {
  establish();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('openai / primary')
  );
  await vi.waitFor(() => expect(field('discount-kind').value).toBe('openai'));
  edit('discount-multiplier', ' 0.8 ');
  submit();
  await vi.waitFor(() => expect(vi.mocked(createDiscount)).toHaveBeenCalled());
  const [draft, requestId] = vi.mocked(createDiscount).mock.calls[0];
  expect(draft).toMatchObject({
    provider_kind: 'openai',
    multiplier: '0.8',
    vendor_id: null,
    provider_id: null
  });
  expect(typeof requestId).toBe('string');
});

it('blocks submission without a multiplier', async () => {
  establish();
  await vi.waitFor(() => expect(field('discount-kind').value).toBe('openai'));
  submit();
  expect(vi.mocked(createDiscount)).not.toHaveBeenCalled();
  expect(host.textContent).toContain('required');
});
