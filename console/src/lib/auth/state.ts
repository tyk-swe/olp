import type { FixedRole } from './authorization';

export type AuthenticatedUser = {
  id: string;
  email: string;
  display_name: string;
  role: FixedRole;
};

export type AuthenticatedSession = {
  user: AuthenticatedUser;
  csrf_token: string;
};

export type AuthenticationPhase =
  'anonymous' | 'checking' | 'authenticated' | 'transitioning' | 'unavailable';

export type PrincipalAbsentSnapshot = {
  phase: Exclude<AuthenticationPhase, 'authenticated'>;
  user: null;
  error: string;
  principalExitError: string;
  lastValidatedAt: null;
};

type AuthenticatedSnapshot = {
  phase: 'authenticated';
  user: AuthenticatedUser;
  error: string;
  principalExitError: string;
  lastValidatedAt: number;
};

export type AuthenticationSnapshot =
  PrincipalAbsentSnapshot | AuthenticatedSnapshot;

export type AuthenticationAction =
  | { type: 'gate'; phase: PrincipalAbsentSnapshot['phase']; error?: string }
  | { type: 'anonymous' }
  | {
      type: 'authenticated';
      session: AuthenticatedSession;
      validatedAt: number;
    }
  | { type: 'validation-error'; error: string }
  | { type: 'principal-exit-error'; error: string };

export function anonymousAuthenticationSnapshot(): PrincipalAbsentSnapshot {
  return {
    phase: 'anonymous',
    user: null,
    error: '',
    principalExitError: '',
    lastValidatedAt: null
  };
}

/** Pure state transition; time and side effects are supplied by the coordinator. */
export function reduceAuthentication(
  snapshot: AuthenticationSnapshot,
  action: AuthenticationAction
): AuthenticationSnapshot {
  switch (action.type) {
    case 'gate':
      return {
        phase: action.phase,
        user: null,
        error: action.error ?? '',
        principalExitError: '',
        lastValidatedAt: null
      };
    case 'anonymous':
      return anonymousAuthenticationSnapshot();
    case 'authenticated':
      return {
        phase: 'authenticated',
        user: action.session.user,
        error: '',
        principalExitError: snapshot.principalExitError,
        lastValidatedAt: action.validatedAt
      };
    case 'validation-error':
      return snapshot.phase === 'authenticated'
        ? { ...snapshot, error: action.error }
        : snapshot;
    case 'principal-exit-error':
      return { ...snapshot, principalExitError: action.error };
  }
}
