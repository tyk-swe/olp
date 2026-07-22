import { afterEach, describe, expect, it, vi } from 'vitest';
import { clearCsrfToken, getCsrfToken } from '$lib/api/session';
import { AuthenticationLifecycle, type AuthenticatedSession } from './lifecycle';

const session = (csrfToken = 'csrf-session'): AuthenticatedSession => ({
  user: {
    id: '01980000-0000-7000-8000-000000000001',
    email: 'operator@example.com',
    display_name: 'Operator',
    role: 'operator'
  },
  csrf_token: csrfToken
});

function boundary(loadSession: (signal: AbortSignal) => Promise<AuthenticatedSession>) {
  return {
    loadSession,
    unauthenticatedDestination: vi.fn(async () => '/login'),
    loginDestination: vi.fn(() => '/login'),
    navigate: vi.fn(async () => undefined)
  };
}

afterEach(() => {
  clearCsrfToken();
  vi.useRealTimers();
});

describe('authentication lifecycle', () => {
  it('keeps the authenticated snapshot mounted during passive validation', async () => {
    const lifecycle = new AuthenticationLifecycle();
    let resolveSession!: (value: AuthenticatedSession) => void;
    const loaded = new Promise<AuthenticatedSession>((resolve) => {
      resolveSession = resolve;
    });
    lifecycle.registerBoundary(boundary(async () => loaded));
    lifecycle.establishSession(session());

    const validation = lifecycle.validateSession({ passive: true });

    expect(lifecycle.snapshot()).toMatchObject({
      phase: 'authenticated',
      user: session().user
    });
    resolveSession(session('csrf-refreshed'));
    await validation;
    expect(lifecycle.snapshot().phase).toBe('authenticated');
    expect(getCsrfToken()).toBe('csrf-refreshed');
  });

  it('keeps a stale-session mutation mounted while freshness is checked', async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-01-01T00:00:00Z'));
    const lifecycle = new AuthenticationLifecycle();
    let resolveSession!: (value: AuthenticatedSession) => void;
    const loaded = new Promise<AuthenticatedSession>((resolve) => {
      resolveSession = resolve;
    });
    lifecycle.registerBoundary(boundary(async () => loaded));
    lifecycle.establishSession(session());
    vi.advanceTimersByTime(60_001);

    const preparation = lifecycle.prepareRequest(
      new Request('https://console.example.test/api/v1/profile', { method: 'PATCH' })
    );

    expect(lifecycle.snapshot().phase).toBe('authenticated');
    resolveSession(session('csrf-refreshed'));
    const request = await preparation;
    expect(request.headers.get('x-csrf-token')).toBe('csrf-refreshed');
  });

  it('preserves CSRF until the logout request has been prepared', async () => {
    const lifecycle = new AuthenticationLifecycle();
    lifecycle.establishSession(session('csrf-for-logout'));
    let prepared: Request | undefined;

    await lifecycle.signOut(async (signal) => {
      prepared = await lifecycle.prepareRequest(
        new Request('https://console.example.test/api/v1/sessions/current', {
          method: 'DELETE',
          signal
        })
      );
    });

    expect(prepared?.headers.get('x-csrf-token')).toBe('csrf-for-logout');
    expect(getCsrfToken()).toBeNull();
    expect(lifecycle.snapshot().phase).toBe('anonymous');
  });

  it('keeps an authenticated session read-only when CSRF recovery is empty', async () => {
    const lifecycle = new AuthenticationLifecycle();
    const loadSession = vi.fn(async () => session(''));
    lifecycle.registerBoundary(boundary(loadSession));
    lifecycle.establishSession(session(''));

    const error = await lifecycle
      .prepareRequest(
        new Request('https://console.example.test/api/v1/profile', { method: 'PATCH' })
      )
      .catch((value: unknown) => value);

    expect(loadSession).toHaveBeenCalledOnce();
    expect(error).toBeInstanceOf(DOMException);
    expect((error as DOMException).name).toBe('InvalidStateError');
    expect(lifecycle.snapshot().phase).toBe('authenticated');
    expect(getCsrfToken()).toBeNull();
  });

  it('reports a later validation failure after a handled 401', async () => {
    const lifecycle = new AuthenticationLifecycle();
    const loadSession = vi
      .fn<(signal: AbortSignal) => Promise<AuthenticatedSession>>()
      .mockRejectedValueOnce({ problem: { status: 401 } })
      .mockRejectedValueOnce(new Error('Session service unavailable'));
    const registeredBoundary = boundary(loadSession);
    lifecycle.registerBoundary(registeredBoundary);

    await lifecycle.validateSession();
    expect(registeredBoundary.navigate).toHaveBeenCalledWith('/login');

    lifecycle.markProtectedBoundaryChecking();
    await lifecycle.validateSession();

    expect(lifecycle.snapshot()).toMatchObject({
      phase: 'unavailable',
      error: 'Session service unavailable'
    });
  });
});
