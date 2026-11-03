import { afterEach, describe, expect, it, onTestFinished, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import { json, route, serve } from '$lib/api/test/transport';
import { authLifecycle } from '$lib/features/access/session/lifecycle';
import type { ProviderKindCapability } from './api/models';
import type { Provider, ProviderProbe } from './api/providers';
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

function serveWizard(probe = vi.fn(() => probeResponse(true))) {
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
  serve(handler);
  authLifecycle.establishSession({
    user: {
      id: 'owner',
      email: 'owner@example.test',
      display_name: 'Owner',
      role: 'owner',
      access_scope: 'global',
      operations: ['read', 'configure']
    },
    csrf_token: 'fixture-csrf'
  });
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
});
