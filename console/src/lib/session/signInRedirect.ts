import { abortError, errorMessage } from '#lib/management/http.js';
import type { AuthenticationAction } from '#lib/session/state.js';

export type SignInBoundary = {
  unauthenticatedDestination(
    signal: AbortSignal,
    sessionExpired: boolean
  ): Promise<string>;
  navigate(destination: string): Promise<void>;
};

export async function redirectToSignIn(
  boundary: SignInBoundary | null,
  sessionExpired: boolean,
  cacheCleared: Promise<void>,
  report: (action: AuthenticationAction) => void,
  signal: AbortSignal
): Promise<void> {
  try {
    await cacheCleared;
    if (signal.aborted) return;
    if (!boundary) {
      report({ type: 'anonymous' });
      return;
    }
    const destination = await boundary.unauthenticatedDestination(
      signal,
      sessionExpired
    );
    if (signal.aborted) return;
    await boundary.navigate(destination);
  } catch (error) {
    if (signal.aborted || abortError(error)) return;
    report({
      type: 'gate',
      phase: 'unavailable',
      error: errorMessage(error, 'The login destination could not be loaded.')
    });
  }
}
