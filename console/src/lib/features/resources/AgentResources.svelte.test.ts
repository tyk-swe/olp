import { afterEach, describe, expect, it, onTestFinished, vi } from 'vitest';
import { flushSync, getAllContexts, mount, unmount } from 'svelte';
import { json, serve } from '#lib/session/test/transport.js';
import StateHarness from '../../../../tests/unit/support/StateHarness.svelte';
import { eventually } from '../../../../tests/unit/support/eventually.js';
import AgentResources from './AgentResources.svelte';

afterEach(() => vi.unstubAllGlobals());

const hex32 = (digit: string) => digit.repeat(32);

const agentRoute = {
  id: 'route-1',
  slug: 'gemini-agents',
  state: 'active',
  latest_revision: { operations: ['agent_resource'] }
};

function mountAgents(
  onGateway: (request: Request) => Response | Promise<Response>
) {
  serve(
    (input: RequestInfo | URL, init?: RequestInit) => {
      const request =
        input instanceof Request
          ? input
          : new Request(new URL(String(input), 'http://localhost'), init);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/routes') return json({ items: [agentRoute] });
      return onGateway(request);
    },
    ['read']
  );

  const target = document.createElement('div');
  const setup = vi.fn(() =>
    mount(AgentResources, { target, context: getAllContexts() })
  );
  const harness = mount(StateHarness, { target, props: { setup } });
  onTestFinished(async () => {
    await unmount(setup.mock.results[0].value);
    await unmount(harness);
  });

  const button = (label: string) =>
    [...target.querySelectorAll('button')].find(
      (item) => item.textContent?.trim() === label
    );
  const inputByLabel = <T extends HTMLElement>(label: string) =>
    [...target.querySelectorAll('label')].find((item) =>
      item.textContent?.includes(label)
    )?.control as T | undefined;

  return { target, button, inputByLabel };
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
    setValue(inputByLabel<HTMLTextAreaElement>('Agent definition')!, '{broken');

    button('Create agent')?.click();
    flushSync();

    expect(target.querySelector('[role="alert"]')?.textContent).toBeTruthy();
    expect(gateway).not.toHaveBeenCalled();
  });
});
