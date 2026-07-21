import { afterEach, describe, expect, it, vi } from 'vitest';
import { acceptInvitation, currentSession, login, logout, type FixedRole } from './auth';
import { ApiProblem } from './http';
import { clearCsrfToken, getCsrfToken, setCsrfToken } from './session';
import { captureRequests, jsonResponse } from './test/requestCapture';

function sessionResponse(role: string, csrfToken = `csrf-${role}`) {
  return {
    user: {
      id: '01980000-0000-7000-8000-000000000001',
      email: `${role}@example.com`,
      display_name: 'Session User',
      role
    },
    csrf_token: csrfToken
  };
}

afterEach(() => {
  clearCsrfToken();
  vi.unstubAllGlobals();
});

describe('session API', () => {
  it.each<FixedRole>(['owner', 'operator', 'developer', 'viewer'])(
    'accepts the supported %s role and records its CSRF token',
    async (role) => {
      captureRequests(() => jsonResponse(sessionResponse(role)));

      const session = await currentSession();

      expect(session.user.role).toBe(role);
      expect(getCsrfToken()).toBe(`csrf-${role}`);
    }
  );

  it('fails closed on an unknown role without replacing CSRF state', async () => {
    setCsrfToken('known-good-token');
    captureRequests(() => jsonResponse(sessionResponse('administrator', 'untrusted-token')));

    const error = await currentSession().catch((value: unknown) => value);

    expect(error).toBeInstanceOf(ApiProblem);
    expect((error as ApiProblem).problem).toMatchObject({
      title: 'The session response contains an invalid role',
      status: 502
    });
    expect(getCsrfToken()).toBe('known-good-token');
  });

  it('fails closed on a malformed session without replacing CSRF state', async () => {
    setCsrfToken('known-good-token');
    captureRequests(() => jsonResponse({ csrf_token: 'untrusted-token' }));

    const error = await currentSession().catch((value: unknown) => value);

    expect(error).toBeInstanceOf(ApiProblem);
    expect((error as ApiProblem).problem).toMatchObject({
      title: 'The session response is invalid',
      status: 502
    });
    expect(getCsrfToken()).toBe('known-good-token');
  });

  it('clears stale CSRF state when the current session has no valid CSRF cookie', async () => {
    setCsrfToken('stale-token');
    captureRequests(() => jsonResponse(sessionResponse('operator', '')));

    await currentSession();

    expect(getCsrfToken()).toBeNull();
  });

  it('sends only login credentials and installs the returned CSRF token', async () => {
    const requests = captureRequests(() =>
      jsonResponse(sessionResponse('operator', 'csrf-from-login'))
    );

    await login('operator@example.com', 'correct horse battery staple');

    expect(new URL(requests[0]!.url).pathname).toBe('/api/v1/sessions');
    expect(requests[0]!.method).toBe('POST');
    expect(JSON.parse(await requests[0]!.clone().text())).toEqual({
      email: 'operator@example.com',
      password: 'correct horse battery staple'
    });
    expect(getCsrfToken()).toBe('csrf-from-login');
  });

  it('preserves the invitation contract and replaces CSRF state', async () => {
    setCsrfToken('pre-invitation-token');
    const requests = captureRequests(() =>
      jsonResponse(sessionResponse('developer', 'csrf-from-invitation'))
    );
    const input = {
      token: 'opaque-invitation-token',
      display_name: 'Invited Developer',
      password: 'another correct horse battery staple'
    };

    await acceptInvitation(input);

    expect(new URL(requests[0]!.url).pathname).toBe('/api/v1/invitations/accept');
    expect(requests[0]!.method).toBe('POST');
    expect(JSON.parse(await requests[0]!.clone().text())).toEqual(input);
    expect(requests[0]!.headers.get('x-csrf-token')).toBe('pre-invitation-token');
    expect(getCsrfToken()).toBe('csrf-from-invitation');
  });

  it.each([204, 401])('clears local state when logout returns %s', async (status) => {
    setCsrfToken('csrf-before-logout');
    captureRequests(() =>
      status === 204
        ? new Response(null, { status })
        : jsonResponse({ title: 'No active session', status }, { status })
    );

    await expect(logout()).resolves.toBeUndefined();
    expect(getCsrfToken()).toBeNull();
  });

  it('preserves local state when the server cannot complete logout', async () => {
    setCsrfToken('csrf-before-failed-logout');
    captureRequests(() =>
      jsonResponse(
        { title: 'Logout unavailable', detail: 'Try again later', status: 503 },
        { status: 503, headers: { 'content-type': 'application/problem+json' } }
      )
    );

    const error = await logout().catch((value: unknown) => value);

    expect(error).toBeInstanceOf(ApiProblem);
    expect((error as ApiProblem).problem.status).toBe(503);
    expect(getCsrfToken()).toBe('csrf-before-failed-logout');
  });
});
