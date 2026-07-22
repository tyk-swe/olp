import { afterEach, describe, expect, it, vi } from 'vitest';
import type { QueryClient } from '@tanstack/svelte-query';
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

const sessionFor = (id: string, csrfToken: string): AuthenticatedSession => ({
  user: {
    ...session().user,
    id,
    email: `${id}@example.com`
  },
  csrf_token: csrfToken
});

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((promiseResolve, promiseReject) => {
    resolve = promiseResolve;
    reject = promiseReject;
  });
  return { promise, resolve, reject };
}

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
  it('does not dispatch an authentication request after it is superseded', async () => {
    const lifecycle = new AuthenticationLifecycle();
    let releaseFirstCancellation!: () => void;
    const firstCancellation = new Promise<void>((resolve) => {
      releaseFirstCancellation = resolve;
    });
    const cancelQueries = vi
      .fn<() => Promise<void>>()
      .mockImplementationOnce(async () => firstCancellation)
      .mockResolvedValue(undefined);
    lifecycle.attachQueryClient({
      cancelQueries,
      clear: vi.fn()
    } as unknown as QueryClient);
    const firstRequest = vi.fn(async () => session());
    const secondRequest = vi.fn(async () => session('csrf-second'));

    const firstAuthentication = lifecycle.authenticate(firstRequest).catch((error: unknown) => error);
    const secondAuthentication = lifecycle.authenticate(secondRequest);

    await expect(secondAuthentication).resolves.toMatchObject({ csrf_token: 'csrf-second' });
    releaseFirstCancellation();
    const firstError = await firstAuthentication;

    expect(firstRequest).not.toHaveBeenCalled();
    expect(secondRequest).toHaveBeenCalledOnce();
    expect(firstError).toBeInstanceOf(DOMException);
    expect((firstError as DOMException).name).toBe('AbortError');
    expect(lifecycle.snapshot().user).toEqual(session().user);
    expect(getCsrfToken()).toBe('csrf-second');
  });

  it('does not let a superseded anonymous transition erase a new session', async () => {
    const lifecycle = new AuthenticationLifecycle();
    const transitionCancellation = deferred<void>();
    const cancelQueries = vi
      .fn<() => Promise<void>>()
      .mockImplementationOnce(async () => transitionCancellation.promise)
      .mockResolvedValue(undefined);
    lifecycle.attachQueryClient({
      cancelQueries,
      clear: vi.fn()
    } as unknown as QueryClient);
    const registeredBoundary = boundary(async () => session());
    lifecycle.registerBoundary(registeredBoundary);
    lifecycle.establishSession(session('csrf-old'));

    const transition = lifecycle.principalInvalidated();
    await vi.waitFor(() => expect(cancelQueries).toHaveBeenCalledOnce());
    const authenticated = lifecycle.authenticate(async () => session('csrf-new'));

    await expect(authenticated).resolves.toMatchObject({ csrf_token: 'csrf-new' });
    transitionCancellation.resolve(undefined);
    await transition;

    expect(registeredBoundary.unauthenticatedDestination).not.toHaveBeenCalled();
    expect(lifecycle.snapshot()).toMatchObject({
      phase: 'authenticated',
      user: session().user
    });
    expect(getCsrfToken()).toBe('csrf-new');
  });

  it('does not let an obsolete transition clear a newer transition handle', async () => {
    const lifecycle = new AuthenticationLifecycle();
    const firstCancellation = deferred<void>();
    const secondCancellation = deferred<void>();
    const cancelQueries = vi
      .fn<() => Promise<void>>()
      .mockImplementationOnce(async () => firstCancellation.promise)
      .mockImplementationOnce(async () => secondCancellation.promise);
    lifecycle.attachQueryClient({
      cancelQueries,
      clear: vi.fn()
    } as unknown as QueryClient);
    lifecycle.registerBoundary(boundary(async () => session()));

    const firstTransition = lifecycle.principalInvalidated();
    await vi.waitFor(() => expect(cancelQueries).toHaveBeenCalledOnce());
    lifecycle.registerBoundary(boundary(async () => session()));
    const secondTransition = lifecycle.principalInvalidated();
    await vi.waitFor(() => expect(cancelQueries).toHaveBeenCalledTimes(2));

    firstCancellation.resolve(undefined);
    await firstTransition;
    const coalescedTransition = lifecycle.principalInvalidated();

    expect(cancelQueries).toHaveBeenCalledTimes(2);
    secondCancellation.resolve(undefined);
    await Promise.all([secondTransition, coalescedTransition]);
  });

  it('does not establish a validation result superseded during cache clearing', async () => {
    const lifecycle = new AuthenticationLifecycle();
    const validationCancellation = deferred<void>();
    const cancelQueries = vi
      .fn<() => Promise<void>>()
      .mockImplementationOnce(async () => validationCancellation.promise)
      .mockResolvedValue(undefined);
    lifecycle.attachQueryClient({
      cancelQueries,
      clear: vi.fn()
    } as unknown as QueryClient);
    const staleSession = sessionFor('stale-principal', 'csrf-stale');
    const currentSession = sessionFor('current-principal', 'csrf-current');
    lifecycle.registerBoundary(boundary(async () => staleSession));
    lifecycle.establishSession(session('csrf-old'));

    const validation = lifecycle.validateSession();
    await vi.waitFor(() => expect(cancelQueries).toHaveBeenCalledOnce());
    await lifecycle.authenticate(async () => currentSession);
    validationCancellation.resolve(undefined);
    await validation;

    expect(lifecycle.snapshot()).toMatchObject({
      phase: 'authenticated',
      user: currentSession.user
    });
    expect(getCsrfToken()).toBe('csrf-current');
    expect(lifecycle.queryKeyHash(['current'])).toContain('principal:current-principal:operator');
  });

  it('does not run obsolete validation error cleanup after authentication', async () => {
    const lifecycle = new AuthenticationLifecycle();
    const validationCancellation = deferred<void>();
    const cancelQueries = vi
      .fn<() => Promise<void>>()
      .mockImplementationOnce(async () => validationCancellation.promise)
      .mockResolvedValue(undefined);
    lifecycle.attachQueryClient({
      cancelQueries,
      clear: vi.fn()
    } as unknown as QueryClient);
    lifecycle.registerBoundary(boundary(async () => Promise.reject(new Error('Unavailable'))));
    const currentSession = sessionFor('current-principal', 'csrf-current');

    const validation = lifecycle.validateSession();
    await vi.waitFor(() => expect(cancelQueries).toHaveBeenCalledOnce());
    await lifecycle.authenticate(async () => currentSession);
    validationCancellation.resolve(undefined);
    await validation;

    expect(lifecycle.snapshot()).toMatchObject({
      phase: 'authenticated',
      user: currentSession.user
    });
    expect(getCsrfToken()).toBe('csrf-current');
    expect(lifecycle.queryKeyHash(['current'])).toContain('principal:current-principal:operator');
  });

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

  it('shares one freshness validation between concurrent stale-session mutations', async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-01-01T00:00:00Z'));
    const lifecycle = new AuthenticationLifecycle();
    let resolveSession!: (value: AuthenticatedSession) => void;
    const loaded = new Promise<AuthenticatedSession>((resolve) => {
      resolveSession = resolve;
    });
    const loadSession = vi.fn(async () => loaded);
    lifecycle.registerBoundary(boundary(loadSession));
    lifecycle.establishSession(session());
    vi.advanceTimersByTime(60_001);

    const firstPreparation = lifecycle.prepareRequest(
      new Request('https://console.example.test/api/v1/providers/one', { method: 'PATCH' })
    );
    const secondPreparation = lifecycle.prepareRequest(
      new Request('https://console.example.test/api/v1/providers/two', { method: 'DELETE' })
    );

    expect(loadSession).toHaveBeenCalledOnce();
    resolveSession(session('csrf-refreshed'));
    const [firstRequest, secondRequest] = await Promise.all([
      firstPreparation,
      secondPreparation
    ]);

    expect(firstRequest.headers.get('x-csrf-token')).toBe('csrf-refreshed');
    expect(secondRequest.headers.get('x-csrf-token')).toBe('csrf-refreshed');
  });

  it('keeps a passive refresh from superseding a stale mutation validation', async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-01-01T00:00:00Z'));
    const lifecycle = new AuthenticationLifecycle();
    let resolveSession!: (value: AuthenticatedSession) => void;
    const loaded = new Promise<AuthenticatedSession>((resolve) => {
      resolveSession = resolve;
    });
    const loadSession = vi.fn(async () => loaded);
    lifecycle.registerBoundary(boundary(loadSession));
    lifecycle.establishSession(session());
    vi.advanceTimersByTime(60_001);

    const preparation = lifecycle.prepareRequest(
      new Request('https://console.example.test/api/v1/profile', { method: 'PATCH' })
    );
    vi.advanceTimersByTime(501);
    const passiveValidation = lifecycle.validateSession({ passive: true });

    expect(loadSession).toHaveBeenCalledOnce();
    resolveSession(session('csrf-refreshed'));
    const [request, passiveSession] = await Promise.all([preparation, passiveValidation]);

    expect(passiveSession).toMatchObject({ csrf_token: 'csrf-refreshed' });
    expect(request.headers.get('x-csrf-token')).toBe('csrf-refreshed');
  });

  it('rejects a stale mutation when session recovery changes principals', async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-01-01T00:00:00Z'));
    const lifecycle = new AuthenticationLifecycle();
    let resolveSession!: (value: AuthenticatedSession) => void;
    const loaded = new Promise<AuthenticatedSession>((resolve) => {
      resolveSession = resolve;
    });
    lifecycle.registerBoundary(boundary(async () => loaded));
    lifecycle.establishSession(sessionFor('first-principal', 'csrf-first'));
    vi.advanceTimersByTime(60_001);

    const preparation = lifecycle.prepareRequest(
      new Request('https://console.example.test/api/v1/profile', { method: 'PATCH' })
    );
    resolveSession(sessionFor('second-principal', 'csrf-second'));
    const error = await preparation.catch((value: unknown) => value);

    expect(error).toBeInstanceOf(DOMException);
    expect((error as DOMException).name).toBe('AbortError');
    expect(lifecycle.snapshot()).toMatchObject({
      phase: 'authenticated',
      user: sessionFor('second-principal', 'csrf-second').user
    });
    expect(getCsrfToken()).toBe('csrf-second');
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

  it('does not let a pre-logout validation restore the signed-out session', async () => {
    const lifecycle = new AuthenticationLifecycle();
    const loaded = deferred<AuthenticatedSession>();
    lifecycle.registerBoundary(boundary(async () => loaded.promise));
    lifecycle.establishSession(session('csrf-old'));

    const validation = lifecycle.validateSession({ passive: true });
    await lifecycle.signOut(async () => undefined);
    loaded.resolve(session('csrf-stale'));
    await validation;

    expect(lifecycle.snapshot()).toMatchObject({ phase: 'anonymous', user: null });
    expect(getCsrfToken()).toBeNull();
  });

  it('uses the same cleanup path when the current session is revoked', async () => {
    const lifecycle = new AuthenticationLifecycle();
    const loaded = deferred<AuthenticatedSession>();
    const registeredBoundary = boundary(async () => loaded.promise);
    lifecycle.registerBoundary(registeredBoundary);
    lifecycle.establishSession(session('csrf-old'));

    const validation = lifecycle.validateSession({ passive: true });
    await lifecycle.endCurrentSession(async () => undefined);
    loaded.resolve(session('csrf-stale'));
    await validation;

    expect(lifecycle.snapshot()).toMatchObject({ phase: 'anonymous', user: null });
    expect(getCsrfToken()).toBeNull();
    expect(registeredBoundary.navigate).toHaveBeenCalledWith('/login');
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
