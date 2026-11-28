import { describe, expect, it, onTestFinished, vi } from 'vitest';
import { mount, unmount } from 'svelte';
import { json, route, serve } from '#lib/session/test/transport.js';
import { authLifecycle } from '#lib/session/lifecycle.js';
import { eventually } from '#test/eventually.js';
import {
  findButton,
  findControl,
  mountWithState,
  reportedError,
  setFieldValue,
  submitForm
} from '#test/mount.js';
import NotificationDeliveries from './NotificationDeliveries.svelte';
import NotificationDestinationTest from './NotificationDestinationTest.svelte';
import NotificationsPanel from './NotificationsPanel.svelte';
import RuleSilenceEditor from './RuleSilenceEditor.svelte';
import type {
  NotificationDelivery,
  NotificationDestination,
  NotificationRule
} from './api';

const destination: NotificationDestination = {
  id: 'destination-id',
  name: 'Alerts',
  url: 'https://example.test/alerts',
  type: 'webhook',
  configuration: {},
  secret_configured: false,
  enabled: true,
  etag: '018f1234-5678-7abc-8def-0123456789ab',
  project_id: null,
  project_name: null,
  created_at: '2020-01-01T00:00:00Z',
  updated_at: '2020-01-01T00:00:00Z',
  created_by: 'user-id',
  created_by_email: 'user@example.test'
};
const rule: NotificationRule = {
  id: 'rule-id',
  name: 'Monitor',
  event: 'worker.stale',
  destination_id: destination.id,
  destination_name: destination.name,
  configuration: {},
  enabled: true,
  etag: '018f1234-5678-7abc-8def-0123456789ac',
  project_id: null,
  project_name: null,
  silence: null,
  subject_id: null,
  subject_kind: null,
  subject_name: null,
  threshold_percent: null,
  window_kind: null,
  created_at: destination.created_at,
  updated_at: destination.updated_at,
  created_by: destination.created_by,
  created_by_email: destination.created_by_email
};
const delivery: NotificationDelivery = {
  id: 'delivery-id',
  rule_id: rule.id,
  rule_name: rule.name,
  event: rule.event,
  status: 'pending',
  attempts: 0,
  created_at: rule.created_at,
  accrued: null,
  credential_version: null,
  credential_version_id: null,
  currency: null,
  delivered_at: null,
  last_attempt_at: null,
  last_error_code: null,
  limit: null,
  project_id: null,
  provider_id: null,
  provider_name: null,
  threshold_percent: null,
  window_id: null
};

function renderDeliveries() {
  const { target } = mountWithState(NotificationDeliveries, undefined, {
    configureClient: (client) =>
      client.setDefaultOptions({ queries: { retry: false } })
  });
  return {
    target,
    button: (label: string) => findButton(target, label)
  };
}

describe('saved destination test', () => {
  it('checks and retries an unanswered action without starting a new test', async () => {
    const send = vi
      .fn<(path: string) => Response>()
      .mockImplementationOnce(() => json({ status: 504 }, 504))
      .mockImplementationOnce(() =>
        json({ id: 'test-id', status: 'unconfirmed', failure: 'timeout' })
      )
      .mockImplementationOnce(() =>
        json({ id: 'new-test-id', status: 'delivered' })
      );
    const read = vi.fn((path: string) => {
      expect(path).toBe(send.mock.calls[0][0]);
      return json({ status: 404 }, 404);
    });
    serve((request) => {
      const { pathname } = new URL(request.url);
      expect(pathname).toMatch(
        /^\/api\/v1\/notifications\/destinations\/destination-id\/tests\/[a-f0-9-]+$/
      );
      return request.method === 'POST' ? send(pathname) : read(pathname);
    });
    const target = document.createElement('div');
    document.body.append(target);
    const component = mount(NotificationDestinationTest, {
      target,
      props: { destination }
    });
    onTestFinished(async () => {
      await unmount(component);
      target.remove();
    });
    const click = async (label: string, inDialog = true) => {
      const button = await eventually(() => {
        const scope = inDialog ? target.querySelector('dialog') : target;
        const found = scope ? findButton(scope, label) : undefined;
        expect(found?.disabled).toBe(false);
        return found;
      });
      button?.click();
      return button;
    };

    await click('Send test', false);
    expect(send).not.toHaveBeenCalled();
    const confirm = await click('Send test');
    confirm?.click();
    await eventually(() =>
      expect(target.textContent).toContain('Result pending.')
    );
    await click('Refresh status');
    await click('Retry same test');
    await eventually(() =>
      expect(target.textContent).toContain(
        'Delivery unconfirmed. The receiver may have accepted the test.'
      )
    );
    expect(send).toHaveBeenCalledTimes(2);
    expect(send.mock.calls[1]).toEqual(send.mock.calls[0]);

    await click('Start a new test');
    expect(send).toHaveBeenCalledTimes(2);
    await click('Send test');
    await eventually(() =>
      expect(target.textContent).toContain('Test delivered.')
    );
    expect(send).toHaveBeenCalledTimes(3);
    expect(send.mock.calls[2]).not.toEqual(send.mock.calls[0]);
  });
});

describe('rule silence', () => {
  it('reports a bad window on its inputs, whether the console or the API refuses it', async () => {
    const writes = vi.fn<(request: Request) => void>();
    serve(
      (request) => {
        writes(request);
        const message = 'End must be after the start and in the future.';
        return json(
          {
            title: 'Validation failed',
            status: 422,
            detail: message,
            errors: { silence: [{ code: 'validation_failed', message }] }
          },
          422
        );
      },
      ['settings']
    );
    const { target } = mountWithState(
      RuleSilenceEditor,
      { rule, disabled: false, saved: vi.fn() },
      { attach: true }
    );
    const start = findControl(target, 'Start (UTC)');
    const end = findControl(target, 'End (UTC)');

    submitForm(target);
    expect(reportedError(start)).toBe('Enter a start time.');
    expect(reportedError(end)).toBe('Enter an end time.');

    setFieldValue(start, '2001-01-02T10:00');
    setFieldValue(end, '2001-01-02T09:00');
    submitForm(target);
    expect(reportedError(start)).toBeUndefined();
    expect(reportedError(end)).toBe('Enter an end time after the start time.');
    expect(writes).not.toHaveBeenCalled();

    setFieldValue(end, '2001-01-02T11:00');
    submitForm(target);
    await eventually(() =>
      expect(reportedError(end)).toBe(
        'End must be after the start and in the future.'
      )
    );
    await eventually(() => expect(document.activeElement).toBe(end));
    expect(target.querySelector('[role="alert"]')).toBeNull();
    expect(await writes.mock.calls[0][0].json()).toEqual({
      silence: {
        starts_at: '2001-01-02T10:00:00.000Z',
        ends_at: '2001-01-02T11:00:00.000Z'
      }
    });
  });
});

describe('notification delivery history', () => {
  it('shows one page at a time even when the history exceeds 10,000 deliveries', async () => {
    const total = 10_001;
    const requestPage = vi.fn((request: Request) => {
      route(request, 'GET', '/api/v1/notifications/deliveries');
      const query = new URL(request.url).searchParams;
      const cursor = query.get('cursor');
      const offset = cursor ? Number(cursor.replace('after:', '')) : 0;
      const length = Math.min(Number(query.get('limit')), total - offset);
      const items = Array.from({ length }, (_, index) => {
        const number = total - offset - index;
        return {
          ...delivery,
          id: `id-${number}`,
          rule_name: `Delivery ${number}`
        };
      });
      return json({
        items,
        next_cursor: offset + length < total ? `after:${offset + length}` : null
      });
    });
    serve(requestPage);
    const { target, button } = renderDeliveries();
    const names = () =>
      [...target.querySelectorAll('tbody td:first-child')].map(
        (cell) => cell.textContent
      );

    await eventually(() => expect(names()[0]).toBe('Delivery 10001'));
    expect(requestPage).toHaveBeenCalledTimes(1);
    expect(names()).toHaveLength(50);
    expect(button('Previous')?.disabled).toBe(true);
    expect(button('Next')?.disabled).toBe(false);

    button('Next')?.click();
    await eventually(() => expect(names()[0]).toBe('Delivery 9951'));
    expect(requestPage).toHaveBeenCalledTimes(2);
    expect(names()).toHaveLength(50);
    expect(target.textContent).toContain('Page 2');
    expect(button('Previous')?.disabled).toBe(false);

    button('Previous')?.click();
    await eventually(() => expect(names()[0]).toBe('Delivery 10001'));
    expect(target.textContent).toContain('Page 1');
  });

  it('can retry a failed page and go back from the last page', async () => {
    const olderPage = vi
      .fn(() => json({ items: [{ ...delivery, rule_name: 'Older delivery' }] }))
      .mockReturnValueOnce(json({ title: 'Unavailable', status: 503 }, 503));
    serve((request) =>
      new URL(request.url).searchParams.get('cursor') === 'older-page'
        ? olderPage()
        : json({ items: [delivery], next_cursor: 'older-page' })
    );
    const { target, button } = renderDeliveries();

    await eventually(() => expect(button('Next')?.disabled).toBe(false));
    button('Next')?.click();
    await eventually(() =>
      expect(target.querySelector('[role="alert"]')?.textContent).toContain(
        'Deliveries are unavailable.'
      )
    );
    expect(button('Previous')?.disabled).toBe(false);
    expect(button('Next')?.disabled).toBe(true);

    button('Retry')?.click();
    await eventually(() =>
      expect(target.textContent).toContain('Older delivery')
    );
    expect(button('Next')?.disabled).toBe(true);
    expect(button('Previous')?.disabled).toBe(false);
    button('Previous')?.click();
    await eventually(() =>
      expect(target.textContent).toContain(delivery.rule_name)
    );
    expect(target.textContent).not.toContain('Older delivery');
  });
});

describe.each([
  { view: 'destinations', deletion: 'Delete destination' },
  { view: 'rules', deletion: 'Delete rule' }
] as const)('notification $view actions', ({ view, deletion }) => {
  it.each(['assigned', 'global'] as const)(
    'honors %s access',
    async (scope) => {
      const installation = scope === 'global';
      const projectDestination: NotificationDestination = {
        ...destination,
        id: 'project-destination',
        name: 'Project alerts',
        project_id: 'project-id',
        project_name: 'Project'
      };
      const projectRule: NotificationRule = {
        ...rule,
        id: 'project-rule',
        name: 'Project monitor',
        event: 'key.expiring',
        destination_id: projectDestination.id,
        destination_name: projectDestination.name,
        project_id: projectDestination.project_id,
        project_name: projectDestination.project_name
      };
      const toggle = Promise.withResolvers<Response>();
      authLifecycle.establishSession({
        user: {
          id: 'user-id',
          email: 'user@example.test',
          display_name: 'User',
          role: installation ? 'owner' : 'developer',
          access_scope: scope,
          operations: installation
            ? ['read', 'keys', 'settings']
            : ['read', 'keys']
        },
        csrf_token: 'fixture-csrf'
      });
      vi.stubGlobal('fetch', (request: Request) => {
        if (request.method === 'PATCH') return toggle.promise;
        switch (new URL(request.url).pathname) {
          case '/api/v1/auth/capabilities':
            return json({ notifications_active: true });
          case '/api/v1/notifications/destinations':
            return json({ items: [destination, projectDestination] });
          case '/api/v1/notifications/rules':
            return json({ items: [rule, projectRule] });
          default:
            return json({ items: [] });
        }
      });
      const { target } = mountWithState(
        NotificationsPanel,
        { view },
        { attach: true }
      );
      const rowButtons = (name: string) => {
        const row = [...target.querySelectorAll('tr')].find((row) =>
          row.textContent?.includes(name)
        );
        expect(row).toBeDefined();
        return [...(row?.querySelectorAll('button') ?? [])];
      };
      const rowActions = (name: string) =>
        rowButtons(name).map((button) => button.textContent?.trim());
      const actions = ['Edit', 'Disable', deletion];
      const [globalRow, projectRow] =
        view === 'destinations'
          ? [destination, projectDestination]
          : [rule, projectRule];

      await eventually(() => {
        expect(rowActions(projectRow.name)).toEqual(
          expect.arrayContaining(actions)
        );
        const globalActions = rowActions(globalRow.name);
        expect(
          actions.filter((action) => globalActions.includes(action))
        ).toEqual(installation ? actions : []);
      });
      if (installation) {
        const disable = (name: string) =>
          rowButtons(name).find(
            (button) => button.textContent?.trim() === 'Disable'
          );
        disable(globalRow.name)?.click();
        await eventually(() =>
          expect(disable(projectRow.name)?.disabled).toBe(true)
        );
        toggle.resolve(json({ ...globalRow, enabled: false }));
        await eventually(() =>
          expect(disable(projectRow.name)?.disabled).toBe(false)
        );
      }
    }
  );
});
