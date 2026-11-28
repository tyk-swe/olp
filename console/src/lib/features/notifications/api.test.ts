import { describe, expect, it, vi } from 'vitest';
import { json, route, serve } from '#lib/session/test/transport.js';
import {
  createDestinationInput,
  editDestinationDraft,
  newDestinationDraft,
  parseDestinationSecret,
  updateDestinationInput
} from './destinationDraft';
import {
  createRuleInput,
  newRuleConfiguration,
  newRuleDraft,
  parseRuleConfiguration
} from './ruleDraft';
import {
  createNotificationDestination,
  createNotificationRule,
  listNotificationDestinations,
  listNotificationRules,
  updateNotificationDestination,
  updateNotificationRule,
  type NotificationDestination,
  type NotificationRule
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

describe('notification API', () => {
  it.each([
    {
      path: 'destinations',
      item: destination,
      list: () => listNotificationDestinations()
    },
    { path: 'rules', item: rule, list: () => listNotificationRules() }
  ])('collects every page of $path', async ({ path, item, list }) => {
    const cursors = vi.fn<(cursor: string | null) => void>();
    serve(async (request) => {
      route(request, 'GET', `/api/v1/notifications/${path}`);
      const url = new URL(request.url);
      const cursor = url.searchParams.get('cursor');
      cursors(cursor);
      expect(url.searchParams.get('limit')).toBe('50');
      return json(
        cursor
          ? { items: [{ ...item, id: 'second' }] }
          : { items: [item], next_cursor: 'next' }
      );
    });

    expect(await list()).toEqual([item, { ...item, id: 'second' }]);
    expect(cursors.mock.calls).toEqual([[null], ['next']]);
  });

  it('creates a destination and rule with their idempotency keys', async () => {
    const destinationInput = { name: destination.name, url: destination.url };
    const ruleInput = {
      name: rule.name,
      event: rule.event,
      destination_id: destination.id
    };
    serve(async (request) => {
      const path = new URL(request.url).pathname;
      route(request, 'POST', path);
      if (path.endsWith('/destinations')) {
        expect(request.headers.get('Idempotency-Key')).toBe(
          'destination-request'
        );
        expect(await request.json()).toEqual(destinationInput);
        return json(destination);
      }
      expect(path).toBe('/api/v1/notifications/rules');
      expect(request.headers.get('Idempotency-Key')).toBe('rule-request');
      expect(await request.json()).toEqual(ruleInput);
      return json(rule);
    });

    expect(
      await createNotificationDestination(
        destinationInput,
        'destination-request'
      )
    ).toEqual(destination);
    expect(await createNotificationRule(ruleInput, 'rule-request')).toEqual(
      rule
    );
  });

  it.each([
    {
      path: `/api/v1/notifications/destinations/${destination.id}`,
      etag: destination.etag,
      updated: { ...destination, enabled: false },
      update: () =>
        updateNotificationDestination(destination, { enabled: false })
    },
    {
      path: `/api/v1/notifications/rules/${rule.id}`,
      etag: rule.etag,
      updated: { ...rule, enabled: false },
      update: () => updateNotificationRule(rule, { enabled: false })
    }
  ])(
    'updates $path with the quoted JSON revision',
    async ({ path, etag, updated, update }) => {
      serve(async (request) => {
        route(request, 'PATCH', path);
        expect(request.headers.get('If-Match')).toBe(`"${etag}"`);
        expect(await request.json()).toEqual({ enabled: false });
        return json(updated);
      });

      expect(await update()).toEqual(updated);
    }
  );
});

describe('notification draft requests', () => {
  it('sends email configuration and replaces only explicitly supplied credentials', async () => {
    const email = {
      ...destination,
      type: 'email' as const,
      configuration: {
        from: ' sender@example.test ',
        to: ['first@example.test', 'second@example.test'],
        subject_prefix: ' alerts '
      }
    };
    const draft = editDestinationDraft(email);
    const kept = parseDestinationSecret(email.type, '', false);
    expect(kept.kind).toBe('valid');
    if (kept.kind !== 'valid') return;
    const requests: unknown[] = [];
    serve(async (request) => {
      requests.push(await request.json());
      return json(email);
    });
    await updateNotificationDestination(
      email,
      updateDestinationInput(draft, kept.change)
    );
    const replacement = parseDestinationSecret(
      email.type,
      '{"username":"mail-user","password":"secret"}',
      false
    );
    expect(replacement.kind).toBe('valid');
    if (replacement.kind !== 'valid') return;
    await updateNotificationDestination(
      email,
      updateDestinationInput(draft, replacement.change)
    );
    expect(requests).toEqual([
      {
        name: destination.name,
        url: destination.url,
        configuration: {
          from: 'sender@example.test',
          to: ['first@example.test', 'second@example.test'],
          subject_prefix: 'alerts'
        }
      },
      {
        name: destination.name,
        url: destination.url,
        configuration: {
          from: 'sender@example.test',
          to: ['first@example.test', 'second@example.test'],
          subject_prefix: 'alerts'
        },
        secret: { username: 'mail-user', password: 'secret' }
      }
    ]);
    for (const raw of ['{}', 'null']) {
      const change = parseDestinationSecret(email.type, raw, false);
      expect(change.kind).toBe('valid');
      if (change.kind !== 'valid') continue;
      expect(updateDestinationInput(draft, change.change).secret).toEqual(
        JSON.parse(raw)
      );
    }
    expect(
      parseDestinationSecret(email.type, '{"username":7}', false).kind
    ).toBe('invalid');
  });

  it('keeps transport-specific credentials out of unrelated destination fields', async () => {
    const input = createDestinationInput({
      type: 'slack',
      name: ' Chat ',
      url: ' https://fallback.example.test ',
      projectId: '',
      webhookUrl: ' https://chat.example.test/secret '
    });
    serve(async (request) => {
      expect(await request.json()).toEqual({
        type: 'slack',
        name: 'Chat',
        url: 'https://fallback.example.test',
        project_id: null,
        secret: { webhook_url: 'https://chat.example.test/secret' }
      });
      return json({ ...destination, type: 'slack' });
    });
    await createNotificationDestination(input, 'chat-create');
  });

  it('preserves decimal rule thresholds and sends integer sampling windows', async () => {
    const parsed = parseRuleConfiguration({
      ...newRuleConfiguration('provider.error_rate'),
      values: {
        threshold: '0.0500',
        recovery_threshold: '0.0100',
        window_seconds: '120',
        minimum_samples: '5'
      }
    });
    expect(parsed.ok).toBe(true);
    if (!parsed.ok) return;
    serve(async (request) => {
      const body = await request.json();
      expect(body.configuration).toEqual({
        threshold: '0.0500',
        recovery_threshold: '0.0100',
        window_seconds: 120,
        minimum_samples: 5
      });
      return json(rule);
    });
    await createNotificationRule(
      {
        name: 'Error rate',
        event: 'provider.error_rate',
        destination_id: destination.id,
        configuration: parsed.value
      },
      'rate-create'
    );
    expect(
      parseRuleConfiguration({
        ...newRuleConfiguration('provider.error_rate'),
        values: { window_seconds: '1.5' }
      })
    ).toEqual({
      ok: false,
      fieldErrors: { window_seconds: 'Window seconds must be a whole number.' }
    });
  });
});

it('writes only the subject and scope supported by the selected rule event', async () => {
  const key = newRuleDraft('key.expiring', {
    name: 'Key expiry',
    destinationId: destination.id,
    projectId: 'project-id'
  });
  expect(key.event).toBe('key.expiring');
  if (key.event !== 'key.expiring') return;
  const selected = createRuleInput({ ...key, subjectId: 'key-id' });
  expect(selected.ok).toBe(true);
  if (!selected.ok) return;
  const requests: unknown[] = [];
  serve(async (request) => {
    requests.push(await request.json());
    return json(rule);
  });
  await createNotificationRule(selected.value, 'key-expiry');
  const installation = createRuleInput(
    newRuleDraft('provider.grant.lapsed', key)
  );
  expect(installation.ok).toBe(true);
  if (!installation.ok) return;
  await createNotificationRule(installation.value, 'provider-grant');
  expect(requests).toEqual([
    {
      name: 'Key expiry',
      event: 'key.expiring',
      destination_id: destination.id,
      project_id: 'project-id',
      subject_kind: 'api_key',
      subject_id: 'key-id'
    },
    {
      name: 'Key expiry',
      event: 'provider.grant.lapsed',
      destination_id: destination.id
    }
  ]);
});

it('requires a project and preserves spike thresholds', () => {
  const draft = newRuleDraft('spend.spike', {
    name: 'Spend spike',
    destinationId: destination.id,
    projectId: ''
  });
  expect(createRuleInput(draft)).toEqual({
    ok: false,
    fieldErrors: {},
    formError: 'Choose a project.'
  });
  const scoped = createRuleInput({
    ...draft,
    projectId: 'project-id',
    values: {
      baseline_days: '21',
      minimum_history_days: '10',
      increase_percent: '75.50',
      minimum_increase: '25.00'
    }
  });
  expect(scoped.ok).toBe(true);
  if (!scoped.ok) return;
  expect(scoped.value).toEqual({
    name: 'Spend spike',
    event: 'spend.spike',
    destination_id: destination.id,
    project_id: 'project-id',
    configuration: {
      baseline_days: 21,
      minimum_history_days: 10,
      increase_percent: '75.50',
      minimum_increase: '25.00'
    }
  });
});

it('clears channel credentials when the destination type changes', () => {
  const email = {
    type: 'email' as const,
    name: 'Mail',
    url: 'smtp+starttls://mail.example.test:587',
    projectId: '',
    username: 'mail-user',
    password: 'mail-password',
    from: 'sender@example.test',
    to: 'recipient@example.test',
    subjectPrefix: '',
    caCertificate: ''
  };
  const switched = newDestinationDraft('webhook', email);
  expect(JSON.stringify(switched)).not.toContain(email.password);
  expect(createDestinationInput(switched).secret).toBeNull();
});
