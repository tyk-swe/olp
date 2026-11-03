import type { ExportSink, FocusDataset } from './api';

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

// A one-line summary of the sink's format-specific settings.
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
  string,
  { value: string; label: string }[]
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
export const sinkTypeLabels: Record<string, string> = {
  https: 'HTTPS',
  otlp_logs: 'OTLP logs',
  s3: 'S3',
  gcs: 'GCS',
  azure_blob: 'Azure Blob',
  stripe_meter: 'Stripe meter'
};
export const sinkStreamOptions = [
  { value: 'requests', label: 'Requests' },
  { value: 'attempts', label: 'Attempts' },
  { value: 'usage_rollups', label: 'Usage rollups' },
  { value: 'guardrail_decisions', label: 'Guardrail decisions' },
  { value: 'audit', label: 'Audit' },
  { value: 'metering', label: 'Metering' },
  { value: 'runs', label: 'Runs (gateway observed)' },
  { value: 'trace_spans', label: 'Trace spans (client asserted)' },
  { value: 'focus', label: 'FOCUS' }
];
export const stripeMeterValues = [
  { value: 'charge', label: 'Charge' },
  { value: 'cost', label: 'Cost' },
  { value: 'input_tokens', label: 'Input tokens' },
  { value: 'output_tokens', label: 'Output tokens' },
  { value: 'total_tokens', label: 'Total tokens' },
  { value: 'requests', label: 'Requests' }
];
