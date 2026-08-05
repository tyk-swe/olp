import { describe, expect, it } from 'vitest';
import {
  anonymousAuthenticationSnapshot,
  reduceAuthentication,
  type AuthenticatedSession
} from './state';

const session: AuthenticatedSession = {
  user: {
    id: '01980000-0000-7000-8000-000000000001',
    email: 'operator@example.com',
    display_name: 'Operator',
    role: 'operator'
  },
  csrf_token: 'csrf-token'
};

describe('authentication state reducer', () => {
  it('uses coordinator-supplied validation time', () => {
    const authenticated = reduceAuthentication(
      anonymousAuthenticationSnapshot(),
      { type: 'authenticated', session, validatedAt: 42 }
    );

    expect(authenticated).toMatchObject({
      phase: 'authenticated',
      user: session.user,
      lastValidatedAt: 42
    });
  });

  it('retains an authenticated principal on transient validation errors', () => {
    const authenticated = reduceAuthentication(
      anonymousAuthenticationSnapshot(),
      { type: 'authenticated', session, validatedAt: 42 }
    );

    expect(
      reduceAuthentication(authenticated, {
        type: 'validation-error',
        error: 'temporarily unavailable'
      })
    ).toMatchObject({
      phase: 'authenticated',
      error: 'temporarily unavailable',
      lastValidatedAt: 42
    });
  });

  it('clears principal data when a protected boundary is gated', () => {
    const authenticated = reduceAuthentication(
      anonymousAuthenticationSnapshot(),
      { type: 'authenticated', session, validatedAt: 42 }
    );

    expect(
      reduceAuthentication(authenticated, {
        type: 'gate',
        phase: 'transitioning'
      })
    ).toEqual({
      phase: 'transitioning',
      user: null,
      error: '',
      principalExitError: '',
      lastValidatedAt: null
    });
  });
});
