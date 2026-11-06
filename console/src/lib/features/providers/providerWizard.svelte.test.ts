import { afterEach, describe, expect, it, onTestFinished, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import { json, route, serve } from '$lib/management/test/transport';
import type { ProviderKindCapability } from './models';
import type { Provider, ProviderProbe } from './providers';
import StateHarness from '../../../../tests/unit/support/StateHarness.svelte';
import { ProviderWizardState } from './providerWizard.svelte';

const spec: ProviderKindCapability = {
  kind: 'openai_compatible',
  label: 'Compatible provider',
  description: '',
  default_auth_mode: 'api_key',
  auth_modes: [{ mode: 'api_key', label: 'API key', credential: 'required' }],
  fields: [{ field: 'endpoint', label: 'Endpoint', required: true }],
  presets: []
};

const provider: Provider = {
  id: 'provider-id',
  etag: 'provider-version',
  name: 'Service',
  configuration: {
    kind: 'openai_compatible',
    auth_mode: 'api_key',
    endpoint: 'https://example.test/v1'
  },
  capability_count: 0,
  certified_capability_count: 0,
  enabled_model_count: 0,
  model_count: 0,
  connector_ready: false,
  pending_activation: true,
  project_id: null,
  project_name: null,
  state: 'draft',
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z'
};

function probeResponse(succeeded: boolean): Response {
  const body: ProviderProbe = {
    checked_at: '2026-01-01T00:00:00Z',
    detail: succeeded ? 'Connection succeeded' : 'Connection failed',
    probe_type: 'connection',
    provider_id: provider.id,
    succeeded
  };
  return new Response(JSON.stringify(body), {
    headers: {
      'Content-Type': 'application/json',
      ETag: provider.etag
    }
  });
}

function serveWizard(
  probe = vi.fn(() => probeResponse(true)),
  override?: (request: Request) => Response | Promise<Response> | undefined
) {
  const create = vi.fn(async (request: Request) => {
    route(request, 'POST', '/api/v1/providers');
    expect(request.headers.get('Idempotency-Key')).toBeTruthy();
    expect(await request.json()).toMatchObject({
      name: 'Service',
      credential: 'secret',
      configuration: {
        kind: 'openai_compatible',
        endpoint: 'https://example.test/v1',
        auth_mode: 'api_key'
      }
    });
    return json({ id: provider.id });
  });
  const handler = vi.fn((request: Request) => {
    const response = override?.(request);
    if (response) return response;
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/provider-kinds') return json({ items: [spec] });
    if (path === '/api/v1/providers' && request.method === 'POST')
      return create(request);
    if (path === `/api/v1/providers/${provider.id}/models`)
      return json({ items: [], provider });
    if (path === `/api/v1/providers/${provider.id}/probe`) {
      route(request, 'POST', path);
      expect(request.headers.get('If-Match')).toBe(provider.etag);
      return probe();
    }
    if (path === '/api/v1/provider-kinds/openai_compatible/capabilities')
      return json({ provider_kind: spec.kind, capabilities: [] });
    return json({ title: `Unexpected request ${request.method} ${path}` }, 404);
  });
  serve(handler, ['read', 'configure']);
  return { create, handler, probe };
}

async function wizard(): Promise<ProviderWizardState> {
  const ready = vi.fn((state: ProviderWizardState) => state);
  const component = mount(StateHarness, {
    target: document.createElement('div'),
    props: { setup: () => ready(new ProviderWizardState()) }
  });
  onTestFinished(() => unmount(component));
  flushSync();
  const state = ready.mock.results[0].value;
  await vi.waitFor(() => expect(state.draft).not.toBeNull());
  return state;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('provider wizard', () => {
  it('shows an invalid external credential error without submitting', async () => {
    const { create } = serveWizard();
    const state = await wizard();
    const draft = state.draft;
    expect(draft).not.toBeNull();
    if (!draft) return;
    draft.name = 'Service';
    draft.endpoint = 'https://example.test/v1';
    draft.useExternalCredential = true;
    draft.externalCredentialReference = '{broken';

    await state.createDraft(new SubmitEvent('submit', { cancelable: true }));

    expect(state.errorMessage).toBe(
      'Enter a JSON object with store, secret_id and immutable version.'
    );
    expect(create).not.toHaveBeenCalled();
    expect(state.wizardStep).toBe(1);
  });

  it('saves, clears the pasted credential and tests the connection', async () => {
    const { create, probe } = serveWizard();
    const state = await wizard();
    const draft = state.draft;
    expect(draft).not.toBeNull();
    if (!draft) return;
    draft.name = 'Service';
    draft.endpoint = 'https://example.test/v1';
    draft.credential = 'secret';

    await state.createDraft(new SubmitEvent('submit', { cancelable: true }));

    expect(create).toHaveBeenCalledOnce();
    expect(probe).toHaveBeenCalledOnce();
    expect(draft.credential).toBe('');
    expect(state.providerId).toBe(provider.id);
    expect(state.probe?.succeeded).toBe(true);
    expect(state.wizardStep).toBe(2);
  });

  it('retries a failed connection test without creating the saved provider again', async () => {
    const probe = vi
      .fn(() => probeResponse(true))
      .mockReturnValueOnce(probeResponse(false));
    const { create } = serveWizard(probe);
    const state = await wizard();
    const draft = state.draft;
    expect(draft).not.toBeNull();
    if (!draft) return;
    draft.name = 'Service';
    draft.endpoint = 'https://example.test/v1';
    draft.credential = 'secret';

    await state.createDraft(new SubmitEvent('submit', { cancelable: true }));
    expect(state.errorMessage).toBe('Connection failed');
    expect(state.wizardStep).toBe(1);
    expect(draft.credential).toBe('');
    await state.createDraft(new SubmitEvent('submit', { cancelable: true }));

    expect(create).toHaveBeenCalledOnce();
    expect(probe).toHaveBeenCalledTimes(2);
    expect(state.errorMessage).toBe('');
    expect(state.wizardStep).toBe(2);
  });
  it('retries an uncertain save with the original values and request identity', async () => {
    const { create } = serveWizard();
    create.mockResolvedValueOnce(
      json({ title: 'Unavailable', status: 503 }, 503)
    );
    const state = await wizard();
    const draft = state.draft;
    expect(draft).not.toBeNull();
    if (!draft) return;
    draft.name = 'Service';
    draft.endpoint = 'https://example.test/v1';
    draft.credential = 'secret';

    await state.createDraft(new SubmitEvent('submit', { cancelable: true }));
    expect(state.errorMessage).toBe('Unavailable');
    draft.name = 'Edited';
    await state.createDraft(new SubmitEvent('submit', { cancelable: true }));
    expect(state.errorMessage).toContain('Retry its original fields');
    expect(create).toHaveBeenCalledOnce();

    draft.name = 'Service';
    await state.createDraft(new SubmitEvent('submit', { cancelable: true }));
    expect(create).toHaveBeenCalledTimes(2);
    expect(create.mock.calls[0][0].headers.get('Idempotency-Key')).toBeTruthy();
    expect(create.mock.calls[1][0].headers.get('Idempotency-Key')).toBe(
      create.mock.calls[0][0].headers.get('Idempotency-Key')
    );
    expect(state.wizardStep).toBe(2);
  });

  it('lets a rejected save be corrected with a fresh request identity', async () => {
    const { create } = serveWizard();
    create.mockResolvedValueOnce(
      json({ title: 'Invalid credential', status: 400 }, 400)
    );
    const state = await wizard();
    const draft = state.draft;
    expect(draft).not.toBeNull();
    if (!draft) return;
    draft.name = 'Service';
    draft.endpoint = 'https://example.test/v1';
    draft.credential = 'invalid';

    await state.createDraft(new SubmitEvent('submit', { cancelable: true }));
    expect(state.errorMessage).toBe('Invalid credential');
    draft.credential = 'secret';
    await state.createDraft(new SubmitEvent('submit', { cancelable: true }));

    expect(create).toHaveBeenCalledTimes(2);
    expect(create.mock.calls[0][0].headers.get('Idempotency-Key')).toBeTruthy();
    expect(create.mock.calls[1][0].headers.get('Idempotency-Key')).not.toBe(
      create.mock.calls[0][0].headers.get('Idempotency-Key')
    );
    expect(state.wizardStep).toBe(2);
  });

  it('restores the saved connection when going back and clears secrets on reset', async () => {
    serveWizard();
    const state = await wizard();
    const draft = state.draft;
    expect(draft).not.toBeNull();
    if (!draft) return;
    draft.name = 'Service';
    draft.endpoint = 'https://example.test/v1';
    draft.credential = 'secret';
    await state.createDraft(new SubmitEvent('submit', { cancelable: true }));
    flushSync();

    state.goBack();
    expect(state.wizardStep).toBe(1);
    expect(state.draft).toMatchObject({
      name: 'Service',
      endpoint: 'https://example.test/v1',
      credential: ''
    });
    const previous = state.draft;
    expect(previous).not.toBeNull();
    if (!previous) return;
    previous.credential = 'replacement';
    previous.externalCredentialReference = 'reference';
    state.startAnother();
    flushSync();

    expect(previous.credential).toBe('');
    expect(previous.externalCredentialReference).toBe('');
    expect(state.providerId).toBe('');
    expect(state.wizardStep).toBe(1);
    expect(state.probe).toBeNull();
    await vi.waitFor(() => expect(state.draft?.name).toBe(''));
  });

  it('offers manual model declaration when discovery returns no models', async () => {
    const payload = vi.fn<(body: unknown) => void>();
    const discovery = vi.fn(async (request: Request) => {
      route(request, 'POST', `/api/v1/providers/${provider.id}/discovery`);
      payload(await request.json());
      return json({
        ...provider,
        model_count: payload.mock.calls.length === 1 ? 0 : 2
      });
    });
    serveWizard(undefined, (request) =>
      new URL(request.url).pathname.endsWith('/discovery')
        ? discovery(request)
        : undefined
    );
    const state = await wizard();
    const draft = state.draft;
    expect(draft).not.toBeNull();
    if (!draft) return;
    draft.name = 'Service';
    draft.endpoint = 'https://example.test/v1';
    draft.credential = 'secret';
    await state.createDraft(new SubmitEvent('submit', { cancelable: true }));
    await vi.waitFor(() => expect(state.wizardProvider?.id).toBe(provider.id));

    await state.discoverWizardProvider();
    expect(state.errorMessage).toContain('Use the manual identifier fallback');
    state.manualModelNames = 'model-a\nmodel-b';
    await state.declareWizardModels();

    expect(discovery).toHaveBeenCalledTimes(2);
    expect(payload).toHaveBeenNthCalledWith(1, { models: [] });
    expect(payload).toHaveBeenNthCalledWith(2, {
      models: [
        { upstream_model: 'model-a', display_name: 'model-a' },
        { upstream_model: 'model-b', display_name: 'model-b' }
      ]
    });
    expect(state.errorMessage).toBe('');
    expect(state.manualModelNames).toBe('');
    expect(state.wizardStep).toBe(2);
  });

  it('retries an uncertain activation before allowing connection changes', async () => {
    const activate = vi
      .fn((request: Request) => {
        route(request, 'POST', `/api/v1/providers/${provider.id}/activate`);
        return json({ runtime_generation: { sequence: 42 } });
      })
      .mockReturnValueOnce(json({ title: 'Unavailable', status: 503 }, 503));
    serveWizard(undefined, (request) =>
      new URL(request.url).pathname.endsWith('/activate')
        ? activate(request)
        : undefined
    );
    const state = await wizard();
    const draft = state.draft;
    expect(draft).not.toBeNull();
    if (!draft) return;
    draft.name = 'Service';
    draft.endpoint = 'https://example.test/v1';
    draft.credential = 'secret';
    await state.createDraft(new SubmitEvent('submit', { cancelable: true }));
    await vi.waitFor(() => expect(state.wizardProvider?.id).toBe(provider.id));

    await state.activateWizardProvider();
    expect(state.errorMessage).toBe('Unavailable');
    state.goBack();
    expect(state.errorMessage).toContain(
      'Activation may already have committed'
    );
    expect(state.wizardStep).toBe(2);
    await state.activateWizardProvider();

    expect(activate).toHaveBeenCalledTimes(2);
    const firstId = activate.mock.calls[0][0].headers.get('Idempotency-Key');
    expect(firstId).toBeTruthy();
    expect(activate.mock.calls[1][0].headers.get('Idempotency-Key')).toBe(
      firstId
    );
    expect(state.wizardStep).toBe(3);
    expect(state.notice).toContain('runtime generation 42');
  });
  it('retries refresh after confirmed activation without activating twice', async () => {
    let activated = false;
    let refreshUnavailable = true;
    const activate = vi.fn(() => {
      activated = true;
      return json({ runtime_generation: { sequence: 42 } });
    });
    serveWizard(undefined, (request) => {
      const path = new URL(request.url).pathname;
      if (path.endsWith('/activate')) return activate();
      if (activated && refreshUnavailable && path.endsWith('/models'))
        return json({ title: 'Refresh unavailable', status: 503 }, 503);
      return undefined;
    });
    const state = await wizard();
    const draft = state.draft;
    expect(draft).not.toBeNull();
    if (!draft) return;
    draft.name = 'Service';
    draft.endpoint = 'https://example.test/v1';
    draft.credential = 'secret';
    await state.createDraft(new SubmitEvent('submit', { cancelable: true }));
    await state.activateWizardProvider();
    expect(state.errorMessage).toContain('Refresh unavailable');
    refreshUnavailable = false;
    await state.activateWizardProvider();
    expect(activate).toHaveBeenCalledTimes(1);
    expect(state.wizardStep).toBe(3);
    expect(state.errorMessage).toBe('');
  });
});
