import { afterEach, expect, it, onTestFinished, vi } from 'vitest';
import { getAllContexts, mount, unmount } from 'svelte';
import { json, serve } from '#lib/management/test/transport.js';
import StateHarness from '../../../../tests/unit/support/StateHarness.svelte';
import CapturePoliciesPanel from './CapturePoliciesPanel.svelte';
import type { CapturePolicy } from './api';

afterEach(() => vi.unstubAllGlobals());

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
  const target = document.createElement('div');
  document.body.append(target);
  const setup = vi.fn(() =>
    mount(CapturePoliciesPanel, {
      target,
      props: {
        canManageInstallation: true,
        canManageProjects: true,
        canMutate: () => true,
        ownerOnly: true
      },
      context: getAllContexts()
    })
  );
  const harness = mount(StateHarness, { target, props: { setup } });
  onTestFinished(async () => {
    await unmount(setup.mock.results[0].value);
    await unmount(harness);
    target.remove();
  });
  const button = (label: string) =>
    [...document.querySelectorAll('button')].find(
      (item) => item.textContent?.trim() === label
    );

  await vi.waitFor(() => expect(button('Edit')).toBeDefined());
  button('Edit')?.click();
  await vi.waitFor(() =>
    expect(button('Save capture policy')?.disabled).toBe(false)
  );
  button('Save capture policy')?.click();
  await vi.waitFor(() => expect(patched).toHaveBeenCalledOnce());
  const [path, body] = patched.mock.calls[0];
  expect(path).toBe(`/api/v1/observability/capture-policies/${policy.id}`);
  expect(body).toMatchObject({ sink: policy.sink, sample_ratio: '0.5' });
  expect(body).not.toHaveProperty('expires_at');

  await vi.waitFor(() => expect(button('Disable')?.disabled).toBe(false));
  button('Disable')?.click();
  await vi.waitFor(() => expect(patched).toHaveBeenCalledTimes(2));
  expect(button('Disable')?.disabled).toBe(true);
  expect(button('Delete')?.disabled).toBe(true);
  toggle.resolve(json({ ...policy, enabled: false }));
  await vi.waitFor(() => expect(button('Delete')?.disabled).toBe(false));
});
