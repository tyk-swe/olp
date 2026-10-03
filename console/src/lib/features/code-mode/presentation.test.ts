import { describe, expect, it } from 'vitest';
import { ApiProblem } from '$lib/api/http';
import {
  nativeModels,
  tokenLimit,
  tokenCount,
  mutationError,
  utcWindowStart,
  reservationLabel,
  refusalAdvice
} from './presentation';
import { attempt } from './test/fixtures';

describe('native-model and token-budget inputs', () => {
  it('preserves exact native models and accepts safe optional integer budgets', () => {
    expect(nativeModels('Provider/model-v1\nother_model:2')).toEqual([
      'Provider/model-v1',
      'other_model:2'
    ]);
    expect(tokenLimit('')).toBeNull();
    expect(tokenLimit('9007199254740991')).toBe(Number.MAX_SAFE_INTEGER);
  });
  it.each(['x x', '*', 'native model ?', ''])(
    'rejects ambiguous models: %s',
    (input) => expect(() => nativeModels(input)).toThrow()
  );
  it.each(['0', '-1', '1.2', '1e6', 'NaN', '9007199254740992'])(
    'refuses unsafe token limits: %s',
    (input) => expect(() => tokenLimit(input)).toThrow()
  );
  it('distinguishes zero from unknown and displays UTC budget boundaries', () => {
    expect(tokenCount(null)).toBe('Unknown');
    expect(tokenCount(undefined)).toBe('Unknown');
    expect(tokenCount(0)).toBe('0');
    expect(utcWindowStart('2026-10-01T00:00:00Z')).toContain('10/1/2026');
    expect(utcWindowStart('2026-10-01T00:00:00Z')).toContain('UTC');
  });
  it('preserves draft-conflict guidance and field-specific validation', () => {
    expect(
      mutationError(new ApiProblem({ title: 'Conflict', status: 412 }))
    ).toContain('Your edits have been kept');
    expect(
      mutationError(
        new ApiProblem({
          title: 'Invalid',
          status: 422,
          errors: {
            credential_id: [
              { code: 'invalid', message: 'Principal cannot change.' }
            ]
          }
        })
      )
    ).toBe('credential_id: Principal cannot change.');
  });
  it('distinguishes pending, uncertain and released reservations', () => {
    expect(reservationLabel({ ...attempt, state: 'prepared' })).toBe(
      'Pending reservation'
    );
    expect(reservationLabel(attempt)).toBe('Retained uncertain reservation');
    expect(reservationLabel({ ...attempt, state: 'settled' })).toBe(
      'Original reservation'
    );
    expect(refusalAdvice('code_tree_retired')).toContain('permanently retired');
    expect(refusalAdvice('code_account_unavailable')).toContain(
      'Resume cannot switch accounts'
    );
    expect(refusalAdvice('code_budget_exhausted')).toContain(
      'Missing usage is not zero'
    );
  });
});
