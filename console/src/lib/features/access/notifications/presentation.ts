import type { NotificationDestination } from './api';

export type DestinationType = NonNullable<NotificationDestination['type']>;
export const destinationTypes: { value: DestinationType; label: string }[] = [
  { value: 'webhook', label: 'Webhook' },
  { value: 'slack', label: 'Slack' },
  { value: 'msteams', label: 'Microsoft Teams' },
  { value: 'discord', label: 'Discord' },
  { value: 'pagerduty', label: 'PagerDuty' },
  { value: 'email', label: 'Email (SMTP)' }
];
export const isChat = (type: DestinationType) =>
  type === 'slack' || type === 'msteams' || type === 'discord';
export const secretRequired = (type: DestinationType) =>
  isChat(type) || type === 'pagerduty';
