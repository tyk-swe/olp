import { expect, it, vi } from 'vitest';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import { clickButton, mountWithState, setFieldValue } from '#test/mount.js';
import ExposurePage from './ExposurePage.svelte';
import ToolCallsPage from './ToolCallsPage.svelte';

it.each([
  {
    name: 'tool calls',
    path: '/api/v1/mcp-tool-calls',
    mount: () => mountWithState(ToolCallsPage, {}, { attach: true })
  },
  {
    name: 'provisional exposure',
    path: '/api/v1/execution-debits',
    mount: () => mountWithState(ExposurePage, {}, { attach: true })
  }
])('filters $name by an API key UUID', async ({ path, mount }) => {
  const requestedKey = vi.fn();
  serve((request) => {
    const url = new URL(request.url);
    if (url.pathname === path) requestedKey(url.searchParams.get('api_key_id'));
    return json({ items: [] });
  });
  const { target } = mount();
  const input = target.querySelector('input');
  const key = crypto.randomUUID();

  setFieldValue(input!, key);
  clickButton(target, 'Filter');

  await eventually(() => expect(requestedKey).toHaveBeenLastCalledWith(key));
});
