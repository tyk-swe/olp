import { reauthenticateWithPassword } from './reauthentication';
import { afterEach, expect, it, vi } from 'vitest';
import { confirmMFA } from '#lib/session/mfaPrompt.svelte.js';
import { json, route, serve } from '#lib/session/test/transport.js';
import { beginIdentityReauthentication } from './api';

vi.mock('#lib/session/mfaPrompt.svelte.js', () => ({
  confirmMFA: vi.fn()
}));
afterEach(() => {
  vi.resetAllMocks();
});

it('binds password verification to the agent delegation key', async () => {
  serve(async (request) => {
    route(request, 'POST', '/api/v1/profile/reauthenticate');
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
  serve(() => json({ title: 'Invalid password', status: 401 }, 401));
  await expect(
    reauthenticateWithPassword('wrong', 'agent_delegation', 'key')
  ).rejects.toMatchObject({ problem: { status: 401 } });
});

it('requires the offered second factor before password verification completes', async () => {
  const challenge = {
    challenge: 'challenge',
    methods: ['totp'],
    expires_at: '2026-01-01T00:00:00Z',
    enrollment_required: false,
    public_key: null
  };
  vi.mocked(confirmMFA).mockImplementation(async (input) => {
    expect(input.kind === 'challenge' ? input.challenge : undefined).toEqual(
      challenge
    );
    throw new Error('Verification incomplete');
  });
  serve(() => json(challenge, 202));
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
    serve(async (request) => {
      if (request.method === 'GET') {
        route(request, 'GET', '/api/v1/profile/saml-identities');
        return json({
          reauthentication_available: saml,
          identities: []
        });
      }
      route(request, 'POST', endpoint);
      expect(await request.json()).toEqual({
        purpose: 'agent_delegation',
        resource_id: 'key'
      });
      return json({
        authorization_url: 'https://identity.example.test/authorize'
      });
    });
    await expect(
      beginIdentityReauthentication('agent_delegation', 'key')
    ).resolves.toBe('https://identity.example.test/authorize');
  }
);

it.each([true, false])(
  'rejects an invalid identity reauthentication URL (SAML available: %s)',
  async (saml) => {
    serve((request) =>
      request.method === 'GET'
        ? json({ reauthentication_available: saml, identities: [] })
        : json({ authorization_url: 'mailto:help@example.test' })
    );
    await expect(
      beginIdentityReauthentication('agent_delegation', 'key')
    ).rejects.toMatchObject({
      problem: { status: 502, type: 'urn:olp:problem:invalid-api-response' }
    });
  }
);

it('does not start identity verification when identity availability fails', async () => {
  serve((request) => {
    route(request, 'GET', '/api/v1/profile/saml-identities');
    return json({ title: 'Unavailable', status: 503 }, 503);
  });
  await expect(
    beginIdentityReauthentication('agent_delegation', 'key')
  ).rejects.toMatchObject({ problem: { status: 503 } });
});
