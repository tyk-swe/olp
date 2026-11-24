import type { ExportSink } from './api';

type Stream = ExportSink['streams'][number];
type RegularStream = Exclude<Stream, 'focus'>;
type Storage = 's3' | 'gcs' | 'azure_blob';
export type SinkSettingsDraft =
  | { format: 'json' | 'jsonl' | 'otlp' }
  | { format: 'cloudevents'; ceSource: string }
  | {
      format: 'csv_gzip';
      focusCostBasis: 'cost' | 'charge';
      focusStartDate: string;
    }
  | {
      format: 'stripe';
      stripeEvent: string;
      stripeValue: string;
      stripeCustomerKey: string;
      stripeValueKey: string;
      customers: { project: string; customer: string }[];
    };
export type SinkDelivery =
  | { type: 'https'; format: 'json'; streams: RegularStream[] }
  | ({ type: 'https'; streams: ['metering'] } & Extract<
      SinkSettingsDraft,
      { format: 'cloudevents' }
    >)
  | { type: 'otlp_logs'; format: 'otlp'; streams: RegularStream[] }
  | { type: Storage; format: 'jsonl'; streams: RegularStream[] }
  | ({ type: Storage; streams: ['focus'] } & Extract<
      SinkSettingsDraft,
      { format: 'csv_gzip' }
    >)
  | ({ type: 'stripe_meter'; streams: ['metering'] } & Extract<
      SinkSettingsDraft,
      { format: 'stripe' }
    >);

type SinkSettings =
  | { cost_basis: 'cost' | 'charge'; start_date?: string }
  | { source: string }
  | {
      event_name: string;
      value: string;
      customers: Record<string, string>;
      customer_key?: string;
      value_key?: string;
    }
  | null;
export type ParsedSinkSettings =
  | { kind: 'valid'; settings: SinkSettings }
  | { kind: 'invalid'; message: string };

export function newSinkDelivery(
  type: ExportSink['type'],
  format?: ExportSink['format']
): SinkDelivery {
  switch (type) {
    case 'https':
      return format === 'cloudevents'
        ? { type, format, streams: ['metering'], ceSource: '' }
        : { type, format: 'json', streams: ['requests'] };
    case 'otlp_logs':
      return { type, format: 'otlp', streams: ['requests'] };
    case 'stripe_meter':
      return {
        type,
        format: 'stripe',
        streams: ['metering'],
        stripeEvent: 'olp.charge',
        stripeValue: 'charge',
        stripeCustomerKey: '',
        stripeValueKey: '',
        customers: [{ project: '', customer: '' }]
      };
    default:
      return format === 'csv_gzip'
        ? {
            type,
            format,
            streams: ['focus'],
            focusCostBasis: 'cost',
            focusStartDate: ''
          }
        : { type, format: 'jsonl', streams: ['requests'] };
  }
}

export function changeSinkDelivery(
  delivery: SinkDelivery,
  type: ExportSink['type'],
  format?: ExportSink['format']
): SinkDelivery {
  const next = newSinkDelivery(type, format);
  if (
    next.format === 'cloudevents' ||
    next.format === 'stripe' ||
    next.format === 'csv_gzip'
  )
    return next;
  return {
    ...next,
    streams: delivery.streams.filter(
      (stream): stream is RegularStream => stream !== 'focus'
    )
  };
}

export function editSinkDelivery(sink: ExportSink): SinkDelivery {
  const settings = sink.settings ?? {};
  const delivery = newSinkDelivery(sink.type, sink.format);
  switch (delivery.format) {
    case 'cloudevents':
      return {
        ...delivery,
        ceSource: typeof settings.source === 'string' ? settings.source : ''
      };
    case 'csv_gzip':
      return {
        ...delivery,
        focusCostBasis: settings.cost_basis === 'charge' ? 'charge' : 'cost',
        focusStartDate:
          typeof settings.start_date === 'string' ? settings.start_date : ''
      };
    case 'stripe': {
      const customers =
        settings.customers &&
        typeof settings.customers === 'object' &&
        !Array.isArray(settings.customers)
          ? Object.entries(settings.customers).flatMap(([project, customer]) =>
              typeof customer === 'string' ? [{ project, customer }] : []
            )
          : [];
      return {
        ...delivery,
        stripeEvent:
          typeof settings.event_name === 'string'
            ? settings.event_name
            : 'olp.charge',
        stripeValue:
          typeof settings.value === 'string' ? settings.value : 'charge',
        stripeCustomerKey:
          typeof settings.customer_key === 'string'
            ? settings.customer_key
            : '',
        stripeValueKey:
          typeof settings.value_key === 'string' ? settings.value_key : '',
        customers: customers.length
          ? customers
          : [{ project: '', customer: '' }]
      };
    }
    default:
      return {
        ...delivery,
        streams: sink.streams.filter(
          (stream): stream is RegularStream => stream !== 'focus'
        )
      };
  }
}

export function toggleSinkStream(
  delivery: SinkDelivery,
  stream: Stream
): SinkDelivery {
  if (
    delivery.format === 'cloudevents' ||
    delivery.format === 'stripe' ||
    delivery.format === 'csv_gzip' ||
    stream === 'focus'
  )
    return delivery;
  return {
    ...delivery,
    streams: delivery.streams.includes(stream)
      ? delivery.streams.filter((value) => value !== stream)
      : [...delivery.streams, stream]
  };
}

export function parseSinkSettings(
  draft: SinkSettingsDraft,
  credential: { editing: boolean; stripeKey: string }
): ParsedSinkSettings {
  switch (draft.format) {
    case 'csv_gzip':
      return {
        kind: 'valid',
        settings: {
          cost_basis: draft.focusCostBasis,
          ...(draft.focusStartDate ? { start_date: draft.focusStartDate } : {})
        }
      };
    case 'cloudevents':
      return {
        kind: 'valid',
        settings: draft.ceSource.trim()
          ? { source: draft.ceSource.trim() }
          : null
      };
    case 'stripe': {
      const message = stripeSettingsError(draft, credential);
      if (message) return { kind: 'invalid', message };
      return {
        kind: 'valid',
        settings: {
          event_name: draft.stripeEvent.trim(),
          value: draft.stripeValue,
          customers: Object.fromEntries(
            draft.customers
              .filter((row) => row.project && row.customer.trim())
              .map((row) => [row.project, row.customer.trim()])
          ),
          ...(draft.stripeCustomerKey.trim()
            ? { customer_key: draft.stripeCustomerKey.trim() }
            : {}),
          ...(draft.stripeValueKey.trim()
            ? { value_key: draft.stripeValueKey.trim() }
            : {})
        }
      };
    }
    default:
      return { kind: 'valid', settings: null };
  }
}

function stripeSettingsError(
  draft: Extract<SinkSettingsDraft, { format: 'stripe' }>,
  credential: { editing: boolean; stripeKey: string }
): string {
  if (!credential.editing && !credential.stripeKey.trim())
    return 'A restricted API key is required for a new Stripe meter sink.';
  if (!/^[A-Za-z0-9_.:-]{1,100}$/.test(draft.stripeEvent.trim()))
    return 'Enter a valid Stripe event name (1–100 letters, digits, _, ., :, or -).';
  const rows = draft.customers.filter(
    (row) => row.project || row.customer.trim()
  );
  if (!rows.length) return 'Map at least one project to a Stripe customer.';
  if (rows.some((row) => !row.project))
    return 'Every customer mapping needs a project.';
  if (rows.some((row) => !/^cus_[A-Za-z0-9]{1,250}$/.test(row.customer.trim())))
    return 'Stripe customer IDs must start with cus_ and contain letters and digits.';
  if (
    draft.stripeCustomerKey.trim() &&
    !/^[A-Za-z0-9_]{1,64}$/.test(draft.stripeCustomerKey.trim())
  )
    return 'Enter a valid customer key (1–64 letters, digits, or _).';
  if (
    draft.stripeValueKey.trim() &&
    !/^[A-Za-z0-9_]{1,64}$/.test(draft.stripeValueKey.trim())
  )
    return 'Enter a valid value key (1–64 letters, digits, or _).';
  return '';
}

export type SinkCredentialChange =
  | { kind: 'keep' }
  | { kind: 'clear' }
  | { kind: 'replace'; value: Record<string, unknown> };

export function parseSinkCredential(input: {
  clear: boolean;
  type: ExportSink['type'];
  stripeKey: string;
  json: string;
}):
  | { kind: 'valid'; change: SinkCredentialChange }
  | { kind: 'invalid'; message: string } {
  if (input.clear) return { kind: 'valid', change: { kind: 'clear' } };
  if (input.type === 'stripe_meter')
    return {
      kind: 'valid',
      change: input.stripeKey.trim()
        ? { kind: 'replace', value: { api_key: input.stripeKey.trim() } }
        : { kind: 'keep' }
    };
  if (!input.json.trim()) return { kind: 'valid', change: { kind: 'keep' } };
  try {
    const value: unknown = JSON.parse(input.json);
    if (typeof value !== 'object' || value === null || Array.isArray(value))
      return { kind: 'invalid', message: 'Credential JSON must be an object.' };
    return {
      kind: 'valid',
      change: {
        kind: 'replace',
        value: Object.fromEntries(Object.entries(value))
      }
    };
  } catch {
    return { kind: 'invalid', message: 'Credential JSON is not valid JSON.' };
  }
}
