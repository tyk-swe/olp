import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiProblem } from '$lib/management/http';
import { clearCsrfToken, getCsrfToken } from './csrf';
import { AuthenticationLifecycle } from './lifecycle';
import type { AuthenticatedSession } from './state';

const alice: AuthenticatedSession = {
  user: {
    id: 'alice',
    email: 'alice@example.test',
    display_name: 'Alice',
    role: 'operator',
    access_scope: 'global',
    operations: []
  },
  csrf_token: 'alice-csrf',
  session_id: 'alice-session'
};

const bob: AuthenticatedSession = {
  user: {
    ...alice.user,
    id: 'bob',
    email: 'bob@example.test',
    display_name: 'Bob'
  },
  csrf_token: 'bob-csrf',
  session_id: 'bob-session'
};

function boundary(session = alice) {
  return {
    loadSession: vi.fn(async () => session),
    unauthenticatedDestination: vi.fn(async () => '/login'),
    loginDestination: vi.fn(() => '/login'),
    navigate: vi.fn(async () => {})
  };
}

function request(path: string, method = 'GET', signal?: AbortSignal) {
  return new Request(`https://console.example.test${path}`, { method, signal });
}

afterEach(() => {
  clearCsrfToken();
});

describe('authentication lifecycle', () => {
  it('establishes the session and puts mutations behind its CSRF token', async () => {
    const lifecycle = new AuthenticationLifecycle();
    const observations = vi.fn();
    lifecycle.subscribe(observations);
    lifecycle.establishSession(alice);
    const controller = new AbortController();
    const prepared = await lifecycle.prepareRequest(
      request('/api/v1/providers', 'POST', controller.signal)
    );
    const read = await lifecycle.prepareRequest(request('/api/v1/providers'));

    expect(lifecycle.snapshot()).toMatchObject({
      phase: 'authenticated',
      user: alice.user,
      sessionId: alice.session_id,
      error: ''
    });
    expect(observations).toHaveBeenCalledTimes(2);
    expect(prepared.headers.get('x-csrf-token')).toBe(alice.csrf_token);
    expect(read.headers.has('x-csrf-token')).toBe(false);
    controller.abort();
    expect(prepared.signal.aborted).toBe(true);
  });

  it('allows login without a session and refuses protected writes', async () => {
    const lifecycle = new AuthenticationLifecycle();
    const login = request('/api/v1/sessions', 'POST');

    await expect(lifecycle.prepareRequest(login)).resolves.toBe(login);
    await expect(
      lifecycle.prepareRequest(request('/api/v1/providers', 'POST'))
    ).rejects.toMatchObject({
      name: 'AbortError'
    });
  });

  it('refreshes a session before a write when its CSRF token is missing', async () => {
    const lifecycle = new AuthenticationLifecycle();
    const service = boundary();
    lifecycle.registerBoundary(service);
    lifecycle.establishSession({ ...alice, csrf_token: '' });

    const prepared = await lifecycle.prepareRequest(
      request('/api/v1/providers', 'POST')
    );

    expect(service.loadSession).toHaveBeenCalledOnce();
    expect(prepared.headers.get('x-csrf-token')).toBe(alice.csrf_token);
    expect(lifecycle.snapshot()).toMatchObject({
      phase: 'authenticated',
      user: alice.user
    });
  });

  it('cancels requests and clears cached data before changing principal', async () => {
    const lifecycle = new AuthenticationLifecycle();
    const client = new QueryClient();
    lifecycle.attachQueryClient(client);
    lifecycle.establishSession(alice);
    client.setQueryData(['providers'], ['alice-only']);
    const oldPartition = lifecycle.queryKeyHash(['providers']);
    const previous = await lifecycle.prepareRequest(
      request('/api/v1/providers')
    );

    await expect(lifecycle.authenticate(async () => bob)).resolves.toEqual(bob);

    expect(previous.signal.aborted).toBe(true);
    expect(client.getQueryData(['providers'])).toBeUndefined();
    expect(lifecycle.queryKeyHash(['providers'])).not.toBe(oldPartition);
    expect(lifecycle.snapshot()).toMatchObject({
      phase: 'authenticated',
      user: bob.user
    });
    expect(getCsrfToken()).toBe(bob.csrf_token);
  });

  it('does not replace a newer session when an earlier login finishes late', async () => {
    const lifecycle = new AuthenticationLifecycle();
    const pending = Promise.withResolvers<AuthenticatedSession>();
    const firstLogin = vi.fn(() => pending.promise);
    const first = lifecycle.authenticate(firstLogin);
    await vi.waitFor(() => expect(firstLogin).toHaveBeenCalledOnce());

    await lifecycle.authenticate(async () => bob);
    pending.resolve(alice);

    await expect(first).rejects.toMatchObject({ name: 'AbortError' });
    expect(lifecycle.snapshot().user).toEqual(bob.user);
    expect(getCsrfToken()).toBe(bob.csrf_token);
  });

  it('retains the current session when validation has a temporary error', async () => {
    const lifecycle = new AuthenticationLifecycle();
    const service = boundary();
    service.loadSession.mockRejectedValue(new Error('Connection unavailable'));
    lifecycle.registerBoundary(service);
    lifecycle.establishSession(alice);

    await expect(lifecycle.validateSession()).resolves.toBeNull();
    expect(lifecycle.snapshot()).toMatchObject({
      phase: 'authenticated',
      user: alice.user,
      error: 'Connection unavailable'
    });
    await expect(
      lifecycle.prepareRequest(request('/api/v1/providers', 'POST'))
    ).rejects.toThrow('Connection unavailable');
  });

  it('shows an unavailable state when the initial session cannot be loaded', async () => {
    const lifecycle = new AuthenticationLifecycle();
    const service = boundary();
    service.loadSession.mockRejectedValue(new Error('Connection unavailable'));
    lifecycle.registerBoundary(service);

    await expect(lifecycle.validateSession()).resolves.toBeNull();
    expect(lifecycle.snapshot()).toMatchObject({
      phase: 'unavailable',
      user: null,
      error: 'Connection unavailable'
    });
    expect(getCsrfToken()).toBeNull();
  });

  it('clears an expired session and navigates to sign in', async () => {
    const lifecycle = new AuthenticationLifecycle();
    const service = boundary();
    service.loadSession.mockRejectedValue(
      new ApiProblem({ status: 401, title: 'Expired' })
    );
    lifecycle.registerBoundary(service);
    lifecycle.establishSession(alice);

    await expect(lifecycle.validateSession()).resolves.toBeNull();

    expect(service.unauthenticatedDestination).toHaveBeenCalledWith(
      expect.any(AbortSignal),
      true
    );
    expect(service.navigate).toHaveBeenCalledWith('/login');
    expect(lifecycle.snapshot()).toMatchObject({
      phase: 'transitioning',
      user: null
    });
    expect(getCsrfToken()).toBeNull();
  });

  it('uses a rotated CSRF token and ignores older responses', async () => {
    const lifecycle = new AuthenticationLifecycle();
    lifecycle.establishSession(alice);
    const earlier = await lifecycle.prepareRequest(
      request('/api/v1/providers')
    );
    const later = await lifecycle.prepareRequest(request('/api/v1/routes'));

    await lifecycle.handleResponse(
      later,
      new Response(null, { headers: { 'x-csrf-token': 'rotated' } })
    );
    await lifecycle.handleResponse(
      earlier,
      new Response(null, { headers: { 'x-csrf-token': 'stale' } })
    );

    expect(getCsrfToken()).toBe('rotated');
    const mutation = await lifecycle.prepareRequest(
      request('/api/v1/providers', 'POST')
    );
    expect(mutation.headers.get('x-csrf-token')).toBe('rotated');
  });

  it('refreshes an invalid CSRF token and asks the user to retry the write', async () => {
    const lifecycle = new AuthenticationLifecycle();
    const service = boundary({ ...alice, csrf_token: 'refreshed' });
    lifecycle.registerBoundary(service);
    lifecycle.establishSession(alice);
    const mutation = await lifecycle.prepareRequest(
      request('/api/v1/providers', 'POST')
    );
    const response = new Response(
      JSON.stringify({
        type: 'https://openllmproxy.dev/problems/csrf_invalid'
      }),
      { status: 403, headers: { 'content-type': 'application/problem+json' } }
    );

    await expect(
      lifecycle.handleResponse(mutation, response)
    ).rejects.toMatchObject({
      problem: { status: 403, title: 'Session verification required' }
    });
    expect(service.loadSession).toHaveBeenCalledOnce();
    expect(getCsrfToken()).toBe('refreshed');
  });

  it('ignores an unauthorized response from a previous principal', async () => {
    const lifecycle = new AuthenticationLifecycle();
    const service = boundary(bob);
    lifecycle.registerBoundary(service);
    lifecycle.establishSession(alice);
    const earlier = await lifecycle.prepareRequest(
      request('/api/v1/providers')
    );
    await lifecycle.authenticate(async () => bob);

    await lifecycle.handleResponse(
      earlier,
      new Response(null, { status: 401 })
    );

    expect(service.loadSession).not.toHaveBeenCalled();
    expect(lifecycle.snapshot()).toMatchObject({
      phase: 'authenticated',
      user: bob.user
    });
  });

  it('gates and clears the previous account when another tab changes the session', async () => {
    const lifecycle = new AuthenticationLifecycle();
    const service = boundary(bob);
    const client = new QueryClient();
    lifecycle.registerBoundary(service);
    lifecycle.attachQueryClient(client);
    lifecycle.establishSession(alice);
    client.setQueryData(['providers'], ['alice-only']);
    const previous = await lifecycle.prepareRequest(
      request('/api/v1/providers')
    );
    const disconnect = lifecycle.connectTabs();
    const otherTab = new BroadcastChannel('olp-authentication');
    try {
      otherTab.postMessage('changed');
      await vi.waitFor(() =>
        expect(lifecycle.snapshot().user).toEqual(bob.user)
      );
      expect(previous.signal.aborted).toBe(true);
      expect(client.getQueryData(['providers'])).toBeUndefined();
      expect(getCsrfToken()).toBe(bob.csrf_token);
      expect(service.loadSession).toHaveBeenCalledOnce();
    } finally {
      otherTab.close();
      disconnect();
      client.clear();
    }
  });

  it('refreshes a token rotated in another tab and ignores older responses', async () => {
    const lifecycle = new AuthenticationLifecycle();
    const service = boundary({
      ...alice,
      csrf_token: 'rotated-in-another-tab'
    });
    lifecycle.registerBoundary(service);
    lifecycle.establishSession(alice);
    const previous = await lifecycle.prepareRequest(
      request('/api/v1/providers')
    );
    const disconnect = lifecycle.connectTabs();
    const otherTab = new BroadcastChannel('olp-authentication');
    try {
      otherTab.postMessage('rotated');
      await vi.waitFor(() =>
        expect(getCsrfToken()).toBe('rotated-in-another-tab')
      );
      await lifecycle.handleResponse(
        previous,
        new Response(null, { headers: { 'x-csrf-token': 'stale' } })
      );
      expect(getCsrfToken()).toBe('rotated-in-another-tab');
      expect(lifecycle.snapshot().user).toEqual(alice.user);
      expect(service.loadSession).toHaveBeenCalledOnce();
    } finally {
      otherTab.close();
      disconnect();
    }
  });

  it('clears protected state and navigates after sign-out', async () => {
    const lifecycle = new AuthenticationLifecycle();
    const service = boundary();
    const client = new QueryClient();
    lifecycle.registerBoundary(service);
    lifecycle.attachQueryClient(client);
    lifecycle.establishSession(alice);
    client.setQueryData(['providers'], ['private']);
    const previous = await lifecycle.prepareRequest(
      request('/api/v1/providers')
    );
    const signOut = vi.fn(async () => {});

    await lifecycle.signOut(signOut);

    expect(signOut).toHaveBeenCalledWith(expect.any(AbortSignal));
    expect(previous.signal.aborted).toBe(true);
    expect(client.getQueryData(['providers'])).toBeUndefined();
    expect(getCsrfToken()).toBeNull();
    expect(lifecycle.snapshot()).toMatchObject({
      phase: 'anonymous',
      user: null
    });
    expect(service.navigate).toHaveBeenCalledWith('/login');
  });

  it('restores the session and reports a failed sign-out', async () => {
    const lifecycle = new AuthenticationLifecycle();
    const service = boundary();
    lifecycle.registerBoundary(service);
    lifecycle.establishSession(alice);

    await expect(
      lifecycle.signOut(async () => {
        throw new Error('Sign-out failed');
      })
    ).rejects.toThrow('Sign-out failed');

    expect(lifecycle.snapshot()).toMatchObject({
      phase: 'authenticated',
      user: alice.user,
      principalExitError: 'Sign-out failed'
    });
    expect(getCsrfToken()).toBe(alice.csrf_token);
    expect(service.navigate).not.toHaveBeenCalled();
  });
});
