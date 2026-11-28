import { expect, it, vi } from 'vitest';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import {
  findButton,
  mountWithState,
  reportedError,
  setFieldValue,
  submitForm
} from '#test/mount.js';
import BudgetIncreasesPage from './BudgetIncreasesPage.svelte';

function mountPage(written: (request: Request) => Response) {
  serve(
    (request) =>
      request.method === 'GET' ? json({ items: [] }) : written(request),
    ['read', 'keys']
  );
  mountWithState(BudgetIncreasesPage, undefined, { attach: true });
}

async function openDialog(button: string) {
  await eventually(() => expect(findButton(document, button)).toBeDefined());
  findButton(document, button)!.click();
  await eventually(() =>
    expect(document.querySelector('dialog form')).not.toBeNull()
  );
  const dialog = document.querySelector('dialog')!;
  return {
    dialog,
    field: (label: string) =>
      [...dialog.querySelectorAll('label')].find(
        (item) => item.textContent?.trim() === label
      )!.control as HTMLInputElement
  };
}

it('reports a refused increase at the input that holds the mistake', async () => {
  const written = vi.fn(() =>
    json(
      {
        title: 'The request is invalid.',
        status: 422,
        errors: {
          expires_at: [{ code: 'invalid', message: 'Refused expires_at.' }]
        }
      },
      422
    )
  );
  mountPage(written);
  const { dialog, field } = await openDialog('New increase');
  setFieldValue(field('API key ID'), 'key-1');
  setFieldValue(field('Additional USD'), 'ten');
  setFieldValue(field('Reason'), 'Launch week');
  submitForm(dialog);
  await eventually(() =>
    expect(reportedError(field('Additional USD'))).toContain(
      'positive decimals'
    )
  );
  expect(written).not.toHaveBeenCalled();

  setFieldValue(field('Additional USD'), '10');
  submitForm(dialog);
  await eventually(() =>
    expect(reportedError(field('Earlier expiry (optional)'))).toBe(
      'Refused expires_at.'
    )
  );
  expect(reportedError(field('Additional USD'))).toBeUndefined();
  expect(written).toHaveBeenCalledOnce();
});

it('reports every missing value of an increase request at its input', async () => {
  const written = vi.fn(() => json({}));
  mountPage(written);
  const { dialog, field } = await openDialog('Request increase');
  submitForm(dialog);
  await eventually(() =>
    expect(reportedError(field('Reason'))).toBe('Supply a reason.')
  );
  expect(reportedError(field('Project'))).toBe('Choose a project.');
  expect(reportedError(field('Additional USD'))).toBe(
    'Supply a positive USD amount.'
  );
  expect(written).not.toHaveBeenCalled();
});
