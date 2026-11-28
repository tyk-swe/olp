import { flushSync } from 'svelte';
import { assert, expect, it } from 'vitest';
import { projectKeys } from '#lib/domain/projects/projectKeys.js';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import {
  clickButton,
  findButton,
  mountWithState,
  setFieldValue
} from '#test/mount.js';
import CodeModePage from './CodeModePage.svelte';

it('keeps an unsaved account when a later membership refresh fails', async () => {
  let reachable = true;
  serve(
    (request) => {
      const { pathname } = new URL(request.url);
      if (pathname === '/api/v1/project-memberships')
        return reachable
          ? json({ items: [{ id: 'atlas', name: 'Atlas', role: 'manager' }] })
          : json({ title: 'Service Unavailable', status: 503 }, 503);
      if (pathname === '/api/v1/code/accounts') return json({ items: [] });
      return json({ title: 'Not Found', status: 404 }, 404);
    },
    ['read', 'configure']
  );
  const { target, client } = mountWithState(
    CodeModePage,
    {},
    {
      attach: true,
      configureClient: (client) =>
        client.setDefaultOptions({ queries: { retry: false } })
    }
  );
  await eventually(() =>
    assert.isDefined(findButton(target, 'Create account'))
  );
  clickButton(target, 'Create account');
  const nameField = () => target.querySelector('#code-name');
  await eventually(() => assert.instanceOf(nameField(), HTMLInputElement));
  const name = nameField() as HTMLInputElement;
  setFieldValue(name, 'Team subscription');

  reachable = false;
  await client.refetchQueries({ queryKey: projectKeys.memberships });
  await new Promise((resolve) => setTimeout(resolve));
  flushSync();

  expect(client.getQueryState(projectKeys.memberships)?.status).toBe('error');
  expect(name.isConnected).toBe(true);
  expect(name.value).toBe('Team subscription');
});
