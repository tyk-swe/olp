import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { expect, it, onTestFinished, vi } from 'vitest';
import * as api from './api';
import MCPExecutablesProbe from './test/MCPExecutablesProbe.svelte';

vi.mock('./api', async (original) => ({
  ...(await original<typeof import('./api')>()),
  listMCPExecutables: vi.fn(),
  reviewMCPExecutable: vi.fn(),
  revokeMCPExecutable: vi.fn()
}));

function button(host: HTMLElement, text: string): HTMLButtonElement {
  const found = [...host.querySelectorAll('button')].find(
    (item) => item.textContent?.trim() === text
  );
  if (!found) expect.fail(`Missing button ${text}`);
  return found;
}

it('keeps a confirmed revocation when refreshing its standing fails', async () => {
  const permitted: api.MCPExecutablePermission = {
    name: 'native-tools',
    digest: 'a'.repeat(64),
    size_bytes: 1024,
    permitted: true
  };
  vi.mocked(api.listMCPExecutables).mockResolvedValue([permitted]);
  vi.mocked(api.reviewMCPExecutable)
    .mockResolvedValueOnce(permitted)
    .mockRejectedValueOnce(new Error('status is unavailable'))
    .mockResolvedValueOnce({ ...permitted, permitted: false });
  vi.mocked(api.revokeMCPExecutable).mockResolvedValue();
  const host = document.createElement('div');
  document.body.append(host);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  const component = mount(MCPExecutablesProbe, {
    target: host,
    props: { client }
  });
  onTestFinished(async () => {
    await unmount(component);
    client.clear();
    host.remove();
    vi.restoreAllMocks();
  });
  await vi.waitFor(() => {
    flushSync();
    expect(host.textContent).toContain('native-tools');
  });
  button(host, 'Review MCP executable').click();
  await vi.waitFor(() => {
    flushSync();
    expect(host.textContent).toContain('This digest is permitted');
  });
  button(host, 'Revoke MCP permission').click();
  await vi.waitFor(() => {
    flushSync();
    expect(host.textContent).toContain('MCP permission was revoked');
    expect(host.textContent).toContain('status is unavailable');
  });
  expect(host.textContent).not.toContain('This digest is permitted');
  expect(api.revokeMCPExecutable).toHaveBeenCalledTimes(1);
  button(host, 'Retry status refresh').click();
  await vi.waitFor(() => {
    flushSync();
    expect(api.reviewMCPExecutable).toHaveBeenCalledTimes(3);
  });
  expect(api.revokeMCPExecutable).toHaveBeenCalledTimes(1);
  expect(host.textContent).not.toContain('This digest is permitted');
});
