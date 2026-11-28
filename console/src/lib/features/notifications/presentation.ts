import type { components } from '#lib/management/schema.js';
import type { NotificationDestination } from './api';

export type DestinationType = NonNullable<NotificationDestination['type']>;
const destinationLabels: Record<DestinationType, string> = {
  webhook: 'Webhook',
  slack: 'Slack',
  msteams: 'Microsoft Teams',
  discord: 'Discord',
  pagerduty: 'PagerDuty',
  email: 'Email (SMTP)'
};
export const destinationTypes = (
  Object.keys(destinationLabels) as DestinationType[]
).map((value) => ({ value, label: destinationLabels[value] }));
export const ruleSubjectLabels: Record<
  components['schemas']['BudgetSubjectKind'],
  string
> = { api_key: 'API key', budget_group: 'Budget group' };
export const ruleWindowLabels: Record<
  components['schemas']['BudgetWindowKind'],
  string
> = { day: 'Budget day', week: 'Budget week', month: 'Budget month' };
export const isChat = (type: DestinationType) =>
  type === 'slack' || type === 'msteams' || type === 'discord';
export const secretRequired = (type: DestinationType) =>
  isChat(type) || type === 'pagerduty';

export function canChangeNotification(
  projectId: string | null | undefined,
  permissions: { project: boolean; installation: boolean }
): boolean {
  return projectId == null ? permissions.installation : permissions.project;
}
