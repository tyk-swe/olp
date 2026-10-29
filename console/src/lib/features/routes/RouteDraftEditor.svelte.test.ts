import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { expect, it, onTestFinished, vi } from 'vitest';
import * as routes from './api';
import * as agents from '$lib/features/agents/api';
import * as models from '$lib/features/providers/api/models';
import RouteDraftEditorProbe from './test/RouteDraftEditorProbe.svelte';

vi.mock('./api', async (original) => ({
  ...(await original<typeof import('./api')>()),
  getRouteDraft: vi.fn(),
  replaceRouteDraft: vi.fn(),
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
