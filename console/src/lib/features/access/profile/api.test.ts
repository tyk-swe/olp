import { afterEach, expect, it, vi } from 'vitest';
import { confirmMFA } from '$lib/features/access/mfa/prompt.svelte';
import {
  beginIdentityReauthentication,
  reauthenticateWithPassword
} from './api';

vi.mock('$lib/features/access/session/authMiddleware', () => ({
  createAuthMiddleware: () => ({
    onRequest: ({ request }: { request: Request }) => request
  })
}));
vi.mock('$lib/features/access/mfa/prompt.svelte', () => ({
  confirmMFA: vi.fn()
}));
afterEach(() => {
  vi.unstubAllGlobals();
  vi.resetAllMocks();
});

it('binds password verification to the agent delegation key', async () => {
  vi.stubGlobal('fetch', async (request: Request) => {
    expect(new URL(request.url).pathname).toBe(
      '/api/v1/profile/reauthenticate'
    );
    expect(request.method).toBe('POST');
    expect(await request.json()).toEqual({
      current_password: 'example-password',
      purpose: 'agent_delegation',
      resource_id: 'key'
    });
    return new Response(null, { status: 204 });
  });
  await expect(
    reauthenticateWithPassword('example-password', 'agent_delegation', 'key')
  ).resolves.toBeUndefined();
});

it('does not treat a rejected password as fresh authentication', async () => {
  vi.stubGlobal('fetch', async () =>
    Response.json({ title: 'Invalid password', status: 401 }, { status: 401 })
  );
  await expect(
    reauthenticateWithPassword('wrong', 'agent_delegation', 'key')
  ).rejects.toMatchObject({ problem: { status: 401 } });
});

it('requires the offered second factor before password verification completes', async () => {
  const challenge = { challenge_id: 'challenge', methods: ['totp'] };
  vi.mocked(confirmMFA).mockImplementation(async (input) => {
    expect(input.challenge).toEqual(challenge);
    throw new Error('Verification incomplete');
  });
  vi.stubGlobal('fetch', async () => Response.json(challenge, { status: 202 }));
  await expect(
    reauthenticateWithPassword('example-password', 'agent_delegation', 'key')
  ).rejects.toThrow('Verification incomplete');
});

it.each([
  { saml: true, endpoint: '/api/v1/profile/saml/reauthenticate' },
  { saml: false, endpoint: '/api/v1/oidc/reauthenticate' }
])(
  'continues agent delegation through the available linked identity',
  async ({ saml, endpoint }) => {
    vi.stubGlobal('fetch', async (request: Request) => {
      const path = new URL(request.url).pathname;
      if (request.method === 'GET') {
        expect(path).toBe('/api/v1/profile/saml-identities');
        return Response.json({
          reauthentication_available: saml,
          identities: []
        });
      }
      expect(request.method).toBe('POST');
      expect(path).toBe(endpoint);
      expect(await request.json()).toEqual({
        purpose: 'agent_delegation',
        resource_id: 'key'
      });
      return Response.json({
        authorization_url: 'https://identity.example.test/authorize'
      });
    });
    await expect(
      beginIdentityReauthentication('agent_delegation', 'key')
    ).resolves.toBe('https://identity.example.test/authorize');
  }
);

it('does not start identity verification when identity availability fails', async () => {
  vi.stubGlobal('fetch', async (request: Request) => {
    expect(request.method).toBe('GET');
    return Response.json(
      { title: 'Unavailable', status: 503 },
      { status: 503 }
    );
  });
  await expect(
    beginIdentityReauthentication('agent_delegation', 'key')
  ).rejects.toMatchObject({ problem: { status: 503 } });
});
