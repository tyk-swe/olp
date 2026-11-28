import { expect, it, vi } from 'vitest';
import { projectKeys } from '#lib/domain/projects/projectKeys.js';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import { mountWithState, setFieldValue, submitForm } from '#test/mount.js';
import InvestigationsPage from './InvestigationsPage.svelte';
import RunsPage from './RunsPage.svelte';
import TasksPage from './TasksPage.svelte';

it.each([
  { name: 'runs', mount: () => mountWithState(RunsPage) },
  { name: 'tasks', mount: () => mountWithState(TasksPage) },
  { name: 'investigations', mount: () => mountWithState(InvestigationsPage) }
])(
  'keeps the chosen $name project while the project list reloads',
  async ({ mount }) => {
    const memberships = () =>
      json({ items: [{ id: 'project', name: 'Sample project' }] });
    const respond = vi.fn((): Response | Promise<Response> => memberships());
    const listed = vi.fn();
    serve((request) => {
      const url = new URL(request.url);
      if (url.pathname === '/api/v1/project-memberships') return respond();
      listed(url.searchParams.get('project_id'));
      return json({ items: [] });
    });
    const { target, client } = mount();
    const select = target.querySelector('select')!;
    await eventually(() => expect(select.options).toHaveLength(2));
    setFieldValue(select, 'project');
    submitForm(target);
    await eventually(() => expect(listed).toHaveBeenLastCalledWith('project'));

    const held = Promise.withResolvers<Response>();
    respond.mockImplementationOnce(() => held.promise);
    void client.refetchQueries({ queryKey: projectKeys.memberships });
    await eventually(() => expect(select.options).toHaveLength(1));
    held.resolve(memberships());

    await eventually(() => expect(select.options).toHaveLength(2));
    await eventually(() => expect(select.value).toBe('project'));
  }
);
