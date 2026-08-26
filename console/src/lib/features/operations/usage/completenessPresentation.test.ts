import { describe, expect, it } from 'vitest';
import type { UsageCompleteness } from '$lib/api/usage';
import { presentUsageCompleteness } from './completenessPresentation';

const completeUsage: UsageCompleteness = {
  complete: true,
  coverage: {
    approximate: false,
    excluded_partial_aggregate_boundaries: 0,
    range_complete: true
  },
  incomplete_count: 0,
  priced_count: 12,
  request_count: 12,
  request_metadata_consumer: {
    lag_events: 0,
    pending_events: 0,
    state: 'healthy'
  },
  request_metadata_gap_events: 0,
  uncertain_request_metadata_gap_count: 0,
  unpriced_count: 0
};

function incompleteUsage(
  changes: Partial<UsageCompleteness> = {}
): UsageCompleteness {
  return { ...completeUsage, complete: false, unpriced_count: 1, ...changes };
}

describe('presentUsageCompleteness', () => {
  it('uses the compact success presentation only when usage and pricing are complete', () => {
    expect(presentUsageCompleteness(completeUsage)).toEqual({
      kind: 'complete'
    });
    expect(
      presentUsageCompleteness({ ...completeUsage, unpriced_count: 1 }).kind
    ).toBe('warning');
  });

  it('names the approximate range and the priced share in the detail', () => {
    const presentation = presentUsageCompleteness(
      incompleteUsage({
        coverage: {
          approximate: true,
          excluded_partial_aggregate_boundaries: 2,
          range_complete: false
        },
        priced_count: 11,
        unpriced_count: 1
      })
    );
    expect(presentation.kind).toBe('warning');
    if (presentation.kind !== 'warning') return;
    expect(presentation.detail).toContain('11 priced and 1 unpriced requests');
    expect(presentation.detail).toContain(
      'Totals are approximate: 2 partial retained-hour boundaries are excluded.'
    );
  });

  it('uses the singular boundary wording for one excluded hour', () => {
    const presentation = presentUsageCompleteness(
      incompleteUsage({
        coverage: {
          approximate: true,
          excluded_partial_aggregate_boundaries: 1,
          range_complete: false
        }
      })
    );
    expect(presentation.kind === 'warning' && presentation.detail).toContain(
      '1 partial retained-hour boundary is excluded.'
    );
  });

  it('never claims exact totals for an approximate range', () => {
    expect(
      presentUsageCompleteness({
        ...completeUsage,
        coverage: {
          approximate: true,
          excluded_partial_aggregate_boundaries: 1,
          range_complete: false
        }
      }).kind
    ).toBe('warning');
  });

  it('omits the approximation note for an exact range', () => {
    const presentation = presentUsageCompleteness(incompleteUsage());
    expect(
      presentation.kind === 'warning' && presentation.detail
    ).not.toContain('approximate');
  });

  it.each<[string, UsageCompleteness, string, boolean]>([
    [
      'stale consumer before every other condition',
      incompleteUsage({
        coverage: { ...completeUsage.coverage, range_complete: false },
        request_metadata_consumer: {
          ...completeUsage.request_metadata_consumer,
          state: 'stale'
        },
        request_metadata_gap_events: 1,
        uncertain_request_metadata_gap_count: 1,
        incomplete_count: 1
      }),
      'Request metadata worker heartbeat is stale',
      true
    ],
    [
      'consumer backlog before range and accounting conditions',
      incompleteUsage({
        coverage: { ...completeUsage.coverage, range_complete: false },
        request_metadata_consumer: {
          ...completeUsage.request_metadata_consumer,
          state: 'backlogged'
        },
        incomplete_count: 1
      }),
      'Request metadata persistence backlog detected',
      false
    ],
    [
      'unknown consumer before range conditions',
      incompleteUsage({
        coverage: { ...completeUsage.coverage, range_complete: false },
        request_metadata_consumer: {
          ...completeUsage.request_metadata_consumer,
          state: 'unknown'
        }
      }),
      'Request metadata worker has not reported',
      false
    ],
    [
      'incomplete range before gateway uncertainty',
      incompleteUsage({
        coverage: { ...completeUsage.coverage, range_complete: false },
        uncertain_request_metadata_gap_count: 1
      }),
      'Retained boundary data was excluded',
      true
    ],
    [
      'gateway uncertainty before known gap events',
      incompleteUsage({
        request_metadata_gap_events: 1,
        uncertain_request_metadata_gap_count: 1
      }),
      'Unclean request metadata gateway epochs make usage uncertain',
      true
    ],
    [
      'known gap events before reconciling requests',
      incompleteUsage({
        request_metadata_gap_events: 1,
        incomplete_count: 1
      }),
      'Request metadata persistence gaps detected',
      true
    ],
    [
      'reconciling requests before pricing',
      incompleteUsage({ incomplete_count: 1 }),
      'Usage is still reconciling',
      false
    ],
    [
      'pricing as the final fallback',
      incompleteUsage(),
      'Some traffic is unpriced',
      false
    ]
  ])('prioritizes %s', (_label, completeness, title, danger) => {
    expect(presentUsageCompleteness(completeness)).toMatchObject({
      kind: 'warning',
      title,
      danger
    });
  });
});
