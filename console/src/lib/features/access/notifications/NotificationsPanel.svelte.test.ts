// @vitest-environment jsdom
import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { operationsFor } from '$lib/features/access/session/test/grants';
import {
  createNotificationDestination,
  createNotificationRule,
  updateNotificationDestination,
  updateNotificationRule,
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
  listProjectMemberships: vi.fn(async () => [
    {
      id: '01980000-0000-7000-8000-000000000901',
      name: 'Platform',
      role: 'manager' as const
    }
  ])
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
  updateNotificationDestination: vi.fn(),
  updateNotificationRule: vi.fn(),
  createNotificationDestination: vi.fn(),
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
  type: 'webhook',
  configuration: {},
  secret_configured: true,
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
  configuration: {},
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

it('withholds installation events from a developer without settings', async () => {
  Object.assign(role, { current: 'developer' as const, global: true });
  await render();

  const options = [
    ...host.querySelectorAll<HTMLOptionElement>('#rule-event option')
  ];
  const lapsed = options.find((o) => o.value === 'provider.grant.lapsed')!;
  const threshold = options.find((o) => o.value === 'budget.threshold')!;
  expect(lapsed.disabled).toBe(true);
  expect(threshold.disabled).toBe(false);
});

it('scopes an assigned manager to project events', async () => {
  Object.assign(role, { current: 'operator' as const, global: false });
  await render();

  const options = [
    ...host.querySelectorAll<HTMLOptionElement>('#rule-event option')
  ];
  const lapsed = options.find((o) => o.value === 'provider.grant.lapsed')!;
  const latency = options.find((o) => o.value === 'route.latency')!;
  expect(lapsed.disabled).toBe(true);
  expect(latency.disabled).toBe(false);
});

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

it('creates a Slack destination with origin URL and sealed webhook URL', async () => {
  vi.mocked(listNotificationDestinations).mockResolvedValue([]);
  vi.mocked(createNotificationDestination).mockResolvedValue({
    ...installationHook,
    type: 'slack',
    url: 'https://hooks.slack.com'
  });
  await render();

  choose('#dest-type', 'slack');
  choose('#dest-name', 'Ops slack');
  choose('#dest-url', 'https://hooks.slack.com');
  choose('#dest-webhook-url', 'https://hooks.slack.com/services/T/B/secret');
  host
    .querySelector<HTMLSelectElement>('#dest-type')!
    .form!.dispatchEvent(
      new Event('submit', { bubbles: true, cancelable: true })
    );
  await new Promise((resolve) => setTimeout(resolve, 0));

  expect(createNotificationDestination).toHaveBeenCalledWith({
    name: 'Ops slack',
    type: 'slack',
    url: 'https://hooks.slack.com',
    project_id: null,
    secret: { webhook_url: 'https://hooks.slack.com/services/T/B/secret' }
  });
});

it('creates a PagerDuty destination with endpoint and routing key', async () => {
  vi.mocked(listNotificationDestinations).mockResolvedValue([]);
  vi.mocked(createNotificationDestination).mockResolvedValue({
    ...installationHook,
    type: 'pagerduty',
    url: 'https://events.pagerduty.com/v2/enqueue'
  });
  await render();

  choose('#dest-type', 'pagerduty');
  choose('#dest-name', 'PagerDuty');
  choose('#dest-url', 'https://events.pagerduty.com/v2/enqueue');
  choose('#dest-routing-key', 'pd-routing-key');
  host
    .querySelector<HTMLSelectElement>('#dest-type')!
    .form!.dispatchEvent(
      new Event('submit', { bubbles: true, cancelable: true })
    );
  await new Promise((resolve) => setTimeout(resolve, 0));

  expect(createNotificationDestination).toHaveBeenCalledWith({
    name: 'PagerDuty',
    type: 'pagerduty',
    url: 'https://events.pagerduty.com/v2/enqueue',
    project_id: null,
    secret: { routing_key: 'pd-routing-key' }
  });
});

it('creates an email destination with SMTP configuration and credentials', async () => {
  vi.mocked(listNotificationDestinations).mockResolvedValue([]);
  vi.mocked(createNotificationDestination).mockResolvedValue({
    ...installationHook,
    type: 'email',
    url: 'smtps://smtp.example.com:465'
  });
  await render();

  choose('#dest-type', 'email');
  choose('#dest-name', 'Mail');
  choose('#dest-url', 'smtps://smtp.example.com:465');
  choose('#dest-email-from', 'alerts@example.com');
  choose('#dest-email-to', 'ops@example.com, dev@example.com');
  choose('#dest-email-username', 'smtp-user');
  choose('#dest-email-password', 'smtp-pass');
  host
    .querySelector<HTMLSelectElement>('#dest-type')!
    .form!.dispatchEvent(
      new Event('submit', { bubbles: true, cancelable: true })
    );
  await new Promise((resolve) => setTimeout(resolve, 0));

  expect(createNotificationDestination).toHaveBeenCalledWith({
    name: 'Mail',
    type: 'email',
    url: 'smtps://smtp.example.com:465',
    project_id: null,
    configuration: {
      from: 'alerts@example.com',
      to: ['ops@example.com', 'dev@example.com']
    },
    secret: { username: 'smtp-user', password: 'smtp-pass' }
  });
});

it('creates a provider.error_rate rule with ratio configuration', async () => {
  vi.mocked(createNotificationRule).mockResolvedValue({
    ...lapseRule,
    event: 'provider.error_rate'
  });
  await render();

  choose('#rule-event', 'provider.error_rate');
  expect(host.querySelector('#rule-subject')).toBeNull();
  choose('#rule-name', 'Error rate');
  choose('#rule-destination', installationHook.id);
  choose('#rule-config-threshold', '0.5');
  choose('#rule-config-window_seconds', '300');
  choose('#rule-config-minimum_samples', '3');
  host
    .querySelector<HTMLSelectElement>('#rule-event')!
    .form!.dispatchEvent(
      new Event('submit', { bubbles: true, cancelable: true })
    );
  await new Promise((resolve) => setTimeout(resolve, 0));

  expect(createNotificationRule).toHaveBeenCalledWith({
    name: 'Error rate',
    event: 'provider.error_rate',
    destination_id: installationHook.id,
    configuration: {
      threshold: '0.5',
      window_seconds: 300,
      minimum_samples: 3
    }
  });
});

it('omits empty configuration fields so backend defaults apply', async () => {
  vi.mocked(createNotificationRule).mockResolvedValue({
    ...lapseRule,
    event: 'worker.stale'
  });
  await render();

  choose('#rule-event', 'worker.stale');
  choose('#rule-name', 'Stale workers');
  choose('#rule-destination', installationHook.id);
  host
    .querySelector<HTMLSelectElement>('#rule-event')!
    .form!.dispatchEvent(
      new Event('submit', { bubbles: true, cancelable: true })
    );
  await new Promise((resolve) => setTimeout(resolve, 0));

  expect(createNotificationRule).toHaveBeenCalledWith({
    name: 'Stale workers',
    event: 'worker.stale',
    destination_id: installationHook.id
  });
});

it('creates a project-scoped report.spend rule with period', async () => {
  vi.mocked(createNotificationRule).mockResolvedValue({
    ...lapseRule,
    event: 'report.spend'
  });
  await render();

  choose('#rule-event', 'report.spend');
  choose('#rule-name', 'Weekly spend');
  choose('#rule-project', projectHook.project_id!);
  choose('#rule-period', 'weekly');
  choose('#rule-destination', projectHook.id);
  host
    .querySelector<HTMLSelectElement>('#rule-event')!
    .form!.dispatchEvent(
      new Event('submit', { bubbles: true, cancelable: true })
    );
  await new Promise((resolve) => setTimeout(resolve, 0));

  expect(createNotificationRule).toHaveBeenCalledWith({
    name: 'Weekly spend',
    event: 'report.spend',
    project_id: projectHook.project_id,
    destination_id: projectHook.id,
    configuration: { period: 'weekly' }
  });
});

it.each(['msteams', 'discord'] as const)(
  'creates a %s destination with the same sealed webhook shape',
  async (type) => {
    vi.mocked(listNotificationDestinations).mockResolvedValue([]);
    vi.mocked(createNotificationDestination).mockResolvedValue({
      ...installationHook,
      type,
      url: 'https://chat.example.com'
    });
    await render();

    choose('#dest-type', type);
    choose('#dest-name', `${type} hook`);
    choose('#dest-url', 'https://chat.example.com');
    choose('#dest-webhook-url', 'https://chat.example.com/webhook/secret');
    host
      .querySelector<HTMLSelectElement>('#dest-type')!
      .form!.dispatchEvent(
        new Event('submit', { bubbles: true, cancelable: true })
      );
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(createNotificationDestination).toHaveBeenCalledWith({
      name: `${type} hook`,
      type,
      url: 'https://chat.example.com',
      project_id: null,
      secret: { webhook_url: 'https://chat.example.com/webhook/secret' }
    });
  }
);

it('offers all thirteen events', async () => {
  await render();

  const options = [
    ...host.querySelectorAll<HTMLOptionElement>('#rule-event option')
  ].map((option) => option.value);
  expect(options).toEqual([
    'budget.threshold',
    'budget.exhausted',
    'provider.grant.lapsed',
    'provider.circuit.open',
    'provider.circuit.closed',
    'provider.error_rate',
    'provider.credential.failing',
    'route.latency',
    'model.retirement',
    'runtime.install_failed',
    'worker.stale',
    'key.expiring',
    'report.spend'
  ]);
});

it('edits a rule name, destination and event configuration under its ETag', async () => {
  const errorRule = {
    ...lapseRule,
    event: 'provider.error_rate' as const,
    configuration: { threshold: '0.5', minimum_samples: 3 },
    destination_name: installationHook.name
  };
  vi.mocked(listNotificationRules).mockResolvedValue([errorRule]);
  vi.mocked(updateNotificationRule).mockResolvedValue(errorRule);
  await render();

  const edits = [...host.querySelectorAll<HTMLButtonElement>('button')].filter(
    (button) => button.textContent === 'Edit'
  );
  edits[edits.length - 1]!.click();
  flushSync();

  const nameInput = host.querySelector<HTMLInputElement>(
    'input[aria-label="Rule name"]'
  )!;
  nameInput.value = 'Error rate v2';
  nameInput.dispatchEvent(new Event('input', { bubbles: true }));
  const threshold = host.querySelector<HTMLInputElement>(
    'input[aria-label="Error-rate threshold"]'
  )!;
  threshold.value = '0.2';
  threshold.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();

  const save = [...host.querySelectorAll<HTMLButtonElement>('button')].find(
    (button) => button.textContent === 'Save'
  )!;
  save.click();
  flushSync();
  await new Promise((resolve) => setTimeout(resolve, 0));

  expect(updateNotificationRule).toHaveBeenCalledWith(
    errorRule,
    expect.objectContaining({
      name: 'Error rate v2',
      destination_id: errorRule.destination_id,
      configuration: expect.objectContaining({
        threshold: '0.2',
        minimum_samples: 3
      })
    })
  );
});

it('clears an optional webhook secret while the destination is enabled', async () => {
  await render();

  const edit = [...host.querySelectorAll<HTMLButtonElement>('button')].find(
    (button) => button.textContent === 'Edit'
  )!;
  edit.click();
  flushSync();

  const clear = host.querySelector<HTMLInputElement>('input[type="checkbox"]')!;
  expect(clear).not.toBeNull();
  clear.checked = true;
  clear.dispatchEvent(new Event('change', { bubbles: true }));
  const save = [...host.querySelectorAll<HTMLButtonElement>('button')].find(
    (button) => button.textContent === 'Save'
  )!;
  save.click();
  await new Promise((resolve) => setTimeout(resolve, 0));

  expect(updateNotificationDestination).toHaveBeenCalledWith(
    installationHook,
    expect.objectContaining({ secret: null })
  );
});

it('withholds the clear checkbox on an enabled required-secret destination', async () => {
  const slack = { ...installationHook, type: 'slack' as const };
  vi.mocked(listNotificationDestinations).mockResolvedValue([slack]);
  await render();

  const edit = [...host.querySelectorAll<HTMLButtonElement>('button')].find(
    (button) => button.textContent === 'Edit'
  )!;
  edit.click();
  flushSync();

  expect(host.querySelector('input[type="checkbox"]')).toBeNull();
});

it('edits an email destination configuration keeping the stored secret', async () => {
  const email = {
    ...installationHook,
    type: 'email' as const,
    url: 'smtps://smtp.example.com:465',
    configuration: {
      from: 'alerts@example.com',
      to: ['ops@example.com'],
      subject_prefix: '[OLP]'
    }
  };
  vi.mocked(listNotificationDestinations).mockResolvedValue([email]);
  await render();

  const edit = [...host.querySelectorAll<HTMLButtonElement>('button')].find(
    (button) => button.textContent === 'Edit'
  )!;
  edit.click();
  flushSync();

  const to = host.querySelector<HTMLInputElement>(
    'input[aria-label="To addresses"]'
  )!;
  to.value = 'ops@example.com, dev@example.com';
  to.dispatchEvent(new Event('input', { bubbles: true }));
  const save = [...host.querySelectorAll<HTMLButtonElement>('button')].find(
    (button) => button.textContent === 'Save'
  )!;
  save.click();
  await new Promise((resolve) => setTimeout(resolve, 0));

  expect(updateNotificationDestination).toHaveBeenCalledWith(
    email,
    expect.objectContaining({
      configuration: {
        from: 'alerts@example.com',
        to: ['ops@example.com', 'dev@example.com'],
        subject_prefix: '[OLP]'
      }
    })
  );
  expect(
    vi.mocked(updateNotificationDestination).mock.calls[0]?.[1]
  ).not.toHaveProperty('secret');
});

it('sends a route.latency rule with the ttft metric', async () => {
  vi.mocked(createNotificationRule).mockResolvedValue({
    ...lapseRule,
    event: 'route.latency'
  });
  await render();

  choose('#rule-event', 'route.latency');
  choose('#rule-name', 'Latency');
  choose('#rule-destination', installationHook.id);
  choose('#rule-metric', 'ttft');
  choose('#rule-config-threshold', '500');
  host
    .querySelector<HTMLSelectElement>('#rule-event')!
    .form!.dispatchEvent(
      new Event('submit', { bubbles: true, cancelable: true })
    );
  await new Promise((resolve) => setTimeout(resolve, 0));

  expect(createNotificationRule).toHaveBeenCalledWith({
    name: 'Latency',
    event: 'route.latency',
    project_id: null,
    destination_id: installationHook.id,
    configuration: { metric: 'ttft', threshold: '500' }
  });
});

it('never sends stale_after_seconds for worker.stale rules', async () => {
  vi.mocked(createNotificationRule).mockResolvedValue({
    ...lapseRule,
    event: 'worker.stale'
  });
  await render();

  choose('#rule-event', 'worker.stale');
  choose('#rule-name', 'Stale');
  choose('#rule-destination', installationHook.id);
  choose('#rule-config-cooldown_seconds', '120');
  host
    .querySelector<HTMLSelectElement>('#rule-event')!
    .form!.dispatchEvent(
      new Event('submit', { bubbles: true, cancelable: true })
    );
  await new Promise((resolve) => setTimeout(resolve, 0));

  expect(createNotificationRule).toHaveBeenCalledWith({
    name: 'Stale',
    event: 'worker.stale',
    destination_id: installationHook.id,
    configuration: { cooldown_seconds: 120 }
  });
});
