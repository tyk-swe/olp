import type { components } from '#lib/management/schema.js';
import { apiClient } from '#lib/session/client.js';
import {
  type CursorPage,
  ensureOk,
  requestOutcome,
  unwrap,
  unwrapOutcome,
  unwrapPage,
  createHeaders,
  ifMatch,
  mutationHeaders
} from '#lib/management/http.js';
import { collectCursorPages } from '#lib/management/pagination.js';

type Schemas = components['schemas'];

export type NotificationDestination = Schemas['NotificationDestination'];
export type CreateNotificationDestinationInput =
  Schemas['CreateNotificationDestinationRequest'];
export type UpdateNotificationDestinationInput =
  Schemas['UpdateNotificationDestinationRequest'];
export type NotificationEvent = Schemas['NotificationEvent'];
export type NotificationRule = Schemas['NotificationRule'];
export type CreateNotificationRuleInput =
  Schemas['CreateNotificationRuleRequest'];
export type UpdateNotificationRuleInput =
  Schemas['UpdateNotificationRuleRequest'];
export type NotificationDelivery = Schemas['NotificationDelivery'];
export type DestinationTestView =
  | Schemas['NotificationDestinationTest']
  | { status: 'pending' | 'missing'; failure?: never };

const testPath =
  '/api/v1/notifications/destinations/{notification_destination_id}/tests/{action_id}';

export async function sendDestinationTest(
  destinationId: string,
  actionId: string
): Promise<DestinationTestView> {
  const outcome = await requestOutcome(() =>
    apiClient.POST(testPath, {
      params: {
        path: {
          notification_destination_id: destinationId,
          action_id: actionId
        }
      },
      signal: AbortSignal.timeout(3000)
    })
  );
  if (
    outcome.kind === 'unavailable' ||
    outcome.kind === 'canceled' ||
    outcome.kind === 'invalid-response' ||
    (outcome.kind === 'failure' && outcome.problem.problem.status >= 500)
  ) {
    return { status: 'pending' };
  }
  return unwrapOutcome(outcome);
}

export async function readDestinationTest(
  destinationId: string,
  actionId: string
): Promise<DestinationTestView> {
  const response = await apiClient.GET(testPath, {
    params: {
      path: { notification_destination_id: destinationId, action_id: actionId }
    },
    signal: AbortSignal.timeout(5000)
  });
  return response.response.status === 404
    ? { status: 'missing' }
    : unwrap(response);
}

export async function listNotificationDestinations(
  signal?: AbortSignal
): Promise<NotificationDestination[]> {
  return collectCursorPages((cursor) =>
    listNotificationDestinationPage(cursor, signal)
  );
}

export async function listNotificationDestinationPage(
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<NotificationDestination>> {
  const response = await apiClient.GET('/api/v1/notifications/destinations', {
    params: { query: { limit: 50, cursor } },
    signal
  });
  return unwrapPage(response);
}

export async function createNotificationDestination(
  input: CreateNotificationDestinationInput,
  requestId: string
): Promise<NotificationDestination> {
  const response = await apiClient.POST('/api/v1/notifications/destinations', {
    params: { header: createHeaders(requestId) },
    body: input
  });
  return unwrap(response);
}

export async function updateNotificationDestination(
  destination: NotificationDestination,
  input: UpdateNotificationDestinationInput
): Promise<NotificationDestination> {
  const response = await apiClient.PATCH(
    '/api/v1/notifications/destinations/{notification_destination_id}',
    {
      params: {
        path: { notification_destination_id: destination.id },
        header: ifMatch(destination.etag)
      },
      body: input
    }
  );
  return unwrap(response);
}

export async function listNotificationRules(
  signal?: AbortSignal
): Promise<NotificationRule[]> {
  return collectCursorPages((cursor) =>
    listNotificationRulePage(cursor, signal)
  );
}

export async function listNotificationRulePage(
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<NotificationRule>> {
  const response = await apiClient.GET('/api/v1/notifications/rules', {
    params: { query: { limit: 50, cursor } },
    signal
  });
  return unwrapPage(response);
}

export async function createNotificationRule(
  input: CreateNotificationRuleInput,
  requestId: string
): Promise<NotificationRule> {
  const response = await apiClient.POST('/api/v1/notifications/rules', {
    params: { header: createHeaders(requestId) },
    body: input
  });
  return unwrap(response);
}

export async function updateNotificationRule(
  rule: NotificationRule,
  input: UpdateNotificationRuleInput,
  requestId?: string
): Promise<NotificationRule> {
  const response = await apiClient.PATCH(
    '/api/v1/notifications/rules/{notification_rule_id}',
    {
      params: {
        path: { notification_rule_id: rule.id },
        header: requestId
          ? mutationHeaders(rule.etag, requestId)
          : ifMatch(rule.etag)
      },
      body: input
    }
  );
  return unwrap(response);
}

export async function listNotificationDeliveryPage(
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<NotificationDelivery>> {
  const response = await apiClient.GET('/api/v1/notifications/deliveries', {
    params: { query: { limit: 50, cursor } },
    signal
  });
  return unwrapPage(response);
}

export const deleteNotificationDestination = async (
  value: { id: string; etag: string },
  requestId: string
) =>
  ensureOk(
    await apiClient.DELETE(
      '/api/v1/notifications/destinations/{notification_destination_id}',
      {
        params: {
          path: { notification_destination_id: value.id },
          header: mutationHeaders(value.etag, requestId)
        }
      }
    )
  );

export const deleteNotificationRule = async (
  value: { id: string; etag: string },
  requestId: string
) =>
  ensureOk(
    await apiClient.DELETE(
      '/api/v1/notifications/rules/{notification_rule_id}',
      {
        params: {
          path: { notification_rule_id: value.id },
          header: mutationHeaders(value.etag, requestId)
        }
      }
    )
  );
