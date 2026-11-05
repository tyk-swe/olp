import { describe, expect, it } from 'vitest';
import type { UsageCompleteness } from './usage';
import { presentUsageCompleteness } from './completenessPresentation';

const complete: UsageCompleteness = {
  complete: true,
  coverage: {
    approximate: false,
    excluded_partial_aggregate_boundaries: 0,
    range_complete: true
  },
  request_count: 5,
  priced_count: 5,
  unpriced_count: 0,
  incomplete_count: 0,
  request_metadata_gap_events: 0,
  uncertain_request_metadata_gap_count: 0,
  request_metadata_consumer: {
    state: 'healthy',
    lag_events: 0,
    pending_events: 0
  }
};

const incomplete = { ...complete, complete: false };

describe('usage completeness presentation', () => {
  it('reports complete only when the range is exact and every request is priced', () => {
    expect(presentUsageCompleteness(complete)).toEqual({ kind: 'complete' });
    expect(
      presentUsageCompleteness({ ...complete, unpriced_count: 1 })
    ).toMatchObject({ kind: 'warning' });
    expect(
      presentUsageCompleteness({
        ...complete,
        coverage: { ...complete.coverage, approximate: true }
      })
    ).toMatchObject({ kind: 'warning' });
  });

  it.each([
    {
      input: {
        ...incomplete,
        request_metadata_consumer: {
          ...complete.request_metadata_consumer,
          state: 'stale'
        },
        request_metadata_gap_events: 2
      },
      title: 'Request metadata worker heartbeat is stale',
      danger: true
    },
    {
      input: {
        ...incomplete,
        request_metadata_consumer: {
          ...complete.request_metadata_consumer,
          state: 'backlogged'
        },
        coverage: { ...complete.coverage, range_complete: false }
      },
      title: 'Request metadata persistence backlog detected',
      danger: false
    },
    {
      input: {
        ...incomplete,
        request_metadata_consumer: {
          ...complete.request_metadata_consumer,
          state: 'unknown'
        },
        uncertain_request_metadata_gap_count: 1
      },
      title: 'Request metadata worker has not reported',
      danger: true
    },
    {
      input: {
        ...incomplete,
        coverage: { ...complete.coverage, range_complete: false },
        uncertain_request_metadata_gap_count: 1
      },
      title: 'Retained boundary data was excluded',
      danger: true
    },
    {
      input: {
        ...incomplete,
        uncertain_request_metadata_gap_count: 1,
        request_metadata_gap_events: 2
      },
      title: 'Unclean request metadata gateway epochs make usage uncertain',
      danger: true
    },
    {
      input: {
        ...incomplete,
        request_metadata_gap_events: 2,
        incomplete_count: 1
      },
      title: 'Request metadata persistence gaps detected',
      danger: true
    },
    {
      input: { ...incomplete, incomplete_count: 1, unpriced_count: 1 },
      title: 'Usage is still reconciling',
      danger: false
    },
    {
      input: { ...incomplete, unpriced_count: 1 },
      title: 'Some traffic is unpriced',
      danger: false
    }
  ])(
    'prioritizes $title and marks the severity',
    ({ input, title, danger }) => {
      expect(presentUsageCompleteness(input)).toMatchObject({
        kind: 'warning',
        title,
        danger
      });
    }
  );

  it.each([
    [1, '1 partial retained-hour boundary is excluded'],
    [2, '2 partial retained-hour boundaries are excluded']
  ])(
    'explains %i excluded partial boundaries in the warning',
    (excluded, text) => {
      const result = presentUsageCompleteness({
        ...incomplete,
        request_metadata_gap_events: 2,
        uncertain_request_metadata_gap_count: 3,
        incomplete_count: 4,
        priced_count: 5,
        unpriced_count: 6,
        coverage: {
          ...complete.coverage,
          approximate: true,
          excluded_partial_aggregate_boundaries: excluded
        }
      });

      expect(result).toMatchObject({ kind: 'warning' });
      if (result.kind !== 'warning') return;
      expect(result.detail).toContain(text);
      expect(result.detail).toContain(
        '2 request metadata gap-event lower bound'
      );
      expect(result.detail).toContain(
        '3 uncertain request metadata gateway epochs'
      );
      expect(result.detail).toContain('4 incomplete requests');
      expect(result.detail).toContain('5 priced and 6 unpriced requests');
    }
  );
});
