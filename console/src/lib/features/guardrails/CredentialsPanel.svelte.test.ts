import { expect, it, vi } from 'vitest';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import {
  mountWithState,
  reportedError,
  setFieldValue,
  submitForm
} from '#test/mount.js';
import CredentialsPanel from './CredentialsPanel.svelte';

it('reports an unusable secret at the JSON field before writing it', async () => {
  const writes = vi.fn();
  serve(
    (request) => {
      if (request.method === 'POST') writes();
      return json({ items: [], next_cursor: null });
    },
    ['read', 'configure']
  );
  const { target } = mountWithState(
    CredentialsPanel,
    { projects: [{ id: 'project', name: 'Project' }] },
    { attach: true }
  );
  const secret = await eventually(() => {
    const found = target.querySelector('textarea');
    expect(found).not.toBeNull();
    return found!;
  });
  setFieldValue(target.querySelector('select')!, 'project');
  setFieldValue(target.querySelector('input')!, 'moderation');

  setFieldValue(secret, '["not", "an", "object"]');
  submitForm(target);
  await eventually(() => expect(document.activeElement).toBe(secret));
  expect(reportedError(secret)).toBe('Enter a credential JSON object');

  setFieldValue(secret, '{');
  submitForm(target);
  await eventually(() => expect(reportedError(secret)).toBeTruthy());
  expect(target.querySelector('[role="alert"]')).toBeNull();
  expect(writes).not.toHaveBeenCalled();
});
