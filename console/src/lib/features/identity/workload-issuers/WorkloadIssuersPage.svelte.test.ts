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
import WorkloadIssuersPage from './WorkloadIssuersPage.svelte';

it('reports a refused issuer inside the editor', async () => {
  const created = vi.fn();
  serve(
    (request) => {
      if (request.method === 'GET')
        return json({ items: [], next_cursor: null });
      created();
      return json(
        {
          title: 'Invalid request',
          detail: 'Use secure identity endpoint URLs.',
          status: 422,
          errors: {
            issuer: [
              { code: 'invalid', message: 'Use secure identity endpoint URLs.' }
            ]
          }
        },
        422
      );
    },
    ['access']
  );
  const { target } = mountWithState(WorkloadIssuersPage, {}, { attach: true });
  const open = await eventually(() => {
    const button = findButton(target, 'New issuer');
    expect(button).toBeDefined();
    return button!;
  });
  open.click();
  const lifetime = await eventually(() =>
    findControl(document.body, 'Maximum token lifetime (seconds)')
  );
  const algorithm = document.body.querySelector<HTMLInputElement>(
    'input[value="RS256"]'
  )!;
  const mappings = document.body.querySelector<HTMLTextAreaElement>(
    'textarea[rows="17"]'
  )!;
  const validMappings = mappings.value;
  algorithm.click();
  setFieldValue(lifetime, '10');
  setFieldValue(mappings, '[]');

  submitForm(document.body);
  await eventually(() => expect(document.activeElement).toBe(algorithm));
  expect(reportedError(algorithm)).toBe('Choose a signature algorithm.');
  expect(reportedError(lifetime)).toBe(
    'Enter a lifetime from 60 to 86400 seconds.'
  );
  expect(reportedError(mappings)).toBe('Provide one to 32 claim mappings.');
  expect(created).not.toHaveBeenCalled();

  algorithm.click();
  setFieldValue(lifetime, '3600');
  setFieldValue(mappings, validMappings);
  submitForm(document.body);

  const refusal = await eventually(() => {
    const alert = document.body.querySelector('form [role="alert"]');
    expect(alert).not.toBeNull();
    return alert!;
  });
  expect(refusal.textContent).toContain('Use secure identity endpoint URLs.');
  await eventually(() => expect(document.activeElement).toBe(refusal));
  expect(document.body.querySelectorAll('[role="alert"]')).toHaveLength(1);
  expect(reportedError(lifetime)).toBeUndefined();
  expect(created).toHaveBeenCalledOnce();
});
