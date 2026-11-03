import type { NotificationDestination, NotificationEvent } from './api';

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

export const eventLabels: Record<NotificationEvent, string> = {
  'budget.threshold': 'Budget threshold',
  'budget.exhausted': 'Budget exhausted',
  'provider.grant.lapsed': 'Grant lapsed',
  'provider.circuit.open': 'Provider circuit opened',
  'provider.circuit.closed': 'Provider circuit closed',
  'provider.error_rate': 'Provider error rate',
  'provider.credential.failing': 'Provider credential failing',
  'route.latency': 'Route latency',
  'model.retirement': 'Model retirement',
  'runtime.install_failed': 'Runtime install failed',
  'worker.stale': 'Worker stale',
  'key.expiring': 'Key expiry or rotation due',
  'report.spend': 'Spend report'
};

export const projectScopedEvents = new Set<NotificationEvent>([
  'budget.threshold',
  'key.expiring',
  'budget.exhausted',
  'route.latency',
  'model.retirement',
  'report.spend'
]);

export const eventConfigFields: Record<
  string,
  { key: string; label: string; inputmode?: 'numeric' | 'decimal' }[]
> = {
  'provider.error_rate': [
    { key: 'threshold', label: 'Error-rate threshold', inputmode: 'decimal' },
    {
      key: 'recovery_threshold',
      label: 'Recovery threshold',
      inputmode: 'decimal'
    },
    { key: 'window_seconds', label: 'Window seconds', inputmode: 'numeric' },
    {
      key: 'minimum_samples',
      label: 'Minimum samples',
      inputmode: 'numeric'
    },
    {
      key: 'cooldown_seconds',
      label: 'Cooldown seconds',
      inputmode: 'numeric'
    }
  ],
  'route.latency': [
    { key: 'threshold', label: 'Latency threshold ms', inputmode: 'decimal' },
    {
      key: 'recovery_threshold',
      label: 'Recovery threshold ms',
      inputmode: 'decimal'
    },
    { key: 'window_seconds', label: 'Window seconds', inputmode: 'numeric' },
    {
      key: 'minimum_samples',
      label: 'Minimum samples',
      inputmode: 'numeric'
    },
    {
      key: 'cooldown_seconds',
      label: 'Cooldown seconds',
      inputmode: 'numeric'
    }
  ],
  'provider.credential.failing': [
    {
      key: 'threshold',
      label: 'Failure count threshold',
      inputmode: 'decimal'
    },
    {
      key: 'recovery_threshold',
      label: 'Recovery threshold',
      inputmode: 'decimal'
    },
    { key: 'window_seconds', label: 'Window seconds', inputmode: 'numeric' },
    {
      key: 'minimum_samples',
      label: 'Minimum samples',
      inputmode: 'numeric'
    },
    {
      key: 'cooldown_seconds',
      label: 'Cooldown seconds',
      inputmode: 'numeric'
    }
  ],
  'model.retirement': [
    { key: 'lead_days', label: 'Lead days', inputmode: 'numeric' },
    {
      key: 'cooldown_seconds',
      label: 'Cooldown seconds',
      inputmode: 'numeric'
    }
  ],
  'runtime.install_failed': [
    {
      key: 'cooldown_seconds',
      label: 'Cooldown seconds',
      inputmode: 'numeric'
    }
  ],
  'worker.stale': [
    {
      key: 'cooldown_seconds',
      label: 'Cooldown seconds',
      inputmode: 'numeric'
    }
  ],
  'provider.circuit.open': [
    {
      key: 'cooldown_seconds',
      label: 'Cooldown seconds',
      inputmode: 'numeric'
    }
  ],
  'provider.circuit.closed': [
    {
      key: 'cooldown_seconds',
      label: 'Cooldown seconds',
      inputmode: 'numeric'
    }
  ],
  'budget.exhausted': [
    {
      key: 'cooldown_seconds',
      label: 'Cooldown seconds',
      inputmode: 'numeric'
    }
  ],
  'key.expiring': [
    {
      key: 'lead_time_seconds',
      label: 'Lead time seconds',
      inputmode: 'numeric'
    }
  ]
};
