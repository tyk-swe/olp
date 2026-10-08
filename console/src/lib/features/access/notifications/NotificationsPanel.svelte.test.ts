// @vitest-environment jsdom
import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { operationsFor } from '$lib/features/access/session/test/grants';
import {
  createNotificationRule,
  listNotificationDeliveries,
  listNotificationDestinations,
  listNotificationRules,
  type NotificationDelivery,
  type NotificationDestination,
  type NotificationRule
} from '$lib/features/access/notifications/api';
import NotificationsProbe from './test/NotificationsProbe.svelte';

const role = vi.hoisted(
  (): { current: 'operator' | 'developer'; global: boolean } => ({
    current: 'operator',
    global: true
  })
);
vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({
    role: role.current,
    globalScope: role.global,
    user: {
      access_scope: role.global ? 'global' : 'assigned',
      operations: operationsFor(role.current, role.global)
    },
    can: (capability: string) =>
      capability === 'api_keys.manage' ||
      (capability === 'settings.update' && role.current === 'operator')
  })
}));
vi.mock('$lib/features/access/session/serviceCapabilities.svelte', () => ({
  useServiceCapabilities: () => ({ notificationsActive: true })
}));
vi.mock('$lib/features/access/projects/api', async (original) => ({
  ...(await original<typeof import('$lib/features/access/projects/api')>()),
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
vi.mock('$lib/features/access/notifications/api', async (original) => ({
  ...(await original<
    typeof import('$lib/features/access/notifications/api')
  >()),
  createNotificationRule: vi.fn(),
  listNotificationDeliveries: vi.fn(),
  listNotificationDestinations: vi.fn(),
  listNotificationRules: vi.fn()
}));

const installationHook: NotificationDestination = {
  id: '01980000-0000-7000-8000-000000000601',
  name: 'On-call hook',
  url: 'https://hooks.example.com/on-call',
  project_id: null,
  project_name: null,
  enabled: true,
  etag: '01980000-0000-7000-8000-000000000602',
  created_by: '01980000-0000-7000-8000-000000000401',
  created_by_email: 'operator@example.com',
  created_at: '2026-09-27T12:00:00Z',
  updated_at: '2026-09-27T12:00:00Z'
};

const projectHook: NotificationDestination = {
  ...installationHook,
  id: '01980000-0000-7000-8000-000000000603',
  name: 'Project hook',
  project_id: '01980000-0000-7000-8000-000000000901',
  project_name: 'Platform'
};

const lapseRule: NotificationRule = {
  id: '01980000-0000-7000-8000-000000000701',
  name: 'Lapsed grants',
  project_id: null,
  project_name: null,
  event: 'provider.grant.lapsed',
  subject_kind: null,
  subject_id: null,
  subject_name: null,
  window_kind: null,
  threshold_percent: null,
  destination_id: installationHook.id,
  destination_name: installationHook.name,
  enabled: true,
  etag: '01980000-0000-7000-8000-000000000702',
  created_by: installationHook.created_by,
  created_by_email: installationHook.created_by_email,
  created_at: '2026-09-27T12:00:00Z',
  updated_at: '2026-09-27T12:00:00Z'
};

const lapseDelivery: NotificationDelivery = {
  id: '01980000-0000-7000-8000-000000000801',
  rule_id: lapseRule.id,
  rule_name: lapseRule.name,
  project_id: null,
  event: 'provider.grant.lapsed',
  window_id: null,
  threshold_percent: null,
  accrued: null,
  limit: null,
  currency: null,
  provider_id: '01980000-0000-7000-8000-000000000a01',
  provider_name: 'Reference account',
  credential_version_id: '01980000-0000-7000-8000-000000000a02',
  credential_version: 3,
  status: 'delivered',
  attempts: 1,
  last_error_code: null,
  created_at: '2026-09-27T12:00:00Z',
  last_attempt_at: '2026-09-27T12:00:05Z',
  delivered_at: '2026-09-27T12:00:05Z'
};

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;

beforeEach(() => {
  vi.mocked(createNotificationRule).mockReset();
  role.current = 'operator';
  role.global = true;
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
  vi.mocked(listNotificationDestinations).mockResolvedValue([
    installationHook,
    projectHook
  ]);
  vi.mocked(listNotificationRules).mockResolvedValue([lapseRule]);
  vi.mocked(listNotificationDeliveries).mockResolvedValue([lapseDelivery]);
});

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  client.clear();
  host.remove();
});

async function render() {
  component = mount(NotificationsProbe, { target: host, props: { client } });
  flushSync();
  await new Promise((resolve) => setTimeout(resolve, 0));
  await new Promise((resolve) => setTimeout(resolve, 0));
  flushSync();
}

function choose(selector: string, value: string) {
  const field = host.querySelector<HTMLInputElement | HTMLSelectElement>(
    selector
  )!;
  field.value = value;
  field.dispatchEvent(
    new Event(field instanceof HTMLSelectElement ? 'change' : 'input', {
      bubbles: true
    })
  );
  flushSync();
}

it('subscribes an installation-wide destination to grant lapses', async () => {
  vi.mocked(createNotificationRule).mockResolvedValue(lapseRule);
  await render();

  choose('#rule-event', 'provider.grant.lapsed');
  expect(host.querySelector('#rule-subject')).toBeNull();
  expect(host.querySelector('#rule-threshold')).toBeNull();
  expect(host.querySelector('#rule-project')).toBeNull();
  const destinations = [
    ...host.querySelectorAll<HTMLOptionElement>('#rule-destination option')
  ].map((option) => option.textContent);
  expect(destinations).toEqual(['Choose a destination', 'On-call hook']);

  choose('#rule-name', 'Lapsed grants');
  choose('#rule-destination', installationHook.id);
  host
    .querySelector<HTMLSelectElement>('#rule-event')!
    .form!.dispatchEvent(
      new Event('submit', { bubbles: true, cancelable: true })
    );
  await new Promise((resolve) => setTimeout(resolve, 0));

  expect(createNotificationRule).toHaveBeenCalledWith({
    name: 'Lapsed grants',
    event: 'provider.grant.lapsed',
    destination_id: installationHook.id
  });
});

it('lists grant lapse rules and the lapses they delivered', async () => {
  await render();

  const [rules, deliveries] = [...host.querySelectorAll('table')].slice(1);
  expect(rules!.textContent).toContain('Grant lapsed');
  expect(rules!.textContent).toContain('Every provider');
  expect(deliveries!.textContent).toContain(
    'Reference account · credential v3'
  );
});

it.each([
  { current: 'developer' as const, global: true },
  { current: 'operator' as const, global: false }
])(
  'withholds grant lapse subscriptions from $current with global=$global',
  async (principal) => {
    Object.assign(role, principal);
    await render();

    expect(host.querySelector('#rule-event')).toBeNull();
    expect(host.querySelector('#rule-subject')).not.toBeNull();
  }
);

it('names budget windows without assuming a time zone', async () => {
  vi.mocked(listNotificationRules).mockResolvedValue([
    {
      ...lapseRule,
      event: 'budget.threshold',
      subject_kind: 'api_key',
      subject_id: '01980000-0000-7000-8000-000000000b01',
      subject_name: 'Checkout',
      window_kind: 'week',
      threshold_percent: 80
    }
  ]);
  await render();

  const windows = [
    ...host.querySelectorAll<HTMLOptionElement>('#rule-window option')
  ].map((option) => option.textContent);
  expect(windows).toEqual(['Budget day', 'Budget week', 'Budget month']);
  const rules = [...host.querySelectorAll('table')][1]!;
  expect(rules.textContent).toContain('Budget week');
  expect(host.textContent).not.toContain('UTC');
});
