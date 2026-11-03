import { afterEach, describe, expect, it, vi } from 'vitest';
import type { ExportSink, FocusDataset } from './api';
import {
  focusDatasetStatus,
  lagSeconds,
  sinkSettingsSummary,
  streamState,
  streamStatuses
} from './presentation';

const sink: ExportSink = {
  export_sink_id: 'sink-1',
  name: 'Delivery',
  project_id: null,
  type: 'https',
  destination: 'https://example.test/events',
  streams: ['audit'],
  format: 'json',
  filter: null,
  enabled: true,
  credential_configured: false,
  etag: 'revision-1',
  status: []
};

const dataset: FocusDataset = {
  charge_date: '2026-01-01',
  revision: 1,
  generated_at: null,
  delivered_at: null,
  row_count: null,
  part_count: null,
  billed_cost: null,
  effective_cost: null,
  attempts: 0,
  last_error_code: null,
  current: false,
  commitment_totals: null
};

afterEach(() => vi.useRealTimers());

describe('export sink presentation', () => {
  it('shows stream status for pending deliveries and failures', () => {
    const status = [
      { stream: 'audit', pending: 2, pending_error_codes: ['timeout'] }
    ];
    expect(streamStatuses({ ...sink, status })).toEqual(status);
    expect(streamStatuses(sink)).toEqual([]);
    expect(streamState({})).toBe('unknown');
    expect(
      streamState({ pending: 0, pending_error_codes: ['old-error'] })
    ).toBe('idle');
    expect(streamState({ pending: 2 })).toBe('pending');
    expect(streamState(status[0])).toBe('failing');
  });

  it('calculates pending delivery lag and clamps future or invalid timestamps', () => {
    vi.useFakeTimers();
    vi.setSystemTime('2026-01-01T00:05:00Z');
    expect(lagSeconds({})).toBe(0);
    expect(lagSeconds({ oldest_pending_at: 'invalid' })).toBe(0);
    expect(lagSeconds({ oldest_pending_at: '2026-01-01T00:06:00Z' })).toBe(0);
    expect(lagSeconds({ oldest_pending_at: '2026-01-01T00:03:00Z' })).toBe(120);
  });

  it('summarizes the settings relevant to each delivery format', () => {
    expect(sinkSettingsSummary(sink)).toBe('');
    expect(
      sinkSettingsSummary({
        ...sink,
        format: 'csv_gzip',
        settings: { cost_basis: 'charge', start_date: '2026-01-01' }
      })
    ).toBe('Cost basis: charge · from 2026-01-01');
    expect(sinkSettingsSummary({ ...sink, format: 'csv_gzip' })).toBe(
      'Cost basis: cost'
    );
    expect(
      sinkSettingsSummary({
        ...sink,
        format: 'cloudevents',
        settings: { source: 'gateway' }
      })
    ).toBe('Source: gateway');
    expect(
      sinkSettingsSummary({
        ...sink,
        format: 'stripe',
        settings: {
          event_name: 'tokens',
          value: 'amount',
          customers: { first: 'customer-1' }
        }
      })
    ).toBe('Meter: tokens (amount) · 1 customer');
    expect(
      sinkSettingsSummary({ ...sink, format: 'stripe', settings: {} })
    ).toBe('Meter:  () · 0 customers');
  });
});

it('presents current and pending datasets while prioritizing delivery errors', () => {
  expect(focusDatasetStatus(dataset)).toBe('Restatement pending');
  expect(focusDatasetStatus({ ...dataset, current: true })).toBe('Current');
  expect(
    focusDatasetStatus({ ...dataset, current: true, last_error_code: 'failed' })
  ).toBe('failed');
});
