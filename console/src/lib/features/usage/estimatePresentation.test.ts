import { describe, expect, it } from 'vitest';
import {
  estimationError,
  presentEstimate,
  type EstimateTotals
} from './estimatePresentation';

function totals(
  estimated: string,
  reported: string,
  attempts: number
): EstimateTotals {
  return {
    estimated_input_tokens: estimated,
    reported_input_tokens: reported,
    estimated_attempt_count: attempts
  };
}

describe('estimation error', () => {
  it('is positive when admission over-estimated and negative when under', () => {
    expect(estimationError(totals('1100', '1000', 4))).toBeCloseTo(0.1);
    expect(estimationError(totals('900', '1000', 4))).toBeCloseTo(-0.1);
    expect(estimationError(totals('1000', '1000', 4))).toBe(0);
  });

  it('is unknown without a denominator or a sample', () => {
    expect(estimationError(totals('0', '0', 0))).toBeNull();
    expect(estimationError(totals('50', '0', 2))).toBeNull();
    expect(estimationError(totals('50', '100', 0))).toBeNull();
    expect(estimationError(totals('not-a-number', '100', 1))).toBeNull();
  });

  it('keeps counts beyond the safe integer range as a ratio', () => {
    expect(
      estimationError(totals('18446744073709551616', '9223372036854775808', 9))
    ).toBeCloseTo(1);
  });
});

describe('estimate presentation', () => {
  it('signs the error and states both sums and the sample', () => {
    expect(presentEstimate(totals('1032', '1000', 12))).toEqual({
      error: '+3.2%',
      note: 'Admission over-estimated input.',
      comparison: '1,032 / 1,000',
      sample: '12 attempts',
      hasSample: true
    });
    const under = presentEstimate(totals('9600', '10000', 1));
    expect(under.error).toBe('-4.0%');
    expect(under.note).toBe('Admission under-estimated input.');
    expect(under.comparison).toBe('9,600 / 10,000');
    expect(under.sample).toBe('1 attempt');
  });

  it('reports a match without a sign', () => {
    const match = presentEstimate(totals('10000', '10000', 3));
    expect(match.error).toBe('0.0%');
    expect(match.note).toBe('Estimates matched reported input.');
    expect(presentEstimate(totals('100001', '100000', 3)).error).toBe('0.0%');
  });

  it('shows a dash, never zero, when nothing could be compared', () => {
    expect(presentEstimate(totals('0', '0', 0))).toEqual({
      error: '—',
      note: 'No attempt had both an estimate and reported input usage.',
      comparison: '—',
      sample: 'No attempts',
      hasSample: false
    });
  });

  it('keeps the sums but shows no error when nothing was reported', () => {
    expect(presentEstimate(totals('50', '0', 2))).toEqual({
      error: '—',
      note: 'The providers reported no input tokens for these attempts.',
      comparison: '50 / 0',
      sample: '2 attempts',
      hasSample: true
    });
  });
});
