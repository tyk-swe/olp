import type {
  CreateNotificationDestinationInput,
  NotificationDestination,
  UpdateNotificationDestinationInput
} from './api';
import type { DestinationType } from './presentation';

type DestinationFields = { name: string; url: string; projectId: string };
export type EmailDraft = {
  from: string;
  to: string;
  subjectPrefix: string;
  caCertificate: string;
};
export type DestinationDraft = DestinationFields &
  (
    | { type: 'webhook'; secret: string }
    | { type: 'slack' | 'msteams' | 'discord'; webhookUrl: string }
    | { type: 'pagerduty'; routingKey: string }
    | ({ type: 'email'; username: string; password: string } & EmailDraft)
  );
type DestinationSecret = NonNullable<
  CreateNotificationDestinationInput['secret']
>;
export type DestinationSecretChange =
  | { kind: 'keep' }
  | { kind: 'clear' }
  | { kind: 'replace'; value: DestinationSecret };
type ParsedSecret =
  | { kind: 'valid'; change: DestinationSecretChange }
  | { kind: 'invalid'; message: string };

export function newDestinationDraft(
  type: DestinationType,
  fields: DestinationFields = { name: '', url: '', projectId: '' }
): DestinationDraft {
  const common = {
    name: fields.name,
    url: fields.url,
    projectId: fields.projectId
  };
  switch (type) {
    case 'webhook':
      return { ...common, type, secret: '' };
    case 'slack':
    case 'msteams':
    case 'discord':
      return { ...common, type, webhookUrl: '' };
    case 'pagerduty':
      return { ...common, type, routingKey: '' };
    case 'email':
      return {
        ...common,
        type,
        from: '',
        to: '',
        subjectPrefix: '',
        caCertificate: '',
        username: '',
        password: ''
      };
  }
}

export function editDestinationDraft(
  destination: NotificationDestination
): DestinationDraft {
  const draft = newDestinationDraft(destination.type ?? 'webhook', {
    name: destination.name,
    url: destination.url,
    projectId: destination.project_id ?? ''
  });
  if (draft.type !== 'email') return draft;
  const configuration = destination.configuration ?? {};
  return {
    ...draft,
    from: typeof configuration.from === 'string' ? configuration.from : '',
    to: Array.isArray(configuration.to)
      ? configuration.to
          .filter((address): address is string => typeof address === 'string')
          .join(', ')
      : '',
    subjectPrefix:
      typeof configuration.subject_prefix === 'string'
        ? configuration.subject_prefix
        : '',
    caCertificate:
      typeof configuration.ca_certificate === 'string'
        ? configuration.ca_certificate
        : ''
  };
}

function emailConfiguration(draft: EmailDraft) {
  return {
    from: draft.from.trim(),
    to: draft.to
      .split(',')
      .map((address) => address.trim())
      .filter(Boolean),
    ...(draft.subjectPrefix.trim()
      ? { subject_prefix: draft.subjectPrefix.trim() }
      : {}),
    ...(draft.caCertificate.trim()
      ? { ca_certificate: draft.caCertificate.trim() }
      : {})
  };
}

export function createDestinationInput(
  draft: DestinationDraft
): CreateNotificationDestinationInput {
  const common = {
    name: draft.name.trim(),
    url: draft.url.trim(),
    project_id: draft.projectId || null
  };
  switch (draft.type) {
    case 'webhook':
      return {
        ...common,
        type: draft.type,
        secret: draft.secret.trim() || null
      };
    case 'slack':
    case 'msteams':
    case 'discord':
      return {
        ...common,
        type: draft.type,
        secret: { webhook_url: draft.webhookUrl.trim() }
      };
    case 'pagerduty':
      return {
        ...common,
        type: draft.type,
        secret: { routing_key: draft.routingKey.trim() }
      };
    case 'email':
      return {
        ...common,
        type: draft.type,
        configuration: emailConfiguration(draft),
        secret:
          draft.username.trim() || draft.password
            ? { username: draft.username.trim(), password: draft.password }
            : null
      };
  }
}

export function parseDestinationSecret(
  type: DestinationType,
  raw: string,
  clear: boolean
): ParsedSecret {
  if (clear) return { kind: 'valid', change: { kind: 'clear' } };
  const value = raw.trim();
  if (!value) return { kind: 'valid', change: { kind: 'keep' } };
  switch (type) {
    case 'webhook':
      return { kind: 'valid', change: { kind: 'replace', value } };
    case 'slack':
    case 'msteams':
    case 'discord':
      return {
        kind: 'valid',
        change: { kind: 'replace', value: { webhook_url: value } }
      };
    case 'pagerduty':
      return {
        kind: 'valid',
        change: { kind: 'replace', value: { routing_key: value } }
      };
    case 'email': {
      try {
        const parsed: unknown = JSON.parse(value);
        if (parsed === null)
          return { kind: 'valid', change: { kind: 'clear' } };
        if (
          typeof parsed === 'object' &&
          !Array.isArray(parsed) &&
          Object.keys(parsed).length === 0
        )
          return { kind: 'valid', change: { kind: 'replace', value: {} } };
        if (
          !parsed ||
          typeof parsed !== 'object' ||
          Array.isArray(parsed) ||
          Object.keys(parsed).length !== 2 ||
          !('username' in parsed) ||
          !('password' in parsed) ||
          typeof parsed.username !== 'string' ||
          typeof parsed.password !== 'string'
        )
          return {
            kind: 'invalid',
            message: 'SMTP credentials must contain a username and password.'
          };
        return {
          kind: 'valid',
          change: {
            kind: 'replace',
            value: { username: parsed.username, password: parsed.password }
          }
        };
      } catch {
        return {
          kind: 'invalid',
          message: 'SMTP credentials must be a JSON object.'
        };
      }
    }
  }
}

export function updateDestinationInput(
  draft: DestinationDraft,
  change: DestinationSecretChange
): UpdateNotificationDestinationInput {
  return {
    name: draft.name.trim(),
    url: draft.url.trim(),
    ...(draft.type === 'email'
      ? { configuration: emailConfiguration(draft) }
      : {}),
    ...(change.kind === 'clear'
      ? { secret: null }
      : change.kind === 'replace'
        ? { secret: change.value }
        : {})
  };
}
