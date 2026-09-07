import { ApiProblem } from '$lib/api/http';

/** How long a validated session is trusted before a mutation revalidates it. */
export const SESSION_FRESHNESS_MS = 60_000;

/**
 * Whether a mutation may proceed on the last validation alone: the session
 * was validated recently and a CSRF token is still held.
 */
export function sessionIsFresh(
  lastValidatedAt: number | undefined,
  hasCsrfToken: boolean,
  now = Date.now()
): boolean {
  return hasCsrfToken && now - (lastValidatedAt ?? 0) <= SESSION_FRESHNESS_MS;
}

export function unauthorizedError(error: unknown): boolean {
  return error instanceof ApiProblem && error.problem.status === 401;
}

export function abortError(error: unknown): boolean {
  return error instanceof Error && error.name === 'AbortError';
}
