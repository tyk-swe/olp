import { describe, expect, it } from 'vitest';
import { ApiProblem, fieldIssues, isEtagMismatch, result } from '$lib/api/http';

// Mirrors the (unexported) problem type `isEtagMismatch` recognizes; keeping
// the literal here is what makes a silent rename of that constant fail.
const ETAG_MISMATCH_TYPE = 'https://openllmproxy.dev/problems/etag_mismatch';

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

describe('field errors', () => {
  it('keeps the field, code, and message together', () => {
    const body = {
      title: 'Validation failed',
      status: 422,
      errors: {
        endpoint: [{ code: 'required', message: 'Provide an endpoint.' }]
      }
    };
    let caught: unknown;
    try {
      result(undefined, body, new Response(null, { status: 422 }));
    } catch (error) {
      caught = error;
    }
    expect(fieldIssues(caught)).toEqual([
      { field: 'endpoint', code: 'required', message: 'Provide an endpoint.' }
    ]);
    expect(fieldIssues(new Error('network failure'))).toEqual([]);
  });

  it('ignores malformed field errors without losing the problem status', () => {
    let caught: unknown;
    try {
      result(
        undefined,
        {
          title: 'Validation failed',
          status: 422,
          errors: { endpoint: ['invalid shape'] }
        },
        new Response(null, { status: 422 })
      );
    } catch (error) {
      caught = error;
    }
    expect(caught).toBeInstanceOf(ApiProblem);
    expect(fieldIssues(caught)).toEqual([]);
    expect((caught as ApiProblem).problem.status).toBe(422);
  });
});

describe('query retry policy', () => {
  it('does not retry authentication, validation, conflicts or rate limits', async () => {
    const { retryQuery, ApiProblem } = await import('./http');
    for (const status of [400, 401, 403, 404, 409, 412, 422, 429, 500]) {
      expect(retryQuery(0, new ApiProblem({ status, title: 'Rejected' }))).toBe(
        false
      );
    }
    expect(retryQuery(0, new DOMException('Cancelled', 'AbortError'))).toBe(
      false
    );
    expect(
      retryQuery(0, new ApiProblem({ status: 503, title: 'Unavailable' }))
    ).toBe(true);
    expect(retryQuery(1, new TypeError('Network failure'))).toBe(false);
  });
});
