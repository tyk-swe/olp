import type { UsageCompleteness } from '$lib/api/operations';

export type UsageCompletenessPresentation =
  | { kind: 'complete' }
  | {
      kind: 'warning';
      danger: boolean;
      title: string;
      detail: string;
    };

/**
 * Converts accounting health into the single, highest-priority operator
 * message. Keep this precedence explicit: showing a pricing warning must not
 * hide a stale worker or known data loss.
 */
export function presentUsageCompleteness(
  completeness: UsageCompleteness
): UsageCompletenessPresentation {
  if (completeness.complete && completeness.unpriced_count === 0) {
    return { kind: 'complete' };
  }

  const consumerState = completeness.request_metadata_consumer.state;
  let title: string;
  if (consumerState === 'stale') {
    title = 'Request metadata worker heartbeat is stale';
  } else if (consumerState === 'backlogged') {
    title = 'Request metadata persistence backlog detected';
  } else if (consumerState === 'unknown') {
    title = 'Request metadata worker has not reported';
  } else if (!completeness.coverage.range_complete) {
    title = 'Retained boundary data was excluded';
  } else if (completeness.uncertain_request_metadata_gap_count > 0) {
    title = 'Unclean request metadata gateway epochs make usage uncertain';
  } else if (completeness.request_metadata_gap_events > 0) {
    title = 'Request metadata persistence gaps detected';
  } else if (completeness.incomplete_count > 0) {
    title = 'Usage is still reconciling';
  } else {
    title = 'Some traffic is unpriced';
  }

  return {
    kind: 'warning',
    danger:
      consumerState === 'stale' ||
      completeness.request_metadata_gap_events > 0 ||
      completeness.uncertain_request_metadata_gap_count > 0,
    title,
    detail: `${completeness.request_metadata_gap_events} request metadata gap-event lower bound · ${completeness.uncertain_request_metadata_gap_count} uncertain request metadata gateway epochs · ${completeness.incomplete_count} incomplete requests · ${completeness.unpriced_count} unpriced requests. Cost totals exclude anything unpriced and never treat uncertainty as zero.`
  };
}
