import { expect, it, vi } from 'vitest';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import {
  mountWithState,
  reportedError,
  setFieldValue,
  submitForm
} from '#test/mount.js';
import SetupPage from './SetupPage.svelte';

it('holds the owner form until the server can read a setup token', async () => {
  const configured = vi.fn(() => true).mockReturnValueOnce(false);
  serve((request) =>
    new URL(request.url).pathname === '/api/v1/setup/status'
      ? json({
          setup_required: true,
          bootstrap_token_configured: configured()
        })
      : json({ name: 'OpenLLMProxy', logo: '', accent: '#2563eb' })
  );
  const { target } = mountWithState(SetupPage, { onDone: vi.fn() });

  await eventually(() =>
    expect(target.querySelector('[role="alert"] h2')?.textContent).toBe(
      'No setup token on the server'
    )
  );
  expect(target.querySelector('#setup-token')).toBeNull();

  target.querySelector<HTMLButtonElement>('[role="alert"] button')?.click();
  await eventually(() =>
    expect(target.querySelector('#setup-token')).not.toBeNull()
  );
});

it('moves to the refused owner field and keeps what no field holds', async () => {
  serve((request) => {
    if (request.method === 'POST')
      return json(
        {
          title: 'Invalid request',
          detail: 'The owner account was refused.',
          status: 422,
          errors: {
            setup_token: [{ code: 'invalid', message: 'That token expired.' }],
            region: [{ code: 'invalid', message: 'Unknown region.' }]
          }
        },
        422
      );
    return new URL(request.url).pathname === '/api/v1/setup/status'
      ? json({ setup_required: true, bootstrap_token_configured: true })
      : json({ name: 'OpenLLMProxy', logo: '', accent: '#2563eb' });
  });
  const { target } = mountWithState(
    SetupPage,
    { onDone: vi.fn() },
    { attach: true }
  );
  const input = (id: string) => target.querySelector<HTMLInputElement>(id)!;
  await eventually(() => expect(input('#setup-token')).not.toBeNull());

  submitForm(target);
  await eventually(() =>
    expect(document.activeElement).toBe(input('#display-name'))
  );

  setFieldValue(input('#display-name'), 'Olive Owner');
  setFieldValue(input('#owner-email'), 'owner@example.test');
  setFieldValue(input('#owner-password'), 'correct horse battery staple');
  setFieldValue(input('#confirm-password'), 'correct horse battery staple');
  setFieldValue(input('#setup-token'), 'made-up-token');
  submitForm(target);

  await eventually(() =>
    expect(document.activeElement).toBe(input('#setup-token'))
  );
  expect(reportedError(input('#display-name'))).toBeUndefined();
  expect(input('#setup-token').getAttribute('aria-invalid')).toBe('true');
  expect(target.querySelector('#setupToken-error')?.textContent?.trim()).toBe(
    'That token expired.'
  );
  expect(target.querySelector('form [role="alert"]')?.textContent).toContain(
    'The owner account was refused.'
  );
});
