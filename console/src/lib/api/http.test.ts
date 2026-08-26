import { describe, expect, it } from 'vitest';
import { ApiProblem, fieldIssues, isEtagMismatch, result } from './http';

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

describe('fieldIssues', () => {
  it('pairs each message with the code recorded at the same position', () => {
    const problem = new ApiProblem({
      type: 'https://openllmproxy.dev/problems/validation_failed',
      title: 'Validation failed',
      status: 422,
      errors: {
        endpoint: ['Provide a base endpoint URL.'],
        cloud_region: ['This connector does not accept a region.']
      },
      errorCodes: { endpoint: ['required'], cloud_region: ['forbidden'] }
    });

    expect(fieldIssues(problem)).toEqual([
      { field: 'endpoint', message: 'Provide a base endpoint URL.', code: 'required' },
      {
        field: 'cloud_region',
        message: 'This connector does not accept a region.',
        code: 'forbidden'
      }
    ]);
  });

  it('keeps a message without a code rather than borrowing a neighbour', () => {
    const problem = new ApiProblem({
      title: 'Validation failed',
      status: 422,
      errors: { credential: ['Provide a credential no larger than 8 KiB.'] }
    });

    expect(fieldIssues(problem)).toEqual([
      {
        field: 'credential',
        message: 'Provide a credential no larger than 8 KiB.',
        code: undefined
      }
    ]);
    expect(fieldIssues(new Error('network failure'))).toEqual([]);
  });

  it('reads the empty padding code as uncoded rather than as a classification', () => {
    const problem = new ApiProblem({
      title: 'Validation failed',
      status: 422,
      errors: { occurred_after: ['Provide a start no later than the end.'] },
      errorCodes: { occurred_after: [''] }
    });

    expect(fieldIssues(problem)).toEqual([
      {
        field: 'occurred_after',
        message: 'Provide a start no later than the end.',
        code: undefined
      }
    ]);
  });
});

describe('problem parsing', () => {
  it('reads the field codes a 422 sends alongside its messages', () => {
    const body = {
      type: 'https://openllmproxy.dev/problems/validation_failed',
      title: 'Validation failed',
      status: 422,
      detail: 'One or more fields are invalid.',
      errors: { endpoint: ['Provide a base endpoint URL.'] },
      error_codes: { endpoint: ['required'] }
    };

    let caught: unknown;
    try {
      result(undefined, body, new Response(null, { status: 422 }));
    } catch (error) {
      caught = error;
    }

    expect(caught).toBeInstanceOf(ApiProblem);
    expect(fieldIssues(caught)).toEqual([
      { field: 'endpoint', message: 'Provide a base endpoint URL.', code: 'required' }
    ]);
  });

  it('ignores field codes that are not string lists', () => {
    let caught: unknown;
    try {
      result(
        undefined,
        {
          title: 'Validation failed',
          status: 422,
          errors: { endpoint: ['Provide a base endpoint URL.'] },
          error_codes: { endpoint: 'required' }
        },
        new Response(null, { status: 422 })
      );
    } catch (error) {
      caught = error;
    }

    expect(fieldIssues(caught)).toEqual([
      { field: 'endpoint', message: 'Provide a base endpoint URL.', code: undefined }
    ]);
  });
});
