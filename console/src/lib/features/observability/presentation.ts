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
