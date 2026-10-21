// @vitest-environment jsdom
import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import * as api from '$lib/api/code-mode';
import * as clipboard from '$lib/clipboard';
import ResearchConfigurationProbe from './test/ResearchConfigurationProbe.svelte';

vi.mock('$lib/api/code-mode', async (original) => ({
  ...(await original<typeof import('$lib/api/code-mode')>()),
  listCodeAccounts: vi.fn(),
  listCodePools: vi.fn(),
  listCodeRoutes: vi.fn(),
  saveCodeRoute: vi.fn()
}));

const projectId = '01980000-0000-7000-8000-000000000001';
const accountId = '01980000-0000-7000-8000-000000000010';
const poolId = '01980000-0000-7000-8000-000000000020';
const route: api.CodeRoute = {
  id: '01980000-0000-7000-8000-000000000030',
  etag: '01980000-0000-7000-8000-000000000031',
  project_id: projectId,
  slug: 'team-code',
  pool_id: poolId,
  enabled: true,
  models: ['gpt-5.5'],
  revision_id: '01980000-0000-7000-8000-000000000032',
  revision: 1,
  published_at: '2026-10-21T10:00:00Z',
  adapters: ['codex']
};

let host: HTMLDivElement;
let client: QueryClient;
let component: ReturnType<typeof mount>;

beforeEach(() => {
  vi.clearAllMocks();
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.mocked(api.listCodeRoutes).mockResolvedValue({
    items: [route],
    nextCursor: null
  });
  vi.mocked(api.listCodePools).mockResolvedValue({
    items: [
      {
        id: poolId,
        project_id: projectId,
        name: 'Research pool',
        kind: 'shared',
        owner_user_id: null,
        account_ids: [accountId],
        api_key_ids: [],
        etag: route.etag
      }
    ],
    nextCursor: null
  });
  vi.mocked(api.listCodeAccounts).mockResolvedValue({
    items: [
      {
        id: accountId,
        project_id: projectId,
        provider_id: accountId,
        credential_id: accountId,
        principal: 'fixture',
        name: 'Research subscription',
        adapter: 'zai_coding',
        enabled: true,
        eligible: true,
        research_services: ['search', 'reader', 'zread'],
        models: ['glm-5.3'],
        health: 'unknown',
        grant_state: 'current',
        allowance: null,
        etag: route.etag
      }
    ],
    nextCursor: null
  });
});

afterEach(async () => {
  await unmount(component);
  client.clear();
  host.remove();
  vi.restoreAllMocks();
});

async function settle() {
  for (let i = 0; i < 5; i++)
    await new Promise((resolve) => setTimeout(resolve, 0));
  flushSync();
}

async function click(name: string) {
  const button = [...host.querySelectorAll('button')].find(
    (button) => button.textContent?.trim() === name
  );
  expect(button).toBeDefined();
  button!.click();
  flushSync();
  await settle();
}

it('loads pool accounts, preselects the sole eligible research account and saves services independently of models', async () => {
  component = mount(ResearchConfigurationProbe, {
    target: host,
    props: { client, route }
  });
  flushSync();
  await settle();
  await click('Edit draft');
  expect(api.listCodeAccounts).toHaveBeenCalledWith(
    expect.objectContaining({ project_id: projectId }),
    expect.any(AbortSignal)
  );
  host.querySelector<HTMLInputElement>('#code-research-enabled')!.click();
  flushSync();
  await settle();
  expect(
    host.querySelector<HTMLSelectElement>('#code-research-account')!.value
  ).toBe(accountId);
  host.querySelector<HTMLInputElement>('input[value="reader"]')!.click();
  flushSync();
  vi.mocked(api.saveCodeRoute).mockResolvedValue(route);
  host
    .querySelector('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await settle();
  expect(api.saveCodeRoute).toHaveBeenCalledWith(
    expect.objectContaining({
      models: route.models,
      tools: { account_id: accountId, services: ['search', 'zread'] }
    }),
    route,
    expect.any(String)
  );
});

it('shows and copies every companion configuration file', async () => {
  const companion = '{"mcpServers":{"olp_reader":{"type":"http"}}}';
  const load = vi.fn(async (): Promise<api.CodeClientConfiguration> => ({
    route_slug: route.slug,
    base_url: 'https://olp.example/code/team-code',
    native_models: ['minimax-m3'],
    adapters: ['opencode_go'],
    client: 'claude-code',
    supported_clients: ['claude-code'],
    client_version: 'latest',
    model: 'minimax-m3',
    plan_model: 'minimax-m3',
    small_model: 'minimax-m3',
    format: 'shell',
    file: null,
    configuration: 'fixture shell setup',
    qualification_gaps: [],
    additional_files: [
      { file: '.mcp.json', format: 'json', configuration: companion }
    ]
  }));
  vi.spyOn(clipboard, 'copyText').mockResolvedValue(true);
  component = mount(ResearchConfigurationProbe, {
    target: host,
    props: { client, route, load }
  });
  flushSync();
  await settle();
  expect(
    host.querySelector<HTMLTextAreaElement>(
      `#code-client-configuration-${route.id}-additional-0`
    )!.value
  ).toBe(companion);
  await click('Copy .mcp.json');
  expect(clipboard.copyText).toHaveBeenCalledWith(companion);
  expect(host.textContent).toContain('.mcp.json copied.');
  expect(host.textContent?.replace(/\s+/g, ' ')).toContain(
    'preserving other tools and settings'
  );
});
