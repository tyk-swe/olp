import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { expect, it, onTestFinished, vi } from 'vitest';
import { ApiProblem } from '$lib/api/http';
import { goto } from '$app/navigation';
import * as routes from './api';
import * as agents from '$lib/features/agents/api';
import * as models from '$lib/features/providers/api/models';
import RouteDraftEditorProbe from './test/RouteDraftEditorProbe.svelte';

vi.mock('./api', async (original) => ({
  ...(await original<typeof import('./api')>()),
  getRouteDraft: vi.fn(),
  deleteRouteDraft: vi.fn(),
  replaceRouteDraft: vi.fn(),
  simulateRoute: vi.fn(),
  validateRoute: vi.fn(),
  activateRoute: vi.fn(),
  getRoutingPolicy: vi.fn()
}));
vi.mock('$lib/features/agents/api', async (original) => ({
  ...(await original<typeof import('$lib/features/agents/api')>()),
  listPublications: vi.fn()
}));
vi.mock('$lib/features/providers/api/models', async (original) => ({
  ...(await original<typeof import('$lib/features/providers/api/models')>()),
  listProviderModelInventory: vi.fn()
}));
vi.mock('$app/navigation', async (original) => ({
  ...(await original<typeof import('$app/navigation')>()),
  goto: vi.fn()
}));
vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({
    can: () => true,
    allows: () => true,
    user: null,
    globalScope: false
  })
}));

const buildRoute = (toolsets: string[] | undefined): routes.RouteDraft => ({
  id: 'route',
  etag: 'etag',
  slug: 'workflow',
  state: 'draft',
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
  project_id: 'project',
  project_name: 'Project',
  operations: ['generation'],
  overall_timeout_ms: 120000,
  max_attempts: 2,
  fidelity: { mode: 'transformed' },
  content_policy: null,
  fallbacks: [],
  selectors: [],
  retry: {},
  affinity: null,
  budget: null,
  passthrough: null,
  certification: 'certified',
  tool_execution: 'gateway',
  toolsets,
  targets: [
    {
      id: 'target',
      position: 0,
      provider_id: 'provider',
      provider_model_id: 'model',
      provider_name: 'Provider',
      provider_model: 'model',
      priority: 1,
      weight: 1,
      timeout_ms: 60000,
      available: true,
      lifecycle: null,
      tags: [],
      shadow: null
    }
  ]
});

it('preserves workflow settings when editing an existing route', async () => {
  const selected = '0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607';
  const added = '0192cf87-d4ab-7f2e-a8b1-c2d3e4f50608';
  const route = {
    ...buildRoute([selected]),
    run_limits: { max_iterations: 6, max_run_cost: '1.25' }
  };

  vi.mocked(routes.getRouteDraft).mockResolvedValue(route);
  vi.mocked(routes.replaceRouteDraft).mockImplementation(
    async (_id, _etag, input) => ({
      ...route,
      tool_execution: input.tool_execution,
      toolsets: input.toolsets
    })
  );
  vi.mocked(routes.getRoutingPolicy).mockRejectedValue(
    new Error('No route policy')
  );
  vi.mocked(models.listProviderModelInventory).mockResolvedValue([]);
  vi.mocked(agents.listPublications).mockResolvedValue([
    {
      id: selected,
      project_id: 'project',
      name: 'Lookup tools',
      etag: 'etag',
      kind: 'toolset',
      revision_id: 'revision',
      retired: false
    },
    {
      id: added,
      project_id: 'project',
      name: 'Search tools',
      etag: 'etag',
      kind: 'toolset',
      revision_id: 'revision',
      retired: false
    },
    {
      id: 'other',
      project_id: 'other-project',
      name: 'Other project',
      etag: 'etag',
      kind: 'toolset',
      revision_id: 'revision',
      retired: false
    }
  ]);
  const host = document.createElement('div');
  document.body.append(host);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  const component = mount(RouteDraftEditorProbe, {
    target: host,
    props: { client }
  });
  onTestFinished(async () => {
    await unmount(component);
    client.clear();
    host.remove();
  });
  await vi.waitFor(() => expect(host.textContent).toContain('Lookup tools'));
  const execution = host.querySelector<HTMLSelectElement>(
    '#route-tool-execution'
  );
  expect(execution?.value).toBe('gateway');
  const checked = [
    ...host.querySelectorAll<HTMLInputElement>(
      'fieldset input[type="checkbox"]'
    )
  ];
  const lookup = checked.find((item) =>
    item.parentElement?.textContent?.includes('Lookup tools')
  );
  const search = checked.find((item) =>
    item.parentElement?.textContent?.includes('Search tools')
  );
  expect(lookup?.checked).toBe(true);
  expect(search?.checked).toBe(false);
  expect(host.textContent).not.toContain('Other project');
  search!.click();
  const iterations = host.querySelector<HTMLInputElement>(
    '#route-run-max_iterations'
  )!;
  const toolCalls = host.querySelector<HTMLInputElement>(
    '#route-run-max_tool_calls'
  )!;
  const runCost = host.querySelector<HTMLInputElement>(
    '#route-run-max_run_cost'
  )!;
  expect(iterations.value).toBe('6');
  expect(runCost.value).toBe('1.25');
  toolCalls.value = '4';
  toolCalls.dispatchEvent(new Event('input', { bubbles: true }));
  const mcpItems = [
    ...host.querySelectorAll<HTMLInputElement>('input[type="checkbox"]')
  ].find((item) =>
    item.parentElement?.textContent?.includes(
      'Responses clients support MCP output items'
    )
  );
  expect(mcpItems?.checked).toBe(false);
  mcpItems!.click();
  flushSync();
  host
    .querySelector('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() => expect(routes.replaceRouteDraft).toHaveBeenCalled());
  expect(vi.mocked(routes.replaceRouteDraft).mock.calls[0][2]).toMatchObject({
    tool_execution: 'gateway',
    responses_mcp_items: true,
    toolsets: [selected, added],
    run_limits: { max_iterations: 6, max_tool_calls: 4, max_run_cost: '1.25' }
  });
});

it('opens a route with no saved toolsets and saves a newly selected toolset', async () => {
  vi.clearAllMocks();
  const selected = '0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607';
  const route = buildRoute(undefined);
  vi.mocked(routes.getRouteDraft).mockResolvedValue(route);
  vi.mocked(routes.replaceRouteDraft).mockImplementation(
    async (_id, _etag, input) => ({ ...route, toolsets: input.toolsets })
  );
  vi.mocked(routes.getRoutingPolicy).mockRejectedValue(
    new Error('No route policy')
  );
  vi.mocked(models.listProviderModelInventory).mockResolvedValue([]);
  vi.mocked(agents.listPublications).mockResolvedValue([
    {
      id: selected,
      project_id: 'project',
      name: 'Lookup tools',
      etag: 'etag',
      kind: 'toolset',
      revision_id: 'revision',
      retired: false
    }
  ]);
  const host = document.createElement('div');
  document.body.append(host);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  const component = mount(RouteDraftEditorProbe, {
    target: host,
    props: { client }
  });
  onTestFinished(async () => {
    await unmount(component);
    client.clear();
    host.remove();
  });

  await vi.waitFor(() => expect(host.textContent).toContain('Lookup tools'));
  const checkbox = [
    ...host.querySelectorAll<HTMLInputElement>(
      'fieldset input[type="checkbox"]'
    )
  ].find((item) => item.parentElement?.textContent?.includes('Lookup tools'));
  expect(checkbox?.checked).toBe(false);
  checkbox!.click();
  flushSync();
  host
    .querySelector('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() => expect(routes.replaceRouteDraft).toHaveBeenCalled());
  expect(vi.mocked(routes.replaceRouteDraft).mock.calls[0][2]).toMatchObject({
    tool_execution: 'gateway',
    toolsets: [selected]
  });
});

it('keeps unsaved route edits after a conflict until the user reloads', async () => {
  vi.clearAllMocks();
  const original = buildRoute([]);
  const updated = {
    ...original,
    slug: 'updated-by-another-user',
    etag: 'new-etag'
  };
  vi.mocked(routes.getRouteDraft)
    .mockResolvedValueOnce(original)
    .mockResolvedValue(updated);
  vi.mocked(routes.replaceRouteDraft).mockRejectedValueOnce(
    new ApiProblem({
      type: 'https://openllmproxy.dev/problems/etag_mismatch',
      title: 'Changed elsewhere',
      status: 412
    })
  );
  vi.mocked(routes.getRoutingPolicy).mockRejectedValue(
    new Error('No route policy')
  );
  vi.mocked(models.listProviderModelInventory).mockResolvedValue([]);
  vi.mocked(agents.listPublications).mockResolvedValue([]);
  const host = document.createElement('div');
  document.body.append(host);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  const component = mount(RouteDraftEditorProbe, {
    target: host,
    props: { client }
  });
  onTestFinished(async () => {
    await unmount(component);
    client.clear();
    host.remove();
  });
  await vi.waitFor(() =>
    expect(host.querySelector<HTMLInputElement>('#route-slug')?.value).toBe(
      'workflow'
    )
  );
  const slug = host.querySelector<HTMLInputElement>('#route-slug')!;
  slug.value = 'my-unsaved-change';
  slug.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  const button = (name: string) =>
    [...host.querySelectorAll<HTMLButtonElement>('button')].find(
      (candidate) => candidate.textContent?.trim() === name
    );
  expect(button('Simulate order')?.disabled).toBe(true);
  expect(button('Activate route')?.disabled).toBe(true);
  host
    .querySelector('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() =>
    expect(host.textContent).toContain('This item changed elsewhere')
  );
  expect(slug.value).toBe('my-unsaved-change');
  button('Reload')!.click();
  await vi.waitFor(() =>
    expect(host.querySelector<HTMLInputElement>('#route-slug')?.value).toBe(
      'updated-by-another-user'
    )
  );
});

it('discards a route simulation after the user edits the draft', async () => {
  vi.clearAllMocks();
  const route = { ...buildRoute([]), tool_execution: '' as const };
  vi.mocked(routes.getRouteDraft).mockResolvedValue(route);
  vi.mocked(routes.getRoutingPolicy).mockRejectedValue(
    new Error('No route policy')
  );
  vi.mocked(models.listProviderModelInventory).mockResolvedValue([]);
  vi.mocked(agents.listPublications).mockResolvedValue([]);
  const pending = Promise.withResolvers<routes.RouteSimulation>();
  vi.mocked(routes.simulateRoute).mockImplementation(async (id) => {
    expect(id).toBe(route.id);
    return pending.promise;
  });
  const host = document.createElement('div');
  document.body.append(host);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  const component = mount(RouteDraftEditorProbe, {
    target: host,
    props: { client }
  });
  onTestFinished(async () => {
    await unmount(component);
    client.clear();
    host.remove();
  });
  const button = (name: string) =>
    [...host.querySelectorAll<HTMLButtonElement>('button')].find(
      (item) => item.textContent?.trim() === name
    );
  await vi.waitFor(() =>
    expect(button('Simulate order')?.disabled).toBe(false)
  );
  button('Simulate order')!.click();
  await vi.waitFor(() => expect(host.textContent).toContain('Simulating…'));
  const slug = host.querySelector<HTMLInputElement>('#route-slug')!;
  slug.value = 'changed-route';
  slug.dispatchEvent(new Event('input', { bubbles: true }));
  pending.resolve({
    deterministic_seed: 'old-plan',
    mode: 'weighted',
    operation: 'generation',
    surface: 'openai',
    targets: [],
    selectors: [],
    affinity: null,
    legs: [],
    fallbacks: []
  });
  await vi.waitFor(() => expect(host.textContent).not.toContain('Simulating…'));
  expect(host.textContent).not.toContain('old-plan');
  expect(button('Simulate order')?.disabled).toBe(true);
  expect(button('Activate route')?.disabled).toBe(true);
});

it('validates and activates the saved route with the current revision', async () => {
  vi.clearAllMocks();
  const route = { ...buildRoute([]), tool_execution: '' as const };
  const validated = { ...route, state: 'validated', etag: 'validated-etag' };
  vi.mocked(routes.getRouteDraft).mockResolvedValue(route);
  vi.mocked(routes.getRoutingPolicy).mockRejectedValue(
    new Error('No route policy')
  );
  vi.mocked(models.listProviderModelInventory).mockResolvedValue([]);
  vi.mocked(agents.listPublications).mockResolvedValue([]);
  vi.mocked(routes.validateRoute).mockImplementation(async (current) => {
    expect(current.id).toBe(route.id);
    expect(current.etag).toBe(route.etag);
    return validated;
  });
  vi.mocked(routes.activateRoute).mockImplementation(async (current) => {
    expect(current.id).toBe(route.id);
    expect(current.etag).toBe(validated.etag);
    return {
      route_id: route.id,
      revision_id: 'revision',
      revision: 7,
      draft_etag: 'new-etag',
      runtime_generation: { id: 'generation', sequence: 12 }
    };
  });
  const host = document.createElement('div');
  document.body.append(host);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  const component = mount(RouteDraftEditorProbe, {
    target: host,
    props: { client }
  });
  onTestFinished(async () => {
    await unmount(component);
    client.clear();
    host.remove();
  });
  const button = (name: string) =>
    [...host.querySelectorAll<HTMLButtonElement>('button')].find(
      (item) => item.textContent?.trim() === name
    );
  await vi.waitFor(() =>
    expect(button('Validate draft')?.disabled).toBe(false)
  );
  button('Validate draft')!.click();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Validation passed')
  );
  vi.mocked(routes.getRouteDraft).mockResolvedValue({
    ...route,
    etag: 'new-etag'
  });
  button('Activate route')!.click();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Revision 7 active')
  );
  expect(host.textContent).toContain('Runtime generation 12');
  expect(
    host.querySelector<HTMLAnchorElement>('.activation a')?.getAttribute('href')
  ).toContain('/routes/route/revisions');
});

it('validates a native simulation request and displays a successful preview', async () => {
  vi.clearAllMocks();
  const route = { ...buildRoute([]), tool_execution: '' as const };
  vi.mocked(routes.getRouteDraft).mockResolvedValue(route);
  vi.mocked(routes.getRoutingPolicy).mockRejectedValue(
    new Error('No route policy')
  );
  vi.mocked(models.listProviderModelInventory).mockResolvedValue([]);
  vi.mocked(agents.listPublications).mockResolvedValue([]);
  vi.mocked(routes.simulateRoute).mockImplementation(async (id, input) => {
    expect(id).toBe(route.id);
    expect(input).toMatchObject({
      operation: 'generation',
      surface: 'openai',
      mode: 'unary',
      seed: 'preview',
      request: {
        model: 'workflow',
        messages: [{ role: 'user', content: 'hello' }]
      }
    });
    return {
      deterministic_seed: input.seed ?? '',
      mode: input.mode,
      operation: input.operation,
      surface: input.surface,
      targets: [],
      selectors: [],
      affinity: null,
      legs: [],
      fallbacks: []
    };
  });
  const host = document.createElement('div');
  document.body.append(host);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  const component = mount(RouteDraftEditorProbe, {
    target: host,
    props: { client }
  });
  onTestFinished(async () => {
    await unmount(component);
    client.clear();
    host.remove();
  });
  const button = (name: string) =>
    [...host.querySelectorAll<HTMLButtonElement>('button')].find(
      (item) => item.textContent?.trim() === name
    );
  await vi.waitFor(() =>
    expect(button('Simulate order')?.disabled).toBe(false)
  );
  const request = host.querySelector<HTMLTextAreaElement>(
    '#simulation-request'
  )!;
  request.value = '[]';
  request.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  button('Simulate order')!.click();
  await vi.waitFor(() =>
    expect(host.textContent).toContain(
      'The native request must be a JSON object.'
    )
  );
  request.value =
    '  {"model":"workflow","messages":[{"role":"user","content":"hello"}]}  ';
  request.dispatchEvent(new Event('input', { bubbles: true }));
  const mode = host.querySelector<HTMLSelectElement>('#simulation-mode')!;
  mode.value = 'unary';
  mode.dispatchEvent(new Event('change', { bubbles: true }));
  const seed = host.querySelector<HTMLInputElement>('#simulation-seed')!;
  seed.value = '';
  seed.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  button('Simulate order')!.click();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Attempt explanation')
  );
  expect(host.textContent).toContain('seed: preview');
  expect(host.textContent).not.toContain(
    'The native request must be a JSON object.'
  );
});

it('allows validation to be retried after a temporary failure', async () => {
  vi.clearAllMocks();
  const route = { ...buildRoute([]), tool_execution: '' as const };
  vi.mocked(routes.getRouteDraft).mockResolvedValue(route);
  vi.mocked(routes.getRoutingPolicy).mockRejectedValue(
    new Error('No route policy')
  );
  vi.mocked(models.listProviderModelInventory).mockResolvedValue([]);
  vi.mocked(agents.listPublications).mockResolvedValue([]);
  vi.mocked(routes.validateRoute)
    .mockRejectedValueOnce(new Error('Validation service unavailable'))
    .mockResolvedValueOnce({
      ...route,
      state: 'validated',
      etag: 'validated-etag'
    });
  const host = document.createElement('div');
  document.body.append(host);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  const component = mount(RouteDraftEditorProbe, {
    target: host,
    props: { client }
  });
  onTestFinished(async () => {
    await unmount(component);
    client.clear();
    host.remove();
  });
  const button = (name: string) =>
    [...host.querySelectorAll<HTMLButtonElement>('button')].find(
      (item) => item.textContent?.trim() === name
    );
  await vi.waitFor(() =>
    expect(button('Validate draft')?.disabled).toBe(false)
  );
  button('Validate draft')!.click();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Validation service unavailable')
  );
  expect(button('Validate draft')?.disabled).toBe(false);
  button('Validate draft')!.click();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Validation passed')
  );
  expect(host.textContent).not.toContain('Validation service unavailable');
});

it('deletes a draft only after confirmation and returns to routes', async () => {
  vi.clearAllMocks();
  const route = { ...buildRoute([]), tool_execution: '' as const };
  vi.mocked(routes.getRouteDraft).mockResolvedValue(route);
  vi.mocked(routes.getRoutingPolicy).mockRejectedValue(
    new Error('No route policy')
  );
  vi.mocked(models.listProviderModelInventory).mockResolvedValue([]);
  vi.mocked(agents.listPublications).mockResolvedValue([]);
  const dispatched = Promise.withResolvers<void>();
  const deletion = Promise.withResolvers<void>();
  vi.mocked(routes.deleteRouteDraft).mockImplementation(async (id, etag) => {
    expect(id).toBe(route.id);
    expect(etag).toBe(route.etag);
    dispatched.resolve();
    await deletion.promise;
  });
  const confirm = vi
    .fn()
    .mockImplementationOnce((message: string) => {
      expect(message).toContain(route.slug);
      return false;
    })
    .mockImplementationOnce((message: string) => {
      expect(message).toContain(route.slug);
      return true;
    });
  vi.stubGlobal('confirm', confirm);
  const host = document.createElement('div');
  document.body.append(host);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  const component = mount(RouteDraftEditorProbe, {
    target: host,
    props: { client }
  });
  onTestFinished(async () => {
    await unmount(component);
    client.clear();
    host.remove();
    vi.unstubAllGlobals();
  });
  const button = (name: string) =>
    [...host.querySelectorAll<HTMLButtonElement>('button')].find(
      (item) => item.textContent?.trim() === name
    );
  await vi.waitFor(() => expect(button('Delete draft')?.disabled).toBe(false));
  button('Delete draft')!.click();
  expect(host.querySelector<HTMLInputElement>('#route-slug')?.value).toBe(
    route.slug
  );
  expect(goto).not.toHaveBeenCalled();
  expect(routes.deleteRouteDraft).not.toHaveBeenCalled();
  button('Delete draft')!.click();
  await dispatched.promise;
  await vi.waitFor(() => expect(button('Delete draft')?.disabled).toBe(true));
  deletion.resolve();
  await vi.waitFor(() => expect(goto).toHaveBeenCalledWith('/routes'));
});
