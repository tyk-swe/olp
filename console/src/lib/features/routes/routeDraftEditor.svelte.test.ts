import { templateFor } from '$lib/features/inference/playground/templates';
import type { OperationDialect } from './inspectionDialects';
import RoutePublishPanel from './RoutePublishPanel.svelte';
import { afterEach, describe, expect, it, onTestFinished, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import { json, route, serve } from '$lib/management/test/transport';
import StateHarness from '../../../../tests/unit/support/StateHarness.svelte';
import type { RouteActivation, RouteDraft, RouteSimulation } from './api';
import { routeKeys } from './routeKeys';
import { RouteDraftEditorState } from './routeDraftEditor.svelte';

vi.mock('$app/navigation', () => ({ beforeNavigate: vi.fn(), goto: vi.fn() }));

const draft: RouteDraft = {
  id: 'draft-id',
  etag: 'version-1',
  slug: 'example',
  state: 'draft',
  project_id: null,
  project_name: null,
  operations: ['generation'],
  overall_timeout_ms: 30000,
  max_attempts: 2,
  fidelity: { mode: 'strict' },
  content_policy: null,
  targets: [
    {
      id: 'target-id',
      provider_id: 'provider-id',
      provider_model_id: 'model-id',
      provider_name: 'Provider',
      provider_model: 'model',
      available: true,
      lifecycle: null,
      position: 0,
      priority: 1,
      weight: 100,
      timeout_ms: 10000,
      tags: [],
      shadow: null
    }
  ],
  fallbacks: [],
  selectors: [],
  retry: {},
  affinity: null,
  budget: null,
  passthrough: null,
  certification: 'uncertified',
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z'
};

const preview: RouteSimulation = {
  deterministic_seed: 'seed',
  mode: 'streaming',
  operation: 'generation',
  surface: 'openai',
  targets: [],
  selectors: [],
  affinity: null,
  legs: [],
  fallbacks: []
};

const activation: RouteActivation = {
  draft_etag: 'version-2',
  revision: 3,
  revision_id: 'revision-id',
  route_id: 'route-id',
  runtime_generation: { id: 'generation-id', sequence: 4 }
};

function serveEditor(dialects: OperationDialect[] = []) {
  const load = vi.fn(() => json(draft));
  const replace = vi.fn(async (request: Request) => {
    route(request, 'PUT', `/api/v1/route-drafts/${draft.id}`);
    expect(request.headers.get('If-Match')).toBe(draft.etag);
    expect(await request.json()).toMatchObject({
      slug: 'updated',
      operations: ['generation'],
      targets: [{ provider_model_id: 'model-id' }]
    });
    return json({ ...draft, slug: 'updated', etag: 'version-2' });
  });
  const activate = vi.fn((request: Request) => {
    route(request, 'POST', `/api/v1/route-drafts/${draft.id}/activate`);
    expect(request.headers.get('If-Match')).toBe(draft.etag);
    expect(request.headers.get('Idempotency-Key')).toBeTruthy();
    load.mockReturnValue(json({ ...draft, etag: activation.draft_etag }));
    return json(activation);
  });
  const simulate = vi.fn(async (request: Request) => {
    route(request, 'POST', `/api/v1/route-drafts/${draft.id}/simulate`);
    expect(await request.json()).toMatchObject({
      operation: 'generation',
      surface: 'openai',
      mode: 'streaming',
      seed: 'setup-preview',
      preferences: {}
    });
    return json(preview);
  });
  serve(
    (request) => {
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/operation-dialects')
        return json({ items: dialects });
      if (path === '/api/v1/provider-models') return json({ items: [] });
      if (path === `/api/v1/route-drafts/${draft.id}/activate`)
        return activate(request);
      if (path === `/api/v1/route-drafts/${draft.id}/simulate`)
        return simulate(request);
      if (path === `/api/v1/route-drafts/${draft.id}`) {
        if (request.method === 'PUT') return replace(request);
        return load();
      }
      return json(
        { title: `Unexpected request ${request.method} ${path}` },
        404
      );
    },
    ['read', 'configure']
  );
  return { load, replace, simulate, activate };
}

async function editor(): Promise<RouteDraftEditorState> {
  const ready = vi.fn((state: RouteDraftEditorState) => state);
  const component = mount(StateHarness, {
    target: document.createElement('div'),
    props: { setup: () => ready(new RouteDraftEditorState(() => draft.id)) }
  });
  onTestFinished(() => unmount(component));
  flushSync();
  const state = ready.mock.results[0].value;
  await vi.waitFor(() => expect(state.slug).toBe(draft.slug));
  return state;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('route draft editor', () => {
  it('hydrates a draft and maintains operation and policy choices', async () => {
    serveEditor();
    const state = await editor();
    expect(state.targets).toMatchObject([{ providerModelId: 'model-id' }]);
    expect(state.hasUnsavedChanges).toBe(false);

    state.toggleOperation('embeddings', true);
    state.toggleOperation('embeddings', true);
    state.addPolicyRule();
    flushSync();

    expect(state.operations).toEqual(['generation', 'embeddings']);
    expect(state.policyRules).toMatchObject([
      { phase: 'input', action: 'block' }
    ]);
    expect(state.hasUnsavedChanges).toBe(true);
    state.removePolicyRule(0);
    state.toggleOperation('embeddings', false);
    expect(state.policyRules).toEqual([]);
    expect(state.operations).toEqual(['generation']);
  });

  it('saves edits using the loaded version and clears the unsaved state', async () => {
    const { replace } = serveEditor();
    const state = await editor();
    await state.save(draft);
    expect(state.notice).toBe('No unsaved changes.');
    expect(replace).not.toHaveBeenCalled();

    state.slug = 'updated';
    state.touch();
    await state.save(draft);
    flushSync();

    expect(replace).toHaveBeenCalledOnce();
    expect(state.hasUnsavedChanges).toBe(false);
    expect(state.sync.snapshotEtag).toBe('version-2');
    expect(state.notice).toContain('Draft saved');
  });

  it('preserves edits made while a save is in flight', async () => {
    const { replace } = serveEditor();
    const state = await editor();
    const pending = Promise.withResolvers<Response>();
    replace.mockImplementationOnce(() => pending.promise);
    state.slug = 'first-edit';
    state.touch();
    const saving = state.save(draft);
    await vi.waitFor(() => expect(replace).toHaveBeenCalledOnce());

    state.slug = 'later-edit';
    state.touch();
    pending.resolve(json({ ...draft, slug: 'first-edit', etag: 'version-2' }));
    await saving;
    flushSync();

    expect(state.slug).toBe('later-edit');
    expect(state.hasUnsavedChanges).toBe(true);
    expect(state.notice).toBe(
      'Draft saved. You have additional unsaved changes.'
    );
    expect(state.sync.snapshotEtag).toBe('version-2');
  });

  it('retains local edits and reports a conflict when the saved version is stale', async () => {
    const { replace } = serveEditor();
    const state = await editor();
    replace.mockImplementationOnce(() =>
      Promise.resolve(
        json(
          {
            type: 'https://openllmproxy.dev/problems/etag_mismatch',
            title: 'The draft changed',
            status: 412
          },
          412
        )
      )
    );
    state.slug = 'local';
    state.touch();

    await state.save(draft);
    flushSync();

    expect(state.slug).toBe('local');
    expect(state.hasUnsavedChanges).toBe(true);
    expect(state.concurrentNotice).toBe('conflict');
  });

  it('activates only a saved draft and reports its published revision', async () => {
    const { activate } = serveEditor();
    const state = await editor();
    await state.activate(draft);
    flushSync();
    expect(activate).toHaveBeenCalledOnce();
    expect(state.activation).toEqual(activation);
    expect(state.notice).toBe(
      'Route activated as revision 3 in runtime generation 4.'
    );

    state.touch();
    flushSync();
    await state.activate({ ...draft, etag: activation.draft_etag });
    expect(activate).toHaveBeenCalledOnce();
  });

  it('rejects a non-object simulation request before sending it', async () => {
    const { simulate } = serveEditor();
    const state = await editor();
    state.simulationRequestJson = '[]';
    await state.simulate(draft);
    expect(simulate).not.toHaveBeenCalled();
    expect(state.errorMessage).toBe(
      'The native request must be a JSON object.'
    );
  });

  it('keeps local edits when a newer draft arrives and reloads on request', async () => {
    const { load } = serveEditor();
    const state = await editor();
    state.slug = 'local';
    state.touch();
    const remote = { ...draft, slug: 'remote', etag: 'version-2' };
    state.queryClient.setQueryData(routeKeys.draft(draft.id), remote);
    flushSync();

    expect(state.slug).toBe('local');
    expect(state.concurrentNotice).toBe('newer');
    load.mockImplementation(() => json(remote));
    await state.reload();
    flushSync();
    await vi.waitFor(() => expect(state.slug).toBe('remote'));
    expect(state.hasUnsavedChanges).toBe(false);
    expect(state.concurrentNotice).toBeNull();
  });

  it('only simulates saved drafts and clears the preview after edits', async () => {
    const { simulate } = serveEditor();
    const state = await editor();
    await state.simulate(draft);
    expect(state.simulation).toEqual(preview);
    expect(simulate).toHaveBeenCalledOnce();

    state.touch();
    flushSync();
    expect(state.simulation).toBeNull();
    await state.simulate(draft);
    expect(simulate).toHaveBeenCalledOnce();
  });
});

it.each([
  { key: 'legacy-completions', surface: 'openai', mode: 'streaming' },
  { key: 'legacy-completions', surface: 'openai', mode: 'unary' },
  { key: 'generation-multiturn', surface: 'openai', mode: 'streaming' },
  { key: 'anthropic-messages', surface: 'anthropic', mode: 'streaming' },
  { key: 'gemini-generate', surface: 'gemini', mode: 'streaming' },
  { key: 'cohere-typed-embeddings', surface: 'native', mode: 'unary' }
])(
  'submits a compatible $mode simulation for $key',
  async ({ key, surface, mode }) => {
    const template = templateFor(key);
    expect(template).toBeDefined();
    if (!template) return;
    const dialect = template.inspectDialect ?? template.nativeDialect ?? '';
    const registered: OperationDialect[] = template.nativeDialect
      ? [
          {
            id: dialect,
            revision: '1',
            operation: template.operation,
            operation_revision: '1',
            surface,
            mode: 'unary',
            label: template.label,
            request_schema: {},
            result_schema: {},
            documentation: '',
            evidence: ''
          }
        ]
      : [];
    const { load, simulate } = serveEditor(registered);
    const current = { ...draft, operations: [template.operation] };
    load.mockImplementation(() => json(current));
    const state = await editor();
    await vi.waitFor(() => expect(state.dialects.isSuccess).toBe(true));
    state.simulationSurface = template.surface ?? 'openai';
    state.simulationMode = mode;
    const target = document.createElement('div');
    const panel = mount(RoutePublishPanel, {
      target,
      props: { editor: state }
    });
    onTestFinished(() => unmount(panel));
    flushSync();
    const example = target.querySelector<HTMLSelectElement>(
      '#simulation-example'
    );
    expect(example).not.toBeNull();
    if (!example) return;
    example.value = key;
    example.dispatchEvent(new Event('change', { bubbles: true }));
    flushSync();
    const result = { ...preview, operation: template.operation, surface, mode };
    simulate.mockImplementationOnce(async () => json(result));
    await state.simulate(current);
    expect(simulate).toHaveBeenCalledOnce();
    const submitted = await simulate.mock.calls[0][0].json();
    expect(submitted).toMatchObject({
      surface,
      mode,
      ...(dialect ? { dialect } : {}),
      request: {
        ...template.request,
        ...('model' in template.request ? { model: current.slug } : {})
      }
    });
    if (template.operation === 'generation' && surface !== 'gemini') {
      expect(Boolean(submitted.request.stream)).toBe(mode === 'streaming');
    }
    expect(state.simulation).toEqual(result);
  }
);
