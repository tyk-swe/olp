import { afterEach, expect, it, onTestFinished, vi } from 'vitest';
import { flushSync, getAllContexts, mount, unmount } from 'svelte';
import { json, serve } from '#lib/management/test/transport.js';
import StateHarness from '../../../../tests/unit/support/StateHarness.svelte';
import ConfigurationPanel from './ConfigurationPanel.svelte';

afterEach(() => vi.unstubAllGlobals());

it('retires the ready plan when external credential bindings change', async () => {
  const plans = vi.fn<(request: Request) => void>();
  serve(
    (request) => {
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/configuration/plan') {
        plans(request);
        return json({
          digest: 'plan-digest',
          actions: [],
          blockers: [],
          conflicts: []
        });
      }
      return json({});
    },
    ['read', 'configure']
  );
  const target = document.createElement('div');
  const setup = vi.fn(() =>
    mount(ConfigurationPanel, { target, context: getAllContexts() })
  );
  const harness = mount(StateHarness, { target, props: { setup } });
  onTestFinished(async () => {
    await unmount(setup.mock.results[0].value);
    await unmount(harness);
  });
  const button = (name: string) =>
    [...target.querySelectorAll('button')].find(
      (item) => item.textContent?.trim() === name
    );
  const click = (name: string) => {
    button(name)?.click();
    flushSync();
  };
  flushSync();
  const artifact = target.querySelector<HTMLTextAreaElement>(
    '#promotion-artifact'
  );
  expect(artifact).not.toBeNull();
  if (!artifact) return;
  artifact.value = JSON.stringify({
    api_version: 'openllmproxy.dev/config/v2',
    projects: [],
    providers: [],
    routes: [],
    pricing: null
  });
  artifact.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  click('Plan');
  await vi.waitFor(() =>
    expect(
      target.querySelector('[data-testid="promotion-plan"]')
    ).not.toBeNull()
  );
  await vi.waitFor(() => expect(button('Apply')).toBeDefined());
  const external = target.querySelectorAll<HTMLTextAreaElement>('textarea')[1];
  expect(external).toBeDefined();
  external.value =
    '{"provider/primary":{"store":"gcp","secret_id":"projects/project/secrets/provider","version":"1"}}';
  external.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  expect(target.querySelector('[data-testid="promotion-plan"]')).toBeNull();
  expect(button('Apply')).toBeUndefined();
  click('Plan');
  await vi.waitFor(() => expect(plans).toHaveBeenCalledTimes(2));
  await vi.waitFor(() =>
    expect(
      target.querySelector('[data-testid="promotion-plan"]')
    ).not.toBeNull()
  );
  await vi.waitFor(() => expect(button('Apply')).toBeDefined());
});
