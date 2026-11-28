import { expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import type { ManagementOperation } from '#lib/management/requirements.js';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import {
  findButton,
  findControl,
  mountWithState,
  reportedError,
  setFieldValue
} from '#test/mount.js';
import { agentKeys } from './agentKeys';
import type { MCPCatalogPreview, MCPServer } from './api';
import MCPServerEditor from './MCPServerEditor.svelte';
import MCPServersPage from './MCPServersPage.svelte';

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

const reviewedServer: MCPServer = {
  ...server,
  name: 'Tools',
  etag: 'reviewed',
  revision_id: 'revision-1',
  certified_at: '2026-10-22T00:00:00Z',
  catalog: { protocol_version: '2025-11-25', digest: 'saved', tools: [] }
};

const preview: MCPCatalogPreview = {
  server_id: 'server',
  etag: 'reviewed',
  revision_id: 'revision-1',
  revision: 1,
  saved_protocol_version: '2025-11-25',
  saved_digest: 'saved',
  discovered_protocol_version: '2025-11-25',
  discovered_digest: 'live',
  added: [{ name: 'search', description: 'Search', input_schema: {} }],
  removed: [],
  modified: [],
  toolsets: []
};

function renderCatalogApproval(approve: () => Response) {
  const approvals = vi.fn(approve);
  const reads = vi.fn(() => reviewedServer);
  serve(
    (request) => {
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/projects')
        return json({ items: [{ id: 'project', name: 'Project' }] });
      if (path === '/api/v1/mcp-servers') return json({ items: [reads()] });
      if (path === '/api/v1/mcp-servers/server') return json(reads());
      if (path.endsWith('/catalog-preview')) return json(preview);
      if (path.endsWith('/catalog-approval')) return approvals();
      return json({ title: `Unexpected ${path}`, status: 404 }, 404);
    },
    ['configure']
  );
  const { target } = mountWithState(MCPServersPage);
  const click = async (name: string) => {
    const button = await eventually(() => {
      const found = findButton(target, name);
      if (!found) throw new Error(`Missing button ${name}`);
      return found;
    });
    button.click();
    flushSync();
  };
  return { target, click, approvals, reads };
}

function mountServers(operations: ManagementOperation[]) {
  serve((request) => {
    const path = new URL(request.url).pathname;
    if (path.endsWith('/projects'))
      return json({ items: [{ id: 'project', name: 'Project' }] });
    return path.endsWith('/mcp-servers')
      ? json({ items: [server] })
      : json(server);
  }, operations);
  const { target } = mountWithState(MCPServersPage);
  const button = (name: string) => findButton(target, name);
  return { target, button };
}

it('keeps a refused approval visible after the server reloads', async () => {
  const page = renderCatalogApproval(() => {
    page.reads.mockReturnValue({ ...reviewedServer, etag: 'edited' });
    return json(
      {
        type: 'https://openllmproxy.dev/problems/etag_mismatch',
        title: 'The MCP server changed. Preview the live catalog again.',
        status: 412
      },
      412
    );
  });
  await page.click('Edit');
  await page.click('Preview live catalog');
  await page.click('Approve and certify');
  await eventually(() => expect(page.reads).toHaveBeenCalledTimes(4));
  await eventually(() =>
    expect(page.target.textContent).toContain(
      'The MCP server changed. Preview the live catalog again.'
    )
  );
  expect(page.target.textContent).not.toContain('Approve and certify');
});

it('asks before approval discards unsaved server edits', async () => {
  const confirm = vi.fn(() => false);
  vi.stubGlobal('confirm', confirm);
  const page = renderCatalogApproval(() => json(reviewedServer));
  await page.click('Edit');
  const name = await eventually(() => {
    const found = page.target.querySelector('form input');
    if (!(found instanceof HTMLInputElement)) throw new Error('Missing name');
    return found;
  });
  setFieldValue(name, 'Renamed');
  await page.click('Preview live catalog');
  await page.click('Approve and certify');
  expect(confirm).toHaveBeenCalledWith('Discard unsaved changes?');
  expect(page.approvals).not.toHaveBeenCalled();
  expect(name.value).toBe('Renamed');
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
          stdio:
            transport === 'stdio'
              ? { executable: 'tools', digest: 'a'.repeat(64) }
              : null,
          etag: 'certified',
          has_credential: transport !== 'stdio'
        });
      },
      ['read', 'configure']
    );
    const { target } = mountWithState(MCPServerEditor, {
      current: null,
      projects: [{ id: 'project', name: 'Project' }],
      onsaved: saved
    });
    function fill(label: string, value: string) {
      const field = [...target.querySelectorAll('label')].find((item) =>
        item.textContent?.trim().startsWith(label)
      )?.control;
      if (!(
        field instanceof HTMLInputElement || field instanceof HTMLSelectElement
      ))
        throw new Error(`Missing ${label}`);
      setFieldValue(field, value);
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
      setFieldValue(privateKey, 'test-signing-key');
      fill('Transport', transport);
      fill('Approved executable', 'tools');
      fill('Executable SHA256', 'a'.repeat(64));
    }
    target
      .querySelector('form')
      ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await eventually(() => expect(saved).toHaveBeenCalled());
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
    const { target } = mountWithState(MCPServerEditor, {
      current,
      projects: [],
      onsaved: saved
    });
    const secret = target.querySelector<HTMLInputElement | HTMLTextAreaElement>(
      mode === 'delegation' ? 'textarea' : 'fieldset input[type=password]'
    );
    expect(secret).not.toBeNull();
    if (!secret) return;
    setFieldValue(secret, 'test-secret');
    const label =
      mode === 'delegation'
        ? 'Enable delegation'
        : mode === 'Entra'
          ? 'Entra delegated issuer'
          : 'Require individual grants';
    const control = [...target.querySelectorAll('label')].find(
      (item) => item.textContent?.replace(/\s+/g, ' ').trim() === label
    )?.control;
    expect(control).toBeInstanceOf(HTMLInputElement);
    if (!(control instanceof HTMLInputElement)) return;
    if (mode === 'Entra') {
      setFieldValue(control, '');
    } else {
      control.checked = false;
      control.dispatchEvent(new Event('change', { bubbles: true }));
      flushSync();
    }
    target
      .querySelector('form')
      ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await eventually(() => expect(saved).toHaveBeenCalledOnce());
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
    const { target, client } = mountWithState(MCPServersPage);
    const button = (name: string) => findButton(target, name);
    await eventually(() => expect(button('Edit')).toBeDefined());
    button('Edit')?.click();
    await eventually(() =>
      expect(target.querySelector<HTMLInputElement>('form input')?.value).toBe(
        'Original'
      )
    );
    if (operation === 'retire') {
      button('Retire server')?.click();
      flushSync();
      button('Confirm retire server')?.click();
      await eventually(() => expect(target.textContent).toContain('Retired'));
      await eventually(() => expect(target.querySelector('form')).toBeNull());
      return;
    }
    const name = target.querySelector('form input') as HTMLInputElement;
    setFieldValue(name, 'My draft');
    load.mockImplementation(() =>
      json({ ...server, name: 'Other operator', etag: 'remote' })
    );
    if (operation === 'save') {
      await client.invalidateQueries({ queryKey: agentKeys.server(server.id) });
    }
    flushSync();
    expect((target.querySelector('form input') as HTMLInputElement).value).toBe(
      'My draft'
    );
    target
      .querySelector('form')
      ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await eventually(() => expect(writes).toHaveBeenCalledOnce());
    expect(writes.mock.calls[0][0].headers.get('If-Match')).toBe('"before"');
    await eventually(() =>
      expect(target.textContent).toContain('Changed elsewhere')
    );
    await eventually(() => expect(button('Reload')?.disabled).toBe(false));
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

it('marks every invalid field and sends nothing until they are fixed', async () => {
  const writes = vi.fn<(body: unknown) => void>();
  const saved = vi.fn();
  serve(
    async (request) => {
      writes(await request.json());
      return json(server);
    },
    ['read', 'configure']
  );
  const { target } = mountWithState(
    MCPServerEditor,
    {
      current: null,
      projects: [{ id: 'project', name: 'Project' }],
      onsaved: saved
    },
    { attach: true }
  );
  const checkboxes = () => [
    ...target.querySelectorAll<HTMLInputElement>(
      'fieldset input[type=checkbox]'
    )
  ];
  for (const checkbox of checkboxes()) checkbox.click();
  flushSync();
  const invalid = [
    ['Name', '   ', 'Enter a name.'],
    ['Endpoint', 'tools', 'Enter a full URL.'],
    [
      'Price per call',
      'free',
      'Use a non-negative decimal unit price, 0 for free, or blank for unpriced.'
    ],
    [
      'Calls per minute',
      '0',
      'Calls per minute must be a whole number from 1 to 60000.'
    ],
    [
      'Concurrent calls',
      '2000',
      'Max concurrency must be a whole number from 1 to 1024.'
    ],
    [
      'Calls per minute per key',
      '',
      'Per-key calls per minute must be a whole number from 1 to 60000.'
    ],
    [
      'Concurrent calls per key',
      '1.5',
      'Per-key max concurrency must be a whole number from 1 to 1024.'
    ],
    ['Client ID', '', 'Enter the client ID.'],
    ['Audience', '', 'Enter the audience.'],
    ['Scopes separated by spaces', '  ', 'Enter at least one scope.'],
    ['Authorization endpoint', 'authorize', 'Enter a full URL.'],
    ['Token endpoint', '', 'Enter a full URL.'],
    ['Device authorization endpoint', 'device', 'Enter a full URL.'],
    ['Issuer', '', 'Enter a full URL.']
  ] as const;
  for (const [label, value] of invalid)
    setFieldValue(findControl(target, label), value);

  target.querySelector('form')?.requestSubmit();

  await eventually(() =>
    expect(document.activeElement).toBe(findControl(target, 'Name'))
  );
  for (const [label, , message] of invalid)
    expect(reportedError(findControl(target, label))).toBe(message);
  const project = target.querySelector('select') as HTMLSelectElement;
  expect(reportedError(project)).toBe('Choose a project.');
  expect(writes).not.toHaveBeenCalled();

  for (const checkbox of checkboxes()) checkbox.click();
  flushSync();
  setFieldValue(project, 'project');
  for (const [label, value] of [
    ['Name', 'Tools'],
    ['Endpoint', 'https://tools.example.test/mcp'],
    ['Price per call', ' 0.5 '],
    ['Calls per minute', '30'],
    ['Concurrent calls', '4'],
    ['Calls per minute per key', '10'],
    ['Concurrent calls per key', '2']
  ])
    setFieldValue(findControl(target, label), value);
  target.querySelector('form')?.requestSubmit();

  await eventually(() => expect(saved).toHaveBeenCalledOnce());
  expect(writes).toHaveBeenCalledOnce();
  expect(writes.mock.calls[0][0]).toMatchObject({
    unit_price: '0.5',
    calls_per_minute: 30,
    max_concurrency: 4,
    per_key_calls_per_minute: 10,
    per_key_max_concurrency: 2
  });
});

it('marks a new stdio registration without an executable or digest', () => {
  const { target } = mountWithState(
    MCPServerEditor,
    { current: null, projects: [], onsaved: vi.fn() },
    { attach: true }
  );
  setFieldValue(target.querySelectorAll('select')[1], 'stdio');
  setFieldValue(findControl(target, 'Executable SHA256'), 'ABC');

  target.querySelector('form')?.requestSubmit();
  flushSync();

  expect(reportedError(findControl(target, 'Approved executable'))).toBe(
    'Enter the approved executable.'
  );
  expect(reportedError(findControl(target, 'Executable SHA256'))).toBe(
    'Enter 64 lowercase hexadecimal characters.'
  );
});

it('marks the certified tool whose price is not a decimal', () => {
  const tools = ['search', 'fetch'].map((name) => ({
    name,
    description: name,
    input_schema: {}
  }));
  const { target } = mountWithState(
    MCPServerEditor,
    {
      current: { ...server, catalog: { ...server.catalog, tools } },
      projects: [],
      onsaved: vi.fn()
    },
    { attach: true }
  );
  setFieldValue(findControl(target, 'fetch price'), '1,5');

  target.querySelector('form')?.requestSubmit();
  flushSync();

  expect(reportedError(findControl(target, 'fetch price'))).toBe(
    'Use a non-negative decimal price for fetch.'
  );
  expect(reportedError(findControl(target, 'search price'))).toBeUndefined();
});

it.each([
  ['endpoint', 'Endpoint'],
  ['credential', 'Bearer credential'],
  ['per_key_max_concurrency', 'Concurrent calls per key'],
  ['oauth.entra_obo.issuer', 'Entra delegated issuer'],
  ['entra_client_secret', 'Entra client secret']
])('shows a save refused for %s on its field', async (field, label) => {
  serve(
    () =>
      json(
        {
          type: 'https://openllmproxy.dev/problems/validation_failed',
          title: 'Validation failed',
          detail: 'Fix the field.',
          status: 422,
          errors: { [field]: [{ code: 'invalid', message: 'Not allowed.' }] }
        },
        422
      ),
    ['read', 'configure']
  );
  const { target } = mountWithState(
    MCPServerEditor,
    {
      current: {
        ...server,
        oauth: {
          client_id: 'client',
          audience: 'tools',
          scopes: ['tools.read'],
          token_endpoint: 'https://auth.example.test/token'
        }
      },
      projects: [],
      onsaved: vi.fn()
    },
    { attach: true }
  );

  target.querySelector('form')?.requestSubmit();

  await eventually(() =>
    expect(document.activeElement).toBe(findControl(target, label))
  );
  expect(reportedError(findControl(target, label))).toBe('Not allowed.');
  expect(target.querySelector('[role=alert]')).toBeNull();
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
  const { target } = mountWithState(MCPServerEditor, {
    current: { ...server, has_credential: true },
    projects: [],
    onsaved: saved
  });
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
  await eventually(() => expect(saved).toHaveBeenCalledOnce());
  expect(await writes.mock.calls[0][0].json()).toHaveProperty(
    'credential',
    null
  );
  const credential = target.querySelector<HTMLInputElement>(
    'input[type=password]'
  );
  expect(credential?.disabled).toBe(false);
  if (!credential) return;
  setFieldValue(credential, 'replacement');
  target
    .querySelector('form')
    ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await eventually(() => expect(saved).toHaveBeenCalledTimes(2));
  expect(await writes.mock.calls[1][0].json()).toHaveProperty(
    'credential',
    'replacement'
  );
  expect(writes.mock.calls[1][0].headers.get('If-Match')).toBe(
    '"without-credential"'
  );
});

it('offers read-only members inspection but no server create or edit controls', async () => {
  const { target, button } = mountServers(['read']);
  await eventually(() => expect(button('View')).toBeDefined());

  expect(button('New server')).toBeUndefined();
  expect(button('Edit')).toBeUndefined();
  expect(target.querySelector('form')).toBeNull();
});

it('offers projects that loaded before the new server form opened', async () => {
  serve(
    (request) =>
      new URL(request.url).pathname.endsWith('/projects')
        ? json({ items: [{ id: 'project', name: 'Project' }] })
        : json({ items: [] }),
    ['read', 'configure']
  );
  const { target, client } = mountWithState(MCPServersPage);
  await eventually(() => {
    expect(target.textContent).toContain('No MCP servers yet');
    expect(client.isFetching()).toBe(0);
  });

  findButton(target, 'New server')?.click();
  flushSync();

  await eventually(() =>
    expect(
      [...target.querySelectorAll('option')].map((option) =>
        option.textContent?.trim()
      )
    ).toContain('Project')
  );
});

it('keeps an edited server draft when the operator declines to discard it', async () => {
  const decline = vi.fn(() => false);
  vi.stubGlobal('confirm', decline);
  const { target, button } = mountServers(['read', 'configure']);
  await eventually(() => expect(button('Edit')).toBeDefined());
  button('Edit')?.click();
  await eventually(() =>
    expect(target.querySelector<HTMLInputElement>('form input')?.value).toBe(
      'Original'
    )
  );
  const name = () => target.querySelector('form input') as HTMLInputElement;
  setFieldValue(name(), 'My draft');

  target
    .querySelector<HTMLButtonElement>('dialog [aria-label="Close"]')
    ?.click();
  flushSync();

  expect(decline).toHaveBeenCalledOnce();
  expect(name().value).toBe('My draft');
});
