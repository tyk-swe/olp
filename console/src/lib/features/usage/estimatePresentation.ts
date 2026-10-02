import { formatInteger, formatSignedPercent } from '$lib/format';

/** The totals every usage report carries about admission estimates. They cover
 * only the attempts that had both an estimate and reported input usage, so the
 * two token sums are over the same attempts and compare directly. */
export type EstimateTotals = {
  estimated_input_tokens: string;
  reported_input_tokens: string;
  estimated_attempt_count: number;
};

export type EstimatePresentation = {
  /** The signed error as text: `+3.2%` when admission over-estimated, `-3.2%`
   * when it under-estimated, a dash when there is nothing to compare. */
  error: string;
  /** The same finding in words, for readers who cannot rely on a sign. */
  note: string;
  /** The two sums behind the error, estimated then reported, in full: compact
   * figures would hide the difference they exist to show. */
  comparison: string;
  /** How many attempts the error is drawn from. */
  sample: string;
  /** Whether any attempt had both an estimate and reported input usage, which
   * is when the sums and the sample are worth showing. An error can still be
   * unknown then, if the providers reported no input at all. */
  hasSample: boolean;
};

/**
 * The estimation error as a ratio: (estimated − reported) / reported. Null when
 * no attempt could be compared. A range with no reported input has no
 * denominator, so it has no error rather than an infinite one.
 */
export function estimationError(totals: EstimateTotals): number | null {
  const estimated = Number(totals.estimated_input_tokens);
  const reported = Number(totals.reported_input_tokens);
  if (
    totals.estimated_attempt_count <= 0 ||
    !Number.isFinite(estimated) ||
    !Number.isFinite(reported) ||
    reported <= 0
  )
    return null;
  return (estimated - reported) / reported;
}

export function presentEstimate(totals: EstimateTotals): EstimatePresentation {
  const ratio = estimationError(totals);
  const error = formatSignedPercent(ratio);
  const attempts = totals.estimated_attempt_count;
  if (attempts <= 0)
    return {
      error,
      note: 'No attempt had both an estimate and reported input usage.',
      comparison: '—',
      sample: 'No attempts',
      hasSample: false
    };
  const sample = `${formatInteger(attempts)} ${attempts === 1 ? 'attempt' : 'attempts'}`;
  const comparison = `${formatInteger(totals.estimated_input_tokens)} / ${formatInteger(totals.reported_input_tokens)}`;
  if (ratio === null)
    return {
      error,
      note: 'The providers reported no input tokens for these attempts.',
      comparison,
      sample,
      hasSample: true
    };
  // The formatter signs only a ratio that survives rounding, so the sign is
  // the verdict and a bare figure is a match.
  const note = error.startsWith('+')
    ? 'Admission over-estimated input.'
    : error.startsWith('-')
      ? 'Admission under-estimated input.'
      : 'Estimates matched reported input.';
  return { error, note, comparison, sample, hasSample: true };
}
