import { getAllContexts, mount, unmount } from 'svelte';
import { expect, it, onTestFinished } from 'vitest';
import { json, route, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import { mountState } from '#test/mount.js';
import ProjectScopeField from './ProjectScopeField.svelte';

it('offers the current memberships to a field opened after they were first read', async () => {
  let names = ['Atlas'];
  serve(
    (request) => {
      route(request, 'GET', '/api/v1/project-memberships');
      return json({
        items: names.map((name) => ({ id: name, name, role: 'manager' }))
      });
    },
    ['read']
  );
  const { value: openField } = mountState(
    () => {
      const context = getAllContexts();
      return () => {
        const target = document.createElement('div');
        const field = mount(ProjectScopeField, {
          target,
          props: { value: '', unassigned: false },
          context
        });
        onTestFinished(() => unmount(field));
        return () =>
          [...target.querySelectorAll('option')].map((option) =>
            option.textContent?.trim()
          );
      };
    },
    {
      configureClient: (client) =>
        client.setDefaultOptions({ queries: { staleTime: Infinity } })
    }
  );

  const first = openField();
  await eventually(() => expect(first()).toEqual(['Atlas']));

  names = ['Atlas', 'Beacon'];
  const second = openField();

  await eventually(() => expect(second()).toEqual(['Atlas', 'Beacon']));
});
