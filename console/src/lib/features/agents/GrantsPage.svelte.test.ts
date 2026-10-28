import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { beforeEach, expect, it, onTestFinished, vi } from 'vitest';
import * as api from './api';
import GrantsProbe from './test/GrantsProbe.svelte';

vi.mock('./api', async (original) => ({
  ...(await original<typeof import('./api')>()),
  listMCPGrants: vi.fn(),
  listServers: vi.fn(),
  getServer: vi.fn(),
  saveServer: vi.fn(),
  startMCPGrant: vi.fn(),
  claimMCPGrant: vi.fn(),
  authorizeMCPGrant: vi.fn(),
  pollMCPGrant: vi.fn()
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
  vi.mocked(api.listMCPGrants).mockResolvedValue([]);
  vi.mocked(api.getServer).mockImplementation(async (id) =>
    id === a.id ? a : b
  );
});

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
  host
    .querySelector(`option[value="${selected.id}"]`)
    ?.setAttribute('selected', '');
  host
    .querySelector('select')
    ?.dispatchEvent(new Event('change', { bubbles: true }));
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
