import { expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { toasts } from '#lib/components/toasts.svelte.js';
import type { ManagementOperation } from '#lib/management/requirements.js';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import { findButton, mountWithState } from '#test/mount.js';
import type { ProviderResource } from './api';
import RetainedResourceControls from './RetainedResourceControls.svelte';

const cacheResource: ProviderResource = {
  id: 'context_cache_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
  kind: 'context_cache',
  provider_id: '00000000-0000-4000-8000-000000000001',
  provider_name: 'Vertex AI',
  api_key_id: '00000000-0000-4000-8000-000000000002',
  api_key_name: 'prod-key',
  route: 'gemini-main',
  state: 'active',
  upstream_id: 'cachedContents/123',
  upstream_model: 'gemini-2.5-pro',
  created_at: '2026-04-01T00:00:00Z',
  updated_at: '2026-04-01T01:00:00Z',
  expires_at: '2026-04-02T00:00:00Z',
  retained_settlement: {
    state: 'evidence_missing',
    missing_evidence: true,
    remote_terminated: false,
    last_error: 'awaiting usage interval',
    next_observation_at: '2026-04-01T02:00:00Z',
    models: ['gemini-2.5-flash']
  }
};

function mountControls(
  handler: (request: Request) => Response | Promise<Response>,
  operations: readonly ManagementOperation[],
  resource: ProviderResource = cacheResource
) {
  serve(handler, operations);
  const changed = vi.fn();
  const { target } = mountWithState(RetainedResourceControls, {
    resource,
    changed
  });
  const button = (label: string) => findButton(target, label);
  return { target, changed, button };
}

it('reports malformed usage evidence without calling the management API', () => {
  const requests = vi.fn();
  const { target, changed, button } = mountControls(
    (request) => {
      requests(request);
      return json({});
    },
    ['read', 'settings']
  );

  const textarea = target.querySelector<HTMLTextAreaElement>('textarea')!;
  textarea.value = '[1, 2, 3]';
  textarea.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();

  target
    .querySelector('form')
    ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  flushSync();

  expect(requests).not.toHaveBeenCalled();
  expect(changed).not.toHaveBeenCalled();
  expect(target.querySelector('[role="alert"]')?.textContent).toContain(
    'Enter one usage evidence object.'
  );

  button('Prepare usage evidence')?.click();
  flushSync();
  expect(target.querySelector('[role="alert"]')).toBeNull();
});

it('drafts evidence and submits int64 quantities verbatim with a toast notification', async () => {
  const bodies: { path: string; raw: string }[] = [];
  const { target, changed, button } = mountControls(
    async (request) => {
      bodies.push({
        path: new URL(request.url).pathname,
        raw: await request.text()
      });
      return json({ settlement_status: 'closed' });
    },
    ['read', 'settings']
  );

  button('Prepare usage evidence')?.click();
  flushSync();
  const textarea = target.querySelector<HTMLTextAreaElement>('textarea')!;
  const drafted = JSON.parse(textarea.value) as Record<string, unknown>;
  expect(drafted).toMatchObject({
    model: 'gemini-2.5-flash',
    start: cacheResource.created_at,
    cache_token_seconds: '0',
    terminal: false
  });
  expect(typeof drafted.idempotency_key).toBe('string');
  expect((drafted.idempotency_key as string).length).toBeGreaterThan(0);

  textarea.value = textarea.value.replace(
    '"cache_token_seconds": "0"',
    '"cache_token_seconds": 9007199254740993'
  );
  textarea.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();

  target
    .querySelector('form')
    ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));

  await eventually(() => expect(changed).toHaveBeenCalledOnce());
  expect(bodies).toHaveLength(1);
  expect(bodies[0].path).toBe(
    `/api/v1/provider-resources/${cacheResource.id}/usage-evidence`
  );
  expect(bodies[0].raw).toContain('9007199254740993');
  expect(toasts.items.map((item) => item.message)).toContain(
    'Usage evidence recorded. Settlement closed.'
  );
});

it('schedules reconciliation with an optional hint and displays server errors', async () => {
  const payloads: unknown[] = [];
  let fail = false;
  const { target, changed, button } = mountControls(
    async (request) => {
      payloads.push(await request.json());
      if (fail) {
        return json(
          { title: 'Upstream resource not verified', status: 422 },
          422
        );
      }
      return json({ scheduled: true });
    },
    ['read', 'configure']
  );

  const hintInput = target.querySelector<HTMLInputElement>('input')!;
  hintInput.value = '  cachedContents/verified  ';
  hintInput.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();

  button('Reconcile')?.click();
  await eventually(() => expect(changed).toHaveBeenCalledOnce());
  expect(payloads[0]).toEqual({ upstream_id: 'cachedContents/verified' });
  expect(toasts.items.map((item) => item.message)).toContain(
    'Reconciliation scheduled. Refresh to inspect settlement progress.'
  );

  fail = true;
  button('Reconcile')?.click();
  await eventually(() =>
    expect(target.querySelector('[role="alert"]')?.textContent).toContain(
      'Upstream resource not verified'
    )
  );
  expect(changed).toHaveBeenCalledOnce();
});

it('hides settlement controls when the caller lacks required operations', () => {
  const { target, button } = mountControls(() => json({}), ['read']);
  expect(button('Reconcile')).toBeUndefined();
  expect(button('Prepare usage evidence')).toBeUndefined();
  expect(target.querySelector('form')).toBeNull();
});
