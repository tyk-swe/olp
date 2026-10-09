// @vitest-environment jsdom
import { mount, unmount, flushSync } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { expect, it, vi } from 'vitest';
import ScimProbe from './test/ScimProbe.svelte';
import { saveMapping } from './api';
const group = vi.hoisted(() => ({
  id: 'group',
  display_name: 'Builders',
  external_id: null,
  etag: 'revision',
  member_count: 2,
  mapping: { role: 'developer', accessScope: 'assigned', projects: [] }
}));
vi.mock('../session/useRole.svelte', () => ({
  useRole: () => ({ allows: () => true })
}));
vi.mock('../projects/api', () => ({
  listProjects: vi.fn(async () => [{ id: 'project', name: 'Build' }])
}));
vi.mock('./api', () => ({
  listGroups: vi.fn(async () => [group]),
  saveMapping: vi.fn(async (_group, mapping) => ({
    ...group,
    etag: 'updated',
    mapping
  }))
}));
it('edits inherited grants using the current group ETag without changing members', async () => {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  const target = document.createElement('div');
  document.body.append(target);
  const view = mount(ScimProbe, { target, props: { client } });
  const click = (label: string) => {
    const button = [...target.querySelectorAll('button')].find(
      (b) => b.textContent?.trim() === label
    );
    expect(button).toBeTruthy();
    button!.click();
    flushSync();
  };
  try {
    await vi.waitFor(() => expect(target.textContent).toContain('Builders'));
    click('Edit Builders');
    click('Add project membership');
    const project = target.querySelector<HTMLSelectElement>(
      'select[id^="scim-project-"]'
    )!;
    project.value = 'project';
    project.dispatchEvent(new Event('change', { bubbles: true }));
    flushSync();
    target
      .querySelector('form')!
      .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await vi.waitFor(() => expect(saveMapping).toHaveBeenCalledTimes(1));
    expect(vi.mocked(saveMapping).mock.calls[0]).toEqual([
      group,
      {
        role: 'developer',
        accessScope: 'assigned',
        projects: [{ value: 'project', role: 'viewer' }]
      }
    ]);
    await vi.waitFor(() =>
      expect(target.textContent).toContain('Group access saved.')
    );
  } finally {
    await unmount(view);
    client.clear();
    target.remove();
  }
});
