// @vitest-environment jsdom
import { flushSync, mount, unmount, type ComponentProps } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { authLifecycle } from '$lib/features/access/session/lifecycle';
import { ApiProblem } from '$lib/api/http';
import { formatDate } from '$lib/format';
import * as clipboard from '$lib/clipboard';
import {
  listProjectMemberships,
  listAllProjectMembers
} from '$lib/features/access/projects/api';
import { listApiKeys } from '$lib/features/access/api-keys/api';
import {
  listProviders,
  getProvider,
  type Provider
} from '$lib/features/providers/api/providers';
import { listProviderCredentials } from '$lib/features/providers/api/credentials';
import { listProviderKinds } from '$lib/features/providers/api/models';
import {
  startGrantEnrollment,
  continueGrantEnrollment,
  pollGrantEnrollment
} from '$lib/features/providers/api/grants';
import { pluginSpec } from '$lib/features/providers/test/pluginFixtures';
import * as api from '$lib/api/code-mode';
import {
  account,
  pool,
  route,
  budget,
  attempt,
  binding,
  projectId,
  secondProjectId,
  keyId,
  userId
} from './test/fixtures';
import CodeModeProbe from './test/CodeModeProbe.svelte';
import ClientConfigurationsProbe from './test/ClientConfigurationsProbe.svelte';
import { codeKeys } from './codeKeys';

vi.mock('$lib/api/code-mode', async (original) => ({
  ...(await original<typeof import('$lib/api/code-mode')>()),
  listCodeAccounts: vi.fn(),
  listCodePools: vi.fn(),
  listCodeRoutes: vi.fn(),
  listCodeBudgets: vi.fn(),
  listCodeBindings: vi.fn(),
  listCodeAttempts: vi.fn(),
  listCodeRefusals: vi.fn(),
  listCodeTokenWindows: vi.fn(),
  listCodeRevisions: vi.fn(),
  saveCodeAccount: vi.fn(),
  saveCodePool: vi.fn(),
  saveCodeRoute: vi.fn(),
  saveCodeBudget: vi.fn(),
  publishCodeRoute: vi.fn(),
  retireCodeBinding: vi.fn()
}));
vi.mock('$lib/features/access/projects/api', () => ({
  listProjectMemberships: vi.fn(),
  listAllProjectMembers: vi.fn()
}));
vi.mock('$lib/features/access/api-keys/api', () => ({ listApiKeys: vi.fn() }));
vi.mock('$lib/features/providers/api/providers', () => ({
  listProviders: vi.fn(),
  getProvider: vi.fn()
}));
vi.mock('$lib/features/providers/api/credentials', () => ({
  listProviderCredentials: vi.fn()
}));
vi.mock('$lib/features/providers/api/models', () => ({
  listProviderKinds: vi.fn()
}));
vi.mock('$lib/features/providers/api/grants', () => ({
  startGrantEnrollment: vi.fn(),
  continueGrantEnrollment: vi.fn(),
  pollGrantEnrollment: vi.fn(),
  cancelGrantEnrollment: vi.fn()
}));

const provider: Provider = {
  id: account.provider_id,
  name: 'Subscription provider',
  project_id: projectId,
  project_name: 'First project',
  configuration: {
    kind: 'plugin',
    auth_mode: 'grant',
    profile_id: 'fixture-profile',
    profile_revision: 'a'.repeat(64),
    endpoint: 'https://provider.example/v1',
    options: {
      vendor_id: null,
      limits: null,
      credential_headers: [],
      parameter_defaults: {},
      models: {}
    }
  },
  state: 'draft',
  connector_ready: true,
  pending_activation: true,
  etag: account.etag,
  created_at: '2026-10-01T00:00:00Z',
  updated_at: '2026-10-01T00:00:00Z',
  model_count: 1,
  enabled_model_count: 1,
  capability_count: 1,
  certified_capability_count: 0,
  draft_credential_id: null
};
let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;
function session(
  operations: Array<'read' | 'configure'> = ['read', 'configure'],
  scope: 'global' | 'assigned' = 'global'
) {
  authLifecycle.establishSession({
    user: {
      id: userId,
      email: 'owner@example.com',
      display_name: 'Owner',
      role: 'owner',
      access_scope: scope,
      operations
    },
    csrf_token: 'fixture-token'
  });
}
beforeEach(() => {
  vi.resetAllMocks();
  session();
  vi.stubGlobal(
    'confirm',
    vi.fn(() => true)
  );
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
  vi.mocked(listProjectMemberships).mockResolvedValue([
    { id: projectId, name: 'First project', role: 'manager' },
    { id: secondProjectId, name: 'Second project', role: 'viewer' }
  ]);
  vi.mocked(listAllProjectMembers).mockResolvedValue([
    {
      user_id: userId,
      email: 'owner@example.com',
      display_name: 'Owner',
      role: 'owner',
      project_role: 'manager',
      active: true,
      added_by: userId,
      added_by_email: 'owner@example.com',
      created_at: '2026-10-01T00:00:00Z'
    }
  ]);
  vi.mocked(listApiKeys).mockResolvedValue([]);
  vi.mocked(api.listCodeAccounts).mockImplementation(async (filters) => ({
    items: filters?.project_id === secondProjectId ? [] : [account],
    nextCursor: null
  }));
  vi.mocked(api.listCodePools).mockResolvedValue({
    items: [pool],
    nextCursor: null
  });
  vi.mocked(api.listCodeRoutes).mockResolvedValue({
    items: [route],
    nextCursor: null
  });
  vi.mocked(api.listCodeBudgets).mockResolvedValue({
    items: [budget],
    nextCursor: null
  });
  vi.mocked(api.listCodeBindings).mockResolvedValue({
    items: [binding],
    nextCursor: null
  });
  vi.mocked(api.listCodeAttempts).mockResolvedValue({
    items: [attempt],
    nextCursor: null
  });
  vi.mocked(api.listCodeRefusals).mockResolvedValue({
    items: [],
    nextCursor: null
  });
  vi.mocked(api.listCodeTokenWindows).mockResolvedValue({
    items: [
      {
        id: budget.id,
        project_id: projectId,
        windows: [
          {
            period: 'day',
            starts_at: '2026-10-01T00:00:00Z',
            measured: 31,
            reserved: 4096
          }
        ]
      }
    ],
    nextCursor: null
  });
  vi.mocked(api.listCodeRevisions).mockResolvedValue({
    items: [{ id: route.revision_id, route }],
    nextCursor: null
  });
  vi.mocked(listProviders).mockResolvedValue([
    { ...provider, kind: provider.configuration.kind }
  ]);
  vi.mocked(getProvider).mockResolvedValue(provider);
  vi.mocked(listProviderKinds).mockResolvedValue([pluginSpec]);
  vi.mocked(listProviderCredentials).mockResolvedValue([]);
});
afterEach(async () => {
  if (component) await unmount(component);
  vi.useRealTimers();
  component = undefined;
  client.clear();
  host.remove();
  await authLifecycle.principalInvalidated();
  vi.unstubAllGlobals();
});
async function settle() {
  for (let i = 0; i < 5; i++)
    await new Promise((resolve) => setTimeout(resolve, 0));
  flushSync();
}
async function render(
  loadClientConfiguration?: ComponentProps<
    typeof CodeModeProbe
  >['loadClientConfiguration'],
  gatewayURL = 'https://olp.example'
) {
  component = mount(CodeModeProbe, {
    target: host,
    props: { client, gatewayURL, loadClientConfiguration }
  });
  flushSync();
  await settle();
}
function button(name: string): HTMLButtonElement {
  const result = [...host.querySelectorAll('button')].find(
    (button) => button.textContent?.trim() === name
  );
  if (!result) throw new Error(`No button: ${name}`);
  return result;
}
async function click(name: string) {
  button(name).click();
  flushSync();
  await settle();
}
function field(id: string, value: string) {
  const input = host.querySelector<
    HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement
  >(`#${id}`);
  if (!input) throw new Error(`No field: ${id}`);
  input.value = value;
  input.dispatchEvent(
    new Event(input instanceof HTMLSelectElement ? 'change' : 'input', {
      bubbles: true
    })
  );
  flushSync();
}
function clientConfiguration(
  overrides: Partial<api.CodeClientConfiguration> = {}
): api.CodeClientConfiguration {
  return {
    route_slug: route.slug,
    base_url: 'https://olp.example/code/team-code',
    native_models: ['native-model'],
    adapters: ['codex'],
    client: 'codex',
    supported_clients: ['codex'],
    client_version: '0.160.0',
    model: 'native-model',
    plan_model: null,
    small_model: null,
    format: 'toml',
    file: '$CODEX_HOME/config.toml',
    configuration: 'fixture configuration',
    qualification_gaps: [],
    ...overrides
  };
}
async function submit() {
  host
    .querySelector('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await settle();
}

it('keeps provider allowance unknown, project data isolated and management authority reactive', async () => {
  session(['read', 'configure'], 'assigned');
  await render();
  expect(host.textContent).toContain('Unknown — no provider observation');
  expect(button('Create account')).toBeDefined();
  field('code-project', secondProjectId);
  await settle();
  expect(host.textContent).toContain('No accounts in this project');
  expect(host.textContent).not.toContain(account.principal);
  expect(host.textContent).toContain('Read only');
  expect(
    [...host.querySelectorAll('button')].some(
      (b) => b.textContent?.trim() === 'Create account'
    )
  ).toBe(false);
  field('code-project', projectId);
  await settle();
  await click('Edit account');
  session(['read'], 'assigned');
  flushSync();
  await settle();
  expect(button('Save').disabled).toBe(true);
  await submit();
  expect(api.saveCodeAccount).not.toHaveBeenCalled();
});

it('keeps native-model edits on stale ETag refusal and separates save from publication', async () => {
  await render();
  await click('Routes');
  await click('Edit draft');
  field('code-models', 'vendor/native-v2');
  vi.mocked(api.saveCodeRoute).mockRejectedValueOnce(
    new ApiProblem({
      status: 412,
      title: 'Conflict',
      type: 'https://openllmproxy.dev/problems/etag_mismatch'
    })
  );
  await submit();
  expect(host.textContent).toContain('Your edits have been kept');
  expect(host.querySelector<HTMLTextAreaElement>('#code-models')!.value).toBe(
    'vendor/native-v2'
  );
  expect(api.publishCodeRoute).not.toHaveBeenCalled();
  await click('Cancel');
  vi.mocked(api.publishCodeRoute).mockResolvedValue(route);
  await click('Publish route');
  expect(api.publishCodeRoute).toHaveBeenCalledWith(route);
  expect(host.textContent).toContain('without synthetic inference');
});

it('validates hard budget integers without inventing an operation bound', async () => {
  await render();
  await click('Token budgets');
  await click('Create budget');
  field('code-daily', '9007199254740992');
  await submit();
  expect(api.saveCodeBudget).not.toHaveBeenCalled();
  expect(host.textContent).toContain('positive whole numbers');
  field('code-daily', '12000');
  field('code-monthly', '');
  await submit();
  expect(api.saveCodeBudget).toHaveBeenCalledWith(
    {
      project_id: projectId,
      route_id: null,
      api_key_id: null,
      daily_tokens: 12000,
      monthly_tokens: null,
      enabled: true
    },
    undefined
  );
  expect(host.textContent).toContain('Changes saved');
});

it('paginates filtered metadata and keeps uncertain tokens distinct from measured usage', async () => {
  vi.mocked(api.listCodeAttempts).mockResolvedValue({
    items: [attempt],
    nextCursor: 'page-2'
  });
  await render();
  await click('Attempts');
  expect(host.textContent).toContain('Retained uncertain reservation');
  expect(host.textContent).toContain('4,096 tokens');
  const measured = [...host.querySelectorAll('dt')].find(
    (node) => node.textContent === 'Measured tokens'
  );
  expect(measured?.nextElementSibling?.textContent).toBe('Unknown');
  await click('Next');
  expect(api.listCodeAttempts).toHaveBeenLastCalledWith(
    { project_id: projectId, cursor: 'page-2' },
    expect.any(AbortSignal)
  );
  field('code-filter-route', route.id);
  field('code-filter-binding', binding.id);
  await click('Apply filters');
  expect(api.listCodeAttempts).toHaveBeenLastCalledWith(
    {
      project_id: projectId,
      cursor: undefined,
      route_id: route.id,
      api_key_id: undefined,
      account_id: undefined,
      binding_id: binding.id
    },
    expect.any(AbortSignal)
  );
  expect(host.textContent).toContain('Page 1');
  await click('Token windows');
  expect(host.textContent).toContain('Pending + uncertain reserved tokens');
  expect(host.textContent).toContain('UTC');
  expect(host.textContent).not.toContain('money');
});

it('retires a whole pinned tree only after confirmation and removes the retire action', async () => {
  await render();
  await click('Conversation trees');
  expect(host.textContent).toContain(`native-model → ${account.id}`);
  vi.mocked(confirm).mockReturnValueOnce(false);
  await click('Retire entire tree');
  expect(api.retireCodeBinding).not.toHaveBeenCalled();
  vi.mocked(api.listCodeBindings).mockResolvedValue({
    items: [{ ...binding, retired_at: '2026-10-02T00:00:00Z' }],
    nextCursor: null
  });
  await click('Retire entire tree');
  expect(api.retireCodeBinding).toHaveBeenCalledWith(binding.id);
  expect(host.textContent).toContain('fresh independent conversation');
  expect(host.textContent).toContain('Retired tree binding');
  expect(host.textContent).not.toContain('Retire entire tree');
});

it('renders only adapter-generated client configuration and its qualification gaps', async () => {
  const load = vi.fn().mockResolvedValue(
    clientConfiguration({
      client_version: 'fixture-release',
      qualification_gaps: ['Fixture gap: no real subscription tested']
    })
  );
  await render(load);
  await click('Routes');
  await click('Revisions and client setup');
  expect(load).toHaveBeenCalledWith(route, {}, expect.any(AbortSignal));
  expect(
    host.querySelector<HTMLTextAreaElement>(
      `#code-client-configuration-${route.id}`
    )!.value
  ).toBe('fixture configuration');
  expect(host.textContent).toContain(
    'Fixture gap: no real subscription tested'
  );
  expect(host.textContent).toContain('Provider connections are frozen');
});

it('reloads client configuration for a changed public gateway with a fresh cache', async () => {
  const configuration = (gatewayURL: string) =>
    clientConfiguration({
      base_url: `${gatewayURL}/code/${route.slug}`,
      native_models: route.models,
      configuration: `base_url = "${gatewayURL}/code/${route.slug}"`
    });
  const first = vi
    .fn()
    .mockResolvedValue(configuration('https://first.example'));
  await render(first, 'https://first.example');
  await click('Routes');
  await click('Revisions and client setup');
  expect(first).toHaveBeenCalledOnce();
  await unmount(component!);
  component = undefined;

  const second = vi
    .fn()
    .mockResolvedValue(configuration('https://second.example'));
  await render(second, 'https://second.example');
  await click('Routes');
  await click('Revisions and client setup');
  expect(second).toHaveBeenCalledOnce();
  expect(
    host.querySelector<HTMLTextAreaElement>(
      `#code-client-configuration-${route.id}`
    )!.value
  ).toBe(configuration('https://second.example').configuration);
  expect(host.textContent).not.toContain('https://first.example');
});

it('caches selected published models, preserves selection on URL edits and resets it on publication', async () => {
  let publishedModels = ['native-model', 'another-native-model'];
  const load = vi.fn(
    async (current: api.CodeRoute, selection: api.CodeClientSelection) => {
      const model = selection.model ?? publishedModels[0];
      return clientConfiguration({
        route_slug: current.slug,
        base_url: `https://olp.example/code/${current.slug}`,
        native_models: publishedModels,
        model,
        configuration: `model = "${model}"\nX-OLP-Code-Model = "${model}"`
      });
    }
  );
  await render(load);
  await click('Routes');
  await click('Revisions and client setup');
  const modelID = `code-client-model-${route.id}`;
  const selectedModel = () =>
    host.querySelector<HTMLSelectElement>(`#${modelID}`)!.value;
  const configuration = () =>
    host.querySelector<HTMLTextAreaElement>(
      `#code-client-configuration-${route.id}`
    )!.value;
  expect(selectedModel()).toBe('native-model');
  host.querySelector<HTMLSelectElement>(`#${modelID}`)!.focus();
  field(modelID, 'another-native-model');
  await settle();
  expect(document.activeElement).toBe(host.querySelector(`#${modelID}`));
  expect(load).toHaveBeenLastCalledWith(
    route,
    { model: 'another-native-model' },
    expect.any(AbortSignal)
  );
  expect(configuration()).toContain('model = "another-native-model"');
  expect(configuration()).toContain(
    'X-OLP-Code-Model = "another-native-model"'
  );
  field(modelID, 'native-model');
  await settle();
  expect(configuration()).toContain('model = "native-model"');
  field(modelID, 'another-native-model');
  await settle();
  expect(configuration()).toContain('model = "another-native-model"');
  expect(load.mock.calls.map(([, selection]) => selection)).toEqual([
    {},
    { model: 'another-native-model' },
    { model: 'native-model' }
  ]);

  field('probe-gateway-url', 'https://changed-gateway.example');
  await settle();
  expect(load).toHaveBeenCalledTimes(4);
  expect(selectedModel()).toBe('another-native-model');
  expect(load).toHaveBeenLastCalledWith(
    route,
    { model: 'another-native-model' },
    expect.any(AbortSignal)
  );

  const draft = {
    ...route,
    models: ['draft-native-model'],
    etag: 'draft-etag'
  };
  vi.mocked(api.listCodeRoutes).mockResolvedValue({
    items: [draft],
    nextCursor: null
  });
  await client.invalidateQueries({
    queryKey: codeKeys.collection('routes', { project_id: projectId })
  });
  await settle();
  expect(selectedModel()).toBe('another-native-model');
  expect(host.querySelector(`#${modelID}`)!.textContent).not.toContain(
    'draft-native-model'
  );
  expect(load).toHaveBeenCalledTimes(4);

  publishedModels = ['new-published-model'];
  const published = {
    ...draft,
    models: publishedModels,
    revision_id: '01980000-0000-7000-8000-000000000032',
    revision: 2
  };
  vi.mocked(api.listCodeRoutes).mockResolvedValue({
    items: [published],
    nextCursor: null
  });
  await client.invalidateQueries({
    queryKey: codeKeys.collection('routes', { project_id: projectId })
  });
  await settle();
  expect(selectedModel()).toBe('new-published-model');
  expect(configuration()).toContain('X-OLP-Code-Model = "new-published-model"');
  expect(load).toHaveBeenLastCalledWith(published, {}, expect.any(AbortSignal));
  expect(
    load.mock.calls.filter(
      ([current]) => current.revision_id === published.revision_id
    )
  ).toEqual([[published, {}, expect.any(AbortSignal)]]);
});

it.each(['gateway', 'client', 'model', 'background model'])(
  'hides generated text and copying while a changed %s loads',
  async (change) => {
    const copy = vi.spyOn(clipboard, 'copyText').mockResolvedValue(true);
    const initial = clientConfiguration({
      client: 'claude-code',
      supported_clients: ['claude-code', 'opencode'],
      native_models: ['native-model', 'another-native-model'],
      small_model: 'native-model',
      configuration: 'previous query configuration'
    });
    let resolve!: (value: api.CodeClientConfiguration) => void;
    const pending = new Promise<api.CodeClientConfiguration>((done) => {
      resolve = done;
    });
    const load = vi
      .fn()
      .mockResolvedValueOnce(initial)
      .mockReturnValueOnce(pending);
    await render(load);
    await click('Routes');
    await click('Revisions and client setup');
    await click('Copy configuration');
    expect(copy).toHaveBeenLastCalledWith(initial.configuration);
    const modelID = `code-client-model-${route.id}`;
    const picker = host.querySelector<HTMLSelectElement>(`#${modelID}`)!;
    picker.focus();
    if (change === 'gateway')
      field('probe-gateway-url', 'https://changed-gateway.example');
    else if (change === 'client') {
      host
        .querySelectorAll<HTMLInputElement>(
          `input[name="code-client-${route.id}"]`
        )[1]
        .click();
      flushSync();
    } else
      field(
        change === 'model' ? modelID : `code-client-small-model-${route.id}`,
        'another-native-model'
      );
    await settle();
    expect(load).toHaveBeenCalledTimes(2);
    expect(host.querySelector(`#${modelID}`)).toBe(picker);
    expect(document.activeElement).toBe(picker);
    expect(
      host.querySelector(`#code-client-configuration-${route.id}`)
    ).toBeNull();
    expect(host.textContent).toContain('Loading supported configuration');
    expect(host.textContent).not.toContain('Copy configuration');
    resolve({ ...initial, configuration: 'current query configuration' });
    await settle();
    await click('Copy configuration');
    expect(copy).toHaveBeenLastCalledWith('current query configuration');
  }
);

it('associates labels with distinct controls in multiple configuration panels', async () => {
  const load = vi.fn(async (current: api.CodeRoute) =>
    clientConfiguration({
      route_slug: current.slug,
      base_url: `https://olp.example/code/${current.slug}`,
      native_models: current.models,
      model: current.models[0],
      configuration: `configuration for ${current.slug}`
    })
  );
  component = mount(ClientConfigurationsProbe, {
    target: host,
    props: {
      client,
      routes: [route, { ...route, id: 'another-route', slug: 'another-code' }],
      gatewayURL: 'https://olp.example',
      load
    }
  });
  flushSync();
  await settle();
  const controls = [...host.querySelectorAll('textarea, select')];
  expect(controls).toHaveLength(4);
  expect(new Set(controls.map((control) => control.id)).size).toBe(4);
  for (const label of host.querySelectorAll('label')) {
    expect(label.control).not.toBeNull();
    expect(controls).toContain(label.control);
  }
});

it('shows each allowance limit and reset independently with provider credits', async () => {
  vi.mocked(api.listCodeAccounts).mockResolvedValue({
    items: [
      {
        ...account,
        allowance: {
          remaining_tokens: null,
          remaining_requests: null,
          remaining_percent: 80,
          resets_at: '2026-10-02T11:00:00Z',
          observed_at: '2026-10-01T10:00:00Z',
          windows: [
            {
              limit_id: 'codex',
              window: 'primary',
              used_percent: 20,
              remaining_percent: 80,
              window_minutes: 300,
              resets_at: '2026-10-02T11:00:00Z',
              observed_at: '2026-10-01T10:00:00Z'
            },
            {
              limit_id: 'codex',
              window: 'secondary',
              used_percent: 100,
              remaining_percent: 0,
              window_minutes: 10080,
              resets_at: '2026-10-07T12:00:00Z',
              observed_at: '2026-10-01T10:00:01Z'
            },
            {
              limit_id: 'codex_other',
              window: 'primary',
              used_percent: 45,
              remaining_percent: 55,
              window_minutes: null,
              resets_at: null,
              observed_at: '2026-10-01T10:00:02Z'
            }
          ],
          credits: {
            has_credits: true,
            unlimited: false,
            balance: '12.50',
            observed_at: '2026-10-01T10:00:03Z'
          }
        }
      }
    ],
    nextCursor: null
  });
  await render();
  const text = host.textContent?.replace(/\s+/g, ' ');
  expect(text).toContain('codex / primary: 20% used · 80% remaining');
  expect(text).toContain('codex / secondary: 100% used · 0% remaining');
  expect(text).toContain('codex_other / primary: 45% used · 55% remaining');
  expect(text).toContain('300 minutes');
  expect(text).toContain('10080 minutes');
  expect(text).toContain(formatDate('2026-10-02T11:00:00Z'));
  expect(text).toContain(formatDate('2026-10-07T12:00:00Z'));
  expect(text).toContain('Window: Unknown · Reset Unknown');
  expect(text).toContain('Has credits: Yes · Unlimited: No · Balance: 12.50');
  expect(text).toContain(
    'Credits do not override exhausted subscription windows'
  );
});

it('shows exhausted counts beside windows with their own resets and unknown values', async () => {
  vi.mocked(api.listCodeAccounts).mockResolvedValue({
    items: [
      {
        ...account,
        eligible: false,
        allowance: {
          remaining_tokens: 0,
          remaining_requests: null,
          remaining_percent: 80,
          observed_at: '2026-10-01T10:00:00Z',
          resets_at: '2026-10-02T11:00:00Z',
          token_observation: {
            observed_at: '2026-10-01T09:00:00Z',
            resets_at: null
          },
          windows: [
            {
              limit_id: 'codex',
              window: 'primary',
              used_percent: 20,
              remaining_percent: 80,
              window_minutes: 300,
              observed_at: '2026-10-01T10:00:00Z',
              resets_at: '2026-10-02T11:00:00Z'
            }
          ]
        }
      }
    ],
    nextCursor: null
  });
  await render();
  const text = host.textContent?.replace(/\s+/g, ' ');
  expect(text).toContain(
    `Tokens: 0 · Observed ${formatDate('2026-10-01T09:00:00Z')} · Reset Unknown`
  );
  expect(text).toContain('Requests: Unknown');
  expect(text).toContain('codex / primary: 20% used · 80% remaining');
});

it("offers the route's clients and regenerates for a chosen client and models", async () => {
  const models = ['glm-5.3', 'minimax-m3'];
  const load = vi.fn(
    async (_route: api.CodeRoute, selection: api.CodeClientSelection) => {
      const client = selection.client ?? 'claude-code';
      return clientConfiguration({
        native_models: models,
        adapters: ['opencode_go', 'zai_coding'],
        client,
        supported_clients: ['claude-code', 'opencode'],
        client_version: client === 'opencode' ? '1.18.34' : '2.1.286',
        model: selection.model ?? models[0],
        plan_model: selection.plan_model ?? selection.model ?? models[0],
        small_model: selection.small_model ?? selection.model ?? models[0],
        format: client === 'opencode' ? 'json' : 'shell',
        file: client === 'opencode' ? 'opencode.json' : null,
        configuration: `${client} ${selection.model ?? models[0]}`
      });
    }
  );
  await render(load);
  await click('Routes');
  await click('Revisions and client setup');
  const text = () => host.textContent?.replace(/\s+/g, ' ');
  expect(text()).toContain('Claude Code 2.1.286');
  expect(text()).toContain('Source this file in your shell');
  expect(text()).toContain('Generated configuration · SHELL');
  expect(text()).toContain('Subscriptions');
  expect(
    [
      ...host.querySelectorAll(
        '[aria-label="Client configuration"] .badges .badge'
      )
    ].map((badge) => badge.textContent)
  ).toEqual(['OpenCode Go', 'GLM Coding Plan']);
  expect(text()).toContain('Plan mode, through opusplan');
  expect(
    host.querySelector(`#code-client-small-model-${route.id}`)
  ).not.toBeNull();
  const radios = [
    ...host.querySelectorAll<HTMLInputElement>(
      `input[name="code-client-${route.id}"]`
    )
  ];
  expect(radios.map((radio) => radio.value)).toEqual([
    'claude-code',
    'opencode'
  ]);
  radios[1].click();
  flushSync();
  await settle();
  expect(load).toHaveBeenLastCalledWith(
    route,
    { client: 'opencode' },
    expect.any(AbortSignal)
  );
  field(`code-client-model-${route.id}`, 'minimax-m3');
  await settle();
  field(`code-client-plan-model-${route.id}`, 'glm-5.3');
  await settle();
  expect(load).toHaveBeenLastCalledWith(
    route,
    { client: 'opencode', model: 'minimax-m3', plan_model: 'glm-5.3' },
    expect.any(AbortSignal)
  );
  expect(
    host.querySelector<HTMLTextAreaElement>(
      `#code-client-configuration-${route.id}`
    )!.value
  ).toBe('opencode minimax-m3');
  expect(text()).toContain('Save as opencode.json.');
  expect(text()).toContain('OpenCode 1.18.34');
  expect(text()).toContain('The plan agent; the build agent uses');
  // Another client has its own native models, so its models start again
  // from their defaults.
  radios[0].click();
  flushSync();
  await settle();
  expect(load).toHaveBeenLastCalledWith(
    route,
    { client: 'claude-code' },
    expect.any(AbortSignal)
  );
});

it('keeps client pickers after a refused selection and restores the defaults', async () => {
  const load = vi.fn(
    async (_route: api.CodeRoute, selection: api.CodeClientSelection) => {
      if (selection.client === 'opencode')
        throw new ApiProblem({
          status: 422,
          title: 'Validation failed',
          detail: "This route's GLM Coding Plan accounts support Claude Code."
        });
      return clientConfiguration({
        adapters: ['zai_coding'],
        client: 'claude-code',
        supported_clients: ['claude-code', 'opencode'],
        plan_model: 'native-model',
        small_model: 'native-model',
        format: 'shell',
        file: null,
        configuration: 'default configuration'
      });
    }
  );
  await render(load);
  await click('Routes');
  await click('Revisions and client setup');
  host
    .querySelectorAll<HTMLInputElement>(
      `input[name="code-client-${route.id}"]`
    )[1]
    .click();
  flushSync();
  await settle();
  expect(host.querySelector('[role="alert"]')?.textContent).toContain(
    'support Claude Code'
  );
  expect(host.querySelector(`#code-client-model-${route.id}`)).not.toBeNull();
  expect(
    host.querySelector(`#code-client-configuration-${route.id}`)
  ).toBeNull();
  await click('Use defaults');
  expect(
    host.querySelector<HTMLTextAreaElement>(
      `#code-client-configuration-${route.id}`
    )!.value
  ).toBe('default configuration');
});

it('shows the subscription families of accounts and published routes', async () => {
  await render();
  expect(
    [...host.querySelectorAll('.badge')].map((badge) => badge.textContent)
  ).toContain('GLM Coding Plan');
  await click('Routes');
  expect(
    [...host.querySelectorAll('.badge')].map((badge) => badge.textContent)
  ).toContain('GLM Coding Plan');
  vi.mocked(api.listCodeRoutes).mockResolvedValue({
    items: [{ ...route, adapters: ['opencode_go', 'zai_coding'] }],
    nextCursor: null
  });
  await client.invalidateQueries();
  await settle();
  expect(
    [...host.querySelectorAll('.badges .badge')].map(
      (badge) => badge.textContent
    )
  ).toEqual(['OpenCode Go', 'GLM Coding Plan']);
  vi.mocked(api.listCodeRoutes).mockResolvedValue({
    items: [{ ...route, adapters: [], published_at: null }],
    nextCursor: null
  });
  await client.invalidateQueries();
  await settle();
  expect(host.textContent).toContain('Set at publication');
});

it('enrolls a pasted coding-plan key through a masked field', async () => {
  vi.mocked(startGrantEnrollment).mockResolvedValue({
    id: 'enrollment',
    provider_id: provider.id,
    slot_id: 'slot',
    authorization_url: 'https://z.ai/manage-apikey/apikey-list',
    input: 'secret',
    expires_at: '2026-10-02T23:00:00Z'
  });
  vi.mocked(continueGrantEnrollment).mockResolvedValue({
    provider_id: provider.id,
    credential_id: account.credential_id,
    credential_version: 1,
    etag: account.etag,
    principal: account.principal
  });
  await render();
  await click('Create account');
  field('code-provider', provider.id);
  await settle();
  await vi.waitFor(() => {
    flushSync();
    expect(button('Enroll subscription account').disabled).toBe(false);
  });
  await click('Enroll subscription account');
  expect(host.textContent).toContain('Issue an upstream API key');
  expect(host.textContent).toContain('Open API key page');
  const input = host.querySelector<HTMLInputElement>('#grant-input')!;
  expect(input.type).toBe('password');
  expect(input.autocomplete).toBe('off');
  field('grant-input', 'fixture-coding-plan-key-0123456789');
  await click('Continue');
  expect(continueGrantEnrollment).toHaveBeenCalledWith(
    expect.objectContaining({ id: 'enrollment' }),
    'fixture-coding-plan-key-0123456789'
  );
  expect(host.textContent).toContain('Enrollment did not test the key');
  expect(host.textContent).not.toContain('fixture-coding-plan-key');
  expect(host.querySelector('#grant-input')).toBeNull();
});

it('keeps client setup tied to the active publication after saving a disabled draft', async () => {
  const load = vi
    .fn()
    .mockResolvedValue(
      clientConfiguration({ configuration: 'published configuration' })
    );
  await render(load);
  await click('Routes');
  await click('Edit draft');
  field('code-models', 'unpublished-model');
  const edited = {
    ...route,
    enabled: false,
    models: ['unpublished-model'],
    etag: 'new-etag'
  };
  vi.mocked(api.saveCodeRoute).mockResolvedValue(edited);
  vi.mocked(api.listCodeRoutes).mockResolvedValue({
    items: [edited],
    nextCursor: null
  });
  await submit();
  expect(api.publishCodeRoute).not.toHaveBeenCalled();
  expect(host.textContent).toContain('unpublished-model');
  expect(host.textContent).toContain('Revision 1');
  await click('Revisions and client setup');
  expect(load).toHaveBeenCalledWith(edited, {}, expect.any(AbortSignal));
  expect(
    host.querySelector<HTMLTextAreaElement>(
      `#code-client-configuration-${route.id}`
    )!.value
  ).toBe('published configuration');
});

it('distinguishes upstream rejection statuses from interruptions and unknown consumption', async () => {
  vi.mocked(api.listCodeAttempts).mockResolvedValue({
    items: [
      ...[401, 429, 500].map((status) => ({
        ...attempt,
        id: String(status),
        upstream_status: status,
        outcome_origin: 'upstream' as const,
        outcome: 'rejected' as const,
        outcome_observed_at: '2026-10-01T10:03:00Z'
      })),
      {
        ...attempt,
        upstream_status: 200,
        outcome_origin: 'gateway',
        outcome: 'interrupted',
        outcome_observed_at: '2026-10-01T10:03:00Z'
      }
    ],
    nextCursor: null
  });
  await render();
  await click('Attempts');
  const text = host.textContent?.replace(/\s+/g, ' ');
  for (const status of [401, 429, 500, 200])
    expect(text).toContain(String(status));
  expect(text).toContain('upstream / rejected');
  expect(text).toContain('gateway / interrupted');
  expect(text).toContain('uncertain');
  expect(text).toContain('Unknown');
});

it('reuses grant enrollment and associates the observed credential without requesting raw secrets', async () => {
  vi.mocked(startGrantEnrollment).mockResolvedValue({
    id: 'enrollment',
    provider_id: provider.id,
    slot_id: 'slot',
    authorization_url: 'https://login.example/authorize',
    expires_at: '2026-10-02T23:00:00Z'
  });
  vi.mocked(continueGrantEnrollment).mockResolvedValue({
    provider_id: provider.id,
    credential_id: account.credential_id,
    credential_version: 1,
    etag: account.etag,
    principal: account.principal
  });
  await render();
  await click('Create account');
  field('code-provider', provider.id);
  await settle();
  expect(getProvider).toHaveBeenCalledWith(
    provider.id,
    expect.any(AbortSignal)
  );
  expect(listProviderKinds).toHaveBeenCalled();
  expect(client.getQueryData(['providers', 'detail', provider.id])).toEqual(
    provider
  );
  await vi.waitFor(() => {
    flushSync();
    expect(button('Enroll subscription account').disabled).toBe(false);
  });
  await click('Enroll subscription account');
  expect(startGrantEnrollment).toHaveBeenCalledWith(provider);
  expect(button('Save').disabled).toBe(true);
  field('grant-input', 'https://localhost/callback?code=one-time-fixture');
  await click('Continue');
  expect(continueGrantEnrollment).toHaveBeenCalledWith(
    expect.objectContaining({ id: 'enrollment' }),
    'https://localhost/callback?code=one-time-fixture'
  );
  expect(host.querySelector<HTMLSelectElement>('#code-credential')!.value).toBe(
    account.credential_id
  );
  expect(host.textContent).toContain('Enrollment did not test inference');
  field('code-name', 'Enrolled account');
  field('code-models', 'native-model');
  await submit();
  expect(api.saveCodeAccount).toHaveBeenCalledWith(
    {
      project_id: projectId,
      provider_id: provider.id,
      credential_id: account.credential_id,
      name: 'Enrolled account',
      models: ['native-model'],
      enabled: true
    },
    undefined
  );
});

it.each(['automatic', 'manual'])(
  'continues device polling after %s recovery from a transient error',
  async (recovery) => {
    vi.mocked(startGrantEnrollment).mockResolvedValue({
      id: 'device-enrollment',
      provider_id: provider.id,
      slot_id: 'slot',
      device: {
        verification_url: 'https://login.example/device',
        user_code: 'WDJB-MJHT',
        interval: 5
      },
      expires_at: '2026-10-02T23:00:00Z'
    });
    vi.mocked(pollGrantEnrollment)
      .mockRejectedValueOnce(new TypeError('Failed to fetch'))
      .mockResolvedValueOnce({ status: 'pending', interval: 10 })
      .mockResolvedValueOnce({
        status: 'completed',
        completion: {
          provider_id: provider.id,
          credential_id: account.credential_id,
          credential_version: 1,
          etag: account.etag,
          principal: account.principal
        }
      });
    await render();
    await click('Create account');
    field('code-provider', provider.id);
    await settle();
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    button('Enroll subscription account').click();
    await vi.advanceTimersByTimeAsync(0);
    flushSync();

    await vi.advanceTimersByTimeAsync(5000);
    flushSync();
    expect(pollGrantEnrollment).toHaveBeenCalledOnce();
    expect(host.querySelector('[role="alert"]')?.textContent).toContain(
      'Failed to fetch'
    );
    expect(button('Check authorization again').disabled).toBe(false);

    if (recovery === 'manual') {
      button('Check authorization again').click();
      await vi.advanceTimersByTimeAsync(0);
    } else {
      await vi.advanceTimersByTimeAsync(4999);
      expect(pollGrantEnrollment).toHaveBeenCalledOnce();
      await vi.advanceTimersByTimeAsync(1);
    }
    flushSync();
    expect(pollGrantEnrollment).toHaveBeenCalledTimes(2);
    expect(host.querySelector('[role="alert"]')).toBeNull();
    expect(host.textContent).not.toContain('Check authorization again');

    await vi.advanceTimersByTimeAsync(recovery === 'manual' ? 5000 : 10000);
    flushSync();
    expect(pollGrantEnrollment).toHaveBeenCalledTimes(3);
    expect(
      host.querySelector<HTMLSelectElement>('#code-credential')!.value
    ).toBe(account.credential_id);
    expect(host.textContent).toContain('Enrollment did not test inference');
    await vi.advanceTimersByTimeAsync(60000);
    expect(pollGrantEnrollment).toHaveBeenCalledTimes(3);
  }
);

it.each([
  [404, 'not_found'],
  [409, 'grant_enrollment_used'],
  [410, 'grant_enrollment_expired'],
  [422, 'grant_enrollment_failed']
])('stops device polling after %s %s', async (status, code) => {
  vi.mocked(startGrantEnrollment).mockResolvedValue({
    id: 'device-enrollment',
    provider_id: provider.id,
    slot_id: 'slot',
    device: {
      verification_url: 'https://login.example/device',
      user_code: 'WDJB-MJHT',
      interval: 5
    },
    expires_at: '2026-10-02T23:00:00Z'
  });
  vi.mocked(pollGrantEnrollment).mockRejectedValue(
    new ApiProblem({
      status,
      type: `https://openllmproxy.dev/problems/${code}`,
      title: 'This enrollment ended. Start another.'
    })
  );
  await render();
  await click('Create account');
  field('code-provider', provider.id);
  await settle();
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
  button('Enroll subscription account').click();
  await vi.advanceTimersByTimeAsync(5000);
  flushSync();

  expect(pollGrantEnrollment).toHaveBeenCalledOnce();
  expect(host.querySelector('[role="alert"]')?.textContent).toContain(
    'This enrollment ended. Start another.'
  );
  await vi.advanceTimersByTimeAsync(60000);
  expect(pollGrantEnrollment).toHaveBeenCalledOnce();
  expect(button('Enroll subscription account').disabled).toBe(false);
  button('Enroll subscription account').click();
  await vi.advanceTimersByTimeAsync(0);
  flushSync();
  expect(startGrantEnrollment).toHaveBeenCalledTimes(2);
  expect(host.querySelector('[role="alert"]')).toBeNull();
});

it.each([408, 429, 500, 502, 503, 504])(
  'retains device authorization after HTTP %s and completes on retry',
  async (status) => {
    vi.mocked(startGrantEnrollment).mockResolvedValue({
      id: 'device-enrollment',
      provider_id: provider.id,
      slot_id: 'slot',
      device: {
        verification_url: 'https://login.example/device',
        user_code: 'WDJB-MJHT',
        interval: 5
      },
      expires_at: '2026-10-02T23:00:00Z'
    });
    vi.mocked(pollGrantEnrollment)
      .mockRejectedValueOnce(
        new ApiProblem({ status, title: 'Please try again.' })
      )
      .mockResolvedValueOnce({
        status: 'completed',
        completion: {
          provider_id: provider.id,
          credential_id: account.credential_id,
          credential_version: 1,
          etag: account.etag,
          principal: account.principal
        }
      });
    await render();
    await click('Create account');
    field('code-provider', provider.id);
    await settle();
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    button('Enroll subscription account').click();
    await vi.advanceTimersByTimeAsync(5000);
    flushSync();
    expect(pollGrantEnrollment).toHaveBeenCalledOnce();
    expect(host.querySelector('[role="alert"]')?.textContent).toContain(
      'Please try again.'
    );

    await vi.advanceTimersByTimeAsync(5000);
    flushSync();
    expect(pollGrantEnrollment).toHaveBeenCalledTimes(2);
    expect(startGrantEnrollment).toHaveBeenCalledOnce();
    expect(host.querySelector('[role="alert"]')).toBeNull();
    expect(
      host.querySelector<HTMLSelectElement>('#code-credential')!.value
    ).toBe(account.credential_id);
    await vi.advanceTimersByTimeAsync(60000);
    expect(pollGrantEnrollment).toHaveBeenCalledTimes(2);
  }
);

it('retains explicit pool memberships and makes ownership immutable', async () => {
  await render();
  await click('Pools');
  await click('Edit pool');
  expect(host.querySelector<HTMLSelectElement>('#code-kind')!.disabled).toBe(
    true
  );
  expect(
    host.querySelector<HTMLInputElement>(`input[value="${account.id}"]`)!
      .checked
  ).toBe(true);
  expect(
    host.querySelector<HTMLInputElement>(`input[value="${keyId}"]`)!.checked
  ).toBe(true);
  field('code-name', 'Updated team pool');
  await submit();
  expect(api.saveCodePool).toHaveBeenCalledWith(
    {
      project_id: projectId,
      name: 'Updated team pool',
      kind: 'shared',
      owner_user_id: null,
      account_ids: [account.id],
      api_key_ids: [keyId]
    },
    pool
  );
});

it('retains an immutable key budget scope even when its key cannot be listed', async () => {
  const keyBudget = { ...budget, api_key_id: keyId };
  vi.mocked(api.listCodeBudgets).mockResolvedValue({
    items: [keyBudget],
    nextCursor: null
  });
  await render();
  await click('Token budgets');
  await click('Edit budget');
  const scope = host.querySelector<HTMLSelectElement>('#code-budget-key')!;
  expect(scope.disabled).toBe(true);
  expect(scope.value).toBe(keyId);
  field('code-daily', '99000');
  await submit();
  expect(api.saveCodeBudget).toHaveBeenCalledWith(
    {
      project_id: projectId,
      route_id: null,
      api_key_id: keyId,
      daily_tokens: 99000,
      monthly_tokens: null,
      enabled: true
    },
    keyBudget
  );
});

it('does not load code-mode data without read authority', async () => {
  session([]);
  await render();
  expect(host.textContent).toContain(
    'Your role cannot read code-mode configuration'
  );
  expect(listProjectMemberships).not.toHaveBeenCalled();
  expect(api.listCodeAccounts).not.toHaveBeenCalled();
});
