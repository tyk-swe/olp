import { afterEach, expect, it, onTestFinished, vi } from 'vitest';
import { flushSync, getAllContexts, mount, unmount } from 'svelte';
import { json, serve } from '#lib/session/test/transport.js';
import StateHarness from '../../../../../tests/unit/support/StateHarness.svelte';
import ConfigurationPanel from './ConfigurationPanel.svelte';
import { eventually } from '../../../../../tests/unit/support/eventually.js';

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
  await eventually(() =>
    expect(
      target.querySelector('[data-testid="promotion-plan"]')
    ).not.toBeNull()
  );
  await eventually(() => expect(button('Apply')).toBeDefined());
  const external = target.querySelectorAll<HTMLTextAreaElement>('textarea')[1];
  expect(external).toBeDefined();
  external.value =
    '{"provider/primary":{"store":"gcp","secret_id":"projects/project/secrets/provider","version":"1"}}';
  external.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  expect(target.querySelector('[data-testid="promotion-plan"]')).toBeNull();
  expect(button('Apply')).toBeUndefined();
  click('Plan');
  await eventually(() => expect(plans).toHaveBeenCalledTimes(2));
  await eventually(() =>
    expect(
      target.querySelector('[data-testid="promotion-plan"]')
    ).not.toBeNull()
  );
  await eventually(() => expect(button('Apply')).toBeDefined());
});

it('ignores a deferred plan result after the artifact changed', async () => {
  let release: (value: Response) => void = () => {};
  const pending = new Promise<Response>((resolve) => (release = resolve));
  serve(
    (request) => {
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/configuration/plan') return pending;
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
  const setArtifact = (id: string) => {
    const field = target.querySelector<HTMLTextAreaElement>(
      '#promotion-artifact'
    );
    if (!field) throw new Error('Missing artifact field');
    field.value = JSON.stringify({
      api_version: 'openllmproxy.dev/config/v2',
      projects: [{ id }],
      providers: [],
      routes: [],
      pricing: null
    });
    field.dispatchEvent(new Event('input', { bubbles: true }));
    flushSync();
  };
  flushSync();
  setArtifact('first');
  click('Plan');
  await eventually(() => expect(button('Plan')?.disabled).toBe(true));
  setArtifact('second');
  release(
    json({
      digest: 'plan-digest',
      actions: [],
      blockers: [],
      conflicts: []
    })
  );
  await eventually(() => expect(button('Plan')?.disabled).toBe(false));
  expect(target.querySelector('[data-testid="promotion-plan"]')).toBeNull();
  expect(button('Apply')).toBeUndefined();
  click('Plan');
});

it('keeps Apply and Plan disabled until a conflict refresh finishes', async () => {
  const refresh = Promise.withResolvers<Response>();
  const planResponse = (digest: string) =>
    json({ digest, actions: [], blockers: [], conflicts: [] });
  const plans = vi
    .fn(() => refresh.promise)
    .mockResolvedValueOnce(planResponse('initial'));
  const applies = vi.fn(() => json({ title: 'Conflict', status: 409 }, 409));
  serve(
    (request) => {
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/configuration/plan') return plans();
      if (path === '/api/v1/configuration/apply') return applies();
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
    [...target.querySelectorAll('button')].find((item) =>
      item.textContent?.trim().startsWith(name)
    );
  flushSync();
  const artifact = target.querySelector<HTMLTextAreaElement>(
    '#promotion-artifact'
  )!;
  artifact.value = JSON.stringify({
    api_version: 'openllmproxy.dev/config/v2',
    projects: [],
    providers: [],
    routes: [],
    pricing: null
  });
  artifact.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  button('Plan')?.click();
  await eventually(() => expect(button('Apply')?.disabled).toBe(false));
  button('Apply')?.click();
  await eventually(() => expect(plans).toHaveBeenCalledTimes(2));
  expect(button('Apply')?.disabled).toBe(true);
  expect(button('Plan')?.disabled).toBe(true);
  expect(applies).toHaveBeenCalledOnce();
  refresh.resolve(planResponse('refreshed'));
  await eventually(() => expect(button('Apply')?.disabled).toBe(false));
  expect(target.textContent).toContain('Digest refreshed');
  expect(target.querySelector('[role="alert"]')?.textContent).toBe('Conflict');
});
