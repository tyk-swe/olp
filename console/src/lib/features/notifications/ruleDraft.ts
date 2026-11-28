import {
  validated,
  type FieldErrors,
  type Validation
} from '#lib/editing/validation.js';
import type { CreateNotificationRuleInput, NotificationEvent } from './api';

type FieldKey =
  | 'threshold'
  | 'recovery_threshold'
  | 'window_seconds'
  | 'minimum_samples'
  | 'cooldown_seconds'
  | 'lead_days'
  | 'lead_time_seconds'
  | 'baseline_days'
  | 'minimum_history_days'
  | 'increase_percent'
  | 'minimum_increase';
export type RuleField = 'name' | 'subjectId' | 'destinationId' | FieldKey;
export const ruleRowServerFields: Readonly<Record<string, RuleField>> = {
  name: 'name',
  destination_id: 'destinationId'
};
export const ruleServerFields: Readonly<Record<string, RuleField>> = {
  ...ruleRowServerFields,
  subject_id: 'subjectId',
  threshold_percent: 'threshold'
};
type ConfigurationField = {
  key: FieldKey;
  label: string;
  inputmode: 'numeric' | 'decimal';
};
type EventDefinition = {
  event: NotificationEvent;
  label: string;
  scope: 'project' | 'installation';
  fields: readonly ConfigurationField[];
};
const cooldown: ConfigurationField = {
  key: 'cooldown_seconds',
  label: 'Cooldown seconds',
  inputmode: 'numeric'
};
const samples: readonly ConfigurationField[] = [
  { key: 'window_seconds', label: 'Window seconds', inputmode: 'numeric' },
  { key: 'minimum_samples', label: 'Minimum samples', inputmode: 'numeric' },
  cooldown
];
function thresholdFields(
  threshold: string,
  recovery: string
): readonly ConfigurationField[] {
  return [
    { key: 'threshold', label: threshold, inputmode: 'decimal' },
    { key: 'recovery_threshold', label: recovery, inputmode: 'decimal' },
    ...samples
  ];
}
export const notificationEvents: Record<NotificationEvent, EventDefinition> = {
  'budget.threshold': {
    event: 'budget.threshold',
    label: 'Budget threshold',
    scope: 'project',
    fields: []
  },
  'budget.exhausted': {
    event: 'budget.exhausted',
    label: 'Budget exhausted',
    scope: 'project',
    fields: [cooldown]
  },
  'provider.grant.lapsed': {
    event: 'provider.grant.lapsed',
    label: 'Grant lapsed',
    scope: 'installation',
    fields: []
  },
  'provider.circuit.open': {
    event: 'provider.circuit.open',
    label: 'Provider circuit opened',
    scope: 'installation',
    fields: [cooldown]
  },
  'provider.circuit.closed': {
    event: 'provider.circuit.closed',
    label: 'Provider circuit closed',
    scope: 'installation',
    fields: [cooldown]
  },
  'provider.error_rate': {
    event: 'provider.error_rate',
    label: 'Provider error rate',
    scope: 'installation',
    fields: thresholdFields('Error-rate threshold', 'Recovery threshold')
  },
  'provider.credential.failing': {
    event: 'provider.credential.failing',
    label: 'Provider credential failing',
    scope: 'installation',
    fields: thresholdFields('Failure count threshold', 'Recovery threshold')
  },
  'route.latency': {
    event: 'route.latency',
    label: 'Route latency',
    scope: 'project',
    fields: thresholdFields('Latency threshold ms', 'Recovery threshold ms')
  },
  'model.retirement': {
    event: 'model.retirement',
    label: 'Model retirement',
    scope: 'project',
    fields: [
      { key: 'lead_days', label: 'Lead days', inputmode: 'numeric' },
      cooldown
    ]
  },
  'runtime.install_failed': {
    event: 'runtime.install_failed',
    label: 'Runtime install failed',
    scope: 'installation',
    fields: [cooldown]
  },
  'worker.stale': {
    event: 'worker.stale',
    label: 'Worker stale',
    scope: 'installation',
    fields: [cooldown]
  },
  'key.expiring': {
    event: 'key.expiring',
    label: 'Key expiry or rotation due',
    scope: 'project',
    fields: [
      {
        key: 'lead_time_seconds',
        label: 'Lead time seconds',
        inputmode: 'numeric'
      }
    ]
  },
  'report.spend': {
    event: 'report.spend',
    label: 'Spend report',
    scope: 'project',
    fields: []
  },
  'spend.spike': {
    event: 'spend.spike',
    label: 'Daily spend spike',
    scope: 'project',
    fields: [
      { key: 'baseline_days', label: 'Baseline days', inputmode: 'numeric' },
      {
        key: 'minimum_history_days',
        label: 'Minimum history days',
        inputmode: 'numeric'
      },
      {
        key: 'increase_percent',
        label: 'Increase percent',
        inputmode: 'decimal'
      },
      {
        key: 'minimum_increase',
        label: 'Minimum increase',
        inputmode: 'decimal'
      }
    ]
  }
};

type Values = Partial<Record<FieldKey, string>>;
export type RuleConfigurationDraft = {
  [Event in NotificationEvent]: {
    event: Event;
    values: Values;
  } & (Event extends 'route.latency'
    ? { metric: 'ttft' | 'latency' }
    : Event extends 'report.spend'
      ? { period: 'daily' | 'weekly' | 'monthly' }
      : object);
}[NotificationEvent];

type RuleIdentity = { name: string; destinationId: string; projectId: string };
export type RuleDraft = {
  [Event in NotificationEvent]: Extract<
    RuleConfigurationDraft,
    { event: Event }
  > &
    RuleIdentity &
    (Event extends 'budget.threshold'
      ? {
          subjectKind: 'api_key' | 'budget_group';
          subjectId: string;
          window: 'day' | 'week' | 'month';
          threshold: string;
        }
      : Event extends 'key.expiring'
        ? { subjectKind: 'api_key'; subjectId: string }
        : object);
}[NotificationEvent];

export function newRuleDraft(
  event: NotificationEvent,
  identity: RuleIdentity = { name: '', destinationId: '', projectId: '' }
): RuleDraft {
  const configuration = newRuleConfiguration(event);
  const fields = {
    name: identity.name,
    destinationId: identity.destinationId,
    projectId: identity.projectId
  };
  if (configuration.event === 'budget.threshold')
    return {
      ...configuration,
      ...fields,
      subjectKind: 'api_key',
      subjectId: '',
      window: 'month',
      threshold: '80'
    };
  if (configuration.event === 'key.expiring')
    return {
      ...configuration,
      ...fields,
      subjectKind: 'api_key',
      subjectId: ''
    };
  return { ...configuration, ...fields };
}

export function resetRuleDraft(draft: RuleDraft): RuleDraft {
  return {
    ...draft,
    name: '',
    destinationId: '',
    values: {},
    ...('subjectId' in draft ? { subjectId: '' } : {})
  };
}

function identityErrors(draft: RuleDraft): FieldErrors<RuleField> {
  return {
    name: draft.name.trim() ? undefined : 'Enter a rule name.',
    destinationId: draft.destinationId ? undefined : 'Choose a destination.'
  };
}

function subjectErrors(draft: RuleDraft): FieldErrors<RuleField> {
  if (draft.event === 'key.expiring')
    return { subjectId: draft.subjectId ? undefined : 'Choose an API key.' };
  if (draft.event !== 'budget.threshold') return {};
  const threshold = Number(draft.threshold);
  return {
    subjectId: draft.subjectId ? undefined : 'Choose a budget subject.',
    threshold:
      Number.isInteger(threshold) && threshold >= 1 && threshold <= 100
        ? undefined
        : 'Threshold must be a whole percentage from 1 to 100.'
  };
}

function ruleInput(draft: RuleDraft): CreateNotificationRuleInput {
  const base = {
    name: draft.name.trim(),
    event: draft.event,
    destination_id: draft.destinationId
  };
  if (draft.event === 'budget.threshold')
    return {
      ...base,
      project_id: draft.projectId || null,
      subject_kind: draft.subjectKind,
      subject_id: draft.subjectId,
      window_kind: draft.window,
      threshold_percent: Number(draft.threshold)
    };
  const values = configurationValues(draft);
  const configuration = Object.keys(values).length
    ? { configuration: values }
    : {};
  if (draft.event === 'key.expiring')
    return {
      ...base,
      ...configuration,
      project_id: draft.projectId || null,
      subject_kind: 'api_key',
      subject_id: draft.subjectId
    };
  return {
    ...base,
    ...configuration,
    ...(notificationEvents[draft.event].scope === 'project'
      ? { project_id: draft.projectId || null }
      : {})
  };
}

export function createRuleInput(
  draft: RuleDraft
): Validation<CreateNotificationRuleInput, RuleField> {
  const checked = validated<CreateNotificationRuleInput, RuleField>(
    {
      ...identityErrors(draft),
      ...subjectErrors(draft),
      ...configurationErrors(draft)
    },
    () => ruleInput(draft)
  );
  if (draft.event !== 'spend.spike' || draft.projectId) return checked;
  return {
    ok: false,
    fieldErrors: checked.ok ? {} : checked.fieldErrors,
    formError: 'Choose a project.'
  };
}

type ParsedConfiguration = Record<string, string | number>;

export function newRuleConfiguration(
  event: NotificationEvent,
  configuration: Readonly<Record<string, unknown>> = {}
): RuleConfigurationDraft {
  const values = Object.fromEntries(
    notificationEvents[event].fields.flatMap(({ key }) => {
      const value = configuration[key];
      return typeof value === 'string' || typeof value === 'number'
        ? [[key, String(value)]]
        : [];
    })
  );
  if (event === 'route.latency')
    return {
      event,
      values,
      metric: configuration.metric === 'latency' ? 'latency' : 'ttft'
    };
  if (event === 'report.spend')
    return {
      event,
      values,
      period:
        configuration.period === 'weekly' || configuration.period === 'monthly'
          ? configuration.period
          : 'daily'
    };
  return { event, values };
}

function configurationErrors(
  draft: RuleConfigurationDraft
): FieldErrors<FieldKey> {
  return Object.fromEntries(
    notificationEvents[draft.event].fields.flatMap((field) => {
      const value = (draft.values[field.key] ?? '').trim();
      return value &&
        field.inputmode === 'numeric' &&
        (!/^\d+$/.test(value) || !Number.isSafeInteger(Number(value)))
        ? [[field.key, `${field.label} must be a whole number.`]]
        : [];
    })
  );
}

function configurationValues(
  draft: RuleConfigurationDraft
): ParsedConfiguration {
  return {
    ...Object.fromEntries(
      notificationEvents[draft.event].fields.flatMap((field) => {
        const value = (draft.values[field.key] ?? '').trim();
        return value
          ? [[field.key, field.inputmode === 'decimal' ? value : Number(value)]]
          : [];
      })
    ),
    ...(draft.event === 'route.latency' ? { metric: draft.metric } : {}),
    ...(draft.event === 'report.spend' ? { period: draft.period } : {})
  };
}

export function parseRuleConfiguration(draft: RuleConfigurationDraft) {
  return validated(configurationErrors(draft), () =>
    configurationValues(draft)
  );
}
