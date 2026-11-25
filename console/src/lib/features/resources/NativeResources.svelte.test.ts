import { afterEach, describe, expect, it, onTestFinished, vi } from 'vitest';
import { flushSync, getAllContexts, mount, unmount } from 'svelte';
import { useQueryClient } from '@tanstack/svelte-query';
import { toasts } from '#lib/components/toasts.svelte.js';
import { json, serve } from '#lib/session/test/transport.js';
import StateHarness from '../../../../tests/unit/support/StateHarness.svelte';
import { eventually } from '../../../../tests/unit/support/eventually.js';
import NativeResources from './NativeResources.svelte';
import { resourceKeys } from './resourceKeys';

afterEach(() => {
  vi.unstubAllGlobals();
  for (const item of toasts.items) toasts.dismiss(item.id);
});

const hex32 = (digit: string) => digit.repeat(32);

const contracts = {
  endpoints: [
    {
      operation: 'fine_tuning',
      surface: 'openai',
      kind: 'fine_tuning_job',
      action: 'create',
      method: 'POST',
      pattern: '/v1/fine_tuning/jobs',
      ancestors: [],
      metered: true
    },
    {
      operation: 'fine_tuning',
      surface: 'openai',
      kind: 'fine_tuning_job',
      action: 'get',
      method: 'GET',
      pattern: '/v1/fine_tuning/jobs/{id}',
      ancestors: [],
      metered: false
    }
  ]
};

const trainingRoute = {
  id: 'route-1',
  slug: 'training',
  state: 'active',
  latest_revision: { operations: ['fine_tuning'] }
};

function mountNative(
  handler: (request: Request) => Response | Promise<Response>
) {
  serve(
    (input: RequestInfo | URL, init?: RequestInit) => {
      const request =
        input instanceof Request
          ? input
          : new Request(new URL(String(input), 'http://localhost'), init);
      return handler(request);
    },
    ['read']
  );

  const target = document.createElement('div');
  const setup = vi.fn(() => {
    useQueryClient().setQueryDefaults(resourceKeys.contracts(), {
      retry: false
    });
    return mount(NativeResources, { target, context: getAllContexts() });
  });
  const harness = mount(StateHarness, { target, props: { setup } });
  onTestFinished(async () => {
    await unmount(setup.mock.results[0].value);
    await unmount(harness);
  });

  const control = <T extends HTMLElement>(label: string) =>
    [...target.querySelectorAll('label')].find((item) =>
      item.textContent?.includes(label)
    )?.control as T | undefined;
  const button = (label: string) =>
    [...target.querySelectorAll('button')].find(
      (item) => item.textContent?.trim() === label
    );

  return { target, control, button };
}

function setValue(
  element: HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement,
  value: string,
  event: 'input' | 'change' = 'input'
) {
  element.value = value;
  element.dispatchEvent(new Event(event, { bubbles: true }));
  flushSync();
}

function submitForm(target: HTMLElement) {
  target
    .querySelector('form')
    ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
}

describe('NativeResources', () => {
  it('reuses idempotency keys across equivalent retries, rotates on key change, captures gateway IDs, and notifies on success', async () => {
    const recoveryId = `fine_tuning_job_${hex32('1')}`;
    const createdId = `fine_tuning_job_${hex32('2')}`;
    const gatewayCalls: {
      method: string;
      path: string;
      key: string | null;
      auth: string | null;
    }[] = [];
    let posts = 0;

    const { target, control } = mountNative((request) => {
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/provider-resource-contracts')
        return json(contracts);
      if (path === '/api/v1/routes') return json({ items: [trainingRoute] });
      gatewayCalls.push({
        method: request.method,
        path,
        key: request.headers.get('Idempotency-Key'),
        auth: request.headers.get('Authorization')
      });
      if (request.method === 'POST') {
        posts += 1;
        if (posts <= 2) {
          return new Response('ambiguous upstream', {
            status: 409,
            headers: { 'X-OLP-Resource-ID': recoveryId }
          });
        }
        return json({ id: createdId });
      }
      return json({ id: createdId, status: 'running' });
    });

    await eventually(() =>
      expect(target.querySelector('option[value="training"]')).not.toBeNull()
    );

    const apiKeyInput = control<HTMLInputElement>('Inference API key')!;
    const routeSelect = control<HTMLSelectElement>('Published route')!;
    const bodyInput = control<HTMLTextAreaElement>('Request body')!;
    const actionSelect = control<HTMLSelectElement>('Action')!;

    setValue(apiKeyInput, 'key-one');
    setValue(routeSelect, 'training', 'change');
    setValue(bodyInput, '{"b":2,"a":1}');

    submitForm(target);
    await eventually(() =>
      expect(target.querySelector('[role="alert"]')?.textContent).toContain(
        'HTTP 409'
      )
    );
    const firstKey = gatewayCalls[0]?.key;
    expect(firstKey).toBeTruthy();

    setValue(bodyInput, '{ "a": 1, "b": 2 }');
    submitForm(target);
    await eventually(() => expect(gatewayCalls).toHaveLength(2));
    expect(gatewayCalls[1].key).toBe(firstKey);

    setValue(apiKeyInput, 'key-two');
    submitForm(target);
    await eventually(() =>
      expect(
        target.querySelector('pre[aria-label="Native resource result"]')
          ?.textContent
      ).toContain(createdId)
    );
    expect(gatewayCalls[2].key).toBeTruthy();
    expect(gatewayCalls[2].key).not.toBe(firstKey);
    expect(toasts.items.map((item) => item.message)).toContain(
      'Resource operation completed.'
    );

    setValue(actionSelect, 'GET /v1/fine_tuning/jobs/{id}', 'change');
    const idInput = control<HTMLInputElement>('fine tuning job ID')!;
    expect(idInput.value).toBe(createdId);

    submitForm(target);
    await eventually(() => expect(gatewayCalls).toHaveLength(4));
    expect(gatewayCalls[3]).toEqual({
      method: 'GET',
      path: `/v1/fine_tuning/jobs/${createdId}`,
      key: null,
      auth: 'Bearer key-two'
    });
  });

  it('shows a retryable QueryView error when contracts fail to load', async () => {
    let attempts = 0;
    const { target, button } = mountNative((request) => {
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/routes') return json({ items: [trainingRoute] });
      attempts += 1;
      if (attempts === 1) return json({ title: 'Contracts down' }, 500);
      return json(contracts);
    });

    await eventually(() =>
      expect(target.querySelector('[role="alert"]')?.textContent).toContain(
        'Contracts down'
      )
    );
    button('Retry')?.click();

    await eventually(() => expect(target.querySelector('form')).not.toBeNull());
  });
});
