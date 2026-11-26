import { expect, it, vi } from 'vitest';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import { findButton, mountWithState, setFieldValue } from '#test/mount.js';
import { agentKeys } from './agentKeys';
import type { Publication } from './api';
import PublicationsPage from './PublicationsPage.svelte';

it('retries publication refresh after a saved write without submitting the write again', async () => {
  const publication: Publication = {
    id: 'prompt',
    project_id: 'project',
    kind: 'prompt',
    name: 'Original',
    etag: 'before',
    revision_id: null,
    retired: false,
    draft: {
      project_id: 'project',
      name: 'Original',
      prompt: { template: 'Hello', variables: [] }
    }
  };
  const writes = vi.fn();
  const recovered = vi.fn();
  serve(
    async (request) => {
      const path = new URL(request.url).pathname;
      if (path.endsWith('/projects'))
        return json({ items: [{ id: 'project', name: 'Project' }] });
      if (request.method === 'PUT') {
        const input = await request.json();
        writes(input);
        return json({
          ...publication,
          name: 'Saved',
          etag: 'after',
          draft: input
        });
      }
      if (path.endsWith('/prompt')) return json(publication);
      if (writes.mock.calls.length && !recovered.mock.calls.length)
        return json({ status: 503, title: 'Refresh unavailable' }, 503);
      return json({ items: [publication] });
    },
    ['read', 'configure']
  );
  const { target } = mountWithState(
    PublicationsPage,
    { kind: 'prompts' },
    {
      configureClient: (client) =>
        client.setQueryDefaults(agentKeys.publications, { retry: false })
    }
  );
  const button = (name: string) => findButton(target, name);
  await eventually(() => expect(button('Open')).toBeDefined());
  button('Open')?.click();
  await eventually(() => expect(target.querySelector('form')).not.toBeNull());
  const name = target.querySelector<HTMLInputElement>('form input');
  expect(name).not.toBeNull();
  if (!name) return;
  setFieldValue(name, 'Saved');
  target
    .querySelector('form')
    ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await eventually(() => expect(button('Retry refresh')?.disabled).toBe(false));
  expect(writes).toHaveBeenCalledOnce();
  expect(name.value).toBe('Saved');
  expect(target.textContent).toContain('Draft saved.');
  recovered();
  button('Retry refresh')?.click();
  await eventually(() => expect(button('Retry refresh')).toBeUndefined());
  expect(writes).toHaveBeenCalledOnce();
  expect(target.textContent).not.toContain(
    'Write saved, but refreshing failed.'
  );
});
