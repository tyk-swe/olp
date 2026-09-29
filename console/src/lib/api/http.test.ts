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

  it('throws the problem document of a failed response', () => {
    let caught: unknown;
    try {
      unwrap({
        error: { title: 'Not found', status: 404, detail: 'No such route.' },
        response: new Response(null, { status: 404 })
      });
    } catch (error) {
      caught = error;
    }
    expect(caught).toBeInstanceOf(ApiProblem);
    expect((caught as ApiProblem).problem).toMatchObject({
      status: 404,
      detail: 'No such route.'
    });
  });

  it('fails closed when a successful response omits its body', () => {
    let caught: unknown;
    try {
      unwrap({ response: new Response(null, { status: 200 }) });
    } catch (error) {
      caught = error;
    }
    expect(caught).toBeInstanceOf(ApiProblem);
    expect((caught as ApiProblem).problem).toEqual({
      type: 'urn:olp:problem:invalid-api-response',
      title: 'The API response did not include the expected JSON body',
      status: 502
    });
  });
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
