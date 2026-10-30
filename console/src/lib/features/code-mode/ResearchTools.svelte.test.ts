// @vitest-environment jsdom
import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import * as api from '$lib/api/code-mode';
import * as clipboard from '$lib/clipboard';
import { formatDate } from '$lib/format';
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
  messages_to_codex: false,
  models: ['gpt-5.5'],
  revision_id: '01980000-0000-7000-8000-000000000032',
  revision: 1,
  published_at: '2026-10-21T10:00:00Z',
  adapters: ['codex']
};

let host: HTMLDivElement;
let client: QueryClient;
let component: ReturnType<typeof mount>;
let researchAccount: api.CodeAccount;

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
        account_weights: { [accountId]: 1 },
        api_key_ids: [],
        etag: route.etag
      }
    ],
    nextCursor: null
  });
  researchAccount = {
    id: accountId,
    project_id: projectId,
    provider_id: accountId,
    credential_id: accountId,
    principal: 'fixture',
    name: 'Research subscription',
    adapter: 'zai_coding',
    enabled: true,
    eligible: true,
    research_eligible: true,
    research_services: ['search', 'reader', 'zread'],
    models: ['glm-5.3'],
    health: 'unknown',
    grant_state: 'current',
    allowance: null,
    etag: route.etag
  };
  vi.mocked(api.listCodeAccounts).mockResolvedValue({
    items: [researchAccount],
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

it.each([true, false])(
  'saves research independently of inference eligibility (%s)',
  async (inferenceEligible) => {
    researchAccount.eligible = inferenceEligible;
    researchAccount.health = inferenceEligible ? 'unknown' : 'unavailable';
    researchAccount.allowance = inferenceEligible
      ? null
      : {
          remaining_tokens: 0,
          remaining_requests: null,
          remaining_percent: null,
          resets_at: null,
          observed_at: '2026-10-21T10:00:00Z'
        };
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
  }
);

it('shows and copies every companion configuration file', async () => {
  const companion = '{"mcpServers":{"olp_reader":{"type":"http"}}}';
  const load = vi.fn(async (): Promise<api.CodeClientConfiguration> => ({
    route_slug: route.slug,
    base_url: 'https://olp.example/code/team-code',
    native_models: ['minimax-m3'],
    models: [
      {
        id: 'minimax-m3',
        object: 'model',
        capabilities: [
          { protocol: 'messages', path: 'v1/messages', transports: ['http'] }
        ],
        clients: ['claude-code']
      }
    ],
    adapters: ['opencode_go'],
    client: 'claude-code',
    platform: 'linux',
    instructions: 'Merge companion MCP servers into the project configuration',
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

it('keeps platform across client changes and models across platform changes', async () => {
  const load = vi.fn(
    async (
      _route: api.CodeRoute,
      selection: api.CodeClientSelection
    ): Promise<api.CodeClientConfiguration> => ({
      route_slug: route.slug,
      base_url: 'https://olp.example/code/team-code',
      native_models: ['main-model', 'other-model'],
      models: ['main-model', 'other-model'].map(
        (id): api.CodeClientConfiguration['models'][number] => ({
          id,
          object: 'model',
          capabilities: [
            { protocol: 'messages', path: 'v1/messages', transports: ['http'] }
          ],
          clients: ['claude-code', 'codex', 'opencode']
        })
      ),
      adapters: ['opencode_go'],
      client: selection.client ?? 'claude-code',
      platform: selection.platform ?? 'linux',
      instructions: `Merge setup for ${selection.platform ?? 'linux'}`,
      supported_clients: ['claude-code', 'codex', 'opencode'],
      client_version: 'fixture',
      model: selection.model ?? 'main-model',
      plan_model: null,
      small_model: null,
      format: selection.platform === 'windows' ? 'powershell' : 'shell',
      file:
        selection.platform === 'windows' ? 'olp-claude.ps1' : 'olp-claude.sh',
      configuration: `${selection.client ?? 'claude-code'} on ${selection.platform ?? 'linux'}`,
      qualification_gaps: []
    })
  );
  component = mount(ResearchConfigurationProbe, {
    target: host,
    props: { client, route, load }
  });
  flushSync();
  await settle();
  const model = host.querySelector<HTMLSelectElement>(
    `#code-client-model-${route.id}`
  )!;
  model.value = 'other-model';
  model.dispatchEvent(new Event('change', { bubbles: true }));
  await settle();
  const windows = host.querySelector<HTMLInputElement>(
    'input[value="windows"]'
  );
  expect(windows).not.toBeNull();
  windows!.click();
  await settle();
  expect(load).toHaveBeenLastCalledWith(
    route,
    { model: 'other-model', platform: 'windows' },
    expect.any(AbortSignal)
  );
  expect(host.textContent).toContain('Merge setup for windows');
  expect(
    host.querySelector<HTMLTextAreaElement>(
      `#code-client-configuration-${route.id}`
    )!.value
  ).toBe('claude-code on windows');
  host.querySelector<HTMLInputElement>('input[value="codex"]')!.click();
  await settle();
  expect(load).toHaveBeenLastCalledWith(
    route,
    { client: 'codex', platform: 'windows' },
    expect.any(AbortSignal)
  );
});

it('saves explicit Messages translation in the route draft', async () => {
  component = mount(ResearchConfigurationProbe, {
    target: host,
    props: { client, route }
  });
  flushSync();
  await settle();
  await click('Edit draft');
  const translation = host.querySelector<HTMLInputElement>(
    '#code-messages-to-codex'
  );
  expect(translation).not.toBeNull();
  expect(translation!.checked).toBe(false);
  translation!.click();
  flushSync();
  vi.mocked(api.saveCodeRoute).mockResolvedValue(route);
  host
    .querySelector('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await settle();
  expect(api.saveCodeRoute).toHaveBeenCalledWith(
    expect.objectContaining({
      messages_to_codex: true,
      models: route.models,
      tools: null
    }),
    route,
    expect.any(String)
  );
});

it.each([
  {
    health: 'quota_limited',
    grant: 'current',
    label: 'Provider rate or quota limit',
    retry: '2026-10-30T11:00:00Z'
  },
  {
    health: 'unavailable',
    grant: 'current',
    label: 'Temporary upstream failure',
    retry: '2026-10-30T11:00:00Z'
  },
  {
    health: 'unknown',
    grant: 'lapsed',
    label: 'Grant lapsed, enroll again',
    retry: null
  }
] as const)(
  'shows distinct account availability evidence for $health and $grant',
  async ({ health, grant, label, retry }) => {
    const observed = '2026-10-30T10:00:00Z';
    const account: api.CodeAccount = {
      ...researchAccount,
      health,
      grant_state: grant,
      eligible: false,
      health_observed_at: observed,
      unavailable_until: retry,
      allowance: {
        remaining_tokens: 0,
        remaining_requests: null,
        remaining_percent: null,
        resets_at: null,
        observed_at: observed
      }
    };
    vi.mocked(api.listCodeAccounts).mockResolvedValue({
      items: [account],
      nextCursor: null
    });
    component = mount(ResearchConfigurationProbe, {
      target: host,
      props: { client, route, kind: 'accounts' }
    });
    flushSync();
    await settle();
    const card = host.querySelector('article')!;
    expect(card.textContent).toContain(label);
    expect(card.textContent).toContain('Tokens: 0');
    expect(card.textContent).toContain('Reset Unknown');
    expect(card.textContent).toContain(formatDate(observed));
    expect(card.textContent).toContain(
      retry ? formatDate(retry) : 'No serving cooldown reported'
    );
    expect(host.textContent).toContain('A rejected refresh lapses the grant');
  }
);
