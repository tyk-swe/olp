import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { apiClient } from '$lib/api/client';
import { providerKeys } from '$lib/features/providers/providerKeys';
import { routeKeys } from '$lib/features/routes/routeKeys';
import {
  listProviderModelInventory,
  type ProviderModelInventory
} from '$lib/features/providers/models';
import {
  diffRouteRevisions,
  getRouteDraft,
  replaceRouteDraft,
  type RouteRevision
} from '$lib/features/routes/api';
import InteractionInspector from './InteractionInspector.svelte';
import RouteFidelityProbe from './test/RouteFidelityProbe.svelte';
import { draft } from '$lib/forms/test/draftFixtures';

vi.mock('$app/navigation', () => ({ beforeNavigate: vi.fn(), goto: vi.fn() }));
vi.mock('$app/state', () => ({
  page: { url: new URL('http://localhost/routes/route-a') }
}));
vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({ can: () => true })
}));
vi.mock('$lib/features/providers/models', async (original) => ({
  ...(await original<typeof import('$lib/features/providers/models')>()),
  listProviderModelInventory: vi.fn()
}));
vi.mock('$lib/features/routes/api', async (original) => ({
  ...(await original<typeof import('$lib/features/routes/api')>()),
  getRouteDraft: vi.fn(),
  replaceRouteDraft: vi.fn(),
  diffRouteRevisions: vi.fn()
}));

const model: ProviderModelInventory = {
  available: true,
  metadata: {
    canonical_model: null,
    context_length: null,
    data_collection: null,
    deployment: null,
    input_modalities: ['text'],
    max_output_tokens: null,
    output_modalities: ['text'],
    quantization: null,
    region: null,
    source: null,
    observed_at: null,
    supported_parameters: null,
    zero_data_retention: null
  },
  provider_id: 'provider-a',
  provider_name: 'Original provider',
  provider_kind: 'openai',
  model: {
    id: 'model-a',
    upstream_model: 'test-model',
    display_name: 'test-model',
    enabled: true,
    capabilities: [
      {
        operation: 'generation',
        surface: 'openai',
        mode: 'streaming',
        source: 'certified'
      }
    ]
  }
};

function revision(
  number: number,
  mode: 'strict' | 'transformed'
): RouteRevision {
  return {
    ...draft,
    route_id: 'route-a',
    id: `revision-${number}`,
    revision: number,
    activated_at: draft.updated_at,
    activated_by: 'owner',
    source_draft_id: draft.id,
    routing_policy: {
      allowed_strategies: null,
      constraints: {
        deny_data_collection: false,
        ignore: [],
        max_price: null,
        only: null,
        quantizations: null,
        regions: null,
        require_parameters: false,
        require_zero_data_retention: false
      },
      defaults: {
        allow_fallbacks: null,
        deny_data_collection: false,
        ignore: [],
        max_price: null,
        only: null,
        order: null,
        preferred_max_latency_ms: null,
        preferred_min_throughput: null,
        quantizations: null,
        regions: null,
        require_parameters: false,
        require_zero_data_retention: false,
        strategy: null
      }
    },
    fidelity: { mode }
  };
}

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;

beforeEach(() => {
  vi.resetAllMocks();
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
  client.setQueryData(routeKeys.draft(draft.id), draft);
  client.setQueryData(providerKeys.enabledModels(), [model]);
  vi.mocked(getRouteDraft).mockResolvedValue(draft);
  vi.mocked(listProviderModelInventory).mockResolvedValue([model]);
  vi.spyOn(apiClient, 'GET').mockResolvedValue({
    data: { policy: {}, etag: 'p0' },
    response: new Response(null, { status: 200 })
  } as never);
});

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  client.clear();
  host.remove();
  vi.restoreAllMocks();
});

function render(view: 'editor' | 'history', routeId?: string) {
  component = mount(RouteFidelityProbe, {
    target: host,
    props: { client, view, routeId }
  });
  flushSync();
}

async function fidelitySelect() {
  let select: HTMLSelectElement | null = null;
  await vi.waitFor(() => {
    flushSync();
    select = host.querySelector<HTMLSelectElement>('#route-fidelity');
    expect(select).not.toBeNull();
  });
  return select!;
}

function button(label: string) {
  const result = [...host.querySelectorAll('button')].find(
    (item) => item.textContent?.trim() === label
  );
  if (!result) throw new Error(`Missing button: ${label}`);
  return result;
}

describe('route fidelity editor', () => {
  it('offers strict and transformed with strict preselected for a new route', async () => {
    render('editor');
    const select = await fidelitySelect();
    expect([...select.options].map((option) => option.value)).toEqual([
      'strict',
      'transformed'
    ]);
    expect(select.value).toBe('strict');
    const editor = host.querySelector('.fidelity-editor')!;
    expect(editor.textContent?.toLowerCase()).not.toContain('legacy');
  });

  it('saves a declared transformed route as a change of the existing draft', async () => {
    render('editor', 'route-a');
    const select = await fidelitySelect();
    await vi.waitFor(() => {
      flushSync();
      expect(host.querySelector<HTMLInputElement>('#route-slug')?.value).toBe(
        draft.slug
      );
    });
    expect(select.value).toBe('strict');
    select.value = 'transformed';
    select.dispatchEvent(new Event('change', { bubbles: true }));
    flushSync();
    expect(host.querySelector('.change-note')?.textContent).toContain(
      'strict → transformed'
    );
    vi.mocked(replaceRouteDraft).mockResolvedValue({
      ...draft,
      etag: 'v2',
      fidelity: { mode: 'transformed' }
    });
    host
      .querySelector('form.studio')!
      .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await vi.waitFor(() =>
      expect(replaceRouteDraft).toHaveBeenCalledWith(
        draft.id,
        draft.etag,
        expect.objectContaining({ fidelity: { mode: 'transformed' } })
      )
    );
  });
});

describe('route revision history', () => {
  it('labels each revision and a fidelity change as strict or transformed', async () => {
    client.setQueryData(routeKeys.revisions('route-a'), [
      revision(2, 'transformed'),
      revision(1, 'strict')
    ]);
    vi.mocked(diffRouteRevisions).mockResolvedValue({
      from_revision: 1,
      to_revision: 2,
      slug_changed: false,
      timeout_changed: false,
      max_attempts_changed: false,
      content_policy_changed: false,
      fidelity_changed: true,
      fidelity_before: { mode: 'strict' },
      fidelity_after: { mode: 'transformed' },
      routing_policy_changed: false,
      routing_policy_before: revision(1, 'strict').routing_policy,
      routing_policy_after: revision(2, 'transformed').routing_policy,
      operations_added: [],
      operations_removed: [],
      targets_added: [],
      targets_removed: [],
      targets_changed: []
    });
    render('history');
    const cells = [
      ...host.querySelectorAll<HTMLElement>('td[data-label="Fidelity"]')
    ].map((cell) => cell.textContent?.trim());
    expect(cells).toEqual(['Transformed', 'Strict']);
    button('Compare').click();
    const differences = await vi.waitFor(() => {
      flushSync();
      const section = host.querySelector('[aria-label="Revision differences"]');
      expect(section).not.toBeNull();
      return section!;
    });
    expect(differences.textContent).toMatch(/Strict\s*→\s*Transformed/);
    expect(host.textContent?.toLowerCase()).not.toContain('legacy');
  });
});

describe('interaction inspector', () => {
  it('reports a transformed preview in transformed vocabulary', () => {
    component = mount(InteractionInspector, {
      target: host,
      props: {
        inspection: {
          status: 'transformed',
          fidelity: 'transformed',
          class: 'transformed',
          evidence: []
        }
      }
    });
    flushSync();
    expect(host.textContent).toContain('transformed route');
    expect(host.textContent).toContain(
      'Transformed route behavior is shown without a strict interaction'
    );
    expect(host.textContent?.toLowerCase()).not.toContain('legacy');
  });
});
