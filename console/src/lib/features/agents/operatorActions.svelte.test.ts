import { PersonalGrantEnrollment } from './grantEnrollment.svelte';
import { authLifecycle } from '#lib/features/access/session/lifecycle.js';
import { useQueryClient } from '@tanstack/svelte-query';
import { SvelteMap } from 'svelte/reactivity';
import MCPServersPage from './MCPServersPage.svelte';
import { agentKeys } from './agentKeys';
import type { MCPServer, Publication } from './api';
import PublicationsPage from './PublicationsPage.svelte';
import GrantsPage from './GrantsPage.svelte';
import { afterEach, expect, it, onTestFinished, vi } from 'vitest';
import { flushSync, getAllContexts, mount, unmount } from 'svelte';
import { json, serve } from '#lib/management/test/transport.js';
import type { ManagementOperation } from '#lib/management/requirements.js';
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
    await vi.waitFor(() => expect(button('Edit')).toBeDefined());
    button('Edit')?.click();
    await vi.waitFor(() =>
      expect(target.querySelector<HTMLInputElement>('form input')?.value).toBe(
        'Original'
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
    await import('../../../routes/(console)/activity/tasks/[taskId]/+page.svelte');
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
  await vi.waitFor(() => expect(button('View')).toBeDefined());

  expect(button('New server')).toBeUndefined();
  expect(button('Edit')).toBeUndefined();
  expect(target.querySelector('form')).toBeNull();
});

it('keeps an edited server draft when the operator declines to discard it', async () => {
  const decline = vi.fn(() => false);
  vi.stubGlobal('confirm', decline);
  const { target, button } = mountServers(['read', 'configure']);
  await vi.waitFor(() => expect(button('Edit')).toBeDefined());
  button('Edit')?.click();
  await vi.waitFor(() =>
    expect(target.querySelector<HTMLInputElement>('form input')?.value).toBe(
      'Original'
    )
  );
  const name = () => target.querySelector('form input') as HTMLInputElement;
  name().value = 'My draft';
  name().dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();

  target
    .querySelector<HTMLButtonElement>('dialog [aria-label="Close"]')
    ?.click();
  flushSync();

  expect(decline).toHaveBeenCalledOnce();
  expect(name().value).toBe('My draft');
});

it('retries publication refresh after a saved write without submitting the write again', async () => {
  const publication: Publication = {
    id: 'prompt',
    project_id: 'project',
    kind: 'prompt',
    name: 'Original',
    etag: 'before',
    revision_id: null,
    retired: false,
    draft: {
      project_id: 'project',
      name: 'Original',
      prompt: { template: 'Hello', variables: [] }
    }
  };
  const writes = vi.fn();
  const recovered = vi.fn();
  serve(
    async (request) => {
      const path = new URL(request.url).pathname;
      if (path.endsWith('/projects'))
        return json({ items: [{ id: 'project', name: 'Project' }] });
      if (request.method === 'PUT') {
        const input = await request.json();
        writes(input);
        return json({
          ...publication,
          name: 'Saved',
          etag: 'after',
          draft: input
        });
      }
      if (path.endsWith('/prompt')) return json(publication);
      if (writes.mock.calls.length && !recovered.mock.calls.length)
        return json({ status: 503, title: 'Refresh unavailable' }, 503);
      return json({ items: [publication] });
    },
    ['read', 'configure']
  );
  const target = document.createElement('div');
  const setup = vi.fn(() => {
    useQueryClient().setQueryDefaults(agentKeys.publications, { retry: false });
    return mount(PublicationsPage, {
      target,
      props: { kind: 'prompts' },
      context: getAllContexts()
    });
  });
  const harness = mount(StateHarness, { target, props: { setup } });
  onTestFinished(async () => {
    await unmount(setup.mock.results[0].value);
    await unmount(harness);
  });
  const button = (name: string) =>
    [...target.querySelectorAll('button')].find(
      (item) => item.textContent?.trim() === name
    );
  await vi.waitFor(() => expect(button('Open')).toBeDefined());
  button('Open')?.click();
  await vi.waitFor(() => expect(target.querySelector('form')).not.toBeNull());
  const name = target.querySelector<HTMLInputElement>('form input');
  expect(name).not.toBeNull();
  if (!name) return;
  name.value = 'Saved';
  name.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  target
    .querySelector('form')
    ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() => expect(button('Retry refresh')?.disabled).toBe(false));
  expect(writes).toHaveBeenCalledOnce();
  expect(name.value).toBe('Saved');
  expect(target.textContent).toContain('Draft saved.');
  recovered();
  button('Retry refresh')?.click();
  await vi.waitFor(() => expect(button('Retry refresh')).toBeUndefined());
  expect(writes).toHaveBeenCalledOnce();
  expect(target.textContent).not.toContain(
    'Write saved, but refreshing failed.'
  );
});

function personalEnrollment() {
  const ready = vi.fn(() => new PersonalGrantEnrollment());
  const component = mount(StateHarness, {
    target: document.createElement('div'),
    props: { setup: ready }
  });
  onTestFinished(() => unmount(component));
  flushSync();
  return ready.mock.results[0].value;
}

it('consumes and clears handoff and callback secrets after their confirmed writes', async () => {
  const writes = vi.fn();
  serve(
    async (request) => {
      writes(new URL(request.url).pathname, await request.json());
      if (new URL(request.url).pathname.endsWith('/claim'))
        return json({ enrollment: { id: 'grant' }, state: 'pending' });
      return json({ state: 'active' });
    },
    ['read', 'configure']
  );
  const state = personalEnrollment();
  expect(
    state.consumeLocation(
      new URL('https://console.example.test/grants?tab=mine#grant.token')
    )
  ).toBe('/grants?tab=mine');
  await state.claim();
  expect(writes).toHaveBeenCalledWith('/api/v1/mcp-grant-enrollments/claim', {
    enrollment_id: 'grant',
    handoff_token: 'token'
  });
  expect(state.view.phase.kind).toBe('claimed');
  expect(state.view.handoffToken).toBe('');
  expect(
    state.consumeLocation(
      new URL(
        'https://console.example.test/grants?code=secret-code&state=grant.signature'
      )
    )
  ).toBe('/grants');
  await state.completeCallback();
  expect(writes).toHaveBeenCalledWith(
    '/api/v1/mcp-grant-enrollments/grant/complete',
    { code: 'secret-code', state: 'grant.signature' }
  );
  expect(state.view.callbackCode).toBe('');
  expect(state.view.authorization?.state).toBe('active');
});

it('ignores an obsolete enrollment response after consuming another handoff', async () => {
  const response = Promise.withResolvers<Response>();
  serve(() => response.promise, ['read', 'configure']);
  const state = personalEnrollment();
  const pending = state.begin({ kind: 'server', id: 'server', keyId: 'key' });
  state.consumeLocation(
    new URL('https://console.example.test/grants#new.token')
  );
  response.resolve(
    json({ enrollment_url: 'https://console.example.test/grants#old.token' })
  );
  await pending;
  expect(state.view.enrollmentID).toBe('new');
  expect(state.view.handoffToken).toBe('token');
  expect(state.view.enrollmentURL).toBe('');
  expect(state.view.busy).toBe(false);
});

it('retires a pending authorization and its secrets when the principal changes', async () => {
  const response = Promise.withResolvers<Response>();
  serve(() => response.promise, ['read', 'configure']);
  const state = personalEnrollment();
  state.consumeLocation(
    new URL(
      'https://console.example.test/grants?code=secret&state=grant.signature'
    )
  );
  const pending = state.completeCallback();
  authLifecycle.establishSession({
    user: {
      id: 'other',
      email: 'other@example.test',
      display_name: 'Other',
      role: 'owner',
      access_scope: 'global',
      operations: ['read', 'configure']
    },
    csrf_token: 'other-csrf'
  });
  response.resolve(json({ state: 'active' }));
  await pending;
  expect(state.view.phase.kind).toBe('idle');
  expect(state.view.callbackCode).toBe('');
  expect(state.view.authorization).toBeUndefined();
});

it('shows the latest personal grant result after saving authorization settings', async () => {
  const configured: MCPServer = {
    ...server,
    oauth: {
      client_id: 'client',
      audience: 'audience',
      scopes: ['read'],
      authorization_endpoint: 'https://auth.example.test/authorize',
      token_endpoint: 'https://auth.example.test/token'
    }
  };
  serve(
    async (request) => {
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/mcp-servers/server') return json(configured);
      if (path === '/api/v1/mcp-servers') return json({ items: [configured] });
      if (path === '/api/v1/mcp-grant-enrollments/claim')
        return json({ enrollment: { id: 'grant' }, state: 'pending' });
      return json({ items: [] });
    },
    ['read', 'configure', 'keys', 'self']
  );
  const originalURL = window.location.href;
  window.history.replaceState(null, '', window.location.pathname);
  const target = document.createElement('div');
  const setup = vi.fn(() =>
    mount(GrantsPage, {
      target,
      context: getAllContexts()
    })
  );
  const harness = mount(StateHarness, { target, props: { setup } });
  onTestFinished(async () => {
    await unmount(setup.mock.results[0].value);
    await unmount(harness);
    window.history.replaceState(null, '', originalURL);
  });
  const button = (label: string) =>
    [...target.querySelectorAll('button')].find(
      (item) => item.textContent?.trim() === label
    );
  await vi.waitFor(() =>
    expect(target.querySelector('select option[value="server"]')).not.toBeNull()
  );
  const selection = target
    .querySelector('select option[value="server"]')
    ?.closest<HTMLSelectElement>('select');
  expect(selection).not.toBeNull();
  if (!selection) return;
  selection.value = configured.id;
  selection.dispatchEvent(new Event('change', { bubbles: true }));
  await vi.waitFor(() =>
    expect(button('Save authorization settings')).toBeDefined()
  );
  button('Save authorization settings')
    ?.closest('form')
    ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() =>
    expect(target.textContent).toContain(
      'Personal authorization configuration saved'
    )
  );
  window.history.replaceState(null, '', '#grant.token');
  window.dispatchEvent(new HashChangeEvent('hashchange'));
  await vi.waitFor(() => expect(button('Claim enrollment')).toBeDefined());
  button('Claim enrollment')?.click();
  await vi.waitFor(() =>
    expect(target.textContent).toContain(
      'Enrollment claimed. Authorization is pending'
    )
  );
  expect(target.textContent).not.toContain(
    'Personal authorization configuration saved'
  );
});

it('retains the callback while starting and clearing a separate invitation', async () => {
  const writes = vi.fn();
  serve(
    async (request) => {
      writes(new URL(request.url).pathname, await request.json());
      return new URL(request.url).pathname.endsWith('/complete')
        ? json({ state: 'active' })
        : json({
            enrollment_url: 'https://console.example.test/grants#next.token'
          });
    },
    ['read', 'configure']
  );
  const state = personalEnrollment();
  state.consumeLocation(
    new URL(
      'https://console.example.test/grants?code=secret-code&state=grant.signature'
    )
  );
  await state.begin({ kind: 'server', id: 'server', keyId: 'key' });
  expect(state.view.callbackCode).toBe('secret-code');
  expect(state.view.enrollmentURL).toContain('#next.token');
  state.clearInvitation();
  expect(state.view.enrollmentURL).toBe('');
  await state.completeCallback();
  expect(writes).toHaveBeenCalledWith(
    '/api/v1/mcp-grant-enrollments/grant/complete',
    { code: 'secret-code', state: 'grant.signature' }
  );
  expect(state.view.callbackCode).toBe('');
  expect(state.view.authorization?.state).toBe('active');
});
