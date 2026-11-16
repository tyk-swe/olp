import { afterEach, describe, expect, it, vi } from 'vitest';
import { json, route, serve } from '#lib/management/test/transport.js';
import { activatePolicy, attachPolicy, savePolicy, type Policy } from './api';

afterEach(() => vi.unstubAllGlobals());

const policy: Policy = {
  id: 'policy-id',
  policy_id: 'policy-id',
  project_id: 'project-id',
  name: 'Baseline',
  entries: [],
  etag: 'policy-etag',
  revision_id: 'revision-id'
};

describe('guardrail management API', () => {
  it('sends the caller-owned request identity on policy save and activation', async () => {
    const keys = vi.fn<(key: string | null) => void>();
    serve(async (request) => {
      keys(request.headers.get('Idempotency-Key'));
      if (request.method === 'PUT') {
        route(request, 'PUT', '/api/v1/guardrail-policies/policy-id');
        return json(policy);
      }
      route(request, 'POST', '/api/v1/guardrail-policies/policy-id/activate');
      expect(await request.json()).toEqual({
        revision_id: policy.revision_id
      });
      return json({});
    });
    await savePolicy(
      policy,
      'project-id',
      { name: 'Baseline', entries: [] },
      'save-attempt'
    );
    await activatePolicy(policy, 'activate-attempt');
    await activatePolicy(policy, 'activate-attempt');
    expect(keys.mock.calls).toEqual([
      ['save-attempt'],
      ['activate-attempt'],
      ['activate-attempt']
    ]);
  });

  it('sends the caller-owned request identity when attaching a policy', async () => {
    serve((request) => {
      route(request, 'POST', '/api/v1/guardrail-attachments');
      expect(request.headers.get('Idempotency-Key')).toBe('attach-attempt');
      return json({});
    });
    await attachPolicy(
      {
        scope: 'project',
        scope_id: 'project-id',
        policy_revision_id: 'revision-id',
        order: 0
      },
      'attach-attempt'
    );
  });
});
