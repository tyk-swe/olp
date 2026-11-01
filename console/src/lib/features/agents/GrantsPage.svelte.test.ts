import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { beforeEach, expect, it, onTestFinished, vi } from 'vitest';
import * as api from './api';
import * as keys from '$lib/features/access/api-keys/api';
import * as profile from '$lib/features/access/profile/api';
import GrantsProbe from './test/GrantsProbe.svelte';

vi.mock('./api', async (original) => ({
  ...(await original<typeof import('./api')>()),
  listMCPGrants: vi.fn(),
  listServers: vi.fn(),
  listPublications: vi.fn(),
  getServer: vi.fn(),
  saveServer: vi.fn(),
  startMCPGrant: vi.fn(),
  startAgentGrant: vi.fn(),
  claimMCPGrant: vi.fn(),
  authorizeMCPGrant: vi.fn(),
  pollMCPGrant: vi.fn(),
  createDelegationConsent: vi.fn(),
  getDelegationOptions: vi.fn()
}));
vi.mock('$lib/features/access/api-keys/api', () => ({ listApiKeys: vi.fn() }));
vi.mock('$lib/features/access/profile/api', () => ({
  getProfile: vi.fn(),
  reauthenticateWithPassword: vi.fn(),
  beginIdentityReauthentication: vi.fn()
}));
vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({ allows: () => true })
}));

const server = (id: string): api.MCPServer => ({
  id,
  project_id: 'project',
  name: id,
  transport: 'streamable_http',
  endpoint: `https://${id}.example.test/mcp`,
  enabled: true,
  has_credential: false,
  etag: `${id}-etag`,
  revision_id: `${id}-revision`,
  revision: 1,
  certified_at: '2026-10-22T00:00:00Z',
  retired_at: null,
  unit_price: null,
  tool_prices: {},
  calls_per_minute: 60,
  max_concurrency: 8,
  per_key_calls_per_minute: 60,
  per_key_max_concurrency: 8,
  catalog: {
    protocol_version: '2025-11-25',
    digest: 'a'.repeat(64),
    tools: []
  },
  delegation: null,
  oauth: {
    client_id: `${id}-client`,
    audience: `${id}-audience`,
    scopes: ['tools.read'],
    authorization_endpoint: `https://${id}.example.test/authorize`,
    token_endpoint: `https://${id}.example.test/token`
  }
});
const a = server('server-a');
const b = server('server-b');

beforeEach(() => {
  vi.clearAllMocks();
  window.history.replaceState(null, '', '/agents/grants');
  vi.mocked(api.listServers).mockResolvedValue([a, b]);
  vi.mocked(api.listPublications).mockResolvedValue([]);
  vi.mocked(api.listMCPGrants).mockResolvedValue([]);
  vi.mocked(keys.listApiKeys).mockResolvedValue([]);
  vi.mocked(api.getDelegationOptions).mockResolvedValue({ agents: [] });
  vi.mocked(profile.getProfile).mockResolvedValue({
    id: 'member',
    email: 'member@example.test',
    display_name: 'Member',
    role: 'developer',
    access_scope: 'assigned',
    active: true,
    etag: 'etag',
    created_at: '2026-01-01',
    updated_at: '2026-01-01',
    projects: [{ id: 'project', name: 'Project', role: 'manager' }]
  });
  vi.mocked(api.getServer).mockImplementation(async (id) =>
    id === a.id ? a : b
  );
});

it('starts agent enrollment for the selected published agent and key', async () => {
  vi.mocked(api.listPublications).mockResolvedValue([
    {
      id: 'agent',
      project_id: 'project',
      kind: 'agent',
      name: 'Agent',
      etag: 'etag',
      revision_id: 'revision',
      retired: false
    },
    {
      id: 'draft',
      project_id: 'project',
      kind: 'agent',
      name: 'Draft',
      etag: 'etag',
      revision_id: null,
      retired: false
    }
  ]);
  vi.mocked(api.startAgentGrant).mockResolvedValue({
    enrollment_id: 'enrollment',
    enrollment_url: '/agents/grants#enrollment.private',
    expires_at: '2030-01-01T00:00:00Z'
  });
  const { host } = page();
  await vi.waitFor(() =>
    expect(host.querySelector('option[value="agent"]')).not.toBeNull()
  );
  expect(host.querySelector('option[value="draft"]')).toBeNull();
  const select = host.querySelector<HTMLSelectElement>('option[value="agent"]')!
    .parentElement as HTMLSelectElement;
  select.value = 'agent';
  select.dispatchEvent(new Event('change', { bubbles: true }));
  fill(host, 'Agent API key UUID', 'agent-key');
  button(host, 'Start agent key enrollment').click();
  await vi.waitFor(() =>
    expect(api.startAgentGrant).toHaveBeenCalledWith('agent', 'agent-key')
  );
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Continue personal enrollment')
  );
});

function consentFixtures() {
  const budgetWindow = {
    accrued: '0',
    limit: null,
    window_ends_at: '2030-01-01'
  };
  const budget = {
    daily: budgetWindow,
    monthly: budgetWindow,
    unpriced_attempts: 0
  };
  const key: keys.ApiKey = {
    id: 'consumer',
    lookup_id: 'consumer',
    name: 'Member key',
    project_id: 'project',
    project_name: 'Project',
    budget_group_id: null,
    created_by: 'member',
    created_by_email: 'member@example.test',
    scopes: ['inference', 'delegate'],
    allowed_routes: ['chat'],
    allow_provider_state: false,
    budget,
    etag: 'key-etag',
    created_at: '2026-01-01',
    allowed_attribution_keys: [],
    response_metadata: false,
    regional_limits: {}
  };
  const agent: keys.ApiKey = {
    ...key,
    id: 'agent',
    name: 'Workload agent',
    scopes: ['inference'],
    workload_issuer_id: 'issuer',
    workload_mapping: 'agent'
  };
  const foreign: keys.ApiKey = { ...agent, id: 'foreign', project_id: 'other' };
  const ordinary: keys.ApiKey = {
    ...agent,
    id: 'ordinary',
    name: 'Ordinary workload'
  };
  return { key, agent, foreign, ordinary };
}

it('shows the exact consent and delivers its one-use code after password verification', async () => {
  const { key, agent, foreign, ordinary } = consentFixtures();
  vi.mocked(keys.listApiKeys).mockResolvedValue([
    key,
    agent,
    foreign,
    ordinary
  ]);
  vi.mocked(api.getDelegationOptions).mockResolvedValue({
    agents: [{ id: 'agent', allowed_routes: ['chat'] }]
  });
  vi.mocked(api.createDelegationConsent).mockImplementation(async (input) => {
    if (
      input.agent_id !== 'agent' ||
      input.api_key_id !== 'consumer' ||
      input.allowed_routes?.join(',') !== 'chat'
    )
      throw new Error('incorrect consent');
    return {
      consent: 'olpc_one_use',
      expires_at: '2030-01-01T00:00:00Z',
      delegation_pair_digest: 'pair-budget-id'
    };
  });
  const { host } = page();
  await vi.waitFor(() => {
    flushSync();
    expect(host.querySelector('option[value="consumer"]')).not.toBeNull();
  });
  const keySelect = host.querySelector<HTMLSelectElement>('#delegation-key');
  if (!keySelect) expect.fail('Missing key selection');
  keySelect.value = 'consumer';
  keySelect.dispatchEvent(new Event('change', { bubbles: true }));
  await vi.waitFor(() => {
    flushSync();
    expect(host.querySelector('option[value="agent"]')).not.toBeNull();
  });
  expect(host.querySelector('option[value="foreign"]')).toBeNull();
  expect(host.querySelector('option[value="ordinary"]')).toBeNull();
  const agentSelect =
    host.querySelector<HTMLSelectElement>('#delegation-agent');
  if (!agentSelect) expect.fail('Missing agent selection');
  agentSelect.value = 'agent';
  agentSelect.dispatchEvent(new Event('change', { bubbles: true }));
  fill(host, 'Allowed inference routes', 'unapproved');
  expect(button(host, 'Confirm agent access').disabled).toBe(true);
  expect(host.textContent).toContain('chat');
  fill(host, 'Allowed inference routes', 'chat');
  fill(host, 'Current password', 'password');
  expect(host.textContent).toContain('Member key');
  expect(host.textContent).toContain('chat');
  vi.mocked(profile.reauthenticateWithPassword).mockRejectedValueOnce(
    new Error('Verification failed')
  );
  button(host, 'Confirm agent access').click();
  await vi.waitFor(() => {
    flushSync();
    expect(host.textContent).toContain('Verification failed');
  });
  expect(host.textContent).not.toContain('Copy this one-use consent code');
  button(host, 'Confirm agent access').click();
  await vi.waitFor(() => {
    flushSync();
    expect(textInput(host, 'Consent code').value).toBe('olpc_one_use');
    expect(textInput(host, 'Pair budget ID').value).toBe('pair-budget-id');
  });
});

it('allows consent to an eligible toolset without granting inference', async () => {
  const { key, agent } = consentFixtures();
  vi.mocked(keys.listApiKeys).mockResolvedValue([key, agent]);
  vi.mocked(api.getDelegationOptions).mockResolvedValue({
    agents: [{ id: 'agent', allowed_routes: [], allowed_toolsets: ['toolset'] }]
  });
  vi.mocked(api.createDelegationConsent).mockResolvedValue({
    consent: 'olpc_one_use',
    expires_at: '2030-01-01T00:00:00Z',
    delegation_pair_digest: 'pair-budget-id'
  });
  const { host } = page();
  await vi.waitFor(() => {
    flushSync();
    expect(host.querySelector('option[value="consumer"]')).not.toBeNull();
  });
  const keySelect = host.querySelector<HTMLSelectElement>('#delegation-key');
  if (!keySelect) expect.fail('Missing key selection');
  keySelect.value = 'consumer';
  keySelect.dispatchEvent(new Event('change', { bubbles: true }));
  await vi.waitFor(() => {
    flushSync();
    expect(host.querySelector('option[value="agent"]')).not.toBeNull();
  });
  const agentSelect =
    host.querySelector<HTMLSelectElement>('#delegation-agent');
  if (!agentSelect) expect.fail('Missing agent selection');
  agentSelect.value = 'agent';
  agentSelect.dispatchEvent(new Event('change', { bubbles: true }));
  fill(host, 'Allowed toolsets', 'other');
  expect(button(host, 'Confirm agent access').disabled).toBe(true);
  fill(host, 'Allowed toolsets', 'toolset');
  fill(host, 'Current password', 'password');
  button(host, 'Confirm agent access').click();
  await vi.waitFor(() => {
    flushSync();
    expect(api.createDelegationConsent).toHaveBeenCalledWith({
      api_key_id: 'consumer',
      agent_id: 'agent',
      scopes: ['tools'],
      allowed_toolsets: ['toolset']
    });
  });
});

it('allows consent only to eligible published agents and explains remote visibility', async () => {
  const { key, agent } = consentFixtures();
  const publication = '88a13706-6e96-4b19-9e07-3d1ad0a600ba';
  vi.mocked(keys.listApiKeys).mockResolvedValue([key, agent]);
  vi.mocked(api.getDelegationOptions).mockResolvedValue({
    agents: [{ id: 'agent', allowed_routes: [], allowed_agents: [publication] }]
  });
  vi.mocked(api.createDelegationConsent).mockImplementation(async (input) => {
    if (
      input.scopes?.join(',') !== 'agents' ||
      input.allowed_agents?.join(',') !== publication
    ) {
      throw new Error('incorrect consent');
    }
    return {
      consent: 'olpc_one_use',
      expires_at: '2030-01-01T00:00:00Z',
      delegation_pair_digest: 'pair-budget-id'
    };
  });
  const { host } = page();
  await vi.waitFor(() => {
    flushSync();
    expect(host.querySelector('option[value="consumer"]')).not.toBeNull();
  });
  const keySelect = host.querySelector<HTMLSelectElement>('#delegation-key');
  if (!keySelect) expect.fail('Missing key selection');
  keySelect.value = 'consumer';
  keySelect.dispatchEvent(new Event('change', { bubbles: true }));
  await vi.waitFor(() => {
    flushSync();
    expect(host.querySelector('option[value="agent"]')).not.toBeNull();
  });
  const agentSelect =
    host.querySelector<HTMLSelectElement>('#delegation-agent');
  if (!agentSelect) expect.fail('Missing agent selection');
  agentSelect.value = 'agent';
  agentSelect.dispatchEvent(new Event('change', { bubbles: true }));
  fill(host, 'Allowed A2A agents', 'other');
  expect(button(host, 'Confirm agent access').disabled).toBe(true);
  fill(host, 'Allowed A2A agents', publication);
  fill(host, 'Current password', 'password');
  expect(host.textContent).toContain(
    'cannot count or stop hidden remote iterations'
  );
  button(host, 'Confirm agent access').click();
  await vi.waitFor(() => {
    flushSync();
    expect(textInput(host, 'Consent code').value).toBe('olpc_one_use');
  });
});

it('starts linked identity verification for the selected key', async () => {
  const { key } = consentFixtures();
  vi.mocked(keys.listApiKeys).mockResolvedValue([key]);
  vi.mocked(profile.beginIdentityReauthentication).mockReturnValue(
    new Promise<string>(() => {})
  );
  const { host } = page();
  await vi.waitFor(() => {
    flushSync();
    expect(host.querySelector('option[value="consumer"]')).not.toBeNull();
  });
  const keySelect = host.querySelector<HTMLSelectElement>('#delegation-key');
  if (!keySelect) expect.fail('Missing key selection');
  keySelect.value = 'consumer';
  keySelect.dispatchEvent(new Event('change', { bubbles: true }));
  flushSync();
  button(host, 'Verify with linked identity').click();
  expect(profile.beginIdentityReauthentication).toHaveBeenCalledWith(
    'agent_delegation',
    'consumer'
  );
});

it.each(['OIDC', 'SAML'])(
  'resumes consent after %s verification for the selected key',
  async () => {
    window.history.replaceState(
      null,
      '',
      '/agents/grants?reauthenticated=agent_delegation&resource_id=consumer'
    );
    const { key, agent } = consentFixtures();
    vi.mocked(keys.listApiKeys).mockResolvedValue([key, agent]);
    vi.mocked(api.getDelegationOptions).mockResolvedValue({
      agents: [{ id: 'agent', allowed_routes: ['chat'] }]
    });
    vi.mocked(api.createDelegationConsent).mockImplementation(async (input) => {
      if (
        input.agent_id !== 'agent' ||
        input.api_key_id !== 'consumer' ||
        input.allowed_routes?.join(',') !== 'chat'
      )
        throw new Error('incorrect consent');
      return {
        consent: 'olpc_one_use',
        expires_at: '2030-01-01T00:00:00Z',
        delegation_pair_digest: 'pair-budget-id'
      };
    });
    const { host } = page();
    await vi.waitFor(() => {
      flushSync();
      expect(host.querySelector('option[value="agent"]')).not.toBeNull();
    });
    expect(host.textContent).toContain('Identity verified.');
    const keySelect = host.querySelector<HTMLSelectElement>('#delegation-key');
    expect(keySelect?.value).toBe('consumer');
    const agentSelect =
      host.querySelector<HTMLSelectElement>('#delegation-agent');
    if (!agentSelect) expect.fail('Missing agent selection');
    agentSelect.value = 'agent';
    agentSelect.dispatchEvent(new Event('change', { bubbles: true }));
    fill(host, 'Allowed inference routes', 'chat');
    button(host, 'Confirm agent access').click();
    await vi.waitFor(() => {
      flushSync();
      expect(textInput(host, 'Consent code').value).toBe('olpc_one_use');
    });
    expect(profile.reauthenticateWithPassword).not.toHaveBeenCalled();
  }
);

function page() {
  const host = document.createElement('div');
  document.body.append(host);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  const component = mount(GrantsProbe, { target: host, props: { client } });
  onTestFinished(async () => {
    await unmount(component);
    client.clear();
    host.remove();
    vi.restoreAllMocks();
  });
  return { host, client };
}
function button(host: HTMLElement, label: string): HTMLButtonElement {
  const found = [...host.querySelectorAll('button')].find(
    (item) => item.textContent?.trim() === label
  );
  if (!found) expect.fail(`Missing button ${label}`);
  return found;
}
function textInput(host: HTMLElement, label: string): HTMLInputElement {
  const found = [...host.querySelectorAll('label')]
    .find((item) => item.textContent?.trim() === label)
    ?.querySelector('input');
  if (!found) expect.fail(`Missing input ${label}`);
  return found;
}
function fill(host: HTMLElement, label: string, value: string) {
  const input = textInput(host, label);
  input.setRangeText(value, 0, input.value.length);
  input.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
}
async function choose(host: HTMLElement, selected: api.MCPServer) {
  await vi.waitFor(() => {
    flushSync();
    expect(host.querySelector(`option[value="${selected.id}"]`)).not.toBeNull();
  });
  const option = host.querySelector(`option[value="${selected.id}"]`);
  const selector = option?.parentElement;
  if (!(selector instanceof HTMLSelectElement))
    expect.fail('Missing server selection');
  selector.value = selected.id;
  selector.dispatchEvent(new Event('change', { bubbles: true }));
  await vi.waitFor(() => {
    flushSync();
    expect(textInput(host, 'Client ID').value).toBe(selected.oauth?.client_id);
  });
}

it('keeps the selected server when a previous server save completes late', async () => {
  const pending = Promise.withResolvers<api.MCPServer>();
  vi.mocked(api.saveServer)
    .mockReturnValueOnce(pending.promise)
    .mockResolvedValueOnce(b);
  const { host } = page();
  await choose(host, a);
  button(host, 'Save authorization settings').click();
  await choose(host, b);
  pending.resolve({ ...a, etag: 'saved-a' });
  await pending.promise;
  await vi.waitFor(() => {
    flushSync();
    expect(button(host, 'Save authorization settings').disabled).toBe(false);
  });
  button(host, 'Save authorization settings').click();
  await vi.waitFor(() => {
    expect(vi.mocked(api.saveServer).mock.calls.at(-1)?.[0].id).toBe(b.id);
    expect(
      vi.mocked(api.saveServer).mock.calls.at(-1)?.[1].oauth?.client_id
    ).toBe(b.oauth?.client_id);
  });
});

it('retains ambiguous save identity and treats refresh failure as a saved write', async () => {
  const saved = { ...a, etag: 'saved-a' };
  vi.mocked(api.saveServer)
    .mockRejectedValueOnce(new Error('Connection closed'))
    .mockResolvedValue(saved);
  const { host, client } = page();
  await choose(host, a);
  fill(host, 'Client ID', 'changed-client');
  fill(host, 'Delegated issuer', 'https://fixture.invalid/issuer');
  fill(host, 'Delegated audience', 'fixture-tools');
  const signingKey = host.querySelector('textarea');
  expect(signingKey).not.toBeNull();
  if (!signingKey) return;
  signingKey.value = 'fixture-private-key';
  signingKey.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  button(host, 'Save authorization settings').click();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Connection closed')
  );
  const original = vi.mocked(api.saveServer).mock.calls[0];
  vi.spyOn(client, 'invalidateQueries').mockRejectedValueOnce(
    new Error('Refresh offline')
  );
  button(host, 'Save authorization settings').click();
  await vi.waitFor(() => {
    flushSync();
    expect(host.textContent).toContain(
      'Personal authorization configuration saved'
    );
    expect(host.textContent).toContain('Write saved, but refreshing failed');
  });
  const retry = vi.mocked(api.saveServer).mock.calls[1];
  expect(retry).toEqual(original);
  expect(original?.[1].delegation_private_key).toBe('fixture-private-key');
  expect(signingKey.value).toBe('');
  button(host, 'Save authorization settings').click();
  await vi.waitFor(() => {
    const next = vi.mocked(api.saveServer).mock.calls.at(-1);
    expect(next?.[0].etag).toBe(saved.etag);
    expect(next?.[1].delegation_private_key).toBeUndefined();
    expect(next?.[2]).not.toBe(original?.[2]);
  });
});

it('keeps an unsent signing key when an earlier key save completes', async () => {
  const pending = Promise.withResolvers<api.MCPServer>();
  vi.mocked(api.saveServer)
    .mockReturnValueOnce(pending.promise)
    .mockResolvedValue(a);
  const { host } = page();
  await choose(host, a);
  fill(host, 'Delegated issuer', 'https://fixture.invalid/issuer');
  fill(host, 'Delegated audience', 'fixture-tools');
  const signingKey = host.querySelector('textarea');
  if (!signingKey) expect.fail('Missing signing key input');
  const setKey = (value: string) => {
    signingKey.value = value;
    signingKey.dispatchEvent(new Event('input', { bubbles: true }));
    flushSync();
  };
  setKey('submitted-private-key');
  button(host, 'Save authorization settings').click();
  await vi.waitFor(() => expect(api.saveServer).toHaveBeenCalled());
  setKey('unsent-private-key');
  pending.resolve({ ...a, etag: 'saved-a' });
  await vi.waitFor(() => {
    flushSync();
    expect(button(host, 'Save authorization settings').disabled).toBe(false);
    expect(signingKey.value).toBe('unsent-private-key');
  });
  button(host, 'Save authorization settings').click();
  await vi.waitFor(() => {
    expect(
      vi.mocked(api.saveServer).mock.calls.at(-1)?.[1].delegation_private_key
    ).toBe('unsent-private-key');
  });
  const submitted = vi.mocked(api.saveServer).mock.calls;
  expect(submitted[0]?.[1].delegation_private_key).toBe(
    'submitted-private-key'
  );
  expect(submitted[1]?.[0].etag).toBe('saved-a');
  expect(submitted[1]?.[2]).not.toBe(submitted[0]?.[2]);
});

it('starts, continues and claims a key enrollment without leaving the page', async () => {
  const enrollmentID = '0192cf87-d4ab-7f2e-a8b1-c2d3e4f50701';
  const token = 'private-handoff';
  vi.mocked(api.startMCPGrant).mockResolvedValue({
    enrollment_id: enrollmentID,
    enrollment_url: `/agents/grants#${enrollmentID}.${token}`,
    expires_at: '2026-10-22T00:10:00Z'
  });
  vi.mocked(api.claimMCPGrant).mockResolvedValue({
    state: 'pending',
    enrollment: {
      id: enrollmentID,
      expires_at: '2026-10-22T00:10:00Z',
      claimed_at: null,
      binding: {
        server_id: a.id,
        project_id: a.project_id,
        endpoint: a.endpoint,
        oauth: a.oauth,
        identity: { kind: 'api_key', id: 'key' }
      }
    }
  });
  const { host } = page();
  await choose(host, a);
  fill(host, 'API key UUID', 'key');
  button(host, 'Start key enrollment').click();
  await vi.waitFor(() =>
    expect(host.querySelector('a')?.textContent).toBe(
      'Continue personal enrollment'
    )
  );
  host.querySelector('a')?.click();
  await vi.waitFor(() => {
    flushSync();
    expect(button(host, 'Claim enrollment').disabled).toBe(false);
  });
  expect(window.location.hash).toBe('');
  button(host, 'Claim enrollment').click();
  await vi.waitFor(() => {
    flushSync();
    expect(host.textContent).toContain(
      'Enrollment claimed. Authorization is pending'
    );
    expect(vi.mocked(api.claimMCPGrant).mock.calls.at(-1)).toEqual([
      enrollmentID,
      token
    ]);
  });
  expect(
    [...host.querySelectorAll('button')].some(
      (item) => item.textContent === 'Claim enrollment'
    )
  ).toBe(false);
});

it('completes the upstream callback for its enrollment without retaining callback secrets in the URL', async () => {
  window.history.replaceState(
    null,
    '',
    '/agents/grants?code=fixture-code&state=enrollment.fixture-state'
  );
  const { host } = page();
  await vi.waitFor(() => {
    flushSync();
    expect(host.textContent).toContain('Complete authorization');
  });
  expect(window.location.search).toBe('');
});

const claimedEnrollment = (
  id: string
): Awaited<ReturnType<typeof api.claimMCPGrant>> => ({
  state: 'pending',
  enrollment: {
    id,
    expires_at: '2026-10-22T00:10:00Z',
    claimed_at: null,
    binding: {
      server_id: a.id,
      project_id: a.project_id,
      endpoint: a.endpoint,
      oauth: a.oauth,
      identity: { kind: 'api_key', id: `key-${id}` }
    }
  }
});

async function enroll(host: HTMLElement, id: string) {
  vi.mocked(api.startMCPGrant).mockResolvedValueOnce({
    enrollment_id: id,
    enrollment_url: `/agents/grants#${id}.private-${id}`,
    expires_at: '2026-10-22T00:10:00Z'
  });
  vi.mocked(api.claimMCPGrant).mockResolvedValueOnce(claimedEnrollment(id));
  fill(host, 'API key UUID', `key-${id}`);
  button(host, 'Start key enrollment').click();
  await vi.waitFor(() => {
    flushSync();
    expect(
      host.querySelector(`a[href$="#${id}.private-${id}"]`)
    ).not.toBeNull();
  });
  host
    .querySelector<HTMLAnchorElement>(`a[href$="#${id}.private-${id}"]`)
    ?.click();
  await vi.waitFor(() => {
    flushSync();
    expect(button(host, 'Claim enrollment').disabled).toBe(false);
  });
  button(host, 'Claim enrollment').click();
  await vi.waitFor(() => {
    flushSync();
    expect(host.textContent).toContain(
      'Enrollment claimed. Authorization is pending'
    );
    expect(
      [...host.querySelectorAll('button')].some(
        (item) => item.textContent?.trim() === 'Claim enrollment'
      )
    ).toBe(false);
  });
}

const deviceAuthorization = (id: string): api.MCPGrantAuthorization => ({
  state: 'pending',
  device: {
    user_code: `${id}-code`,
    verification_url: `https://identity.example.test/${id}`,
    expires_at: '2026-10-22T00:10:00Z',
    interval: 5
  }
});

it.each(['active', 'pending', 'denied'] as const)(
  'starts a second enrollment after a device authorization is %s',
  async (previousState) => {
    vi.mocked(api.authorizeMCPGrant).mockImplementation(async (id) =>
      deviceAuthorization(id)
    );
    vi.mocked(api.pollMCPGrant).mockResolvedValueOnce({ state: previousState });
    const { host } = page();
    await choose(host, a);
    await enroll(host, 'first');
    button(host, 'Use device authorization').click();
    await vi.waitFor(() => {
      flushSync();
      expect(host.textContent).toContain('first-code');
    });
    button(host, 'Check device approval').click();
    await vi.waitFor(() => {
      flushSync();
      expect(host.textContent).toContain(
        `Personal authorization is ${previousState}`
      );
    });
    await enroll(host, 'second');
    expect(button(host, 'Authorize in browser').disabled).toBe(false);
    expect(button(host, 'Use device authorization').disabled).toBe(false);
    expect(host.textContent).not.toContain('first-code');
    button(host, 'Use device authorization').click();
    await vi.waitFor(() => {
      flushSync();
      expect(host.textContent).toContain('second-code');
      expect(
        host.querySelector('a[href="https://identity.example.test/second"]')
      ).not.toBeNull();
    });
  }
);

it('ignores a late device result after another enrollment is adopted', async () => {
  const pending = Promise.withResolvers<api.MCPGrantAuthorization>();
  vi.mocked(api.authorizeMCPGrant).mockImplementation(async (id) =>
    deviceAuthorization(id)
  );
  vi.mocked(api.pollMCPGrant).mockReturnValueOnce(pending.promise);
  const { host } = page();
  await choose(host, a);
  await enroll(host, 'first');
  button(host, 'Use device authorization').click();
  await vi.waitFor(() => {
    flushSync();
    expect(host.textContent).toContain('first-code');
  });
  button(host, 'Check device approval').click();
  await vi.waitFor(() => {
    flushSync();
    expect(button(host, 'Check device approval').disabled).toBe(true);
  });
  window.history.replaceState(null, '', '/agents/grants#second.private-second');
  window.dispatchEvent(new HashChangeEvent('hashchange'));
  await vi.waitFor(() => {
    flushSync();
    expect(button(host, 'Claim enrollment').disabled).toBe(false);
  });
  vi.mocked(api.claimMCPGrant).mockResolvedValueOnce(
    claimedEnrollment('second')
  );
  button(host, 'Claim enrollment').click();
  await vi.waitFor(() => {
    flushSync();
    expect(button(host, 'Use device authorization').disabled).toBe(false);
  });
  pending.resolve({ state: 'active' });
  await pending.promise;
  flushSync();
  expect(host.textContent).not.toContain('Personal authorization is active');
  expect(host.textContent).not.toContain('first-code');
  expect(button(host, 'Use device authorization').disabled).toBe(false);
});

it('saves Entra trust and clears a client secret without replaying it on later saves', async () => {
  const saved: api.MCPServer = {
    ...a,
    oauth: {
      client_id: 'middle-tier',
      audience: 'api://tools',
      scopes: ['api://tools/read'],
      token_endpoint: 'https://identity.example.test/token',
      entra_obo: { issuer: 'https://identity.example.test' }
    }
  };
  vi.mocked(api.saveServer).mockResolvedValue(saved);
  const { host } = page();
  await choose(host, a);
  fill(host, 'Trusted Entra issuer', 'https://identity.example.test');
  fill(host, 'Middle-tier client secret', 'fixture-client-secret');
  fill(host, 'Client ID', 'middle-tier');
  fill(host, 'Audience', 'api://tools');
  fill(host, 'Scopes', 'api://tools/read');
  fill(host, 'Token endpoint', 'https://identity.example.test/token');
  button(host, 'Save authorization settings').click();
  await vi.waitFor(() => {
    flushSync();
    expect(textInput(host, 'Middle-tier client secret').value).toBe('');
    expect(button(host, 'Save authorization settings').disabled).toBe(false);
  });
  expect(vi.mocked(api.saveServer).mock.calls[0]?.[1]).toMatchObject({
    entra_client_secret: 'fixture-client-secret',
    oauth: { entra_obo: { issuer: 'https://identity.example.test' } }
  });
  expect(button(host, 'Start key enrollment').disabled).toBe(true);
  button(host, 'Save authorization settings').click();
  await vi.waitFor(() => expect(api.saveServer).toHaveBeenCalledTimes(2));
  expect(vi.mocked(api.saveServer).mock.calls[1]?.[1]).not.toHaveProperty(
    'entra_client_secret'
  );
});
