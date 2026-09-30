import { describe, expect, it } from 'vitest';
import {
  ApiProblem,
  ensureOk,
  fieldIssues,
  isEtagMismatch,
  unwrap,
  unwrapPage
} from '$lib/api/http';

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

describe('unwrap', () => {
  it('returns the decoded body of a successful response', () => {
    expect(
      unwrap({
        data: { id: 'one' },
        response: new Response(null, { status: 200 })
      })
    ).toEqual({ id: 'one' });
  });

  it('preserves structured problem details and their declared status', () => {
    const problem = {
      type: 'urn:olp:problem:rate-limited',
      title: 'Rate limited',
      detail: 'Retry after the window',
      status: 429,
      instance: '/api/v1/requests',
      errors: {
        request: [
          { code: 'invalid', message: 'Retry after the advertised window.' }
        ]
      }
    };
    let caught: unknown;
    try {
      unwrap({ error: problem, response: new Response(null, { status: 503 }) });
    } catch (error) {
      caught = error;
    }
    expect(caught).toBeInstanceOf(ApiProblem);
    expect((caught as ApiProblem).problem).toEqual(problem);
  });

  it.each(['gateway unavailable', { status: '503', title: false }, null])(
    'falls back to the response status for unstructured error %s',
    (error) => {
      expect(() =>
        unwrap({ error, response: new Response(null, { status: 503 }) })
      ).toThrow(
        expect.objectContaining({
          problem: expect.objectContaining({
            type: 'about:blank',
            title: 'Request failed (503)',
            status: 503
          })
        })
      );
    }
  );

  it.each([undefined, null])(
    'fails closed when a successful response has body %s',
    (data) => {
      let caught: unknown;
      try {
        unwrap({ data, response: new Response(null, { status: 200 }) });
      } catch (error) {
        caught = error;
      }
      expect(caught).toBeInstanceOf(ApiProblem);
      expect((caught as ApiProblem).problem).toEqual({
        type: 'urn:olp:problem:invalid-api-response',
        title: 'The API response did not include the expected JSON body',
        status: 502
      });
    }
  );
});

describe('unwrapPage', () => {
  it('maps the wire envelope to a cursor page', () => {
    expect(
      unwrapPage({
        data: { items: [{ id: 'a' }], next_cursor: 'next' },
        response: new Response(null, { status: 200 })
      })
    ).toEqual({ items: [{ id: 'a' }], nextCursor: 'next' });
    expect(
      unwrapPage({
        data: { items: [] },
        response: new Response(null, { status: 200 })
      })
    ).toEqual({ items: [], nextCursor: null });
  });

  it('throws the problem document of a failed response', () => {
    let caught: unknown;
    try {
      unwrapPage({
        error: { title: 'Gone', status: 410 },
        response: new Response(null, { status: 410 })
      });
    } catch (error) {
      caught = error;
    }
    expect(caught).toBeInstanceOf(ApiProblem);
    expect((caught as ApiProblem).problem.status).toBe(410);
  });
});

describe('ensureOk', () => {
  it('returns nothing on success and throws the problem on failure', () => {
    expect(() =>
      ensureOk({ response: new Response(null, { status: 204 }) })
    ).not.toThrow();
    let caught: unknown;
    try {
      ensureOk({
        error: { title: 'Conflict', status: 409 },
        response: new Response(null, { status: 409 })
      });
    } catch (error) {
      caught = error;
    }
    expect(caught).toBeInstanceOf(ApiProblem);
    expect((caught as ApiProblem).problem.status).toBe(409);
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
      unwrap({ error: body, response: new Response(null, { status: 422 }) });
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
      unwrap({
        error: {
          title: 'Validation failed',
          status: 422,
          errors: { endpoint: ['invalid shape'] }
        },
        response: new Response(null, { status: 422 })
      });
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
