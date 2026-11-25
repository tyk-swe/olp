import { useQueryClient } from '@tanstack/svelte-query';
import { flushSync, getAllContexts, mount, unmount } from 'svelte';
import { afterEach, expect, it, onTestFinished, vi } from 'vitest';
import { toasts } from '#lib/components/toasts.svelte.js';
import type { ManagementOperation } from '#lib/management/requirements.js';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '../../../../tests/unit/support/eventually.js';
import StateHarness from '../../../../tests/unit/support/StateHarness.svelte';
import type { RetrievalBinding } from './api';
import RetrievalBindings from './RetrievalBindings.svelte';

afterEach(() => {
  vi.unstubAllGlobals();
  for (const item of toasts.items) toasts.dismiss(item.id);
});

const bindingFixture: RetrievalBinding = {
  id: '00000000-0000-4000-8000-000000000010',
  project_id: '00000000-0000-4000-8000-000000000011',
  name: 'Docs KB',
  route: 'docs-search',
  target_id: '00000000-0000-4000-8000-000000000012',
  provider_id: '00000000-0000-4000-8000-000000000013',
  backend: 'bedrock_knowledge_base',
  backend_resource: 'KB123456',
  fields: { metadata: { tenant: 'tenant_id' } },
  key_grants: ['00000000-0000-4000-8000-000000000014'],
  enabled: true,
  revision_id: '00000000-0000-4000-8000-000000000021',
  revision: 1,
  etag: 'etag-1',
  retired_at: null
};

function mountBindings(
  handler: (request: Request) => Response | Promise<Response>,
  operations: readonly ManagementOperation[] = ['read', 'configure']
) {
  serve(handler, operations);
  const target = document.createElement('div');
  const setup = vi.fn(() => {
    useQueryClient().setDefaultOptions({ queries: { retry: false } });
    return mount(RetrievalBindings, {
      target,
      context: getAllContexts()
    });
  });
  const harness = mount(StateHarness, { target, props: { setup } });
  onTestFinished(async () => {
    await unmount(setup.mock.results[0].value);
    await unmount(harness);
  });
  flushSync();
  const button = (label: string) =>
    [...target.querySelectorAll('button')].find(
      (item) => item.textContent?.trim() === label
    );
  const fieldByLabel = <T extends HTMLInputElement | HTMLTextAreaElement>(
    labelPrefix: string
  ) => {
    const label = [...target.querySelectorAll('label')].find((item) =>
      item.textContent?.trim().startsWith(labelPrefix)
    );
    const control = label?.querySelector<T>('input, textarea');
    expect(control).toBeDefined();
    return control!;
  };
  return { target, button, fieldByLabel };
}

function setFieldValue(
  control: HTMLInputElement | HTMLTextAreaElement,
  value: string
) {
  control.value = value;
  control.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
}

it('shows list query failures with retry and renders the empty state after recovery', async () => {
  let failList = true;
  const { target, button } = mountBindings(() => {
    if (failList) {
      return json(
        { title: 'Retrieval inventory unavailable', status: 503 },
        503
      );
    }
    return json({ items: [], next_cursor: null });
  });

  await eventually(() =>
    expect(target.textContent).toContain('Retrieval inventory unavailable')
  );
  expect(button('Retry')).toBeDefined();

  failList = false;
  button('Retry')?.click();
  await eventually(() =>
    expect(target.textContent).toContain(
      'No cloud retrieval bindings are configured.'
    )
  );
});

it('validates field mappings and duplicate key grants before calling the API', async () => {
  const writes = vi.fn();
  const { target, button, fieldByLabel } = mountBindings((request) => {
    if (request.method !== 'GET') writes(request);
    return json({ items: [], next_cursor: null });
  });

  await eventually(() =>
    expect(target.textContent).toContain(
      'No cloud retrieval bindings are configured.'
    )
  );
  button('Add binding')?.click();
  flushSync();

  setFieldValue(fieldByLabel('Name'), 'Search KB');
  setFieldValue(
    fieldByLabel('Owning project ID'),
    '00000000-0000-4000-8000-000000000011'
  );
  setFieldValue(fieldByLabel('Route'), 'search-route');
  setFieldValue(
    fieldByLabel('Target ID'),
    '00000000-0000-4000-8000-000000000012'
  );
  setFieldValue(fieldByLabel('Backend resource'), 'KB999');
  setFieldValue(fieldByLabel('Field mappings (JSON)'), '{"unknown":"x"}');

  target
    .querySelector('form')
    ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  flushSync();

  expect(writes).not.toHaveBeenCalled();
  expect(target.querySelector('[role="alert"]')?.textContent).toContain(
    'Unknown field mapping key "unknown".'
  );

  setFieldValue(fieldByLabel('Field mappings (JSON)'), '{}');
  setFieldValue(
    fieldByLabel('API-key grants'),
    '00000000-0000-4000-8000-000000000014\n00000000-0000-4000-8000-000000000014'
  );
  target
    .querySelector('form')
    ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  flushSync();

  expect(writes).not.toHaveBeenCalled();
  expect(target.querySelector('[role="alert"]')?.textContent).toContain(
    'Each API-key grant must appear once.'
  );
});

it('handles save conflicts with ConflictNotice, reloads the latest version, and saves cleanly', async () => {
  const updatedBinding: RetrievalBinding = {
    ...bindingFixture,
    name: 'Docs KB (updated elsewhere)',
    revision: 2,
    revision_id: '00000000-0000-4000-8000-000000000022',
    etag: 'etag-2'
  };
  let remote = bindingFixture;
  const writes: { method: string; ifMatch: string | null; body: unknown }[] =
    [];

  const { target, button, fieldByLabel } = mountBindings(async (request) => {
    const pathname = new URL(request.url).pathname;
    if (request.method === 'GET' && pathname === '/api/v1/retrieval-bindings') {
      return json({ items: [remote], next_cursor: null });
    }
    if (
      request.method === 'GET' &&
      pathname === `/api/v1/retrieval-bindings/${bindingFixture.id}`
    ) {
      return json(remote);
    }
    if (request.method === 'PUT') {
      const ifMatch = request.headers.get('If-Match');
      const body: unknown = await request.json();
      writes.push({ method: request.method, ifMatch, body });
      if (ifMatch !== `"${remote.etag}"`) {
        return json(
          {
            title: 'Changed elsewhere',
            status: 412,
            type: 'https://openllmproxy.dev/problems/etag_mismatch'
          },
          412
        );
      }
      remote = {
        ...remote,
        name: 'Docs KB (re-saved)',
        revision: 3,
        revision_id: '00000000-0000-4000-8000-000000000023',
        etag: 'etag-3'
      };
      return json(remote);
    }
    return json({}, 404);
  });

  await eventually(() => expect(button('Inspect / edit')).toBeDefined());
  button('Inspect / edit')?.click();
  await eventually(() =>
    expect(fieldByLabel<HTMLInputElement>('Name').value).toBe('Docs KB')
  );

  remote = updatedBinding;
  setFieldValue(fieldByLabel<HTMLInputElement>('Name'), 'Docs KB (my edit)');
  target
    .querySelector('form')
    ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));

  await eventually(() => expect(button('Reload')).toBeDefined());
  expect(writes[0]?.ifMatch).toBe('"etag-1"');
  expect(button('Save revision')?.disabled).toBe(true);
  expect(button('Retire binding')?.disabled).toBe(true);

  button('Reload')?.click();
  await eventually(() =>
    expect(fieldByLabel<HTMLInputElement>('Name').value).toBe(
      'Docs KB (updated elsewhere)'
    )
  );
  expect(button('Reload')).toBeUndefined();
  expect(button('Save revision')?.disabled).toBe(false);

  setFieldValue(fieldByLabel<HTMLInputElement>('Name'), 'Docs KB (re-saved)');
  target
    .querySelector('form')
    ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));

  await eventually(() => expect(writes).toHaveLength(2));
  expect(writes[1]?.ifMatch).toBe('"etag-2"');
  expect(toasts.items.map((item) => item.message)).toContain(
    'Binding revision saved.'
  );
  expect(fieldByLabel<HTMLInputElement>('Revision ID').value).toBe(
    '00000000-0000-4000-8000-000000000023'
  );
});

it('reuses the same Idempotency-Key when retrying a failed create and inspects revisions', async () => {
  const idempotencyKeys: (string | null)[] = [];
  let failCreate = true;

  const { target, button, fieldByLabel } = mountBindings((request) => {
    const pathname = new URL(request.url).pathname;
    if (request.method === 'GET' && pathname === '/api/v1/retrieval-bindings') {
      return json({
        items: failCreate ? [] : [bindingFixture],
        next_cursor: null
      });
    }
    if (
      request.method === 'POST' &&
      pathname === '/api/v1/retrieval-bindings'
    ) {
      idempotencyKeys.push(request.headers.get('Idempotency-Key'));
      if (failCreate) {
        failCreate = false;
        return json({ title: 'Temporary outage', status: 503 }, 503);
      }
      return json(bindingFixture, 201);
    }
    if (
      request.method === 'GET' &&
      pathname ===
        `/api/v1/retrieval-bindings/${bindingFixture.id}/revisions/${bindingFixture.revision_id}`
    ) {
      return json({
        id: bindingFixture.revision_id,
        binding_id: bindingFixture.id,
        revision: 1,
        definition: { name: bindingFixture.name }
      });
    }
    return json({}, 404);
  });

  await eventually(() => expect(button('Add binding')).toBeDefined());
  button('Add binding')?.click();
  flushSync();

  setFieldValue(fieldByLabel('Name'), bindingFixture.name);
  setFieldValue(fieldByLabel('Owning project ID'), bindingFixture.project_id);
  setFieldValue(fieldByLabel('Route'), bindingFixture.route);
  setFieldValue(fieldByLabel('Target ID'), bindingFixture.target_id);
  setFieldValue(
    fieldByLabel('Backend resource'),
    bindingFixture.backend_resource
  );

  target
    .querySelector('form')
    ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await eventually(() =>
    expect(target.querySelector('[role="alert"]')?.textContent).toContain(
      'Temporary outage'
    )
  );

  target
    .querySelector('form')
    ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await eventually(() =>
    expect(toasts.items.map((item) => item.message)).toContain(
      'Binding revision saved.'
    )
  );

  expect(idempotencyKeys).toHaveLength(2);
  expect(idempotencyKeys[0]).toBeTruthy();
  expect(idempotencyKeys[1]).toBe(idempotencyKeys[0]);

  button('Inspect revision')?.click();
  await eventually(() =>
    expect(
      target.querySelector('pre[aria-label="Retrieval binding revision"]')
        ?.textContent
    ).toContain(bindingFixture.revision_id)
  );
});
