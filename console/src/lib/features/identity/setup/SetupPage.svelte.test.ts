import { expect, it, vi } from 'vitest';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import { mountWithState } from '#test/mount.js';
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
