import { expect, it, vi } from 'vitest';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import { findButton, mountWithState } from '#test/mount.js';
import CapturePoliciesPanel from './CapturePoliciesPanel.svelte';
import type { CapturePolicy } from './api';

const policy: CapturePolicy = {
  id: '00000000-0000-4000-8000-000000000020',
  project_id: null,
  route_slug: null,
  sink: '00000000-0000-4000-8000-000000000021',
  sample_ratio: '0.5',
  include: ['input'],
  redact: [],
  key_ids: [],
  end_user_digests: [],
  max_bytes: 1024,
  enabled: true,
  expires_at: '2030-01-01T00:00:00Z',
  etag: '00000000-0000-4000-8000-000000000022'
};

it('keeps the stored expiry when an edit leaves the expiry picker untouched', async () => {
  const patched = vi.fn();
  const toggle = Promise.withResolvers<Response>();
  serve(
    async (request) => {
      const path = new URL(request.url).pathname;
      if (request.method === 'PATCH') {
        const body = await request.json();
        patched(path, body);
        if (body.enabled === false) return toggle.promise;
        return json(policy);
      }
      if (path === '/api/v1/observability/capture')
        return json({ enabled: true, etag: 'configuration' });
      if (path === '/api/v1/observability/capture-policies')
        return json({ items: [policy] });
      if (path === '/api/v1/observability/capture-sinks')
        return json({
          items: [
            { export_sink_id: policy.sink, name: 'Capture', type: 'https' }
          ]
        });
      return json({ items: [] });
    },
    ['read', 'settings']
  );
  mountWithState(
    CapturePoliciesPanel,
    {
      canManageInstallation: true,
      canManageProjects: true,
      canMutate: () => true,
      ownerOnly: true
    },
    { attach: true }
  );
  const button = (label: string) => findButton(document, label);

  await eventually(() => expect(button('Edit')).toBeDefined());
  button('Edit')?.click();
  await eventually(() =>
    expect(button('Save capture policy')?.disabled).toBe(false)
  );
  button('Save capture policy')?.click();
  await eventually(() => expect(patched).toHaveBeenCalledOnce());
  const [path, body] = patched.mock.calls[0];
  expect(path).toBe(`/api/v1/observability/capture-policies/${policy.id}`);
  expect(body).toMatchObject({ sink: policy.sink, sample_ratio: '0.5' });
  expect(body).not.toHaveProperty('expires_at');

  await eventually(() => expect(button('Disable')?.disabled).toBe(false));
  button('Disable')?.click();
  await eventually(() => expect(patched).toHaveBeenCalledTimes(2));
  expect(button('Disable')?.disabled).toBe(true);
  expect(button('Delete')?.disabled).toBe(true);
  toggle.resolve(json({ ...policy, enabled: false }));
  await eventually(() => expect(button('Delete')?.disabled).toBe(false));
});
