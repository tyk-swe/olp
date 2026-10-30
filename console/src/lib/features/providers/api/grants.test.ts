import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  cancelGrantEnrollment,
  continueGrantEnrollment,
  importSubscriptionCredentials,
  pollGrantEnrollment,
  startGrantEnrollment,
  type GrantEnrollment,
  type GrantEnrollmentCompletion,
  type GrantEnrollmentStatus
} from './grants';

vi.mock('$lib/features/access/session/authMiddleware', () => ({
  createAuthMiddleware: () => ({
    onRequest: ({ request }: { request: Request }) => request
  })
}));

const provider = {
  id: '10000000-0000-4000-8000-000000000001',
  etag: '10000000-0000-4000-8000-000000000002'
};
const enrollment: GrantEnrollment = {
  id: '10000000-0000-4000-8000-000000000003',
  provider_id: provider.id,
  slot_id: '10000000-0000-4000-8000-000000000004',
  expires_at: new Date(0).toISOString(),
  authorization_url: 'https://fixture.example/authorize'
};
const completion: GrantEnrollmentCompletion = {
  provider_id: provider.id,
  etag: '10000000-0000-4000-8000-000000000005',
  credential_id: '10000000-0000-4000-8000-000000000006',
  credential_version: 2,
  principal: 'verified-fixture-principal'
};

function respond(handler: (request: Request) => Response | Promise<Response>) {
  vi.stubGlobal('fetch', vi.fn(handler));
}

afterEach(() => vi.unstubAllGlobals());

describe('provider grant API', () => {
  it.each([
    ['codex-subscription', 'codex-import'],
    ['claude-subscription', 'claude-import'],
    ['antigravity-subscription', 'antigravity-import']
  ] as const)(
    'imports %s through its authorized endpoint',
    async (profile, path) => {
      const credentials = '{"fixture":"not-a-real-secret"}';
      respond(async (request) => {
        expect(new URL(request.url).pathname).toBe(
          `/api/v1/providers/${provider.id}/${path}`
        );
        expect(request.method).toBe('POST');
        expect(request.headers.get('If-Match')).toBe(`"${provider.etag}"`);
        expect(await request.json()).toEqual({ credentials });
        return Response.json(completion, { status: 201 });
      });
      expect(
        await importSubscriptionCredentials(provider, credentials, profile)
      ).toEqual(completion);
    }
  );

  it.each([undefined, enrollment.slot_id])(
    'starts enrollment for slot %s',
    async (slotId) => {
      respond(async (request) => {
        expect(new URL(request.url).pathname).toBe(
          `/api/v1/providers/${provider.id}/grant-enrollments`
        );
        expect(request.method).toBe('POST');
        expect(request.headers.get('If-Match')).toBe(`"${provider.etag}"`);
        if (slotId) expect(await request.json()).toEqual({ slot_id: slotId });
        else expect(await request.text()).toBe('');
        return Response.json(enrollment, { status: 201 });
      });
      expect(await startGrantEnrollment(provider, slotId)).toEqual(enrollment);
    }
  );

  it('continues the selected enrollment with the exact pasted input', async () => {
    const input = 'https://fixture.example/callback?code=fixture&state=bound';
    respond(async (request) => {
      expect(new URL(request.url).pathname).toBe(
        `/api/v1/providers/${provider.id}/grant-enrollments/${enrollment.id}/continue`
      );
      expect(request.method).toBe('POST');
      expect(await request.json()).toEqual({ input });
      return Response.json(completion);
    });
    expect(await continueGrantEnrollment(enrollment, input)).toEqual(
      completion
    );
  });

  it('polls the selected device enrollment and retains its completion', async () => {
    const status: GrantEnrollmentStatus = { status: 'completed', completion };
    respond((request) => {
      expect(new URL(request.url).pathname).toBe(
        `/api/v1/providers/${provider.id}/grant-enrollments/${enrollment.id}/poll`
      );
      expect(request.method).toBe('POST');
      return Response.json(status);
    });
    expect(await pollGrantEnrollment(enrollment)).toEqual(status);
  });

  it('cancels the selected enrollment without expecting a response body', async () => {
    respond((request) => {
      expect(new URL(request.url).pathname).toBe(
        `/api/v1/providers/${provider.id}/grant-enrollments/${enrollment.id}`
      );
      expect(request.method).toBe('DELETE');
      return new Response(null, { status: 204 });
    });
    await expect(cancelGrantEnrollment(enrollment)).resolves.toBeUndefined();
  });

  it('surfaces import refusal without returning a partial credential', async () => {
    respond(() =>
      Response.json({ title: 'Import denied', status: 403 }, { status: 403 })
    );
    await expect(
      importSubscriptionCredentials(provider, '{}', 'claude-subscription')
    ).rejects.toMatchObject({
      problem: { status: 403, title: 'Import denied' }
    });
  });
});
