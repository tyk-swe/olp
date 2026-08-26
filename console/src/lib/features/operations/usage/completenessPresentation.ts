import type { UsageCompleteness } from '$lib/api/usage';

export type UsageCompletenessPresentation =
  | { kind: 'complete' }
  | {
      kind: 'warning';
      danger: boolean;
      title: string;
      detail: string;
    };

/**
 * An approximate range was answered from hourly rollups with partial boundary
 * hours left out, so its totals are a floor rather than an exact count. Saying
 * so is the difference between an operator trusting a number and checking it.
 */
function approximateNote(completeness: UsageCompleteness): string {
  if (!completeness.coverage.approximate) return '';
  const excluded = completeness.coverage.excluded_partial_aggregate_boundaries;
  return ` Totals are approximate: ${excluded} partial retained-hour ${excluded === 1 ? 'boundary is' : 'boundaries are'} excluded.`;
}

/**
 * Converts accounting health into the single, highest-priority operator
 * message. Keep this precedence explicit: showing a pricing warning must not
 * hide a stale worker or known data loss.
 */
export function presentUsageCompleteness(
  completeness: UsageCompleteness
): UsageCompletenessPresentation {
  if (
    completeness.complete
    && completeness.unpriced_count === 0
    && !completeness.coverage.approximate
  ) {
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
    detail: `${completeness.request_metadata_gap_events} request metadata gap-event lower bound · ${completeness.uncertain_request_metadata_gap_count} uncertain request metadata gateway epochs · ${completeness.incomplete_count} incomplete requests · ${completeness.priced_count} priced and ${completeness.unpriced_count} unpriced requests.${approximateNote(completeness)} Cost totals exclude anything unpriced and never treat uncertainty as zero.`
  };
}
