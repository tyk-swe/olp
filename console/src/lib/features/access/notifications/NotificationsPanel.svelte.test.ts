// @vitest-environment jsdom
import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { captureRequests, jsonResponse } from '$lib/api/test/requestCapture';
import {
  listNotificationDestinations,
  type BudgetAlertDelivery,
  type NotificationDestination
} from './api';
import NotificationsProbe from './test/NotificationsProbe.svelte';

vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({ role: 'owner', can: () => true })
}));
vi.mock('$lib/features/access/session/serviceCapabilities.svelte', () => ({
  useServiceCapabilities: () => ({ notificationsActive: true })
}));
vi.mock('$lib/features/access/api', async (original) => ({
  ...(await original<typeof import('$lib/features/access/api')>()),
  listProjectMemberships: vi.fn(async () => [])
}));
vi.mock('$lib/features/access/api-keys/api', async (original) => ({
  ...(await original<typeof import('$lib/features/access/api-keys/api')>()),
  listApiKeys: vi.fn(async () => [])
}));
vi.mock('$lib/features/access/budget-groups/api', async (original) => ({
  ...(await original<
    typeof import('$lib/features/access/budget-groups/api')
  >()),
  listBudgetGroups: vi.fn(async () => [])
}));
vi.mock('./api', async (original) => ({
  ...(await original<typeof import('./api')>()),
  listNotificationDestinations: vi.fn(),
  listBudgetAlertRules: vi.fn(async () => [])
}));

const destination: NotificationDestination = {
  id: '01980000-0000-7000-8000-000000000501',
  name: 'Finance hook',
  url: 'https://hooks.example.com/budget',
  project_id: null,
  project_name: null,
  enabled: true,
  etag: '01980000-0000-7000-8000-000000000502',
  created_by: '01980000-0000-7000-8000-000000000503',
  created_by_email: 'owner@example.com',
  created_at: '2026-07-12T12:00:00Z',
  updated_at: '2026-07-12T12:00:00Z'
};

function delivery(id: string, ruleName: string): BudgetAlertDelivery {
  return {
    id,
    rule_id: '01980000-0000-7000-8000-000000000504',
    rule_name: ruleName,
    project_id: null,
    window_id: 20260712,
    threshold_percent: 80,
    accrued: '8.00',
    limit: '10.00',
    currency: 'USD',
    status: 'delivered',
    attempts: 1,
    last_error_code: null,
    last_attempt_at: '2026-07-12T12:00:00Z',
    delivered_at: '2026-07-12T12:00:00Z',
    created_at: '2026-07-12T12:00:00Z'
  };
}

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;
let deliveryRequests: Request[];

beforeEach(() => {
  vi.mocked(listNotificationDestinations).mockResolvedValue([destination]);
  const pages: Record<string, unknown> = {
    '': {
      items: [delivery('01980000-0000-7000-8000-000000000602', 'Newest rule')],
      next_cursor: 'deliveries-2'
    },
    'deliveries-2': {
      items: [delivery('01980000-0000-7000-8000-000000000601', 'Oldest rule')],
      next_cursor: null
    }
  };
  deliveryRequests = captureRequests((request) =>
    jsonResponse(pages[new URL(request.url).searchParams.get('cursor') ?? ''])
  );
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
});

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  client.clear();
  host.remove();
  vi.unstubAllGlobals();
});

async function settle() {
  for (let tick = 0; tick < 5; tick += 1)
    await new Promise((resolve) => setTimeout(resolve, 0));
  flushSync();
}

async function render() {
  component = mount(NotificationsProbe, { target: host, props: { client } });
  flushSync();
  await settle();
}

function button(label: string, scope: ParentNode = host) {
  return [...scope.querySelectorAll<HTMLButtonElement>('button')].find(
    (candidate) => candidate.textContent?.trim() === label
  )!;
}

it('masks webhook signing secrets while they are typed', async () => {
  await render();
  expect(host.querySelector<HTMLInputElement>('#dest-secret')?.type).toBe(
    'password'
  );
  button('Edit').click();
  flushSync();
  expect(
    host.querySelector<HTMLInputElement>('[aria-label="New signing secret"]')
      ?.type
  ).toBe('password');
});

it('pages delivery history instead of loading every delivery', async () => {
  await render();
  expect(host.textContent).toContain('Newest rule');
  expect(host.textContent).not.toContain('Oldest rule');
  expect(deliveryRequests).toHaveLength(1);

  const pages = host.querySelector('nav[aria-label="Delivery pages"]')!;
  button('Next', pages).click();
  await settle();
  expect(host.textContent).toContain('Oldest rule');
  expect(host.textContent).not.toContain('Newest rule');
  expect(new URL(deliveryRequests.at(-1)!.url).searchParams.get('cursor')).toBe(
    'deliveries-2'
  );
});
