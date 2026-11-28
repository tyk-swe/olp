import type { ExportSink, FocusDataset, MeteringDelivery } from './api';

const meteringDeliveryLabels: Record<MeteringDelivery['outcome'], string> = {
  pending: 'Pending',
  retrying: 'Retrying',
  accepted: 'Accepted by sink',
  delivery_unconfirmed: 'Delivery unconfirmed',
  expired_before_delivery: 'Expired before delivery',
  no_retained_evidence: 'No retained evidence'
};

export function meteringDeliveryLabel(
  outcome: MeteringDelivery['outcome']
): string {
  return meteringDeliveryLabels[outcome];
}

export type SinkStreamStatus = {
  stream?: string;
  pending?: number;
  pending_error_codes?: string[];
  delivered_total?: number;
  failures_total?: number;
  gaps_total?: number;
  last_success_at?: string | null;
  last_attempt_at?: string | null;
  oldest_pending_at?: string | null;
};

export function streamStatuses(sink: ExportSink): SinkStreamStatus[] {
  return (sink.status ?? []) as SinkStreamStatus[];
}

export function lagSeconds(status: SinkStreamStatus): number {
  return status.oldest_pending_at &&
    Number.isFinite(Date.parse(status.oldest_pending_at))
    ? Math.max(0, (Date.now() - Date.parse(status.oldest_pending_at)) / 1000)
    : 0;
}

export function streamState(status: SinkStreamStatus): string {
  if (status.pending == null) return 'unknown';
  if (status.pending > 0)
    return (status.pending_error_codes ?? []).length > 0
      ? 'failing'
      : 'pending';
  return 'idle';
}

export function focusDatasetStatus(dataset: FocusDataset): string {
  if (dataset.last_error_code) return dataset.last_error_code;
  return dataset.current ? 'Current' : 'Restatement pending';
}

export function sinkSettingsSummary(sink: ExportSink): string {
  const settings = (sink.settings ?? {}) as Record<string, unknown>;
  const parts: string[] = [];
  if (sink.format === 'csv_gzip') {
    parts.push(
      `Cost basis: ${settings.cost_basis === 'charge' ? 'charge' : 'cost'}`
    );
    if (settings.start_date)
      parts.push(`from ${settings.start_date as string}`);
  } else if (sink.format === 'cloudevents') {
    if (settings.source) parts.push(`Source: ${settings.source as string}`);
  } else if (sink.format === 'stripe') {
    parts.push(
      `Meter: ${(settings.event_name as string) ?? ''} (${(settings.value as string) ?? ''})`
    );
    const customers = (settings.customers ?? {}) as Record<string, string>;
    parts.push(
      `${Object.keys(customers).length} customer${Object.keys(customers).length === 1 ? '' : 's'}`
    );
  }
  return parts.join(' · ');
}

export const sinkTypeFormats: Record<
  ExportSink['type'],
  { value: ExportSink['format']; label: string }[]
> = {
  https: [
    { value: 'json', label: 'JSON' },
    { value: 'cloudevents', label: 'CloudEvents' }
  ],
  otlp_logs: [{ value: 'otlp', label: 'OTLP logs' }],
  s3: [
    { value: 'jsonl', label: 'JSON Lines' },
    { value: 'csv_gzip', label: 'FOCUS 1.4 (gzip CSV)' }
  ],
  gcs: [
    { value: 'jsonl', label: 'JSON Lines' },
    { value: 'csv_gzip', label: 'FOCUS 1.4 (gzip CSV)' }
  ],
  azure_blob: [
    { value: 'jsonl', label: 'JSON Lines' },
    { value: 'csv_gzip', label: 'FOCUS 1.4 (gzip CSV)' }
  ],
  stripe_meter: [{ value: 'stripe', label: 'Stripe meter events' }]
};
export function isSinkType(value: string): value is ExportSink['type'] {
  return Object.hasOwn(sinkTypeFormats, value);
}

export const sinkTypeLabels: Record<string, string> = {
  https: 'HTTPS',
  otlp_logs: 'OTLP logs',
  s3: 'S3',
  gcs: 'GCS',
  azure_blob: 'Azure Blob',
  stripe_meter: 'Stripe meter'
} satisfies Record<ExportSink['type'], string>;
const sinkStreamLabels: Record<ExportSink['streams'][number], string> = {
  requests: 'Requests',
  attempts: 'Attempts',
  usage_rollups: 'Usage rollups',
  guardrail_decisions: 'Guardrail decisions',
  audit: 'Audit',
  metering: 'Metering',
  runs: 'Runs (gateway observed)',
  trace_spans: 'Trace spans (client asserted)',
  focus: 'FOCUS'
};
export const sinkStreamOptions = (
  Object.keys(sinkStreamLabels) as ExportSink['streams'][number][]
).map((value) => ({ value, label: sinkStreamLabels[value] }));
export type SinkOutcome = NonNullable<
  NonNullable<ExportSink['filter']>['outcome']
>;
export const sinkOutcomeLabels: Record<SinkOutcome, string> = {
  success: 'Success',
  failure: 'Failure'
};
export const stripeMeterValues = [
  { value: 'charge', label: 'Charge' },
  { value: 'cost', label: 'Cost' },
  { value: 'input_tokens', label: 'Input tokens' },
  { value: 'output_tokens', label: 'Output tokens' },
  { value: 'total_tokens', label: 'Total tokens' },
  { value: 'requests', label: 'Requests' }
];
