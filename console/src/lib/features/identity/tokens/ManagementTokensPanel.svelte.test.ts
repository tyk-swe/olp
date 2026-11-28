import { expect, it, vi } from 'vitest';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import {
  findButton,
  findControl,
  mountWithState,
  reportedError,
  setFieldValue,
  submitForm
} from '#test/mount.js';
import ManagementTokensPanel from './ManagementTokensPanel.svelte';

it('reports a refused token at the controls that hold it', async () => {
  const created = vi.fn();
  serve(
    async (request) => {
      if (request.method === 'GET')
        return json({ items: [], next_cursor: null });
      created(await request.json());
      return json(
        {
          title: 'Invalid request',
          status: 422,
          errors: {
            scopes: [{ code: 'invalid', message: 'Select a delegable scope.' }]
          }
        },
        422
      );
    },
    ['manage_tokens']
  );
  const { target } = mountWithState(
    ManagementTokensPanel,
    {},
    { attach: true }
  );
  const open = await eventually(() => {
    const button = findButton(target, 'New token');
    expect(button).toBeDefined();
    return button!;
  });
  open.click();
  const name = await eventually(() => findControl(document.body, 'Name'));
  const read = document.body.querySelector<HTMLInputElement>(
    '.token-form input[type="checkbox"]'
  )!;
  const selected = [
    ...document.body.querySelectorAll<HTMLInputElement>(
      'input[name="token-projects"]'
    )
  ][1];
  read.click();
  selected.click();

  submitForm(document.body);
  await eventually(() => expect(document.activeElement).toBe(name));
  expect(reportedError(name)).toBe('Enter a token name.');
  expect(reportedError(read)).toBe('Select at least one scope.');
  expect(reportedError(selected)).toBe(
    'Select at least one project, or choose all projects.'
  );
  expect(created).not.toHaveBeenCalled();

  setFieldValue(name, 'deploy-automation');
  read.click();
  document.body
    .querySelector<HTMLInputElement>('input[name="token-projects"]')!
    .click();
  submitForm(document.body);

  await eventually(() =>
    expect(reportedError(read)).toBe('Select a delegable scope.')
  );
  expect(reportedError(name)).toBeUndefined();
  expect(document.body.querySelector('[role="alert"]')).toBeNull();
  expect(created).toHaveBeenCalledOnce();
});
