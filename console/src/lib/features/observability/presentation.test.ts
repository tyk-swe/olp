import { describe, expect, it, vi } from 'vitest';
import type { ExportSink, FocusDataset } from './api';
import {
  focusDatasetStatus,
  lagSeconds,
  sinkSettingsSummary,
  streamState,
  streamStatuses
} from './presentation';

describe('streamState', () => {
  it.each([
    [{}, 'unknown'],
    [{ pending_error_codes: ['timeout'] }, 'unknown'],
    [
      { pending: 0, pending_error_codes: ['timeout'], failures_total: 5 },
      'idle'
    ],
    [{ pending: 1, failures_total: 5, gaps_total: 2 }, 'pending'],
    [{ pending: 1, pending_error_codes: [] }, 'pending'],
    [{ pending: 1, pending_error_codes: ['timeout'] }, 'failing']
  ])(
    'prioritizes pending counts and current errors for %j',
    (status, expected) => {
      expect(streamState(status)).toBe(expected);
    }
  );
});

it('handles missing, invalid and future lag timestamps', () => {
  vi.spyOn(Date, 'now').mockReturnValue(Date.parse('2026-10-14T12:00:00Z'));
  expect(lagSeconds({})).toBe(0);
  expect(lagSeconds({ oldest_pending_at: 'invalid' })).toBe(0);
  expect(lagSeconds({ oldest_pending_at: '2026-10-14T12:00:01Z' })).toBe(0);
  expect(lagSeconds({ oldest_pending_at: '2026-10-14T11:59:58.500Z' })).toBe(
    1.5
  );
});

it('treats absent stream statuses as empty', () => {
  expect(streamStatuses({} as ExportSink)).toEqual([]);
});

it.each([
  [{ current: true, last_error_code: 'status' }, 'status'],
  [{ current: false, last_error_code: 'timeout' }, 'timeout'],
  [{ current: true, last_error_code: null }, 'Current'],
  [{ current: false, last_error_code: null }, 'Restatement pending']
])(
  'shows dataset errors before the current flag for %j',
  (dataset, expected) => {
    expect(focusDatasetStatus(dataset as FocusDataset)).toBe(expected);
  }
);

it.each([
  [{ format: 'json' }, ''],
  [{ format: 'csv_gzip' }, 'Cost basis: cost'],
  [
    {
      format: 'csv_gzip',
      settings: { cost_basis: 'charge', start_date: '2026-10-01' }
    },
    'Cost basis: charge · from 2026-10-01'
  ],
  [{ format: 'cloudevents' }, ''],
  [{ format: 'cloudevents', settings: { source: '/olp' } }, 'Source: /olp'],
  [
    {
      format: 'stripe',
      settings: {
        event_name: 'olp.charge',
        value: 'cost',
        customers: { one: 'cus_1' }
      }
    },
    'Meter: olp.charge (cost) · 1 customer'
  ],
  [{ format: 'stripe' }, 'Meter:  () · 0 customers']
])('summarizes settings for %j', (sink, expected) => {
  expect(sinkSettingsSummary(sink as ExportSink)).toBe(expected);
});
