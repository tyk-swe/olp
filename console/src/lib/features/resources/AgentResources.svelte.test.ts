import { describe, expect, it, vi } from 'vitest';
import { routeKeys } from '#lib/domain/routes/routeKeys.js';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import {
  findButton,
  mountWithState,
  reportedError,
  setFieldValue as setValue
} from '#test/mount.js';
import AgentResources from './AgentResources.svelte';

const hex32 = (digit: string) => digit.repeat(32);

const agentRoute = {
  id: 'route-1',
  slug: 'gemini-agents',
  state: 'active',
  latest_revision: { operations: ['agent_resource'] }
};

function mountAgents(
  onGateway: (request: Request) => Response | Promise<Response>,
  readRoutes = () => json({ items: [agentRoute] })
) {
  serve(
    (request) => {
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/routes') return readRoutes();
      return onGateway(request);
    },
    ['read']
  );

  const { target, client } = mountWithState(AgentResources, undefined, {
    attach: true,
    configureClient: (queryClient) =>
      queryClient.setQueryDefaults(routeKeys.all(), { retry: false })
  });

  const button = (label: string) => findButton(target, label);
  const inputByLabel = <T extends HTMLElement>(label: string) =>
    [...target.querySelectorAll('label')].find((item) =>
      item.textContent?.includes(label)
    )?.control as T | undefined;

  const refreshRoutes = () =>
    client.refetchQueries({ queryKey: routeKeys.all() });

  return { target, button, inputByLabel, refreshRoutes };
}

describe('AgentResources', () => {
  it('reuses the submission key across equivalent retries, rotates on key change, recovers resource IDs, and follows pagination', async () => {
    const recoveryId = `agent_resource_${hex32('a')}`;
    const createdId = `agent_resource_${hex32('b')}`;
    const calls: { url: string; method: string; key: string | null }[] = [];
    let attempt = 0;

    const { target, button, inputByLabel } = mountAgents((request) => {
      calls.push({
        url: new URL(request.url).pathname + new URL(request.url).search,
        method: request.method,
        key: request.headers.get('Idempotency-Key')
      });
      if (request.method === 'POST') {
        attempt += 1;
        if (attempt <= 2) {
          return new Response(`ambiguous upstream #${attempt}`, {
            status: 409,
            headers: { 'X-OLP-Resource-ID': recoveryId }
          });
        }
        return json({ id: createdId, next_page_token: 'page-2' });
      }
      return json({ agents: [] });
    });

    await eventually(() =>
      expect(
        target.querySelector('option[value="gemini-agents"]')
      ).not.toBeNull()
    );

    const keyInput = inputByLabel<HTMLInputElement>('Inference API key')!;
    const routeSelect = inputByLabel<HTMLSelectElement>('Published route')!;
    const defInput = inputByLabel<HTMLTextAreaElement>('Agent definition')!;
    const idInput = inputByLabel<HTMLInputElement>('Agent resource ID')!;

    setValue(keyInput, 'olp_key_one');
    setValue(routeSelect, 'gemini-agents', 'change');
    setValue(defInput, '{"b":2,"a":1}');

    button('Create agent')?.click();
    await eventually(() =>
      expect(target.querySelector('[role="alert"]')?.textContent).toContain(
        'HTTP 409'
      )
    );
    expect(idInput.value).toBe(recoveryId);
    expect(calls).toHaveLength(1);
    const firstKey = calls[0].key;
    expect(firstKey).toBeTruthy();

    setValue(defInput, '{ "a": 1, "b": 2 }');
    button('Create agent')?.click();
    await eventually(() => {
      expect(target.querySelector('[role="alert"]')?.textContent).toContain(
        'ambiguous upstream #2'
      );
      expect(button('Create agent')?.disabled).toBe(false);
    });
    expect(calls).toHaveLength(2);
    expect(calls[1].key).toBe(firstKey);

    setValue(keyInput, 'olp_key_two');
    button('Create agent')?.click();
    await eventually(() =>
      expect(
        target.querySelector('pre[aria-label="Agent resource result"]')
          ?.textContent
      ).toContain(createdId)
    );
    expect(calls).toHaveLength(3);
    expect(calls[2].key).toBeTruthy();
    expect(calls[2].key).not.toBe(firstKey);
    expect(idInput.value).toBe(createdId);

    button('Next page')?.click();
    await eventually(() => {
      expect(
        target.querySelector('pre[aria-label="Agent resource result"]')
          ?.textContent
      ).toContain('"agents"');
      expect(button('Create agent')?.disabled).toBe(false);
    });
    expect(calls[3]).toEqual({
      url: '/gemini/v1beta/agents?page_token=page-2',
      method: 'GET',
      key: null
    });

    button('Create agent')?.click();
    await eventually(() => expect(calls).toHaveLength(5));
    expect(calls[4].key).toBeTruthy();
    expect(calls[4].key).not.toBe(calls[2].key);
  });

  it('rejects malformed JSON before contacting the gateway', async () => {
    const gateway = vi.fn(() => json({}));
    const { target, button, inputByLabel } = mountAgents(gateway);

    await eventually(() =>
      expect(
        target.querySelector('option[value="gemini-agents"]')
      ).not.toBeNull()
    );

    setValue(inputByLabel<HTMLInputElement>('Inference API key')!, 'olp_key');
    setValue(
      inputByLabel<HTMLSelectElement>('Published route')!,
      'gemini-agents',
      'change'
    );
    const definition = inputByLabel<HTMLTextAreaElement>('Agent definition')!;
    setValue(definition, '{broken');

    button('Create agent')?.click();

    await eventually(() => expect(document.activeElement).toBe(definition));
    expect(reportedError(definition)).toBeTruthy();
    expect(gateway).not.toHaveBeenCalled();
  });

  it('keeps the create key and recovers the agent when the success response cannot be read', async () => {
    const id = `agent_resource_${hex32('c')}`;
    const gateway = vi
      .fn<(request: Request) => Response>()
      .mockReturnValueOnce(
        new Response('{"id":', {
          status: 201,
          headers: { 'X-OLP-Resource-ID': id }
        })
      )
      .mockReturnValueOnce(
        new Response('This submission already exists.', {
          status: 409,
          headers: { 'X-OLP-Resource-ID': id }
        })
      )
      .mockReturnValueOnce(json({ id }));
    const { target, button, inputByLabel } = mountAgents(gateway);
    await eventually(() =>
      expect(inputByLabel('Published route')).toBeDefined()
    );

    setValue(inputByLabel<HTMLInputElement>('Inference API key')!, 'olp_key');
    setValue(
      inputByLabel<HTMLSelectElement>('Published route')!,
      agentRoute.slug,
      'change'
    );
    button('Create agent')?.click();
    await eventually(() => {
      expect(
        target.querySelector('[role="alert"]')?.textContent ?? ''
      ).toContain('invalid');
      expect(button('Create agent')?.disabled).toBe(false);
    });
    expect(inputByLabel<HTMLInputElement>('Agent resource ID')?.value).toBe(id);

    button('Create agent')?.click();
    await eventually(() => {
      expect(target.querySelector('[role="alert"]')?.textContent).toContain(
        'HTTP 409'
      );
      expect(button('Inspect agent')?.disabled).toBe(false);
    });
    const keys = gateway.mock.calls.map(([request]) =>
      request.headers.get('Idempotency-Key')
    );
    expect(keys).toHaveLength(2);
    expect(keys[0]).toBeTruthy();
    expect(keys[1]).toBe(keys[0]);

    button('Inspect agent')?.click();
    await eventually(() =>
      expect(target.querySelector('pre')?.textContent).toContain(id)
    );
    expect(new URL(gateway.mock.calls[2][0].url).pathname).toBe(
      `/gemini/v1beta/agents/${id}`
    );
  });

  it('blocks creation while a failed route refresh hides the selected route', async () => {
    const gateway = vi.fn(() => json({ agents: [] }));
    const readRoutes = vi
      .fn(() => json({ items: [agentRoute] }))
      .mockReturnValueOnce(json({ items: [agentRoute] }))
      .mockReturnValueOnce(json({ title: 'Routes unavailable' }, 503));
    const { button, inputByLabel, refreshRoutes } = mountAgents(
      gateway,
      readRoutes
    );
    await eventually(() =>
      expect(inputByLabel('Published route')).toBeDefined()
    );

    setValue(inputByLabel<HTMLInputElement>('Inference API key')!, 'olp_key');
    setValue(
      inputByLabel<HTMLSelectElement>('Published route')!,
      agentRoute.slug,
      'change'
    );
    expect(button('Create agent')?.disabled).toBe(false);

    await refreshRoutes();
    await eventually(() => expect(button('Retry')).toBeDefined());
    expect(inputByLabel('Published route')).toBeUndefined();
    expect(button('Create agent')?.disabled).toBe(true);
    expect(button('List my agents')?.disabled).toBe(false);

    button('Retry')?.click();
    await eventually(() =>
      expect(button('Create agent')?.disabled).toBe(false)
    );
    expect(inputByLabel<HTMLSelectElement>('Published route')?.value).toBe(
      agentRoute.slug
    );
    expect(gateway).not.toHaveBeenCalled();
  });
});
