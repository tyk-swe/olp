import { afterEach, expect, it, onTestFinished, vi } from 'vitest';
import { flushSync, getAllContexts, mount, unmount } from 'svelte';
import { json, serve } from '#lib/management/test/transport.js';
import StateHarness from '../../../../tests/unit/support/StateHarness.svelte';
import ExportSinksPanel from './ExportSinksPanel.svelte';
import type { ExportSink } from './api';

afterEach(() => vi.unstubAllGlobals());

const sink: ExportSink = {
  export_sink_id: '00000000-0000-4000-8000-000000000010',
  name: 'Audit archive',
  project_id: null,
  type: 'https',
  destination: 'https://export.example.test/audit',
  streams: ['audit'],
  format: 'json',
  filter: null,
  enabled: true,
  credential_configured: false,
  etag: '00000000-0000-4000-8000-000000000011',
  status: []
};

it('deletes a sink only after the deletion is confirmed', async () => {
  const deleted = vi.fn();
  const deletion = Promise.withResolvers<Response>();
  serve(
    (request) => {
      if (request.method === 'DELETE') {
        deleted(new URL(request.url).pathname);
        return deletion.promise;
      }
      if (new URL(request.url).pathname === '/api/v1/observability/sinks')
        return json({ items: [sink] });
      return json({ items: [] });
    },
    ['read', 'settings']
  );
  const target = document.createElement('div');
  document.body.append(target);
  const setup = vi.fn(() =>
    mount(ExportSinksPanel, {
      target,
      props: {
        canManageInstallation: true,
        canManageProjects: true,
        canMutate: () => true,
        memberships: []
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
  const button = (root: ParentNode, label: string) =>
    [...root.querySelectorAll('button')].find(
      (item) => item.textContent?.trim() === label
    );

  await vi.waitFor(() => expect(button(target, 'Delete')).toBeDefined());
  button(target, 'Delete')?.click();
  await vi.waitFor(() => expect(button(document, 'Cancel')).toBeDefined());
  button(document, 'Cancel')?.click();
  flushSync();
  await Promise.resolve();
  expect(deleted).not.toHaveBeenCalled();

  button(target, 'Delete')?.click();
  await vi.waitFor(() => expect(button(document, 'Delete sink')).toBeDefined());
  button(document, 'Delete sink')?.click();
  await vi.waitFor(() =>
    expect(deleted).toHaveBeenCalledExactlyOnceWith(
      `/api/v1/observability/sinks/${sink.export_sink_id}`
    )
  );
  expect(button(target, 'Disable')?.disabled).toBe(true);
  expect(button(target, 'Delete')?.disabled).toBe(true);
  deletion.resolve(new Response(null, { status: 204 }));
  await vi.waitFor(() =>
    expect(button(target, 'Disable')?.disabled).toBe(false)
  );
});
