import { expect, it, vi } from 'vitest';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import { mountWithState, reportedError, submitForm } from '#test/mount.js';
import ScimPage from './ScimPage.svelte';

vi.mock('$app/paths', () => ({ resolve: (path: string) => path }));

it('reports a membership without its own project at that row', async () => {
  const saves = vi.fn();
  serve(
    (request) => {
      if (request.method !== 'GET') saves();
      if (new URL(request.url).pathname !== '/api/v1/scim/groups')
        return json({ items: [] });
      return json({
        items: [
          {
            id: 'group-id',
            etag: 'group-etag',
            display_name: 'Platform',
            member_count: 0,
            mapping: {
              accessScope: 'assigned',
              projects: [
                { value: 'project-id', role: 'viewer' },
                { value: 'project-id', role: 'manager' },
                { value: '', role: 'viewer' }
              ]
            }
          }
        ]
      });
    },
    ['access', 'access_read']
  );
  const { target } = mountWithState(ScimPage, {}, { attach: true });
  const open = await eventually(() => {
    const button = [...target.querySelectorAll('button')].find((item) =>
      item.textContent?.includes('Platform')
    );
    expect(button).toBeDefined();
    return button!;
  });
  open.click();
  const projects = await eventually(() => {
    const selects = [...target.querySelectorAll('label')]
      .filter((label) => label.textContent?.trim() === 'Project')
      .map((label) => label.control!);
    expect(selects).toHaveLength(3);
    return selects;
  });

  submitForm(target);
  await eventually(() => expect(document.activeElement).toBe(projects[1]));
  expect(projects.map(reportedError)).toEqual([
    undefined,
    'Choose each project at most once.',
    'Choose a project.'
  ]);
  expect(target.querySelector('[role="alert"]')).toBeNull();
  expect(saves).not.toHaveBeenCalled();
});
