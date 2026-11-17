import { afterEach, expect, it, onTestFinished, vi } from 'vitest';
import { mount, unmount } from 'svelte';
import { json, serve } from '#lib/management/test/transport.js';
import SetupPage from './SetupPage.svelte';

afterEach(() => vi.unstubAllGlobals());

it('holds the owner form until the server can read a setup token', async () => {
  const configured = vi.fn(() => true).mockReturnValueOnce(false);
  serve(() =>
    json({ setup_required: true, bootstrap_token_configured: configured() })
  );
  const target = document.createElement('div');
  const page = mount(SetupPage, { target, props: { onDone: vi.fn() } });
  onTestFinished(() => unmount(page));

  await vi.waitFor(() =>
    expect(target.querySelector('[role="alert"] h2')?.textContent).toBe(
      'No setup token on the server'
    )
  );
  expect(target.querySelector('#setup-token')).toBeNull();

  target.querySelector<HTMLButtonElement>('[role="alert"] button')?.click();
  await vi.waitFor(() =>
    expect(target.querySelector('#setup-token')).not.toBeNull()
  );
});
