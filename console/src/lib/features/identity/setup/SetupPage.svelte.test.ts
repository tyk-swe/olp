import { afterEach, expect, it, onTestFinished, vi } from 'vitest';
import { getAllContexts, mount, unmount } from 'svelte';
import { json, serve } from '#lib/session/test/transport.js';
import StateHarness from '../../../../../tests/unit/support/StateHarness.svelte';
import SetupPage from './SetupPage.svelte';
import { eventually } from '../../../../../tests/unit/support/eventually.js';

afterEach(() => vi.unstubAllGlobals());

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
  const target = document.createElement('div');
  const setup = vi.fn(() =>
    mount(SetupPage, {
      target,
      context: getAllContexts(),
      props: { onDone: vi.fn() }
    })
  );
  const harness = mount(StateHarness, { target, props: { setup } });
  onTestFinished(async () => {
    await unmount(setup.mock.results[0].value);
    await unmount(harness);
  });

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
