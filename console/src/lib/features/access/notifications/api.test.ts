import { describe, expect, it, vi } from 'vitest';
import { json, route, serve } from '$lib/api/test/transport';
import {
  createNotificationDestination,
  createNotificationRule,
  listNotificationDeliveries,
  listNotificationDestinations,
  listNotificationRules,
  updateNotificationDestination,
  updateNotificationRule,
  type NotificationDelivery,
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

describe('notification API', () => {
  it.each([
    {
      path: 'destinations',
      item: destination,
      list: () => listNotificationDestinations()
    },
    { path: 'rules', item: rule, list: () => listNotificationRules() },
    {
      path: 'deliveries',
      item: delivery,
      list: () => listNotificationDeliveries(rule.id)
    }
  ])('collects every page of $path', async ({ path, item, list }) => {
    const cursors = vi.fn<(cursor: string | null) => void>();
    serve(async (request) => {
      route(request, 'GET', `/api/v1/notifications/${path}`);
      const url = new URL(request.url);
      const cursor = url.searchParams.get('cursor');
      cursors(cursor);
      expect(url.searchParams.get('limit')).toBe('50');
      if (path === 'deliveries')
        expect(url.searchParams.get('rule_id')).toBe(rule.id);
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
