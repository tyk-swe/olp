import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { expect, it, onTestFinished, vi } from 'vitest';
import * as api from './api';
import PublicationsProbe from './test/PublicationsProbe.svelte';

vi.mock('./api', async (original) => ({
  ...(await original<typeof import('./api')>()),
  listPublications: vi.fn(),
  listProjects: vi.fn(),
  listServers: vi.fn(),
  getPublication: vi.fn(),
  savePublication: vi.fn()
}));
vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({ allows: () => true, user: null, globalScope: false })
}));

it("saves and reloads a tool's execution classification", async () => {
  const tool = {
    name: 'lookup',
    alias: 'lookup_alias',
    server_id: 'server',
    revision_id: 'revision',
    catalog_digest: 'digest'
  };
  const publication: api.Publication = {
    id: 'toolset',
    project_id: 'project',
    kind: 'toolset',
    name: 'Lookup tools',
    etag: 'etag',
    revision_id: 'revision',
    retired: false,
    draft: {
      project_id: 'project',
      name: 'Lookup tools',
      toolset: { tools: [tool] }
    }
  };
  vi.mocked(api.listPublications).mockResolvedValue([publication]);
  vi.mocked(api.listProjects).mockResolvedValue([]);
  vi.mocked(api.listServers).mockResolvedValue([]);
  vi.mocked(api.getPublication).mockResolvedValue(publication);
  vi.mocked(api.savePublication).mockImplementation(
    async (_kind, current, input) => ({
      ...current!,
      draft: input
    })
  );
  const host = document.createElement('div');
  document.body.append(host);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  const component = mount(PublicationsProbe, {
    target: host,
    props: { client }
  });
  onTestFinished(async () => {
    await unmount(component);
    client.clear();
    host.remove();
  });
  await vi.waitFor(() => expect(host.textContent).toContain('Lookup tools'));
  const open = [...host.querySelectorAll('button')].find(
    (item) => item.textContent?.trim() === 'Open'
  );
  expect(open).toBeDefined();
  open!.click();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Review publication')
  );
  const control = host.querySelector<HTMLSelectElement>(
    'select[aria-label="Execution classification for lookup"]'
  );
  expect(control).not.toBeNull();
  expect(control!.value).toBe('');
  control!.value = 'read_only';
  control!.dispatchEvent(new Event('change', { bubbles: true }));
  flushSync();
  const form = host.querySelector<HTMLFormElement>(
    'section[aria-label="Publication editor"] form'
  );
  expect(form).not.toBeNull();
  form!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() => expect(api.savePublication).toHaveBeenCalled());
  expect(
    vi.mocked(api.savePublication).mock.calls[0][2].toolset?.tools
  ).toEqual([{ ...tool, classification: 'read_only' }]);
  await vi.waitFor(() =>
    expect(
      host.querySelector<HTMLSelectElement>(
        'select[aria-label="Execution classification for lookup"]'
      )?.value
    ).toBe('read_only')
  );
});
