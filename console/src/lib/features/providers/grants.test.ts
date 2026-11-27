import { describe, expect, it, vi } from 'vitest';
import { json, route, serve } from '#lib/session/test/transport.js';
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

describe('provider grant API', () => {
  it.each([
    ['codex-subscription', 'codex-import'],
    ['claude-subscription', 'claude-import'],
    ['antigravity-subscription', 'antigravity-import']
  ] as const)(
    'imports %s through its authorized endpoint',
    async (profile, path) => {
      const credentials = '{"fixture":"not-a-real-secret"}';
      serve(async (request) => {
        route(request, 'POST', `/api/v1/providers/${provider.id}/${path}`);
        expect(request.headers.get('If-Match')).toBe(`"${provider.etag}"`);
        expect(await request.json()).toEqual({ credentials });
        return json(completion, 201);
      });
      expect(
        await importSubscriptionCredentials(provider, credentials, profile)
      ).toEqual(completion);
    }
  );

  it.each([undefined, enrollment.slot_id])(
    'starts enrollment for slot %s',
    async (slotId) => {
      serve(async (request) => {
        route(
          request,
          'POST',
          `/api/v1/providers/${provider.id}/grant-enrollments`
        );
        expect(request.headers.get('If-Match')).toBe(`"${provider.etag}"`);
        if (slotId) expect(await request.json()).toEqual({ slot_id: slotId });
        else expect(await request.text()).toBe('');
        return json(enrollment, 201);
      });
      expect(await startGrantEnrollment(provider, slotId)).toEqual(enrollment);
    }
  );

  it('continues the selected enrollment with the exact pasted input', async () => {
    const input = 'https://fixture.example/callback?code=fixture&state=bound';
    serve(async (request) => {
      route(
        request,
        'POST',
        `/api/v1/providers/${provider.id}/grant-enrollments/${enrollment.id}/continue`
      );
      expect(await request.json()).toEqual({ input });
      return json(completion);
    });
    expect(await continueGrantEnrollment(enrollment, input)).toEqual(
      completion
    );
  });

  it('polls the selected device enrollment and retains its completion', async () => {
    const status: GrantEnrollmentStatus = { status: 'completed', completion };
    serve((request) => {
      route(
        request,
        'POST',
        `/api/v1/providers/${provider.id}/grant-enrollments/${enrollment.id}/poll`
      );
      return json(status);
    });
    expect(await pollGrantEnrollment(enrollment)).toEqual(status);
  });

  it('cancels the selected enrollment without expecting a response body', async () => {
    const transport = vi.fn((request: Request) => {
      route(
        request,
        'DELETE',
        `/api/v1/providers/${provider.id}/grant-enrollments/${enrollment.id}`
      );
      return new Response(null, { status: 204 });
    });
    serve(transport);
    await expect(cancelGrantEnrollment(enrollment)).resolves.toBeUndefined();
    expect(transport).toHaveBeenCalled();
  });

  it('reports cancellation refusal instead of claiming the enrollment was canceled', async () => {
    serve(() => json({ title: 'Cancellation denied', status: 403 }, 403));
    await expect(cancelGrantEnrollment(enrollment)).rejects.toMatchObject({
      problem: { status: 403, title: 'Cancellation denied' }
    });
  });

  it('surfaces import refusal without returning a partial credential', async () => {
    serve(() => json({ title: 'Import denied', status: 403 }, 403));
    await expect(
      importSubscriptionCredentials(provider, '{}', 'claude-subscription')
    ).rejects.toMatchObject({
      problem: { status: 403, title: 'Import denied' }
    });
  });
});
