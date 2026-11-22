import { afterEach, expect, it, onTestFinished, vi } from 'vitest';
import { flushSync, getAllContexts, mount, unmount } from 'svelte';
import { json, serve } from '#lib/management/test/transport.js';
import StateHarness from '../../../../tests/unit/support/StateHarness.svelte';
import type { MCPCatalogPreview, MCPServer } from './api';
import MCPServersPage from './MCPServersPage.svelte';

const server: MCPServer = {
  id: 'server',
  project_id: 'project',
  name: 'Tools',
  transport: 'streamable_http',
  endpoint: 'https://tools.example.test/mcp',
  enabled: true,
  has_credential: false,
  etag: 'reviewed',
  revision_id: 'revision-1',
  revision: 1,
  certified_at: '2026-10-22T00:00:00Z',
  retired_at: null,
  oauth: null,
  delegation: null,
  unit_price: null,
  tool_prices: {},
  calls_per_minute: 60,
  max_concurrency: 8,
  per_key_calls_per_minute: 60,
  per_key_max_concurrency: 8,
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

afterEach(() => vi.unstubAllGlobals());

function render(approve: () => Response) {
  const approvals = vi.fn(approve);
  const reads = vi.fn(() => server);
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
  const target = document.createElement('div');
  const setup = vi.fn(() =>
    mount(MCPServersPage, { target, context: getAllContexts() })
  );
  const harness = mount(StateHarness, { target, props: { setup } });
  onTestFinished(async () => {
    await unmount(setup.mock.results[0].value);
    await unmount(harness);
  });
  const click = async (name: string) => {
    const button = await vi.waitFor(() => {
      const found = [...target.querySelectorAll('button')].find(
        (item) => item.textContent?.trim() === name
      );
      if (!found) throw new Error(`Missing button ${name}`);
      return found;
    });
    button.click();
    flushSync();
  };
  return { target, click, approvals, reads };
}

it('keeps a refused approval visible after the server reloads', async () => {
  const page = render(() => {
    page.reads.mockReturnValue({ ...server, etag: 'edited' });
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
  await vi.waitFor(() => expect(page.reads).toHaveBeenCalledTimes(4));
  await vi.waitFor(() =>
    expect(page.target.textContent).toContain(
      'The MCP server changed. Preview the live catalog again.'
    )
  );
  expect(page.target.textContent).not.toContain('Approve and certify');
});

it('asks before approval discards unsaved server edits', async () => {
  const confirm = vi.fn(() => false);
  vi.stubGlobal('confirm', confirm);
  const page = render(() => json(server));
  await page.click('Edit');
  const name = await vi.waitFor(() => {
    const found = page.target.querySelector('form input');
    if (!(found instanceof HTMLInputElement)) throw new Error('Missing name');
    return found;
  });
  name.value = 'Renamed';
  name.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  await page.click('Preview live catalog');
  await page.click('Approve and certify');
  expect(confirm).toHaveBeenCalledWith('Discard unsaved changes?');
  expect(page.approvals).not.toHaveBeenCalled();
  expect(name.value).toBe('Renamed');
});
