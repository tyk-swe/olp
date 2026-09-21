import { afterEach, describe, expect, it, vi } from 'vitest';
import { authLifecycle } from '$lib/features/access/session/lifecycle';
import { clearCsrfToken } from '$lib/features/access/session/api';
import { captureRequests, jsonResponse } from '$lib/api/test/requestCapture';
import {
  createBudgetAlertRule,
  createNotificationDestination,
  listNotificationDeliveries,
  updateNotificationDestination,
  type NotificationDestination
} from '$lib/features/access/notifications/api';

const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

const session = {
  user: {
    id: '01980000-0000-7000-8000-000000000401',
    email: 'operator@example.com',
    display_name: 'Operator',
    role: 'operator' as const,
    access_scope: 'global' as const
  },
  csrf_token: 'csrf-notification-token'
};

const destination = {
  id: '01980000-0000-7000-8000-000000000601',
  etag: '01980000-0000-7000-8000-000000000602'
} as NotificationDestination;

afterEach(async () => {
  await authLifecycle.principalInvalidated();
  clearCsrfToken();
  vi.unstubAllGlobals();
});

describe('notification destinations', () => {
  it('creates a destination with an idempotency key and write-only secret', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests(() =>
      jsonResponse({ ...destination, name: 'hook', url: 'https://h.example' })
    );

    await createNotificationDestination({
      name: 'hook',
      url: 'https://h.example',
      secret: 'signing-key'
    });

    const request = requests[0]!;
    expect(request.method).toBe('POST');
    expect(new URL(request.url).pathname).toBe(
      '/api/v3/notifications/destinations'
    );
    expect(request.headers.get('idempotency-key')).toMatch(uuid);
    expect(await request.json()).toEqual({
      name: 'hook',
      url: 'https://h.example',
      secret: 'signing-key'
    });
  });

  it('patches a destination under its ETag', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests(() =>
      jsonResponse({ ...destination, enabled: false })
    );

    await updateNotificationDestination(destination, { enabled: false });

    const request = requests[0]!;
    expect(request.method).toBe('PATCH');
    expect(new URL(request.url).pathname).toBe(
      `/api/v3/notifications/destinations/${destination.id}`
    );
    expect(request.headers.get('if-match')).toBe(`"${destination.etag}"`);
  });
});

describe('budget alert rules', () => {
  it('creates a rule with subject, window, and threshold', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests(() =>
      jsonResponse({ id: '01980000-0000-7000-8000-000000000701' })
    );

    await createBudgetAlertRule({
      name: 'monthly 80',
      subject_kind: 'api_key',
      subject_id: '01980000-0000-7000-8000-000000000801',
      window_kind: 'month',
      threshold_percent: 80,
      destination_id: destination.id
    });

    const request = requests[0]!;
    expect(request.method).toBe('POST');
    expect(new URL(request.url).pathname).toBe('/api/v3/notifications/rules');
    expect(request.headers.get('idempotency-key')).toMatch(uuid);
    expect(await request.json()).toMatchObject({
      subject_kind: 'api_key',
      window_kind: 'month',
      threshold_percent: 80
    });
  });
});

describe('notification deliveries', () => {
  it('lists metadata filtered by rule', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests(() =>
      jsonResponse({ items: [], next_cursor: null })
    );

    await listNotificationDeliveries(destination.id);

    const url = new URL(requests[0]!.url);
    expect(url.pathname).toBe('/api/v3/notifications/deliveries');
    expect(url.searchParams.get('rule_id')).toBe(destination.id);
  });
});
