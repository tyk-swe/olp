import { describe, expect, it } from 'vitest';
import {
  estimationError,
  presentEstimate,
  type EstimateTotals
} from './estimatePresentation';

const totals: EstimateTotals = {
  estimated_input_tokens: '120',
  reported_input_tokens: '100',
  estimated_attempt_count: 2
};

describe('usage estimate presentation', () => {
  it('compares estimates and reported input over the same attempts', () => {
    expect(estimationError(totals)).toBe(0.2);
    expect(presentEstimate(totals)).toEqual({
      error: '+20.0%',
      note: 'Admission over-estimated input.',
      comparison: '120 / 100',
      sample: '2 attempts',
      hasSample: true
    });
  });

  it('identifies under-estimates and exact matches', () => {
    expect(
      presentEstimate({
        ...totals,
        estimated_input_tokens: '80',
        estimated_attempt_count: 1
      })
    ).toEqual({
      error: '-20.0%',
      note: 'Admission under-estimated input.',
      comparison: '80 / 100',
      sample: '1 attempt',
      hasSample: true
    });
    expect(
      presentEstimate({ ...totals, estimated_input_tokens: '100' })
    ).toMatchObject({
      error: '0.0%',
      note: 'Estimates matched reported input.'
    });
  });

  it('reports a match when the difference rounds to zero', () => {
    expect(
      presentEstimate({ ...totals, estimated_input_tokens: '100.001' })
    ).toMatchObject({
      error: '0.0%',
      note: 'Estimates matched reported input.'
    });
  });

  it('distinguishes no comparable attempts from attempts with no reported tokens', () => {
    const noAttempts = { ...totals, estimated_attempt_count: 0 };
    expect(estimationError(noAttempts)).toBeNull();
    expect(presentEstimate(noAttempts)).toEqual({
      error: '—',
      note: 'No attempt had both an estimate and reported input usage.',
      comparison: '—',
      sample: 'No attempts',
      hasSample: false
    });

    const noReportedTokens = { ...totals, reported_input_tokens: '0' };
    expect(estimationError(noReportedTokens)).toBeNull();
    expect(presentEstimate(noReportedTokens)).toEqual({
      error: '—',
      note: 'The providers reported no input tokens for these attempts.',
      comparison: '120 / 0',
      sample: '2 attempts',
      hasSample: true
    });
  });

  it.each([
    { ...totals, estimated_input_tokens: 'not-a-number' },
    { ...totals, reported_input_tokens: 'not-a-number' },
    { ...totals, estimated_input_tokens: 'Infinity' },
    { ...totals, reported_input_tokens: '-1' },
    { ...totals, estimated_attempt_count: -1 }
  ])(
    'does not calculate a ratio without valid comparable totals %#',
    (input) => {
      expect(estimationError(input)).toBeNull();
    }
  );
});
