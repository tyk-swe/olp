import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { pageResult, result, type CursorPage } from '$lib/api/http';
import { collectCursorPages } from '$lib/api/pagination';

type Schemas = components['schemas'];

export type NotificationDestination = Schemas['NotificationDestination'];
export type CreateNotificationDestinationInput =
  Schemas['CreateNotificationDestinationRequest'];
export type UpdateNotificationDestinationInput =
  Schemas['UpdateNotificationDestinationRequest'];
export type BudgetAlertRule = Schemas['BudgetAlertRule'];
export type CreateBudgetAlertRuleInput =
  Schemas['CreateBudgetAlertRuleRequest'];
export type UpdateBudgetAlertRuleInput =
  Schemas['UpdateBudgetAlertRuleRequest'];
export type BudgetAlertDelivery = Schemas['BudgetAlertDelivery'];

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
  return pageResult(result(response.data, response.error, response.response));
}

export async function createNotificationDestination(
  input: CreateNotificationDestinationInput
): Promise<NotificationDestination> {
  const response = await apiClient.POST('/api/v1/notifications/destinations', {
    params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
    body: input
  });
  return result(response.data, response.error, response.response);
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
        header: { 'If-Match': destination.etag }
      },
      body: input
    }
  );
  return result(response.data, response.error, response.response);
}

export async function listBudgetAlertRules(
  signal?: AbortSignal
): Promise<BudgetAlertRule[]> {
  return collectCursorPages((cursor) =>
    listBudgetAlertRulePage(cursor, signal)
  );
}

export async function listBudgetAlertRulePage(
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<BudgetAlertRule>> {
  const response = await apiClient.GET('/api/v1/notifications/rules', {
    params: { query: { limit: 50, cursor } },
    signal
  });
  return pageResult(result(response.data, response.error, response.response));
}

export async function createBudgetAlertRule(
  input: CreateBudgetAlertRuleInput
): Promise<BudgetAlertRule> {
  const response = await apiClient.POST('/api/v1/notifications/rules', {
    params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
    body: input
  });
  return result(response.data, response.error, response.response);
}

export async function updateBudgetAlertRule(
  rule: BudgetAlertRule,
  input: UpdateBudgetAlertRuleInput
): Promise<BudgetAlertRule> {
  const response = await apiClient.PATCH(
    '/api/v1/notifications/rules/{budget_alert_rule_id}',
    {
      params: {
        path: { budget_alert_rule_id: rule.id },
        header: { 'If-Match': rule.etag }
      },
      body: input
    }
  );
  return result(response.data, response.error, response.response);
}

export async function listNotificationDeliveries(
  ruleId?: string,
  signal?: AbortSignal
): Promise<BudgetAlertDelivery[]> {
  return collectCursorPages(async (cursor) => {
    const response = await apiClient.GET('/api/v1/notifications/deliveries', {
      params: { query: { limit: 50, cursor, rule_id: ruleId } },
      signal
    });
    return pageResult(result(response.data, response.error, response.response));
  });
}
