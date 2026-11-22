import { afterEach, describe, expect, it, vi } from 'vitest';
import { json, route, serve } from '#lib/management/test/transport.js';
import {
  activatePolicy,
  attachPolicy,
  listDefinitions,
  savePolicy,
  type Policy
} from './api';

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

  it('collects all paginated guardrail definition pages', async () => {
    const fetch = vi.fn(async (request: Request) => {
      route(request, 'GET', '/api/v1/guardrails');
      return new URL(request.url).searchParams.has('cursor')
        ? json({ items: [{ id: 'g-2' }], next_cursor: null })
        : json({ items: [{ id: 'g-1' }], next_cursor: 'page-2' });
    });
    serve(fetch);
    const result = await listDefinitions(new AbortController().signal);
    expect(result.map((item) => item.id)).toEqual(['g-1', 'g-2']);
    expect(new URL(fetch.mock.calls[1][0].url).searchParams.get('cursor')).toBe(
      'page-2'
    );
  });

  it('forwards cancellation while paging guardrail definitions', async () => {
    const controller = new AbortController();
    serve(async (request) => {
      route(request, 'GET', '/api/v1/guardrails');
      if (new URL(request.url).searchParams.has('cursor')) {
        expect(request.signal.aborted).toBe(true);
        throw new DOMException('Request aborted', 'AbortError');
      }
      controller.abort();
      return json({ items: [{ id: 'g-1' }], next_cursor: 'page-2' });
    });
    await expect(listDefinitions(controller.signal)).rejects.toMatchObject({
      name: 'AbortError'
    });
  });
});
