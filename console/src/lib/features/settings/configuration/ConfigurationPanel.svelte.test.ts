import { expect, it, vi } from 'vitest';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import {
  clickButton,
  findButton,
  mountWithState,
  setFieldValue
} from '#test/mount.js';
import ConfigurationPanel from './ConfigurationPanel.svelte';

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
  const { target } = mountWithState(ConfigurationPanel);
  const button = (name: string) => findButton(target, name);
  const click = (name: string) => clickButton(target, name);
  const artifact = target.querySelector<HTMLTextAreaElement>(
    '#promotion-artifact'
  );
  expect(artifact).not.toBeNull();
  if (!artifact) return;
  setFieldValue(
    artifact,
    JSON.stringify({
      api_version: 'openllmproxy.dev/config/v2',
      projects: [],
      providers: [],
      routes: [],
      pricing: null
    })
  );
  click('Plan');
  await eventually(() =>
    expect(
      target.querySelector('[data-testid="promotion-plan"]')
    ).not.toBeNull()
  );
  await eventually(() => expect(button('Apply')).toBeDefined());
  const external = target.querySelectorAll<HTMLTextAreaElement>('textarea')[1];
  expect(external).toBeDefined();
  setFieldValue(
    external,
    '{"provider/primary":{"store":"gcp","secret_id":"projects/project/secrets/provider","version":"1"}}'
  );
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
  const pending = Promise.withResolvers<Response>();
  serve(
    (request) => {
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/configuration/plan') return pending.promise;
      return json({});
    },
    ['read', 'configure']
  );
  const { target } = mountWithState(ConfigurationPanel);
  const button = (name: string) => findButton(target, name);
  const click = (name: string) => clickButton(target, name);
  const setArtifact = (id: string) => {
    const field = target.querySelector<HTMLTextAreaElement>(
      '#promotion-artifact'
    );
    if (!field) throw new Error('Missing artifact field');
    setFieldValue(
      field,
      JSON.stringify({
        api_version: 'openllmproxy.dev/config/v2',
        projects: [{ id }],
        providers: [],
        routes: [],
        pricing: null
      })
    );
  };
  setArtifact('first');
  click('Plan');
  await eventually(() => expect(button('Plan')?.disabled).toBe(true));
  setArtifact('second');
  pending.resolve(
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
  const { target } = mountWithState(ConfigurationPanel);
  const button = (name: string) =>
    [...target.querySelectorAll('button')].find((item) =>
      item.textContent?.trim().startsWith(name)
    );
  const artifact = target.querySelector<HTMLTextAreaElement>(
    '#promotion-artifact'
  )!;
  setFieldValue(
    artifact,
    JSON.stringify({
      api_version: 'openllmproxy.dev/config/v2',
      projects: [],
      providers: [],
      routes: [],
      pricing: null
    })
  );
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
