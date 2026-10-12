import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiProblem } from '$lib/api/http';
import { captureRequests, jsonResponse } from '$lib/api/test/requestCapture';
import { authLifecycle } from '$lib/features/access/session/lifecycle';
import { stringifyNativeJSON } from '$lib/json/nativeJson';
import { probeProvider, type Provider } from './providers';
import { certifyProviderModel } from './models';

const providerId = '01980000-0000-7000-8000-000000000101';
const modelId = '01980000-0000-7000-8000-000000000120';
const originalEtag = '01980000-0000-7000-8000-000000000109';
const committedEtag = '"01980000-0000-7000-8000-000000000110"';
const provider = Object.freeze({
  id: providerId,
  etag: originalEtag
}) as Provider;

function session() {
  authLifecycle.establishSession({
    user: {
      id: 'owner',
      email: 'owner@example.com',
      display_name: 'Owner',
      role: 'owner',
      access_scope: 'global',
      operations: ['read', 'configure']
    },
    csrf_token: 'csrf'
  });
}

afterEach(async () => {
  await authLifecycle.principalInvalidated();
  vi.unstubAllGlobals();
});

const operations = [
  {
    name: 'probe',
    invoke: () => probeProvider(provider),
    path: '/api/v1/providers/' + providerId + '/probe',
    body:
      '{"provider_id":"' +
      providerId +
      '","checked_at":"2026-10-11T12:00:00Z","probe_type":"models","succeeded":true,' +
      '"detail":"Ready: 9007199254740993 1e-1000 -0","discovered_models":1}'
  },
  {
    name: 'model certification',
    invoke: () => certifyProviderModel(provider, modelId),
    path: '/api/v1/providers/' + providerId + '/models/' + modelId + '/certify',
    body:
      '{"provider_id":"' +
      providerId +
      '","model_id":"' +
      modelId +
      '","checked_at":"2026-10-11T12:00:00Z","status":"certified","certified_count":1,"attempted_count":1,' +
      '"results":[{"operation":"generation","surface":"openai.responses","mode":"unary","succeeded":true,"detail":"Ready."}]}'
  }
];

for (const operation of operations) {
  describe(operation.name + ' receipt', () => {
    it('keeps the authoritative header separate from the unchanged operation result and input', async () => {
      session();
      const requests = captureRequests(
        () =>
          new Response(operation.body, {
            headers: { 'content-type': 'application/json', ETag: committedEtag }
          })
      );
      const receipt = await operation.invoke();
      expect(receipt.etag).toBe(committedEtag);
      expect(stringifyNativeJSON(receipt.result)).toBe(operation.body);
      expect(provider.etag).toBe(originalEtag);
      expect(requests).toHaveLength(1);
      expect(new URL(requests[0]!.url).pathname).toBe(operation.path);
      expect(requests[0]!.headers.get('If-Match')).toBe(
        '"' + originalEtag + '"'
      );
    });

    it.each([undefined, '', '   '])(
      'refuses to advance from a successful result without an authoritative tag (%s)',
      async (etag) => {
        session();
        const requests = captureRequests(
          () =>
            new Response(operation.body, {
              headers: {
                'content-type': 'application/json',
                ...(etag === undefined ? {} : { ETag: etag })
              }
            })
        );
        await expect(operation.invoke()).rejects.toThrow('no provider version');
        expect(requests).toHaveLength(1);
        expect(provider.etag).toBe(originalEtag);
      }
    );

    it('keeps an actual operation refusal ahead of absent success metadata', async () => {
      session();
      captureRequests(() =>
        jsonResponse(
          {
            type: 'https://openllmproxy.dev/problems/capability_unavailable',
            title: 'Unavailable',
            status: 422,
            detail: 'The reviewed tuple cannot be certified.'
          },
          {
            status: 422,
            headers: { 'content-type': 'application/problem+json' }
          }
        )
      );
      const error = await operation.invoke().catch((value: unknown) => value);
      expect(error).toBeInstanceOf(ApiProblem);
      expect((error as ApiProblem).problem.status).toBe(422);
    });
  });
}
