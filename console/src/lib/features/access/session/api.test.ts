import { login, acceptInvitation } from './authentication';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { components } from '$lib/management/schema';
import { json, route, serve } from '$lib/management/test/transport';
import { confirmMFA } from '$lib/features/access/mfa/prompt.svelte';
import {
  authenticationCapabilities,
  beginOidcLogin,
  currentSession,
  logout
} from './api';

vi.mock('$lib/features/access/mfa/prompt.svelte', () => ({
  confirmMFA: vi.fn()
}));

type Schemas = components['schemas'];

const session: Schemas['SessionResponse'] = {
  csrf_token: 'csrf',
  installation_name: 'Installation',
  operations: ['read', 'configure'],
  user: {
    id: 'user-id',
    email: 'user@example.test',
    display_name: 'User',
    role: 'owner',
    access_scope: 'global'
  }
};

const challenge: Schemas['MFAChallenge'] = {
  challenge: 'challenge-id',
  expires_at: '2026-01-01T00:00:00Z',
  methods: ['totp'],
  enrollment_required: false,
  public_key: null
};

afterEach(() => {
  vi.unstubAllGlobals();
  vi.resetAllMocks();
});

describe('session API', () => {
  it('attaches server-granted operations to the current user', async () => {
    serve((request) => {
      route(request, 'GET', '/api/v1/sessions/current');
      return json(session);
    });
    const result = await currentSession();
    expect(result.user).toEqual({
      ...session.user,
      operations: session.operations
    });
    expect(result.csrf_token).toBe(session.csrf_token);
  });

  it.each([
    { ...session, csrf_token: null },
    { ...session, user: null },
    { ...session, user: { ...session.user, id: null } },
    { ...session, user: { ...session.user, email: null } },
    { ...session, user: { ...session.user, display_name: null } },
    { ...session, user: { ...session.user, role: 'unknown' } },
    { ...session, user: { ...session.user, access_scope: 'unknown' } },
    { ...session, operations: null },
    { ...session, operations: [1] }
  ])('rejects a malformed session response', async (body) => {
    serve(() => json(body));
    await expect(currentSession()).rejects.toMatchObject({
      problem: { status: 502, title: 'The session response is invalid' }
    });
  });

  it('checks authentication capabilities before exposing them', async () => {
    const capabilities = {
      local_login_enabled: true,
      oidc_login_enabled: false
    };
    serve((request) => {
      route(request, 'GET', '/api/v1/auth/capabilities');
      return json(capabilities);
    });
    await expect(authenticationCapabilities()).resolves.toEqual(capabilities);
    serve(() => json({ ...capabilities, oidc_login_enabled: null }));
    await expect(authenticationCapabilities()).rejects.toMatchObject({
      problem: {
        status: 502,
        title: 'The authentication capabilities response is invalid'
      }
    });
  });

  it('sends the post-login destination and accepts an HTTP authorization URL', async () => {
    serve(async (request) => {
      expect(request.method).toBe('POST');
      expect(new URL(request.url).pathname).toBe('/api/v1/oidc/login');
      expect(await request.json()).toEqual({
        return_to: '/routes?show=active'
      });
      return json({
        authorization_url: 'https://identity.example.test/authorize?state=abc'
      });
    });
    await expect(beginOidcLogin('/routes?show=active')).resolves.toBe(
      'https://identity.example.test/authorize?state=abc'
    );
  });

  it.each([null, '/relative', 'javascript:alert(1)', 'ftp://example.test'])(
    'rejects an invalid authorization URL',
    async (authorization_url) => {
      serve(() => json({ authorization_url }));
      await expect(beginOidcLogin('/')).rejects.toMatchObject({
        problem: {
          status: 502,
          title: 'The OIDC authorization response is invalid'
        }
      });
    }
  );

  it('completes a login challenge before reading the authenticated session', async () => {
    const requests = vi.fn(async (request: Request) => {
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/sessions') {
        expect(request.method).toBe('POST');
        expect(await request.json()).toEqual({
          email: session.user.email,
          password: 'secret'
        });
        return json(challenge, 202);
      }
      route(request, 'GET', '/api/v1/sessions/current');
      expect(confirmMFA).toHaveBeenCalledWith(
        { kind: 'challenge', challenge },
        undefined
      );
      return json(session);
    });
    serve(requests);
    await expect(login(session.user.email, 'secret')).resolves.toMatchObject({
      user: { id: session.user.id, operations: session.operations }
    });
    expect(requests).toHaveBeenCalledTimes(2);
  });

  it('sends an invitation acceptance and returns the authenticated session', async () => {
    const input: Schemas['AcceptInvitationRequest'] = {
      token: 'invitation-token',
      password: 'secret',
      display_name: 'User'
    };
    serve(async (request) => {
      expect(request.method).toBe('POST');
      expect(new URL(request.url).pathname).toBe('/api/v1/invitations/accept');
      expect(await request.json()).toEqual(input);
      return json(session);
    });
    await expect(acceptInvitation(input)).resolves.toMatchObject({
      user: { id: session.user.id, operations: session.operations }
    });
  });

  it('treats an absent session as signed out but reports other failures', async () => {
    serve((request) => {
      route(request, 'DELETE', '/api/v1/sessions/current');
      return json({ title: 'Session expired' }, 401);
    });
    await expect(logout()).resolves.toBeUndefined();
    serve(() => json({ title: 'Unavailable', status: 503 }, 503));
    await expect(logout()).rejects.toMatchObject({ problem: { status: 503 } });
  });
});

it('rejects an invalid second-factor challenge before opening verification', async () => {
  vi.stubGlobal('fetch', async () =>
    Response.json({ challenge: 'token', methods: ['unknown'] }, { status: 202 })
  );
  await expect(login('member@example.test', 'password')).rejects.toMatchObject({
    problem: { status: 502 }
  });
  expect(confirmMFA).not.toHaveBeenCalled();
});
