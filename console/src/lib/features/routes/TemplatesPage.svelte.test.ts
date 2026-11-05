import { useQueryClient } from '@tanstack/svelte-query';
import { routeKeys } from './routeKeys';
import { listRouteDraftPage, listRoutePage } from './api';
import { afterEach, expect, it, onTestFinished, vi } from 'vitest';
import { flushSync, getAllContexts, mount, unmount } from 'svelte';
import { json, serve } from '$lib/management/test/transport';
import type { ManagementOperation } from '$lib/management/requirements';
import StateHarness from '../../../../tests/unit/support/StateHarness.svelte';
import TemplatesPage from './TemplatesPage.svelte';

afterEach(() => vi.unstubAllGlobals());

it('requires confirmation before publishing template placements and displays skipped reasons', async () => {
  const apply = vi.fn<(request: Request) => void>();
  serve(
    (request) => {
      const path = new URL(request.url).pathname;
      if (path.endsWith('/apply')) {
        apply(request);
        return json({
          results: [
            {
              template_id: 'template',
              provider_id: 'provider',
              provider_model_id: 'model',
              upstream_model: 'unpriced-model',
              route_slug: 'generated-route',
              draft_id: null,
              outcome: 'skipped',
              reason: 'No active price'
            }
          ],
          runtime_generation: null
        });
      }
      if (path === '/api/v1/projects')
        return json({ items: [], next_cursor: null });
      return json({
        items: [
          {
            id: 'template',
            etag: 'version',
            name: 'generated',
            provider_selector: 'vendor:fixture',
            model_filter: '*',
            auto_publish: true
          }
        ],
        next_cursor: null
      });
    },
    ['read', 'configure']
  );
  const target = document.createElement('div');
  const setup = vi.fn(() =>
    mount(TemplatesPage, { target, context: getAllContexts() })
  );
  const harness = mount(StateHarness, { target, props: { setup } });
  onTestFinished(async () => {
    await unmount(setup.mock.results[0].value);
    await unmount(harness);
  });
  await vi.waitFor(() =>
    expect(
      [...target.querySelectorAll('button')].some(
        (item) => item.textContent?.trim() === 'Apply template'
      )
    ).toBe(true)
  );
  const button = (label: string) =>
    [...target.querySelectorAll('button')].find(
      (item) => item.textContent?.trim() === label
    );
  button('Apply template')?.click();
  flushSync();
  expect(apply).not.toHaveBeenCalled();
  expect(target.textContent).toContain('publish generated routes immediately');
  button('Confirm apply template')?.click();
  await vi.waitFor(() =>
    expect(target.textContent).toContain('No active price')
  );
  expect(apply.mock.calls[0][0].headers.get('Idempotency-Key')).toBeTruthy();
  expect(target.textContent).toContain('skipped');
});

it.each(['draft', 'conflict', 'project'] as const)(
  'preserves template %s ownership across refreshes',
  async (check) => {
    const value = {
      id: 'template',
      etag: 'before',
      name: 'original',
      project_id: 'project',
      provider_selector: 'vendor:fixture',
      model_filter: '*',
      slug_pattern: '{model}',
      overall_timeout_ms: 60000,
      max_attempts: 2,
      fidelity: { mode: 'strict' },
      auto_publish: false,
      routes: []
    };
    const detail = vi.fn(() => json(value));
    const writes = vi.fn<(request: Request) => void>();
    serve(
      (request) => {
        const path = new URL(request.url).pathname;
        if (request.method === 'PUT') {
          writes(request);
          return json(
            {
              title: 'Changed elsewhere',
              status: 412,
              type: 'https://openllmproxy.dev/problems/etag_mismatch'
            },
            412
          );
        }
        if (path.endsWith('/projects'))
          return json({ items: [{ id: 'project', name: 'Project' }] });
        return path.endsWith('/template') ? detail() : json({ items: [value] });
      },
      ['read', 'configure']
    );
    const target = document.createElement('div');
    const setup = vi.fn(() => ({
      client: useQueryClient(),
      component: mount(TemplatesPage, { target, context: getAllContexts() })
    }));
    const harness = mount(StateHarness, { target, props: { setup } });
    onTestFinished(async () => {
      await unmount(setup.mock.results[0].value.component);
      await unmount(harness);
    });
    const button = (name: string) =>
      [...target.querySelectorAll('button')].find(
        (item) => item.textContent?.trim() === name
      );
    await vi.waitFor(() => expect(button('Inspect and edit')).toBeDefined());
    button('Inspect and edit')?.click();
    await vi.waitFor(() =>
      expect(
        (target.querySelector('form input') as HTMLInputElement)?.value
      ).toBe('original')
    );
    if (check === 'project') {
      expect(
        target.querySelector<HTMLSelectElement>('form select')?.disabled
      ).toBe(true);
      button('New template')?.click();
      flushSync();
      expect(
        target.querySelector<HTMLSelectElement>('form select')?.disabled
      ).toBe(false);
      return;
    }
    const name = target.querySelector('form input') as HTMLInputElement;
    name.value = 'my-draft';
    name.dispatchEvent(new Event('input', { bubbles: true }));
    flushSync();
    detail.mockImplementation(() =>
      json({ ...value, name: 'remote', etag: 'remote' })
    );
    if (check === 'draft') {
      await setup.mock.results[0].value.client.invalidateQueries({
        queryKey: routeKeys.template(value.id)
      });
    }
    flushSync();
    expect((target.querySelector('form input') as HTMLInputElement).value).toBe(
      'my-draft'
    );
    target
      .querySelector('form')
      ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await vi.waitFor(() => expect(writes).toHaveBeenCalledOnce());
    expect(writes.mock.calls[0][0].headers.get('If-Match')).toBe('"before"');
    await vi.waitFor(() =>
      expect(target.textContent).toContain('Changed elsewhere')
    );
    await vi.waitFor(() => expect(button('Reload')?.disabled).toBe(false));
    expect((target.querySelector('form input') as HTMLInputElement).value).toBe(
      'my-draft'
    );
    button('Reload')?.click();
    flushSync();
    expect((target.querySelector('form input') as HTMLInputElement).value).toBe(
      'remote'
    );
  }
);

it.each([false, true])(
  'refreshes recently cached route listings after template Apply with auto_publish %s',
  async (publish) => {
    const applied = vi.fn();
    const placed = { id: 'placed', slug: 'generated-route' };
    serve(
      (request) => {
        const path = new URL(request.url).pathname;
        if (path.endsWith('/apply')) {
          applied();
          return json({ results: [], runtime_generation: null });
        }
        if (path === '/api/v1/routes' || path === '/api/v1/route-drafts')
          return json({
            items: applied.mock.calls.length ? [placed] : [],
            next_cursor: null
          });
        if (path === '/api/v1/projects') return json({ items: [] });
        return json({
          items: [
            {
              id: 'template',
              etag: 'version',
              name: 'Template',
              provider_selector: 'vendor:fixture',
              model_filter: '*',
              auto_publish: publish
            }
          ]
        });
      },
      ['read', 'configure']
    );
    const target = document.createElement('div');
    const setup = vi.fn(() => ({
      client: useQueryClient(),
      component: mount(TemplatesPage, { target, context: getAllContexts() })
    }));
    const harness = mount(StateHarness, { target, props: { setup } });
    onTestFinished(async () => {
      await unmount(setup.mock.results[0].value.component);
      await unmount(harness);
    });
    const options = {
      queryKey: publish ? routeKeys.page() : routeKeys.draftPage(),
      queryFn: () => (publish ? listRoutePage() : listRouteDraftPage()),
      staleTime: 60000
    };
    const client = setup.mock.results[0].value.client;
    expect((await client.fetchQuery(options)).items).toEqual([]);
    const button = (label: string) =>
      [...target.querySelectorAll('button')].find(
        (item) => item.textContent?.trim() === label
      );
    await vi.waitFor(() => expect(button('Apply template')).toBeDefined());
    button('Apply template')?.click();
    flushSync();
    button('Confirm apply template')?.click();
    await vi.waitFor(() => expect(button('Apply template')).toBeDefined());
    expect(applied).toHaveBeenCalledOnce();
    expect((await client.fetchQuery(options)).items).toEqual([placed]);
  }
);

const storedTemplate = {
  id: 'template',
  etag: 'version',
  name: 'stored',
  project_id: null,
  provider_selector: 'vendor:fixture',
  model_filter: '*',
  slug_pattern: '{model}',
  overall_timeout_ms: 60000,
  max_attempts: 2,
  fidelity: { mode: 'strict' },
  routing_policy: null,
  auto_publish: false,
  routes: []
};

function mountTemplates(operations: ManagementOperation[]) {
  serve((request) => {
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/projects') return json({ items: [] });
    return path.endsWith('/route-templates')
      ? json({ items: [storedTemplate] })
      : json(storedTemplate);
  }, operations);
  const target = document.createElement('div');
  const setup = vi.fn(() =>
    mount(TemplatesPage, { target, context: getAllContexts() })
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
  return { target, button };
}

it('offers read-only members inspection but no template create or edit controls', async () => {
  const { target, button } = mountTemplates(['read']);
  await vi.waitFor(() => expect(button('Inspect')).toBeDefined());

  expect(button('New template')).toBeUndefined();
  expect(button('Inspect and edit')).toBeUndefined();
  expect(target.querySelector('form')).toBeNull();
});

it('keeps an edited template draft when the operator declines to discard it', async () => {
  const decline = vi.fn(() => false);
  vi.stubGlobal('confirm', decline);
  const { target, button } = mountTemplates(['read', 'configure']);
  await vi.waitFor(() => expect(button('Inspect and edit')).toBeDefined());
  button('Inspect and edit')?.click();
  await vi.waitFor(() =>
    expect(
      (target.querySelector('form input') as HTMLInputElement)?.value
    ).toBe('stored')
  );
  const name = () => target.querySelector('form input') as HTMLInputElement;
  name().value = 'my-draft';
  name().dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();

  button('New template')?.click();
  flushSync();

  expect(decline).toHaveBeenCalledOnce();
  expect(name().value).toBe('my-draft');
});
