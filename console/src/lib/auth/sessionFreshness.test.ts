import { describe, expect, it } from 'vitest';
import { ApiProblem } from '$lib/api/http';
import {
  SESSION_FRESHNESS_MS,
  abortError,
  sessionIsFresh,
  unauthorizedError
} from './sessionFreshness';

describe('sessionIsFresh', () => {
  const now = 1_000_000;

  it('trusts a recent validation only while a CSRF token is held', () => {
    expect(sessionIsFresh(now - 1_000, true, now)).toBe(true);
    expect(sessionIsFresh(now - 1_000, false, now)).toBe(false);
  });

  it('expires exactly after the freshness window', () => {
    expect(sessionIsFresh(now - SESSION_FRESHNESS_MS, true, now)).toBe(true);
    expect(sessionIsFresh(now - SESSION_FRESHNESS_MS - 1, true, now)).toBe(
      false
    );
  });

  it('treats a never-validated session as stale', () => {
    expect(sessionIsFresh(undefined, true, now)).toBe(false);
  });
});

describe('error classification', () => {
  it('recognises a 401 problem and an abort', () => {
    expect(
      unauthorizedError(new ApiProblem({ title: 'nope', status: 401 }))
    ).toBe(true);
    expect(
      unauthorizedError(new ApiProblem({ title: 'nope', status: 403 }))
    ).toBe(false);
    expect(abortError(new DOMException('stop', 'AbortError'))).toBe(true);
    expect(abortError(new Error('stop'))).toBe(false);
  });
});
