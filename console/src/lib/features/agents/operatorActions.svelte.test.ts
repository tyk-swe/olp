import { useQueryClient } from '@tanstack/svelte-query';
import { SvelteMap } from 'svelte/reactivity';
import MCPServersPage from './MCPServersPage.svelte';
import { agentKeys } from './agentKeys';
import type { MCPServer } from './api';
import { afterEach, expect, it, onTestFinished, vi } from 'vitest';
import { flushSync, getAllContexts, mount, unmount } from 'svelte';
import { json, serve } from '$lib/management/test/transport';
import type { ManagementOperation } from '$lib/management/requirements';
import StateHarness from '../../../../tests/unit/support/StateHarness.svelte';
import ToolCallDetailPage from './ToolCallDetailPage.svelte';
import MCPServerEditor from './MCPServerEditor.svelte';

const server: MCPServer = {
  id: 'server',
  project_id: 'project',
  name: 'Original',
  transport: 'streamable_http',
  endpoint: 'https://tools.example.test/mcp',
  enabled: true,
  has_credential: false,
  etag: 'before',
  revision_id: 'revision',
  revision: 1,
  certified_at: '2026-01-01T00:00:00Z',
  retired_at: null,
  oauth: null,
  delegation: null,
  unit_price: null,
  tool_prices: {},
  calls_per_minute: 60,
  max_concurrency: 8,
  per_key_calls_per_minute: 60,
  per_key_max_concurrency: 8,
  catalog: { protocol_version: '2025-11-25', digest: 'fixture', tools: [] }
};

afterEach(() => vi.unstubAllGlobals());

it('resolves held amounts from the call screen and shows the resulting recorded outcome', async () => {
  const resolve = vi.fn();
  const evidence = 'c'.repeat(64);
  serve(
    async (request) => {
      if (request.method === 'POST') {
        resolve(await request.json());
        return json({ id: 'call', status: 'settled' });
      }
      return json({
        id: 'call',
        status: resolve.mock.calls.length ? 'settled' : 'outcome_unknown',
        remaining_cost: resolve.mock.calls.length ? '0' : '2'
      });
    },
    ['read', 'configure']
  );
  const target = document.createElement('div');
  const setup = vi.fn(() =>
    mount(ToolCallDetailPage, {
      target,
      props: { callID: 'call' },
      context: getAllContexts()
    })
  );
  const harness = mount(StateHarness, { target, props: { setup } });
  onTestFinished(async () => {
    await unmount(setup.mock.results[0].value);
    await unmount(harness);
  });
  await vi.waitFor(() => expect(target.querySelector('form')).not.toBeNull());
  const inputs = target.querySelectorAll('input');
  inputs[0].value = evidence;
  inputs[0].dispatchEvent(new Event('input', { bubbles: true }));
  inputs[1].value = '0.123456789012';
  inputs[1].dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  target
    .querySelector('form')
    ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() =>
    expect(resolve).toHaveBeenCalledWith({
      outcome: 'settled',
      evidence_digest: evidence,
      observed_cost: '0.123456789012',
      observed_charge: '0'
    })
  );
  await vi.waitFor(() =>
    expect(target.textContent).toContain('Outcome settled')
  );
  expect(target.querySelector('form')).toBeNull();
  expect(target.textContent).toContain('"remaining_cost": "0"');
});

it.each(['streamable_http', 'stdio'])(
  'registers a first %s MCP server with transport-specific configuration and free pricing',
  async (transport) => {
    const writes = vi.fn();
    const saved = vi.fn();
    serve(
      async (request) => {
        writes(await request.json());
        return json({
          ...server,
          transport,
          endpoint: transport === 'stdio' ? '' : server.endpoint,
          etag: 'certified',
          has_credential: transport !== 'stdio'
        });
      },
      ['read', 'configure']
    );
    const target = document.createElement('div');
    const setup = vi.fn(() =>
      mount(MCPServerEditor, {
        target,
        props: {
          current: null,
          projects: [{ id: 'project', name: 'Project' }],
          onsaved: saved
        },
        context: getAllContexts()
      })
    );
    const harness = mount(StateHarness, { target, props: { setup } });
    onTestFinished(async () => {
      await unmount(setup.mock.results[0].value);
      await unmount(harness);
    });
    flushSync();
    function fill(label: string, value: string) {
      const field = [...target.querySelectorAll('label')]
        .find((item) => item.textContent?.trim().startsWith(label))
        ?.querySelector('input,select');
      if (!(
        field instanceof HTMLInputElement || field instanceof HTMLSelectElement
      ))
        throw new Error(`Missing ${label}`);
      field.value = value;
      field.dispatchEvent(
        new Event(field instanceof HTMLSelectElement ? 'change' : 'input', {
          bubbles: true
        })
      );
      flushSync();
    }
    fill('Name', 'Tools');
    fill('Project', 'project');
    fill('Endpoint', 'https://tools.example.test/mcp');
    fill('Bearer credential', 'test-secret');
    fill('Price per call', '0');
    if (transport === 'stdio') {
      for (const label of ['Require individual grants', 'Enable delegation']) {
        const checkbox = [...target.querySelectorAll('label')]
          .find(
            (item) => item.textContent?.replace(/\s+/g, ' ').trim() === label
          )
          ?.querySelector('input');
        expect(checkbox).toBeDefined();
        if (!checkbox) return;
        checkbox.checked = true;
        checkbox.dispatchEvent(new Event('change', { bubbles: true }));
        flushSync();
      }
      fill('Entra client secret', 'test-client-secret');
      const privateKey = target.querySelector('textarea');
      expect(privateKey).not.toBeNull();
      if (!privateKey) return;
      privateKey.value = 'test-signing-key';
      privateKey.dispatchEvent(new Event('input', { bubbles: true }));
      fill('Transport', transport);
      fill('Approved executable', 'tools');
      fill('Executable SHA256', 'a'.repeat(64));
    }
    target
      .querySelector('form')
      ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await vi.waitFor(() => expect(saved).toHaveBeenCalled());
    expect(writes).toHaveBeenCalledWith(
      expect.objectContaining({
        name: 'Tools',
        project_id: 'project',
        transport,
        ...(transport === 'stdio'
          ? { stdio: { executable: 'tools', digest: 'a'.repeat(64) } }
          : { credential: 'test-secret' }),
        unit_price: '0'
      })
    );
    if (transport === 'stdio') {
      for (const field of [
        'endpoint',
        'credential',
        'oauth',
        'delegation',
        'delegation_private_key',
        'entra_client_secret'
      ]) {
        expect(writes.mock.calls[0][0]).not.toHaveProperty(field);
      }
      expect(target.querySelector('form')?.checkValidity()).toBe(true);
    }
    expect(
      [...target.querySelectorAll('input[type=password]')].every(
        (input) => (input as HTMLInputElement).value === ''
      )
    ).toBe(true);
  }
);

it.each(['OAuth', 'Entra', 'delegation'] as const)(
  'omits secrets after disabling %s authentication',
  async (mode) => {
    const current: MCPServer = {
      ...server,
      oauth: {
        client_id: 'client',
        audience: 'tools',
        scopes: ['tools.read'],
        authorization_endpoint: 'https://auth.example.test/authorize',
        token_endpoint: 'https://auth.example.test/token',
        ...(mode !== 'delegation'
          ? { entra_obo: { issuer: 'https://auth.example.test' } }
          : {})
      },
      delegation:
        mode === 'delegation'
          ? { issuer: 'https://proxy.example.test', audience: 'tools' }
          : null
    };
    const writes = vi.fn<(request: Request) => void>();
    const saved = vi.fn();
    serve(
      (request) => {
        writes(request);
        return json({ ...current, etag: 'saved' });
      },
      ['read', 'configure']
    );
    const target = document.createElement('div');
    const setup = vi.fn(() =>
      mount(MCPServerEditor, {
        target,
        props: { current, projects: [], onsaved: saved },
        context: getAllContexts()
      })
    );
    const harness = mount(StateHarness, { target, props: { setup } });
    onTestFinished(async () => {
      await unmount(setup.mock.results[0].value);
      await unmount(harness);
    });
    flushSync();
    const secret = target.querySelector<HTMLInputElement | HTMLTextAreaElement>(
      mode === 'delegation' ? 'textarea' : 'fieldset input[type=password]'
    );
    expect(secret).not.toBeNull();
    if (!secret) return;
    secret.value = 'test-secret';
    secret.dispatchEvent(new Event('input', { bubbles: true }));
    const label =
      mode === 'delegation'
        ? 'Enable delegation'
        : mode === 'Entra'
          ? 'Entra delegated issuer'
          : 'Require individual grants';
    const control = [...target.querySelectorAll('label')]
      .find((item) => item.textContent?.replace(/\s+/g, ' ').trim() === label)
      ?.querySelector('input');
    expect(control).toBeDefined();
    if (!control) return;
    if (mode === 'Entra') {
      control.value = '';
      control.dispatchEvent(new Event('input', { bubbles: true }));
    } else {
      control.checked = false;
      control.dispatchEvent(new Event('change', { bubbles: true }));
    }
    flushSync();
    target
      .querySelector('form')
      ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await vi.waitFor(() => expect(saved).toHaveBeenCalledOnce());
    const body = await writes.mock.calls[0][0].json();
    expect(body).not.toHaveProperty(
      mode === 'delegation' ? 'delegation_private_key' : 'entra_client_secret'
    );
    if (mode === 'Entra') {
      expect(body.oauth).toHaveProperty('client_id', current.oauth?.client_id);
      expect(body.oauth).not.toHaveProperty('entra_obo');
      return;
    }
    expect(body).toHaveProperty(
      mode === 'delegation' ? 'delegation' : 'oauth',
      null
    );
  }
);

it.each(['save', 'conflict', 'retire'] as const)(
  'keeps the selected server consistent during %s',
  async (operation) => {
    const retired = vi.fn();
    const writes = vi.fn<(request: Request) => void>();
    const load = vi.fn(() => json(server));
    serve(
      (request) => {
        const path = new URL(request.url).pathname;
        if (request.method === 'DELETE') {
          retired();
          return new Response(null, { status: 204 });
        }
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
        const value = retired.mock.calls.length
          ? { ...server, retired_at: '2026-01-01T00:00:00Z', etag: 'retired' }
          : server;
        if (path.endsWith('/mcp-servers')) return json({ items: [value] });
        return retired.mock.calls.length ? json(value) : load();
      },
      ['read', 'configure']
    );
    const target = document.createElement('div');
    const setup = vi.fn(() => ({
      client: useQueryClient(),
      component: mount(MCPServersPage, { target, context: getAllContexts() })
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
      expect(target.querySelector('form h2')?.textContent).toContain(
        'Edit and recertify'
      )
    );
    if (operation === 'retire') {
      button('Retire server')?.click();
      flushSync();
      button('Confirm retire server')?.click();
      await vi.waitFor(() => expect(target.textContent).toContain('Retired'));
      await vi.waitFor(() => expect(target.querySelector('form')).toBeNull());
      return;
    }
    const name = target.querySelector('form input') as HTMLInputElement;
    name.value = 'My draft';
    name.dispatchEvent(new Event('input', { bubbles: true }));
    flushSync();
    load.mockImplementation(() =>
      json({ ...server, name: 'Other operator', etag: 'remote' })
    );
    if (operation === 'save') {
      await setup.mock.results[0].value.client.invalidateQueries({
        queryKey: agentKeys.server(server.id)
      });
    }
    flushSync();
    expect((target.querySelector('form input') as HTMLInputElement).value).toBe(
      'My draft'
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
      'My draft'
    );
    button('Reload')?.click();
    flushSync();
    expect((target.querySelector('form input') as HTMLInputElement).value).toBe(
      'Other operator'
    );
  }
);

it('clears remote task state on navigation and ignores the previous task response', async () => {
  const params = new SvelteMap([['taskId', 'first']]);
  vi.doMock('$app/state', () => ({
    page: {
      get params() {
        return Object.fromEntries(params);
      }
    }
  }));
  const { default: TaskRoute } =
    await import('../../../routes/(console)/a2a-tasks/[taskId]/+page.svelte');
  const pending = Promise.withResolvers<Response>();
  const remote = vi
    .fn()
    .mockResolvedValueOnce(json({ id: 'first', status: { state: 'working' } }))
    .mockReturnValueOnce(pending.promise);
  serve(
    (request) =>
      new URL(request.url).pathname.endsWith('/remote')
        ? remote()
        : json({
            state: 'submitted',
            remote_fees: [],
            callback_models: [],
            callback_tools: []
          }),
    ['read', 'configure']
  );
  const target = document.createElement('div');
  const setup = vi.fn(() =>
    mount(TaskRoute, { target, context: getAllContexts() })
  );
  const harness = mount(StateHarness, { target, props: { setup } });
  onTestFinished(async () => {
    await unmount(setup.mock.results[0].value);
    await unmount(harness);
    vi.doUnmock('$app/state');
  });
  const fetchRemote = () =>
    [...target.querySelectorAll('button')].find(
      (button) => button.textContent?.trim() === 'Fetch remote status'
    );
  await vi.waitFor(() => expect(fetchRemote()).toBeDefined());
  fetchRemote()?.click();
  await vi.waitFor(() =>
    expect(target.textContent).toContain('Remote state working')
  );
  fetchRemote()?.click();
  await vi.waitFor(() => expect(remote).toHaveBeenCalledTimes(2));
  params.set('taskId', 'second');
  flushSync();
  await vi.waitFor(() => expect(target.textContent).toContain('second'));
  expect(target.textContent).not.toContain('Remote state working');
  pending.resolve(json({ id: 'first', status: { state: 'completed' } }));
  await pending.promise;
  await new Promise((resolve) => setTimeout(resolve, 0));
  flushSync();
  expect(target.textContent).not.toContain('Remote state completed');
  expect(fetchRemote()?.disabled).toBe(false);
});

it('allows a new bearer credential after removing the previous one without leaving the editor', async () => {
  const writes = vi.fn<(request: Request) => void>();
  const saved = vi.fn();
  serve(
    (request) => {
      writes(request);
      return json({ ...server, etag: 'without-credential' });
    },
    ['read', 'configure']
  );
  const target = document.createElement('div');
  const setup = vi.fn(() =>
    mount(MCPServerEditor, {
      target,
      props: {
        current: { ...server, has_credential: true },
        projects: [],
        onsaved: saved
      },
      context: getAllContexts()
    })
  );
  const harness = mount(StateHarness, { target, props: { setup } });
  onTestFinished(async () => {
    await unmount(setup.mock.results[0].value);
    await unmount(harness);
  });
  flushSync();
  const remove = [...target.querySelectorAll('label')]
    .find((label) =>
      label.textContent
        ?.replace(/\s+/g, ' ')
        .includes('Remove stored bearer credential')
    )
    ?.querySelector('input');
  expect(remove).toBeDefined();
  if (!remove) return;
  remove.checked = true;
  remove.dispatchEvent(new Event('change', { bubbles: true }));
  flushSync();
  target
    .querySelector('form')
    ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() => expect(saved).toHaveBeenCalledOnce());
  expect(await writes.mock.calls[0][0].json()).toHaveProperty(
    'credential',
    null
  );
  const credential = target.querySelector<HTMLInputElement>(
    'input[type=password]'
  );
  expect(credential?.disabled).toBe(false);
  if (!credential) return;
  credential.value = 'replacement';
  credential.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  target
    .querySelector('form')
    ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() => expect(saved).toHaveBeenCalledTimes(2));
  expect(await writes.mock.calls[1][0].json()).toHaveProperty(
    'credential',
    'replacement'
  );
  expect(writes.mock.calls[1][0].headers.get('If-Match')).toBe(
    '"without-credential"'
  );
});

it('serializes remote status fetching and cancellation', async () => {
  const { default: TaskDetailPage } = await import('./TaskDetailPage.svelte');
  const status = Promise.withResolvers<Response>();
  const cancelled = Promise.withResolvers<Response>();
  const reads = vi.fn(() => status.promise);
  const cancels = vi.fn(() => cancelled.promise);
  serve(
    (request) => {
      const path = new URL(request.url).pathname;
      if (path.endsWith('/remote')) return reads();
      if (path.endsWith('/cancel')) return cancels();
      return json({
        state: 'submitted',
        remote_fees: [],
        callback_models: [],
        callback_tools: []
      });
    },
    ['read', 'configure']
  );
  const target = document.createElement('div');
  const setup = vi.fn(() =>
    mount(TaskDetailPage, {
      target,
      props: { taskID: 'task' },
      context: getAllContexts()
    })
  );
  const harness = mount(StateHarness, { target, props: { setup } });
  onTestFinished(async () => {
    status.resolve(json({ id: 'task', status: { state: 'working' } }));
    cancelled.resolve(json({ id: 'task', status: { state: 'canceled' } }));
    await unmount(setup.mock.results[0].value);
    await unmount(harness);
  });
  const button = (label: string) =>
    [...target.querySelectorAll('button')].find(
      (item) => item.textContent?.trim() === label
    );
  await vi.waitFor(() => expect(button('Fetch remote status')).toBeDefined());
  button('Cancel remote task')?.click();
  flushSync();
  button('Fetch remote status')?.click();
  await vi.waitFor(() => expect(reads).toHaveBeenCalledOnce());
  expect(button('Confirm cancel remote task')?.disabled).toBe(true);
  button('Confirm cancel remote task')?.click();
  expect(cancels).not.toHaveBeenCalled();
  status.resolve(json({ id: 'task', status: { state: 'working' } }));
  await vi.waitFor(() =>
    expect(button('Confirm cancel remote task')?.disabled).toBe(false)
  );
  button('Confirm cancel remote task')?.click();
  await vi.waitFor(() => expect(cancels).toHaveBeenCalledOnce());
  expect(button('Fetch remote status')?.disabled).toBe(true);
  button('Fetch remote status')?.click();
  expect(reads).toHaveBeenCalledOnce();
  cancelled.resolve(json({ id: 'task', status: { state: 'canceled' } }));
  await vi.waitFor(() =>
    expect(target.textContent).toContain('Remote state canceled')
  );
  await vi.waitFor(() =>
    expect(button('Fetch remote status')?.disabled).toBe(false)
  );
});

function mountServers(operations: ManagementOperation[]) {
  serve((request) => {
    const path = new URL(request.url).pathname;
    if (path.endsWith('/projects'))
      return json({ items: [{ id: 'project', name: 'Project' }] });
    return path.endsWith('/mcp-servers')
      ? json({ items: [server] })
      : json(server);
  }, operations);
  const target = document.createElement('div');
  const setup = vi.fn(() =>
    mount(MCPServersPage, { target, context: getAllContexts() })
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

it('offers read-only members inspection but no server create or edit controls', async () => {
  const { target, button } = mountServers(['read']);
  await vi.waitFor(() => expect(button('Inspect')).toBeDefined());

  expect(button('New server')).toBeUndefined();
  expect(button('Inspect and edit')).toBeUndefined();
  expect(target.querySelector('form')).toBeNull();
});

it('keeps an edited server draft when the operator declines to discard it', async () => {
  const decline = vi.fn(() => false);
  vi.stubGlobal('confirm', decline);
  const { target, button } = mountServers(['read', 'configure']);
  await vi.waitFor(() => expect(button('Inspect and edit')).toBeDefined());
  button('Inspect and edit')?.click();
  await vi.waitFor(() =>
    expect(target.querySelector('form h2')?.textContent).toContain(
      'Edit and recertify'
    )
  );
  const name = () => target.querySelector('form input') as HTMLInputElement;
  name().value = 'My draft';
  name().dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();

  button('New server')?.click();
  flushSync();

  expect(decline).toHaveBeenCalledOnce();
  expect(name().value).toBe('My draft');
});
