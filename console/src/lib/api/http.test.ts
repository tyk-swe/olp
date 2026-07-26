import { describe, expect, it } from 'vitest';
import {
  ApiProblem,
  ETAG_MISMATCH_TYPE,
  isEtagMismatch,
  problemFieldErrors,
  problemMessage
} from './http';

describe('isEtagMismatch', () => {
  it('recognizes only the typed 412 problem', () => {
    expect(
      isEtagMismatch(
        new ApiProblem({
          type: ETAG_MISMATCH_TYPE,
          title: 'Precondition failed',
          status: 412
        })
      )
    ).toBe(true);
    expect(
      isEtagMismatch(
        new ApiProblem({
          type: ETAG_MISMATCH_TYPE,
          title: 'Conflict',
          status: 409
        })
      )
    ).toBe(false);
    expect(
      isEtagMismatch(
        new ApiProblem({
          type: 'https://openllmproxy.dev/problems/idempotency_conflict',
          title: 'Conflict',
          status: 412
        })
      )
    ).toBe(false);
    expect(isEtagMismatch(new Error('network failure'))).toBe(false);
  });
});

describe('problemMessage', () => {
  const validation = () =>
    new ApiProblem({
      title: 'Validation failed',
      status: 422,
      detail: 'One or more fields are invalid.',
      errors: {
        requests_per_minute: ['Use a limit no greater than 2147483647 or null.'],
        expires_at: ['Expiration must be in the future or null.']
      }
    });

  it('surfaces the field reasons a validation problem hides in `errors`', () => {
    // Without these the operator only ever sees "One or more fields are
    // invalid." with no indication of which field or why.
    const message = problemMessage(validation(), 'fallback');
    expect(message).toContain('One or more fields are invalid.');
    expect(message).toContain('requests_per_minute: Use a limit no greater than 2147483647 or null.');
    expect(message).toContain('expires_at: Expiration must be in the future or null.');
  });

  it('flattens the errors map to the first message per field', () => {
    expect(problemFieldErrors(validation())).toEqual({
      requests_per_minute: 'Use a limit no greater than 2147483647 or null.',
      expires_at: 'Expiration must be in the future or null.'
    });
    expect(problemFieldErrors(new Error('boom'))).toEqual({});
  });

  it('falls back for problems without a field map and for non-problems', () => {
    expect(
      problemMessage(new ApiProblem({ title: 'Not found', status: 404 }), 'fallback')
    ).toBe('Not found');
    expect(problemMessage(new Error('offline'), 'fallback')).toBe('offline');
    expect(problemMessage('nope', 'fallback')).toBe('fallback');
  });
});
