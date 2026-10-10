// @vitest-environment jsdom
import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { ApiKey } from '$lib/features/access/api-keys/api';
import { budgetGroupKeys } from '$lib/features/access/budget-groups/budgetGroupKeys';
import ApiKeyPolicyFormProbe from './test/ApiKeyPolicyFormProbe.svelte';

vi.mock('$app/navigation', () => ({ beforeNavigate: vi.fn(), goto: vi.fn() }));
vi.mock('$lib/features/access/session/serviceCapabilities.svelte', () => ({
  useServiceCapabilities: () => ({
    gatewayAvailable: false,
    limitsEnforced: true,
    pending: false
  })
}));

const key = {
  id: '01980000-0000-7000-8000-000000000301',
  lookup_id: 'olp_live_abcd',
  name: 'production SDK',
  project_id: null,
  project_name: null,
  budget_group_id: null,
  scopes: ['inference'],
  allowed_routes: [],
  regional_limits: {},
  allow_provider_state: false,
  response_metadata: false,
  allowed_attribution_keys: [],
  budget: {
    daily: {
      limit: null,
      accrued: '0',
      window_ends_at: '2026-07-13T00:00:00Z'
    },
    monthly: {
      limit: null,
      accrued: '0',
      window_ends_at: '2026-08-01T00:00:00Z'
    },
    unpriced_attempts: 0
  },
  etag: '01980000-0000-7000-8000-000000000302',
  created_by: '01980000-0000-7000-8000-000000000303',
  created_by_email: 'owner@example.com',
  created_at: '2026-07-12T12:00:00Z'
} satisfies ApiKey;

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;

beforeEach(() => {
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
  client.setQueryData(budgetGroupKeys.list(), []);
});

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  client.clear();
  host.remove();
});

function render(editing: ApiKey, canManage = true) {
  const onSubmit = vi.fn(() => true);
  component = mount(ApiKeyPolicyFormProbe, {
    target: host,
    props: { client, editing, canManage, onSubmit }
  });
  flushSync();
  return onSubmit;
}

function metadataCheckbox() {
  const checkbox = [...host.querySelectorAll('label')]
    .find((label) => label.textContent?.includes('gateway metadata headers'))
    ?.querySelector('input');
  if (!checkbox) throw new Error('Missing response metadata checkbox');
  return checkbox;
}

function submit() {
  host
    .querySelector('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
}

it('opts a key into response metadata and sends the choice on save', async () => {
  const onSubmit = render(key);
  expect(metadataCheckbox().checked).toBe(false);

  metadataCheckbox().click();
  flushSync();
  submit();

  await vi.waitFor(() => expect(onSubmit).toHaveBeenCalled());
  expect(onSubmit.mock.calls[0]).toEqual([
    expect.objectContaining({ response_metadata: true }),
    undefined
  ]);
});

it('shows the stored opt-in and lets it be switched off', async () => {
  const onSubmit = render({ ...key, response_metadata: true });
  expect(metadataCheckbox().checked).toBe(true);

  metadataCheckbox().click();
  flushSync();
  submit();

  await vi.waitFor(() => expect(onSubmit).toHaveBeenCalled());
  expect(onSubmit.mock.calls[0]).toEqual([
    expect.objectContaining({ response_metadata: false }),
    undefined
  ]);
});

it('locks the opt-in for a viewer who cannot manage keys', () => {
  render({ ...key, response_metadata: true }, false);
  expect(metadataCheckbox().checked).toBe(true);
  expect(metadataCheckbox().disabled).toBe(true);
});

it('validates route groups and templates against the edited key project', async () => {
  const onSubmit = render({
    ...key,
    project_id: '01980000-0000-7000-8000-000000000304',
    project_name: 'Checkout',
    allowed_route_groups: ['support'],
    limit_template: 'standard'
  });
  submit();

  await vi.waitFor(() => expect(onSubmit).toHaveBeenCalled());
  expect(onSubmit.mock.calls[0]).toEqual([
    expect.objectContaining({
      allowed_route_groups: ['support'],
      limit_template: 'standard'
    }),
    undefined
  ]);
});
