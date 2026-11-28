import { flushSync } from 'svelte';
import { assert, expect, it } from 'vitest';
import { projectKeys } from '#lib/domain/projects/projectKeys.js';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import { mountWithState, setFieldValue } from '#test/mount.js';
import ProjectPoliciesPage from './ProjectPoliciesPage.svelte';

const projectId = '00000000-0000-4000-8000-000000000010';

it('keeps an unsaved policy edit when a later membership refresh fails', async () => {
  let reachable = true;
  serve(
    (request) => {
      const { pathname } = new URL(request.url);
      if (pathname === '/api/v1/project-memberships')
        return reachable
          ? json({
              items: [{ id: projectId, name: 'Atlas', role: 'manager' }]
            })
          : json({ title: 'Service Unavailable', status: 503 }, 503);
      if (pathname.endsWith('/route-groups'))
        return json({ groups: { chat: ['alpha'] }, etag: 'loaded' });
      return json({ title: 'Not Found', status: 404 }, 404);
    },
    ['read', 'keys']
  );
  const { target, client } = mountWithState(
    ProjectPoliciesPage,
    { projectId },
    {
      attach: true,
      configureClient: (client) =>
        client.setDefaultOptions({ queries: { retry: false } })
    }
  );
  const memberRoutes = () =>
    [...target.querySelectorAll('label')].find(
      (label) => label.textContent.trim() === 'Member routes'
    )?.control;
  await eventually(() =>
    assert.instanceOf(memberRoutes(), HTMLTextAreaElement)
  );
  const field = memberRoutes() as HTMLTextAreaElement;
  setFieldValue(field, 'alpha, beta');

  reachable = false;
  await client.refetchQueries({ queryKey: projectKeys.memberships });
  await new Promise((resolve) => setTimeout(resolve));
  flushSync();

  expect(client.getQueryState(projectKeys.memberships)?.status).toBe('error');
  expect(field.isConnected).toBe(true);
  expect(field.value).toBe('alpha, beta');
});
