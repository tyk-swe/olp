import { describe, expect, it } from 'vitest';
import { toasts } from '#lib/components/toasts.svelte.js';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import {
  findButton,
  mountWithState,
  setFieldValue as setValue
} from '#test/mount.js';
import NativeResources from './NativeResources.svelte';
import { resourceKeys } from './resourceKeys';

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
  serve(handler, ['read']);

  const { target } = mountWithState(NativeResources, undefined, {
    configureClient: (client) =>
      client.setQueryDefaults(resourceKeys.contracts(), { retry: false })
  });

  const control = <T extends HTMLElement>(label: string) =>
    [...target.querySelectorAll('label')].find((item) =>
      item.textContent?.includes(label)
    )?.control as T | undefined;
  const button = (label: string) => findButton(target, label);

  return { target, control, button };
}

function submitForm(target: HTMLElement) {
  target
    .querySelector('form')
    ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
}

describe('NativeResources', () => {
  it('reuses idempotency keys after switching actions, rotates on key change, captures gateway IDs, and notifies on success', async () => {
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

    setValue(actionSelect, 'GET /v1/fine_tuning/jobs/{id}', 'change');
    expect(control<HTMLInputElement>('fine tuning job ID')?.value).toBe(
      recoveryId
    );
    setValue(actionSelect, 'POST /v1/fine_tuning/jobs', 'change');
    setValue(
      control<HTMLTextAreaElement>('Request body')!,
      '{ "a": 1, "b": 2 }'
    );
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

  it.each([
    {
      operation: 'vector_store',
      surface: 'openai',
      action: 'update',
      method: 'POST',
      root: '/v1/vector_stores',
      body: '{"expires_after":{"anchor":"last_active_at","days":1}}'
    },
    {
      operation: 'context_cache',
      surface: 'gemini',
      action: 'patch',
      method: 'PATCH',
      root: '/gemini/v1beta/cachedContents',
      body: '{"ttl":"3600s"}'
    }
  ])(
    'rotates keys only for a confirmed $operation rejection',
    async ({ operation, root, body, ...endpoint }) => {
      const id = `${operation}_${hex32('1')}`;
      const submissionKeys: (string | null)[] = [];
      const failures = [
        new Response('Unconfirmed rejection', { status: 400 }),
        new Response('Definitive rejection', {
          status: 400,
          headers: { 'X-OLP-Mutation-Status': 'not_accepted' }
        }),
        new Response('Provider outage', { status: 503 }),
        new Response('idempotency_conflict', { status: 409 }),
        new Response('submission_unknown', { status: 409 }),
        new Response('Gateway timeout', { status: 504 }),
        new Response('Conflict after timeout', { status: 409 })
      ];
      const { target, control } = mountNative((request) => {
        const path = new URL(request.url).pathname;
        if (path === '/api/v1/provider-resource-contracts')
          return json({
            endpoints: [
              {
                ...endpoint,
                operation,
                kind: operation,
                pattern: `${root}/{id}`,
                ancestors: [],
                metered: true
              }
            ]
          });
        if (path === '/api/v1/routes') return json({ items: [] });
        expect(path).toBe(`${root}/${id}`);
        expect(request.method).toBe(endpoint.method);
        const response = failures[submissionKeys.length];
        submissionKeys.push(request.headers.get('Idempotency-Key'));
        return response;
      });

      await eventually(() => expect(control('Request body')).toBeDefined());
      setValue(control<HTMLInputElement>('Inference API key')!, 'test-key');
      setValue(
        control<HTMLInputElement>(`${operation.replaceAll('_', ' ')} ID`)!,
        id
      );
      setValue(control<HTMLTextAreaElement>('Request body')!, body);

      for (const failure of failures) {
        const text = await failure.clone().text();
        submitForm(target);
        await eventually(() =>
          expect(target.querySelector('[role="alert"]')?.textContent).toContain(
            text
          )
        );
      }
      const [first, refused, retried] = submissionKeys;
      expect(first).toBeTruthy();
      expect(refused).toBe(first);
      expect(retried).toBeTruthy();
      expect(retried).not.toBe(refused);
      expect(submissionKeys.slice(3)).toEqual([
        retried,
        retried,
        retried,
        retried
      ]);
    }
  );

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
